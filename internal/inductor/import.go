// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/text/unicode/norm"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func looseAudio(paths []string) ([]string, error) {
	out := []string{}
	for _, p := range paths {
		p = expandHome(p)
		if isDir(p) {
			err := filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() && contains(Playable, strings.ToLower(filepath.Ext(path))) {
					out = append(out, absolute(path))
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		} else if contains(Playable, strings.ToLower(filepath.Ext(p))) {
			out = append(out, absolute(p))
		}
	}
	sort.Strings(out)
	return out, nil
}
func AudioMetadata(ctx context.Context, path string) Record {
	b, e := command(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration:format_tags=title,album,date,originaldate,track,tracknumber", "-of", "json", path)
	if e != nil {
		return Record{}
	}
	var r Record
	if json.Unmarshal(b, &r) != nil {
		return Record{}
	}
	format := record(r["format"])
	out := Record{}
	if truth(format["duration"]) {
		out["duration"] = rounded(number(format["duration"]), 1)
	}
	for k, v := range record(format["tags"]) {
		k = strings.ToLower(k)
		if k == "track" {
			k = "tracknumber"
		}
		s := strings.TrimSpace(str(v))
		if !contains([]string{"unknown", "none", ""}, strings.ToLower(s)) {
			out[k] = s
		}
	}
	return out
}
func addSlug(s string) string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(s) {
		if r < 128 {
			b.WriteRune(r)
		}
	}
	s = strings.Trim(strings.ToLower(nonSlug.ReplaceAllString(b.String(), "-")), "-")
	if s == "" {
		return "untitled"
	}
	return s
}

var addAudience = regexp.MustCompile(`(?i)\[([FMA]4[FMAT]M?)\]`)
var brackets = regexp.MustCompile(`\[[^\]]*\]`)
var separators = regexp.MustCompile(`\s*[-–;,|]+\s*`)
var releaseDate = regexp.MustCompile(`^(\d{4})(-(\d{2}))?(-(\d{2}))?`)

type AddOptions struct {
	Paths                                 []string
	Author, AuthorName, URL, Series, Date string
	Tags                                  []string
	NotExplicit, DryRun                   bool
}

