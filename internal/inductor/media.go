// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var Playable = []string{".mp3", ".m4a", ".m4b", ".aac", ".ogg", ".oga", ".opus", ".wav", ".flac"}
var Video = []string{".mp4", ".m4v", ".mov", ".mkv", ".webm", ".avi", ".wmv", ".flv"}
var suffixFor = map[string]string{"mp3": ".mp3", "aac": ".m4a", "alac": ".m4a", "flac": ".flac", "opus": ".opus", "vorbis": ".ogg", "pcm_s16le": ".wav", "pcm_s24le": ".wav", "pcm_f32le": ".wav", "pcm_u8": ".wav"}

func Sniff(path string) string {
	f, e := os.Open(path)
	if e != nil {
		return ""
	}
	defer f.Close()
	b := make([]byte, 12)
	n, _ := io.ReadFull(f, b)
	b = b[:n]
	if n >= 3 && string(b[:3]) == "ID3" {
		return "mp3"
	}
	if n >= 2 && b[0] == 255 && b[1]&224 == 224 {
		return "mp3"
	}
	if n >= 4 {
		switch string(b[:4]) {
		case "fLaC":
			return "flac"
		case "OggS":
			return "vorbis"
		}
	}
	if n == 12 {
		if string(b[:4]) == "RIFF" && string(b[8:]) == "WAVE" {
			return "pcm_s16le"
		}
		if string(b[4:8]) == "ftyp" && (string(b[8:11]) == "M4A" || string(b[8:11]) == "M4B") {
			return "aac"
		}
	}
	return ""
}
func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	b, e := cmd.Output()
	if e != nil {
		if x, ok := e.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%s: %s", name, strings.TrimSpace(string(x.Stderr)))
		}
		return nil, fmt.Errorf("%s: %w", name, e)
	}
	return b, nil
}
func Inspect(ctx context.Context, path string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	b, e := command(ctx, "ffprobe", "-v", "quiet", "-show_entries", "stream=codec_type,codec_name", "-of", "json", path)
	if e != nil {
		return "", false
	}
	var r Record
	if json.Unmarshal(b, &r) != nil {
		return "", false
	}
	audio := ""
	picture := false
	for _, x := range array(r["streams"]) {
		s := record(x)
		if str(s["codec_type"]) == "audio" && audio == "" {
			audio = str(s["codec_name"])
		}
		picture = picture || str(s["codec_type"]) == "video"
	}
	return audio, picture
}
func NeedsWork(ctx context.Context, path, mode string) string {
	suffix := strings.ToLower(filepath.Ext(path))
	if mode == "never" {
		return "none"
	}
	if contains(Video, suffix) {
		return "demux"
	}
	if mode == "always" {
		return "transcode"
	}
	if contains(Playable, suffix) || Sniff(path) != "" {
		return "none"
	}
	codec, video := Inspect(ctx, path)
	if video {
		return "demux"
	}
	if suffixFor[codec] != "" {
		return "none"
	}
	return "transcode"
}
func ServedAs(ctx context.Context, path string) string {
	s := strings.ToLower(filepath.Ext(path))
	if contains(Playable, s) {
		return s
	}
	c := Sniff(path)
	if c == "" {
		c, _ = Inspect(ctx, path)
	}
	if suffixFor[c] != "" {
		return suffixFor[c]
	}
	return s
}
func HoldsAudio(ctx context.Context, path string) bool {
	s, e := os.Stat(path)
	if e != nil || s.IsDir() || s.Size() < 1024 {
		return false
	}
	if _, e = exec.LookPath("ffprobe"); e != nil {
		return true
	}
	b, e := command(ctx, "ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name", "-of", "csv=p=0", path)
	return e == nil && strings.TrimSpace(string(b)) != ""
}

// ClearLink takes a symlink off a path this is about to write, and leaves
// anything else alone.
//
// Some outputs are files this writes and never links to one -- a cover, above
// all. Where a link has got to that path anyway, writing goes *through* it, and
// a link pointing at itself answers the read with ELOOP. That is not
// ErrNotExist, so AtomicWrite hands the error up, its caller hands it up, and
// the work is reported as a failure to produce the thing rather than as the
// broken link it is. A bad placement once pointed 119 covers at themselves and
// 79 redraws could never land for this reason, with nothing in the log saying
// why. The rename at the end of an atomic write would have replaced the link
// happily; it never got that far, because the read comes first.
func ClearLink(path string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	return os.Remove(path)
}

