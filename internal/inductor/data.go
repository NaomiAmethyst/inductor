// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Record preserves extension fields in the versioned, user-editable documents.
// Typed structures are used for settings and execution; records stay open-ended.
type Record = map[string]any

func str(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case time.Time:
		return x.Format("2006-01-02")
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(v)
}
func number(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	f, _ := strconv.ParseFloat(str(v), 64)
	return f
}
func integer(v any) int { return int(number(v)) }
func truth(v any) bool {
	if v == nil {
		return false
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Bool:
		return r.Bool()
	case reflect.String, reflect.Map, reflect.Slice, reflect.Array:
		return r.Len() > 0
	case reflect.Int, reflect.Int64:
		return r.Int() != 0
	case reflect.Float64:
		return r.Float() != 0
	}
	return true
}
func record(v any) Record {
	if r, ok := v.(map[string]any); ok && r != nil {
		return r
	}
	return Record{}
}
func array(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []string:
		a := make([]any, len(x))
		for i, s := range x {
			a[i] = s
		}
		return a
	}
	return []any{}
}
func texts(v any) []string {
	var r []string
	for _, x := range array(v) {
		r = append(r, str(x))
	}
	return r
}
func first(v ...any) any {
	for _, x := range v {
		if truth(x) {
			return x
		}
	}
	return nil
}
func contains(a []string, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}
func uniqueStrings(a []string) []string {
	r := []string{}
	seen := map[string]bool{}
	for _, v := range a {
		if !seen[v] {
			seen[v] = true
			r = append(r, v)
		}
	}
	return r
}
func sortedKeys[V any](m map[string]V) []string {
	r := make([]string, 0, len(m))
	for k := range m {
		r = append(r, k)
	}
	sort.Strings(r)
	return r
}
func clone(r Record) Record {
	b, _ := json.Marshal(r)
	var out Record
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	_ = d.Decode(&out)
	if out == nil {
		return Record{}
	}
	return out
}
func merge(dst, src Record) {
	for k, v := range src {
		dst[k] = v
	}
}
func nested(r Record, k string) Record {
	if m, ok := r[k].(map[string]any); ok && m != nil {
		return m
	}
	m := Record{}
	r[k] = m
	return m
}
func exists(p string) bool { _, e := os.Stat(p); return e == nil }
func isDir(p string) bool  { s, e := os.Stat(p); return e == nil && s.IsDir() }
func absolute(p string) string {
	p = expandHome(p)
	a, e := filepath.Abs(p)
	if e != nil {
		return filepath.Clean(p)
	}
	return a
}
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, strings.TrimPrefix(p, "~"))
	}
	return p
}
func readJSON(p string) Record {
	b, e := os.ReadFile(p)
	if e != nil {
		return nil
	}
	var r Record
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&r) != nil {
		return nil
	}
	return r
}
func readYAML(p string) (Record, error) {
	b, e := os.ReadFile(p)
	if e != nil {
		return nil, e
	}
	var r Record
	e = yaml.Unmarshal(b, &r)
	if r == nil && e == nil {
		r = Record{}
	}
	return r, e
}
func optionalYAML(p string) Record {
	r, _ := readYAML(p)
	if r == nil {
		return Record{}
	}
	return r
}

// AtomicWrite never truncates the old document on interruption, and does not
// change mtimes when the serialized contents have not changed.
func AtomicWrite(path string, b []byte, mode os.FileMode) (bool, error) {
	old, e := os.ReadFile(path)
	if e == nil && bytes.Equal(old, b) {
		return false, nil
	}
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return false, e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0755); e != nil {
		return false, e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".inductor-*")
	if e != nil {
		return false, e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if info, e := os.Stat(path); e == nil {
		mode = info.Mode().Perm()
	}
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return false, e
	}
	if e = os.Rename(tmp, path); e != nil {
		return false, e
	}
	return true, nil
}
func writeJSON(p string, r any) error {
	b, e := json.MarshalIndent(r, "", " ")
	if e != nil {
		return e
	}
	_, e = AtomicWrite(p, append(b, '\n'), 0644)
	return e
}

