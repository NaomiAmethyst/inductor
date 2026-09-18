// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"encoding/hex"
	"fmt"
	"golang.org/x/crypto/blake2b"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

func Fingerprint(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return "", e
	}
	start, end := int64(0), info.Size()
	head := make([]byte, 10)
	n, _ := f.Read(head)
	if n >= 10 && string(head[:3]) == "ID3" {
		size := int64(0)
		for _, b := range head[6:10] {
			size = (size << 7) | int64(b&127)
		}
		start = 10 + size
		if head[5]&16 != 0 {
			start += 10
		}
	}
	if end > 128 {
		tail := make([]byte, 3)
		_, _ = f.ReadAt(tail, end-128)
		if string(tail) == "TAG" {
			end -= 128
		}
	}
	h, _ := blake2b.New(16, nil)
	if end > start {
		if _, e = f.Seek(start, io.SeekStart); e != nil {
			return "", e
		}
		if _, e = io.CopyN(h, f, end-start); e != nil {
			return "", e
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func TranscriptKey(text string) string {
	normalized := casefold(strings.Join(strings.FieldsFunc(text, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }), " "))
	h, _ := blake2b.New(16, nil)
	_, _ = h.Write([]byte(normalized))
	return hex.EncodeToString(h.Sum(nil))
}

// Source records are parsed once per run, not once per caller.
//
// Decoding them is the most expensive thing a run does before it touches any
// audio -- on a large library, nearly ten thousand records across a few hundred
// files and better than twenty seconds of YAML -- and a full run asks for them
// four or five times over, because the check, the plan, orphans and the tag
// workflows each load them independently.
//
// The result is kept in memory rather than on disk on purpose: round-tripping
// decoded YAML through JSON turns integers into floats and cannot carry a
// non-string key, and quietly altering source records to save a few seconds is
// a bad trade. Freshness is settled by re-stating the files, which costs
// milliseconds, so an edit between two loads is still seen.
type sourceMemo struct {
	stamp  string
	report SourceReport
}

var (
	sourceMemos   = map[string]sourceMemo{}
	sourceMemosMu sync.Mutex
)

// sourceStamp is size and mtime over every file that would be read. Cheap
// enough to compute on each load, and exact enough that a changed, added or
// removed record invalidates it.
func sourceStamp(files []string) string {
	h, _ := blake2b.New(16, nil)
	for _, p := range files {
		info, err := os.Stat(p)
		if err != nil {
			fmt.Fprintf(h, "%s\x00missing\x00", p)
			continue
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00", p, info.Size(), info.ModTime().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil))
}

func memoisedSources(root, stamp string) (SourceReport, bool) {
	sourceMemosMu.Lock()
	defer sourceMemosMu.Unlock()
	m, ok := sourceMemos[root]
	return m.report, ok && m.stamp == stamp
}

func memoiseSources(root, stamp string, r SourceReport) {
	sourceMemosMu.Lock()
	defer sourceMemosMu.Unlock()
	sourceMemos[root] = sourceMemo{stamp: stamp, report: r}
}

// SoundnessCache remembers which audio decodes from end to end.
//
// A file can open, report a duration, and still be broken: a truncated
// download or a damaged frame run decodes to silence, transcribes to a dozen
// characters, and produces an item that looks playable on the site and is not.
// Sampling the first seconds does not find it -- the damage is usually further
// in -- so the whole file is decoded, once, and the verdict kept against the
// same size-and-mtime identity the fingerprints use.
type SoundnessCache struct {
	Path    string
	mu      sync.Mutex
	entries Record
	dirty   bool
}

func NewSoundnessCache(path string) *SoundnessCache {
	return &SoundnessCache{Path: path, entries: record(readJSON(path)["entries"])}
}

// Of decodes a file end to end and reports what the decoder said about it and
// how much audio actually came out, as a fraction of what the file claims.
//
// The fraction is what separates the two kinds of damage. A recording with a
// few malformed frame headers still yields every second of its audio -- the
// decoder skips the bad frames and carries on -- and re-encoding it produces a
// clean file with nothing lost. A truncated or gutted file yields a fraction of
// what it promises, and transcoding that only launders the loss into a file
// that looks healthy. The first is worth repairing; the second is not.
func (c *SoundnessCache) Of(ctx context.Context, path string) (complaint string, recovered float64, err error) {
	s, e := os.Stat(path)
	if e != nil {
		return "", 0, e
	}
	key, ok := fileIdentity(path, s)
	if ok {
		c.mu.Lock()
		row := record(c.entries[key])
		if len(row) > 0 && str(row["size"]) == fmt.Sprint(s.Size()) &&
			str(row["mtime"]) == fmt.Sprint(s.ModTime().UnixNano()) {
			v, f := str(row["complaint"]), number(row["recovered"])
			c.mu.Unlock()
			return v, f, nil
		}
		c.mu.Unlock()
	}
	complaint, recovered = decodeFully(ctx, path)
	if ok {
		c.mu.Lock()
		c.entries[key] = Record{"size": s.Size(), "mtime": s.ModTime().UnixNano(),
			"complaint": complaint, "recovered": recovered}
		c.dirty = true
		c.mu.Unlock()
	}
	return complaint, recovered, nil
}

// Peek answers only from what has already been decoded. Cheap predicates -- the
// ones asked eight times a recording about whether an artefact exists -- must
// never start a decode: doing so turned a two-second sweep of the library into
// one that played every file in it from end to end.
func (c *SoundnessCache) Peek(path string) (complaint string, recovered float64, known bool) {
	s, e := os.Stat(path)
	if e != nil {
		return "", 0, false
	}
	key, ok := fileIdentity(path, s)
	if !ok {
		return "", 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	row := record(c.entries[key])
	if len(row) == 0 || str(row["size"]) != fmt.Sprint(s.Size()) ||
		str(row["mtime"]) != fmt.Sprint(s.ModTime().UnixNano()) {
		return "", 0, false
	}
	return str(row["complaint"]), number(row["recovered"]), true
}

// decodeFully plays the whole file to nowhere, counting the samples that come
// out and keeping the decoder's first complaint.
func decodeFully(ctx context.Context, path string) (string, float64) {
	const rate = 8000
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-i", path,
		"-f", "s16le", "-ac", "1", "-ar", fmt.Sprint(rate), "-")
	var grumbles strings.Builder
	cmd.Stderr = &grumbles
	out, e := cmd.StdoutPipe()
	if e != nil {
		return e.Error(), 0
	}
	if e = cmd.Start(); e != nil {
		return e.Error(), 0
	}
	bytesOut, buf := int64(0), make([]byte, 1<<16)
	for {
		n, readErr := out.Read(buf)
		bytesOut += int64(n)
		if readErr != nil {
			break
		}
	}
	_ = cmd.Wait()
	complaint := strings.TrimSpace(grumbles.String())
	if i := strings.IndexByte(complaint, '\n'); i > 0 {
		complaint = complaint[:i]
	}
	if len(complaint) > 200 {
		complaint = complaint[:200]
	}
	decoded := float64(bytesOut) / 2 / rate
	declared := number(AudioMetadata(ctx, path)["duration"])
	if declared <= 0 {
		if decoded > 0 {
			return complaint, 1
		}
		return complaint, 0
	}
	return complaint, decoded / declared
}

func (c *SoundnessCache) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return nil
	}
	if e := writeJSON(c.Path, Record{"apiVersion": "inductor/v1", "kind": "SoundnessCache", "entries": c.entries}); e != nil {
		return e
	}
	c.dirty = false
	return nil
}

type FingerprintCache struct {
	Path    string
	mu      sync.Mutex
	entries Record
	dirty   bool
}

func NewFingerprintCache(path string) *FingerprintCache {
	return &FingerprintCache{Path: path, entries: record(readJSON(path)["entries"])}
}
func (c *FingerprintCache) Of(path string) (string, error) {
	s, e := os.Stat(path)
	if e != nil {
		return "", e
	}
	key, ok := fileIdentity(path, s)
	if !ok {
		return Fingerprint(path)
	}
	c.mu.Lock()
	row := array(c.entries[key])
	if len(row) == 3 && str(row[0]) == fmt.Sprint(s.Size()) && str(row[1]) == fmt.Sprint(s.ModTime().UnixNano()) {
		v := str(row[2])
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()
	fp, e := Fingerprint(path)
	if e != nil {
		return "", e
	}
	c.mu.Lock()
	c.entries[key] = []any{s.Size(), s.ModTime().UnixNano(), fp}
	c.dirty = true
	c.mu.Unlock()
	return fp, nil
}
func (c *FingerprintCache) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return nil
	}
	if e := writeJSON(c.Path, Record{"apiVersion": "inductor/v1", "kind": "FingerprintCache", "entries": c.entries}); e != nil {
		return e
	}
	c.dirty = false
	return nil
}