func (e *Engine) Add(ctx context.Context, o AddOptions) (Record, error) {
	files, err := looseAudio(o.Paths)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no audio found in those paths")
	}
	if o.Author == "" && o.AuthorName == "" {
		return nil, fmt.Errorf("give --author or --author-name")
	}
	ident := str(first(o.Author, addSlug(o.AuthorName)))
	docs := []Document{}
	if isDir(e.Config.Content) {
		docs, err = Documents(e.Config.Content, "item", "author")
		if err != nil {
			return nil, err
		}
	}
	authorExists := false
	taken, known := map[string]bool{}, map[string]string{}
	for _, d := range docs {
		taken[stemOf(d.Path)] = true
		if d.Kind == "author" && str(d.Data["id"]) == ident {
			authorExists = true
		}
		if fp := str(record(d.Data["provenance"])["fingerprint"]); fp != "" {
			known[fp] = str(d.Data["id"])
		}
	}
	if !authorExists {
		if o.AuthorName == "" {
			return nil, fmt.Errorf("no author %q yet; pass --author-name to create it", ident)
		}
		r := Record{"apiVersion": "hypnotica/v1", "kind": "Author", "id": ident, "name": o.AuthorName, "language": "en", "explicit": !o.NotExplicit, "needs": []string{"summary", "description"}}
		if o.URL != "" {
			r["url"] = o.URL
			r["links"] = Record{"website": o.URL}
		}
		if !o.DryRun {
			if err = writeYAML(filepath.Join(e.Config.Content, ident, "_author.yaml"), r, authorOrder); err != nil {
				return nil, err
			}
		}
	}
	report := Record{"added": 0, "skipped": 0}
	for _, path := range files {
		fp, err := e.Fingerprints.Of(path)
		if err != nil {
			return report, err
		}
		if known[fp] != "" {
			report["skipped"] = integer(report["skipped"]) + 1
			continue
		}
		meta := AudioMetadata(ctx, path)
		raw := str(first(meta["title"], stemOf(path)))
		title := brackets.ReplaceAllString(raw, " ")
		title = separators.ReplaceAllString(title, " - ")
		title = strings.Trim(strings.Join(pythonFields(title), " "), " -–;,|_~")
		if title == "" {
			title = stemOf(path)
		}
		base := ident + "-" + addSlug(title)
		candidate := base
		for n := 2; taken[candidate]; n++ {
			candidate = fmt.Sprintf("%s-%d", base, n)
		}
		taken[candidate] = true
		known[fp] = candidate
		r := Record{"apiVersion": "hypnotica/v1", "kind": "Item", "id": candidate, "title": title, "author": ident, "audio": path, "categories": []string{"Audio", "Hypnosis"}, "explicit": !o.NotExplicit, "needs": []string{"description", "summary", "tags", "spoilers"}, "provenance": Record{"added_from": path, "fingerprint": fp, "transcript": "pending"}}
		date := str(first(o.Date, meta["date"], meta["originaldate"]))
		if match := releaseDate.FindString(date); match != "" {
			r["date"] = match
		}
		if truth(meta["duration"]) {
			r["duration"] = meta["duration"]
		}
		if series := first(o.Series, meta["album"]); truth(series) {
			r["series"] = series
		}
		tags := append([]string{}, o.Tags...)
		for _, m := range addAudience.FindAllStringSubmatch(stemOf(path), -1) {
			tags = append(tags, strings.ToUpper(m[1]))
		}
		tags = uniqueStrings(tags)
		sort.Slice(tags, func(i, j int) bool { return casefold(tags[i]) < casefold(tags[j]) })
		if len(tags) > 0 {
			r["tags"] = tags
		}
		if !o.DryRun {
			if err = writeYAML(filepath.Join(e.Config.Content, ident, candidate+".yaml"), r, itemOrder); err != nil {
				return report, err
			}
			source := Record{"apiVersion": "inductor/v1", "kind": "Source", "audio": path, "title": title, "author": str(first(o.AuthorName, ident)), "author_id": ident}
			for _, f := range []string{"date", "duration", "series", "tags", "categories", "explicit"} {
				if r[f] != nil {
					source[f] = r[f]
				}
			}
			if err = writeYAML(filepath.Join(e.Config.Sources, "added", candidate+".yaml"), source, nil); err != nil {
				return report, err
			}
		}
		report["added"] = integer(report["added"]) + 1
		e.Say("added %s: %s", candidate, title)
	}
	return report, nil
}
func (e *Engine) AcousticApply(dry bool) (Record, error) {
	docs, err := Documents(e.Config.Content, "item")
	if err != nil {
		return nil, err
	}
	report := Record{"written": 0, "disputed": 0, "missing": 0}
	for _, d := range docs {
		fp := str(record(d.Data["provenance"])["fingerprint"])
		m := e.Acoustic.Measurements(fp)
		if m == nil {
			report["missing"] = integer(report["missing"]) + 1
			continue
		}
		block := Record{}
		for _, k := range []string{"seconds", "rms", "peak", "crest_db", "silence_ratio", "spectral_centroid_hz", "f0_median_hz", "f0_spread_hz", "f0_reliable", "voiced_frames", "hnr_db", "level_variation", "voiced_share", "brightness_hz", "brightness_spread_hz"} {
			if m[k] != nil {
				block[k] = m[k]
			}
		}
		if len(block) == 0 {
			continue
		}
		merge(block, Delivery(e.transcript(fp)))
		trustworthy := m["f0_median_hz"] != nil && m["f0_spread_hz"] != nil && ReliablePitch(number(m["f0_median_hz"]), number(m["f0_spread_hz"]))
		block["f0_reliable"] = trustworthy
		filled := false
		if d.Data["duration"] == nil && truth(m["seconds"]) {
			d.Data["duration"] = rounded(number(m["seconds"]), 1)
			filled = true
		}
		if voice := VoiceFromPitch(number(m["f0_median_hz"]), trustworthy); voice != "" {
			block["voice"] = voice
			for _, t := range texts(d.Data["tags"]) {
				if strings.HasPrefix(t, "Voice: ") {
					tagged := strings.TrimPrefix(t, "Voice: ")
					if tagged != voice {
						block["voice_disputed"] = tagged
						report["disputed"] = integer(report["disputed"]) + 1
					}
					break
				}
			}
		}
		if equivalent(d.Data["acoustic"], block) && !filled {
			continue
		}
		d.Data["acoustic"] = block
		report["written"] = integer(report["written"]) + 1
		if !dry {
			if _, err = SaveDocument(d.Path, d.Data); err != nil {
				return report, err
			}
		}
	}
	return report, nil
}
