// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"encoding/hex"
	"fmt"
	"golang.org/x/crypto/blake2b"
	"io"
	"os"
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
	if integer(r["version"]) == 6 {
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
	return r
}
func (i *ItemIndex) Entries(root string) ([]Document, error) {
	if !exists(root) {
		return nil, nil
	}
	files, e := yamlFiles(root, false)
	if e != nil {
		return nil, e
	}
	seen := map[string]bool{}
	out := []Document{}
	for _, p := range files {
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
	err := writeJSON(i.Path, Record{"apiVersion": "inductor/v1", "kind": "ItemIndex", "version": 6, "rows": i.rows})
	if err == nil {
		i.dirty = false
	}
	return err
}