type AnalysisStore struct {
	Root string
	mu   sync.Mutex
}

func (s *AnalysisStore) Get(key, audio string) Record {
	r := readJSON(filepath.Join(s.Root, key+".json"))
	if r != nil || audio == "" || audio == key {
		return r
	}
	old := readJSON(filepath.Join(s.Root, audio+".json"))
	if !truth(old["analysis"]) {
		return nil
	}
	if s.Put(key, old, audio, "") != nil {
		return nil
	}
	return readJSON(filepath.Join(s.Root, key+".json"))
}

// Peek also understands legacy audio-keyed records without migrating them.
// Planning and dry runs must be able to inspect a cache without writing it.
func (s *AnalysisStore) Peek(key, audio string) Record {
	r := readJSON(filepath.Join(s.Root, key+".json"))
	if r != nil || audio == "" || audio == key {
		return r
	}
	old := readJSON(filepath.Join(s.Root, audio+".json"))
	if truth(old["analysis"]) {
		return old
	}
	return nil
}
func (s *AnalysisStore) Put(key string, payload Record, audio, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.Root, key+".json")
	r := Record{"apiVersion": "inductor/v1", "kind": "Analysis", "transcript": key}
	merge(r, readJSON(path))
	merge(r, payload)
	r["prompt_version"] = 3
	if audio != "" {
		a := uniqueStrings(append(texts(r["audio"]), audio))
		sortStrings(a)
		r["audio"] = a
	}
	if model != "" {
		r["model"] = model
	}
	return writeJSON(path, r)
}
func (s *AnalysisStore) IsStale(key string) bool {
	r := s.Get(key, "")
	return len(r) > 0 && integer(r["prompt_version"]) < 3
}