func Place(src, dest, mode string) (string, error) {
	src = absolute(src)
	dest = absolute(dest)
	if src == dest {
		return "unchanged", nil
	}
	if _, e := os.Stat(src); e != nil {
		return "", e
	}
	if e := os.MkdirAll(filepath.Dir(dest), 0755); e != nil {
		return "", e
	}
	if link, e := os.Readlink(dest); e == nil && link == src {
		return "unchanged", nil
	}
	f, e := os.CreateTemp(filepath.Dir(dest), ".inductor-media-*")
	if e != nil {
		return "", e
	}
	tmp := f.Name()
	_ = f.Close()
	_ = os.Remove(tmp)
	defer os.Remove(tmp)
	actual := mode
	if mode == "hardlink" {
		if e = os.Link(src, tmp); e != nil {
			actual = "symlink"
		}
	}
	if actual == "symlink" {
		e = os.Symlink(src, tmp)
	} else if actual != "hardlink" {
		actual = "copy"
		in, err := os.Open(src)
		if err != nil {
			return "", err
		}
		defer in.Close()
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			return "", err
		}
		_, e = io.Copy(out, in)
		ce := out.Close()
		if e == nil {
			e = ce
		}
		if s, err := in.Stat(); err == nil {
			_ = os.Chmod(tmp, s.Mode().Perm())
			_ = os.Chtimes(tmp, s.ModTime(), s.ModTime())
		}
	}
	if e != nil {
		return "", e
	}
	if e = os.Rename(tmp, dest); e != nil {
		return "", e
	}
	return actual, nil
}
func ConvertMedia(ctx context.Context, src, dest, work string) (string, error) {
	base := strings.TrimSuffix(dest, filepath.Ext(dest))
	if e := os.MkdirAll(filepath.Dir(dest), 0755); e != nil {
		return "", e
	}
	if work == "demux" {
		target := base + ".m4a"
		tmp := base + ".inductor-tmp.m4a"
		defer os.Remove(tmp)
		_, e := command(ctx, "ffmpeg", "-v", "error", "-i", src, "-map", "0:a:0", "-c", "copy", "-f", "ipod", "-y", tmp)
		if e == nil && HoldsAudio(ctx, tmp) {
			return target, os.Rename(tmp, target)
		}
	}
	target := base + ".mp3"
	tmp := base + ".inductor-tmp.mp3"
	defer os.Remove(tmp)
	_, e := command(ctx, "ffmpeg", "-v", "error", "-i", src, "-map", "0:a:0", "-vn", "-c:a", "libmp3lame", "-q:a", "2", "-f", "mp3", "-y", tmp)
	if e != nil {
		return "", e
	}
	if !HoldsAudio(ctx, tmp) {
		return "", fmt.Errorf("ffmpeg produced no usable audio for %s", src)
	}
	return target, os.Rename(tmp, target)
}
func PlacedAudio(c Config, s *Source, stem string) string {
	dir := filepath.Join(c.Media, "audio", s.AuthorID())
	entries, _ := os.ReadDir(dir)
	paths := []string{}
	for _, e := range entries {
		n := e.Name()
		if strings.TrimSuffix(n, filepath.Ext(n)) == stem && contains(Playable, strings.ToLower(filepath.Ext(n))) && exists(filepath.Join(dir, n)) {
			paths = append(paths, filepath.Join(dir, n))
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		a, b := filepath.Ext(paths[i]) == ".m4a", filepath.Ext(paths[j]) == ".m4a"
		if a != b {
			return a
		}
		return paths[i] < paths[j]
	})
	if len(paths) > 0 {
		return paths[0]
	}
	return ""
}
func VideoTarget(c Config, s *Source, stem string) string {
	src := s.AudioPath(c.Sources)
	ext := strings.ToLower(filepath.Ext(src))
	if src == "" || !contains(Video, ext) {
		return ""
	}
	return filepath.Join(c.Media, "video", s.AuthorID(), stem+ext)
}

// PlaceMedia puts a recording where the site can serve it. With repair set it
// re-encodes rather than links, which is the only thing that clears malformed
// frames: remuxing copies them through untouched, and a strict decoder -- the
// one faster_whisper uses -- refuses the file either way.
func PlaceMedia(ctx context.Context, c Config, s *Source, stem string, repair bool) (string, error) {
	src := s.AudioPath(c.Sources)
	if src == "" {
		return "", fmt.Errorf("audio not found: %s", s.Audio)
	}
	work := NeedsWork(ctx, src, c.MediaSettings.Transcode)
	if repair && work == "none" && c.MediaSettings.Transcode != "never" {
		work = "transcode"
	}
	target := filepath.Join(c.Media, "audio", s.AuthorID(), stem+ServedAs(ctx, src))
	if work == "demux" {
		if v := VideoTarget(c, s, stem); v != "" {
			if _, e := Place(src, v, c.MediaSettings.Mode); e != nil {
				return "", e
			}
		}
	}
	if work != "none" {
		made, e := ConvertMedia(ctx, src, target, work)
		if e != nil {
			return "", e
		}
		// What was converted is the placement now, and anything else claiming
		// this stem is what it replaced. Leaving the old one behind is not
		// untidiness: PlacedAudio answers with whichever claimant sorts first,
		// which is the .m4a, so a repair written as .mp3 landed *beside* the
		// broken file and was never consulted. The recording stayed damaged,
		// the check stayed unsatisfied, and the re-encode ran again on every
		// run -- a count that never falls, and a broken file still being served.
		supersede(filepath.Dir(target), stem, made, src)
		return made, nil
	}
	_, e := Place(src, target, c.MediaSettings.Mode)
	return target, e
}

// supersede removes the placements this repair replaces: same stem, same
// directory, and demonstrably the same recording -- a link or a hardlink that
// resolves to the very file just converted.
//
// Sameness is the whole guard. Two source records can land on one stem, and a
// stem tells you nothing about which recording owns it, so removing siblings on
// the strength of the name would take the other one's audio. A copy shares no
// identity with its source and is therefore never swept; that leaves the repeat
// in place for a library kept by copying, which is the safe way to be wrong.
func supersede(dir, stem, keep, src string) {
	origin, e := os.Stat(src)
	if e != nil {
		return
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		n := entry.Name()
		if strings.TrimSuffix(n, filepath.Ext(n)) != stem ||
			!contains(Playable, strings.ToLower(filepath.Ext(n))) {
			continue
		}
		p := filepath.Join(dir, n)
		if p == keep {
			continue
		}
		if info, e := os.Stat(p); e == nil && os.SameFile(info, origin) {
			_ = os.Remove(p)
		}
	}
}
