// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"bytes"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Source struct {
	fingerprint                string
	Path, Audio, Title, Author string
	Data                       Record
	resolved                   string
	checked                    bool
}

func (s *Source) AuthorID() string { return str(first(s.Data["author_id"], Slug(s.Author))) }
func (s *Source) AudioPath(root string) string {
	if s.checked {
		return s.resolved
	}
	s.checked = true
	if strings.HasPrefix(s.Audio, "http://") || strings.HasPrefix(s.Audio, "https://") {
		return ""
	}
	p := expandHome(s.Audio)
	candidates := []string{p}
	if !filepath.IsAbs(p) {
		candidates = append(candidates, filepath.Join(filepath.Dir(s.Path), p), filepath.Join(root, p))
	}
	for _, v := range candidates {
		if info, e := os.Stat(v); e == nil && !info.IsDir() {
			s.resolved = absolute(v)
			return s.resolved
		}
	}
	return ""
}

type SourceReport struct {
	Sources []*Source
	// Authors is what a parser learned about the creators themselves, keyed by
	// author id. A mirror of somebody's site has their bio, their picture and
	// their links sitting right there, and without somewhere to put them the
	// pipeline throws all three away and then asks a model to invent a synopsis
	// and draw an avatar -- both strictly worse than what the site already said.
	Authors          map[string]Record
	Errors, Warnings []string
}

func LoadSources(root string, watch ...func(done, total int)) (SourceReport, error) {
	r := SourceReport{}
	files, e := yamlFiles(root, true)
	if e != nil {
		return r, e
	}
	// The several callers in one run share the parse, and the Sources with it:
	// the resolved path and fingerprint a Source remembers are caches too, so
	// passing the same ones along is the point rather than a hazard.
	stamp := sourceStamp(files)
	if cached, ok := memoisedSources(root, stamp); ok {
		return cached, nil
	}
	seen := map[string]string{}
	titles := map[string]map[string]int{}
	known := pythonFields("apiVersion kind audio title author author_id date description summary tags categories series series_index source_url cover explicit variant provenance duration")
	knownAuthor := pythonFields("apiVersion kind id name author url image links language explicit description summary provenance")
	r.Authors = map[string]Record{}
	for n, p := range files {
		for _, w := range watch {
			w(n, len(files))
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return r, e
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		for pos := 1; ; pos++ {
			var doc any
			e = dec.Decode(&doc)
			if e == io.EOF {
				break
			}
			if e != nil {
				r.Errors = append(r.Errors, fmt.Sprintf("%s: invalid YAML - %v", filepath.Base(p), e))
				break
			}
			if doc == nil {
				continue
			}
			entries, ok := doc.([]any)
			if !ok {
				entries = []any{doc}
			}
			for _, v := range entries {
				where := filepath.Base(p)
				if pos > 1 {
					where += fmt.Sprintf(" [%d]", pos)
				}
				d, ok := v.(map[string]any)
				if !ok {
					r.Errors = append(r.Errors, where+": expected a mapping")
					continue
				}
				// A creator, rather than a recording. Same file, because what a
				// parser learns about the two comes from the same pass over the
				// same mirror, and splitting them would mean two things to keep
				// in step.
				if strings.EqualFold(strings.TrimSpace(str(d["kind"])), "author") {
					name := strings.TrimSpace(str(first(d["name"], d["author"])))
					if name == "" {
						r.Errors = append(r.Errors, where+": author record has no name")
						continue
					}
					id := strings.TrimSpace(str(d["id"]))
					if id == "" {
						id = Slug(name)
					}
					unknown := []string{}
					for _, k := range sortedKeys(d) {
						if !contains(knownAuthor, k) {
							unknown = append(unknown, k)
						}
					}
					if len(unknown) > 0 {
						r.Warnings = append(r.Warnings, where+": unrecognised author field(s) "+strings.Join(unknown, ", "))
					}
					if _, twice := r.Authors[id]; twice {
						r.Warnings = append(r.Warnings, where+": a second author record for "+id)
					}
					d["name"] = name
					d["id"] = id
					r.Authors[id] = d
					continue
				}
				missing := []string{}
				for _, k := range []string{"audio", "title", "author"} {
					if strings.TrimSpace(str(d[k])) == "" {
						missing = append(missing, k)
					}
				}
				if len(missing) > 0 {
					r.Errors = append(r.Errors, where+": missing "+strings.Join(missing, ", "))
					continue
				}
				unknown := []string{}
				for _, k := range sortedKeys(d) {
					if !contains(known, k) {
						unknown = append(unknown, k)
					}
				}
				if len(unknown) > 0 {
					r.Warnings = append(r.Warnings, where+": unrecognised field(s) "+strings.Join(unknown, ", "))
				}
				// Cleaned at the boundary, where everything downstream reads
				// it -- the item, the filename, the nameplate and the site.
				title := strings.TrimSpace(StripControls(str(d["title"])))
				author := strings.TrimSpace(StripControls(str(d["author"])))
				d["title"], d["author"] = title, author
				s := &Source{Path: p, Audio: str(d["audio"]), Title: title, Author: author, Data: d}
				if before := seen[s.Audio]; before != "" {
					r.Errors = append(r.Errors, where+": audio already claimed by "+before)
					continue
				}
				seen[s.Audio] = where
				r.Sources = append(r.Sources, s)
				if titles[s.AuthorID()] == nil {
					titles[s.AuthorID()] = map[string]int{}
				}
				// Counted per title *and* variant. A variant group shares a
				// title by design -- that is what makes it a group -- so the
				// repeat worth reporting is two records claiming the same
				// title under the same distinguisher, which is a duplicate
				// import rather than an edition.
				titles[s.AuthorID()][s.Title+"\x00"+strings.TrimSpace(str(d["variant"]))]++
			}
		}
	}
	for _, a := range sortedKeys(titles) {
		for _, t := range sortedKeys(titles[a]) {
			if titles[a][t] > 1 {
				title, variant, _ := strings.Cut(t, "\x00")
				if variant != "" {
					r.Warnings = append(r.Warnings, fmt.Sprintf("%s: repeated title %q under the same variant %q", a, title, variant))
					continue
				}
				r.Warnings = append(r.Warnings, fmt.Sprintf("%s: repeated title %q", a, title))
			}
		}
	}
	memoiseSources(root, stamp, r)
	return r, nil
}