type ItemIndex struct {
	Path  string
	rows  Record
	dirty bool
}

func NewItemIndex(path string) *ItemIndex {
	r := readJSON(path)
	rows := Record{}
	if integer(r["version"]) == 7 {
		rows = record(r["rows"])
	}
	return &ItemIndex{Path: path, rows: rows}
}
func ItemFacts(d Record, path string) Record {
	p := record(d["provenance"])
	complete := true
	for _, f := range []string{"description", "summary", "tags", "spoilers"} {
		complete = complete && truth(d[f])
	}
	r := Record{"author": str(first(d["author"], filepath.Base(filepath.Dir(path)))), "id": str(d["id"]), "title": str(d["title"]), "audio": str(d["audio"]), "needs": texts(d["needs"]), "complete": complete, "enriched": truth(p["enriched"])}
	for _, f := range []string{"source_key", "fingerprint"} {
		r[f] = str(p[f])
	}
	r["merged_source_keys"] = texts(p["merged_source_keys"])
	// Kept so building creators' tag maps can count from the index instead of
	// re-reading every item document in the library for the same figures.
	r["tags"] = texts(d["tags"])
	return r
}
func (i *ItemIndex) Entries(root string, watch ...func(done, total int)) ([]Document, error) {
	if !exists(root) {
		return nil, nil
	}
	files, e := yamlFiles(root, false)
	if e != nil {
		return nil, e
	}
	seen := map[string]bool{}
	out := []Document{}
	for n, p := range files {
		for _, w := range watch {
			w(n, len(files))
		}
		if strings.HasSuffix(p, ".transcript.yaml") || strings.HasSuffix(p, ".transcript.yml") {
			continue
		}
		seen[p] = true
		s, e := os.Stat(p)
		if e != nil {
			continue
		}
		r := record(i.rows[p])
		if str(r["size"]) != fmt.Sprint(s.Size()) || str(r["mtime"]) != fmt.Sprint(s.ModTime().UnixNano()) {
			doc, e := readYAML(p)
			if e != nil {
				continue
			}
			r = Record{"size": s.Size(), "mtime": s.ModTime().UnixNano(), "item": GuessKind(doc, p) == "item"}
			if truth(r["item"]) {
				r["facts"] = ItemFacts(doc, p)
			}
			i.rows[p] = r
			i.dirty = true
		}
		if truth(r["item"]) {
			out = append(out, Document{p, record(r["facts"]), "item"})
		}
	}
	for p := range i.rows {
		if !seen[p] {
			delete(i.rows, p)
			i.dirty = true
		}
	}
	return out, nil
}
func (i *ItemIndex) Save() error {
	if !i.dirty {
		return nil
	}
	err := writeJSON(i.Path, Record{"apiVersion": "inductor/v1", "kind": "ItemIndex", "version": 7, "rows": i.rows})
	if err == nil {
		i.dirty = false
	}
	return err
}