var itemOrder = []string{"apiVersion", "kind", "id", "title", "author", "date", "audio", "video", "duration", "series", "series_index", "variant", "cover", "tags", "categories", "summary", "description", "source_url", "explicit", "needs", "spoilers", "provenance"}
var authorOrder = []string{"apiVersion", "kind", "id", "name", "url", "image", "links", "summary", "description", "language", "explicit", "needs", "provenance"}

// plainNumbers turns json.Number back into a number.
//
// Every cache here is decoded with UseNumber, which keeps an integer an integer
// instead of letting it drift to a float on the way through JSON. The cost is
// that json.Number is a *string* type, so yaml.v3 writes it quoted: a measured
// figure lands in the document as `beat_hz: "8.05"`, and reading it back gives
// a string. Nothing errors -- but the value no longer compares equal to the one
// that produced it, so `equivalent` says the block has changed on every run and
// the same measurements are rewritten across the library for ever. That showed
// up exactly as it always does: a count that never falls.
func plainNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, e := x.Int64(); e == nil {
			return i
		}
		if f, e := x.Float64(); e == nil {
			return f
		}
		return x.String()
	case map[string]any:
		for k, e := range x {
			x[k] = plainNumbers(e)
		}
	case []any:
		for i, e := range x {
			x[i] = plainNumbers(e)
		}
	}
	return v
}
func yamlNode(v any) (*yaml.Node, error) {
	n := &yaml.Node{}
	if e := n.Encode(plainNumbers(v)); e != nil {
		return nil, e
	}
	return n, nil
}
func marshalYAML(r Record, order []string) ([]byte, error) {
	n, e := yamlNode(r)
	if e != nil {
		return nil, e
	}
	if n.Kind == yaml.MappingNode {
		pairs := map[string][2]*yaml.Node{}
		for i := 0; i < len(n.Content); i += 2 {
			pairs[n.Content[i].Value] = [2]*yaml.Node{n.Content[i], n.Content[i+1]}
		}
		n.Content = nil
		keys := append(append([]string{}, order...), sortedKeys(pairs)...)
		used := map[string]bool{}
		for _, k := range keys {
			if p, ok := pairs[k]; ok && !used[k] {
				n.Content = append(n.Content, p[0], p[1])
				used[k] = true
			}
		}
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	e = enc.Encode(n)
	_ = enc.Close()
	return b.Bytes(), e
}
func writeYAML(p string, r Record, order []string) error {
	b, e := marshalYAML(r, order)
	if e != nil {
		return e
	}
	_, e = AtomicWrite(p, b, 0644)
	return e
}

// SaveDocument preserves a file byte-for-byte when its data is unchanged.
// For changed fields it retains the existing order and comments using YAML nodes.
func SaveDocument(p string, r Record) (bool, error) {
	b, e := os.ReadFile(p)
	if e != nil {
		if !errors.Is(e, os.ErrNotExist) {
			return false, e
		}
		out, e := marshalYAML(r, itemOrder)
		if e != nil {
			return false, e
		}
		return AtomicWrite(p, out, 0644)
	}
	var original Record
	if e = yaml.Unmarshal(b, &original); e != nil {
		return false, e
	}
	if equivalent(original, r) {
		return false, nil
	}
	var doc yaml.Node
	if e = yaml.Unmarshal(b, &doc); e != nil {
		return false, e
	}
	fresh, e := yamlNode(r)
	if e != nil {
		return false, e
	}
	if len(doc.Content) > 0 {
		updateNode(doc.Content[0], fresh)
	} else {
		doc.Content = []*yaml.Node{fresh}
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if e = enc.Encode(&doc); e != nil {
		return false, e
	}
	_ = enc.Close()
	return AtomicWrite(p, out.Bytes(), 0644)
}
func equivalent(a, b any) bool {
	x, ex := json.Marshal(a)
	y, ey := json.Marshal(b)
	if ex != nil || ey != nil {
		return false
	}
	var left, right any
	if json.Unmarshal(x, &left) != nil || json.Unmarshal(y, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}
func updateNode(old, new *yaml.Node) {
	var a, b any
	_ = old.Decode(&a)
	_ = new.Decode(&b)
	if reflect.DeepEqual(a, b) {
		return
	}
	if old.Kind == yaml.MappingNode && new.Kind == yaml.MappingNode {
		want := map[string]*yaml.Node{}
		for i := 0; i < len(new.Content); i += 2 {
			want[new.Content[i].Value] = new.Content[i+1]
		}
		out := []*yaml.Node{}
		for i := 0; i < len(old.Content); i += 2 {
			k, v := old.Content[i], old.Content[i+1]
			if nv, ok := want[k.Value]; ok {
				updateNode(v, nv)
				out = append(out, k, v)
				delete(want, k.Value)
			}
		}
		for i := 0; i < len(new.Content); i += 2 {
			if _, ok := want[new.Content[i].Value]; ok {
				out = append(out, new.Content[i], new.Content[i+1])
			}
		}
		old.Content = out
		return
	}
	head, line, foot := old.HeadComment, old.LineComment, old.FootComment
	*old = *new
	old.HeadComment = head
	old.LineComment = line
	old.FootComment = foot
}
func yamlFiles(root string, hidden bool) ([]string, error) {
	if !isDir(root) {
		return nil, fmt.Errorf("expected directory at %s; check the configuration", root)
	}
	out := []string{}
	e := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if p != root && !hidden && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && strings.HasPrefix(filepath.Ext(p), ".y") && strings.HasSuffix(p, "ml") {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out, e
}

type Document struct {
	Path string
	Data Record
	Kind string
}

func KindOf(r Record) string {
	k := strings.ToLower(strings.TrimSpace(str(r["kind"])))
	for kind, aliases := range map[string][]string{"item": {"item", "file", "episode", "track"}, "author": {"author", "authors", "person"}, "config": {"config", "site", "settings", "hypnotica"}, "tags": {"tags", "tag", "vocabulary", "registry"}, "transcript": {"transcript", "transcription"}} {
		if contains(aliases, k) {
			return kind
		}
	}
	return ""
}
func GuessKind(r Record, p string) string {
	if k := KindOf(r); k != "" {
		return k
	}
	if contains([]string{"hypnotica.yaml", "hypnotica.yml"}, filepath.Base(p)) {
		return "config"
	}
	if truth(r["title"]) || truth(r["audio"]) {
		return "item"
	}
	if truth(r["name"]) && !truth(r["author"]) {
		return "author"
	}
	return ""
}
func Documents(root string, kinds ...string) ([]Document, error) {
	files, e := yamlFiles(root, false)
	if e != nil {
		return nil, e
	}
	out := []Document{}
	for _, p := range files {
		if len(kinds) > 0 {
			f, e := os.Open(p)
			if e != nil {
				continue
			}
			head := make([]byte, 2048)
			n, _ := io.ReadFull(f, head)
			_ = f.Close()
			skip := false
			for _, l := range strings.Split(string(head[:n]), "\n") {
				if strings.HasPrefix(l, "kind:") {
					k := KindOf(Record{"kind": strings.Trim(strings.TrimSpace(strings.TrimPrefix(l, "kind:")), "\"'")})
					if k != "" && !contains(kinds, k) {
						skip = true
					}
					break
				}
			}
			if skip {
				continue
			}
		}
		r, e := readYAML(p)
		if e != nil {
			continue
		}
		k := GuessKind(r, p)
		if len(kinds) == 0 || contains(kinds, k) {
			out = append(out, Document{p, r, k})
		}
	}
	return out, nil
}

// pythonFields includes the control separators accepted by Python str.split.
func pythonFields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f })
}
