// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func putFile(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0644); err != nil {
		t.Fatal(err)
	}
}
func putRecord(t *testing.T, p string, r Record) {
	t.Helper()
	if _, err := SaveDocument(p, r); err != nil {
		t.Fatal(err)
	}
}
func testConfig(t *testing.T) Config {
	t.Helper()
	c, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.Enrich.Covers = false
	c.MediaSettings.Transcode = "never"
	putRecord(t, c.RegistryPath(), Record{"content": Record{"Hypnosis": "A hypnotic recording.", "Relaxation": "Relaxing material."}, "voice": Record{"fem": "Feminine voice.", "masc": "Masculine voice."}, "audience": Record{"man": "Addresses a man."}})
	return c
}

func TestFingerprintIgnoresMetadataAndSurvivesRename(t *testing.T) {
	dir := t.TempDir()
	raw := bytes.Repeat([]byte("audio bytes"), 40)
	a := filepath.Join(dir, "a.mp3")
	b := filepath.Join(dir, "b.mp3")
	putFile(t, a, raw)
	tagged := append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 4}, []byte("name")...)
	tagged = append(tagged, raw...)
	tagged = append(tagged, append([]byte("TAG"), make([]byte, 125)...)...)
	putFile(t, b, tagged)
	want, err := Fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Fingerprint(b)
	if err != nil || got != want {
		t.Fatalf("metadata changed hash: %s %s %v", want, got, err)
	}
	cache := NewFingerprintCache(filepath.Join(dir, "cache.json"))
	if got, err = cache.Of(a); err != nil || got != want {
		t.Fatal(got, err)
	}
	if err = cache.Save(); err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(dir, "renamed.mp3")
	if err = os.Rename(a, renamed); err != nil {
		t.Fatal(err)
	}
	cache = NewFingerprintCache(cache.Path)
	if got, err = cache.Of(renamed); err != nil || got != want || cache.dirty {
		t.Fatalf("rename missed inode cache: %s %v", got, err)
	}
	putFile(t, renamed, []byte("different audio"))
	if got, err = cache.Of(renamed); err != nil || got == want {
		t.Fatal("changed audio reused hash", err)
	}
}
func TestSaveDocumentPreservesCommentsExtensionsAndMtime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "item.yaml")
	original := []byte("# curated entry\nkind: Item\nid: immutable # public identifier\ntitle: Original\ncustom:\n  nested: keep me\n")
	putFile(t, p, original)
	stamp := time.Unix(1700000000, 0)
	if err := os.Chtimes(p, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	r, err := readYAML(p)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := SaveDocument(p, r)
	if err != nil || changed {
		t.Fatal("noop rewritten", err)
	}
	st, _ := os.Stat(p)
	if !st.ModTime().Equal(stamp) {
		t.Fatal("noop changed mtime")
	}
	r["title"] = "Edited"
	if _, err = SaveDocument(p, r); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	for _, part := range []string{"# curated entry", "# public identifier", "keep me", "Edited"} {
		if !bytes.Contains(b, []byte(part)) {
			t.Errorf("lost %q: %s", part, b)
		}
	}
}
func TestAnalysisAdoptionAndConcurrentAudioAliases(t *testing.T) {
	store := &AnalysisStore{Root: t.TempDir()}
	old := Record{"analysis": Record{"summary": "known"}, "final": Record{"summary": "reviewed"}, "model": "old-model"}
	if err := writeJSON(filepath.Join(store.Root, "audio.json"), old); err != nil {
		t.Fatal(err)
	}
	r := store.Get("text", "audio")
	if str(record(r["final"])["summary"]) != "reviewed" {
		t.Fatal("legacy result lost", r)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := store.Put("text", Record{}, fmt.Sprint(i), ""); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	r = store.Get("text", "")
	if len(texts(r["audio"])) != 13 || str(r["model"]) != "old-model" {
		t.Fatal("lost aliases or model", r)
	}
}
func TestTheRegistryOutranksAMapRowThatAnswersNothing(t *testing.T) {
	r := NewRegistry(Record{"content": Record{"Known": ""}, "voice": Record{"Shared": "voice"}, "audience": Record{"Shared": "audience"}})
	for _, tc := range []struct {
		raw     string
		mapping Record
		want    string
	}{
		{"Known", nil, "Known"},
		// A bare tag two namespaces both claim is a question, not a default.
		{"Shared", nil, ""},
		{"voice: shared", nil, "Voice: Shared"},
		// A map may only point into the registry. One that points at something
		// the registry has never heard of is not an answer, and must not cost
		// the item a tag the registry does have: the registry rules on it as
		// written. The row itself is a fault, and `check` says so.
		{"Known", Record{"Known": Record{"to": "Invented"}}, "Known"},
		// A tag the registry lacks and the row cannot place is still unresolved,
		// which is what puts it to the adjudicator.
		{"Unheard", Record{"Unheard": Record{"to": "Invented"}}, ""},
		// A ruling to drop is an answer, and it stands.
		{"Known", Record{"Known": Record{"verdict": "drop"}}, ""},
	} {
		got, _ := r.Resolve(tc.raw, tc.mapping)
		if got != tc.want {
			t.Errorf("%s => %q, want %q", tc.raw, got, tc.want)
		}
	}
	if !r.Has("Known") {
		t.Fatal("empty definition is still registered")
	}
	p := Record{}
	MarkGenerated(p, "summary", "invented")
	MarkGenerated(p, "cover")
	if !equivalent(p["generated"], []string{"cover", "summary"}) {
		t.Fatal(p)
	}
}
func TestLedgerConcurrentAppendAndLatestRuling(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rulings.yaml")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := AppendLedger(p, []Record{RulingEntry(fmt.Sprint(i), "reject", "reason: contains a colon", "", 1, "fixture", "test")})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	rows, err := ReadLedger(p)
	if err != nil || len(rows) != 16 {
		t.Fatal(len(rows), err)
	}
	if _, err = AppendLedger(p, []Record{RulingEntry("0", "adopt", "changed evidence", "", 2, "fixture", "test")}); err != nil {
		t.Fatal(err)
	}
	standing, err := Standing(p)
	if err != nil || str(standing["0"]["verdict"]) != "adopt" {
		t.Fatal(standing, err)
	}
}
func TestMediaPlacementDoesNotDestroySource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "audio.mp3")
	putFile(t, src, []byte("audio"))
	if _, err := Place(src, src, "symlink"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"copy", "hardlink", "symlink"} {
		dest := filepath.Join(dir, mode)
		if _, err := Place(src, dest, mode); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(dest)
		if err != nil || string(b) != "audio" {
			t.Fatal(mode, err)
		}
		if _, err = Place(src, dest, mode); err != nil {
			t.Fatal(err)
		}
	}
	if st, _ := os.Lstat(src); st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("source became self-link")
	}
}
func TestMigrationRebasesPathsAndPreservesIDs(t *testing.T) {
	c := testConfig(t)
	src := filepath.Join(c.Content, "legacy.yaml")
	audio := filepath.Join(c.Root, "recordings", "a.mp3")
	putFile(t, audio, []byte("audio"))
	putFile(t, src, []byte("# keep this note\nkind: Item\nid: public-id\nauthor: creator\ntitle: A Title\naudio: ../recordings/a.mp3\ncustom: retained\n"))
	side := filepath.Join(c.Content, "old-transcript.yaml")
	putRecord(t, side, Record{"kind": "Transcript", "item": "public-id", "text": "words"})
	r, err := Migrate(c, true)
	if err != nil || integer(r["moved"]) != 2 || !exists(src) {
		t.Fatal(r, err)
	}
	r, err = Migrate(c, false)
	if err != nil {
		t.Fatal(r, err)
	}
	target := filepath.Join(c.Content, "creator", "a-title.yaml")
	doc, err := readYAML(target)
	if err != nil || str(doc["id"]) != "public-id" || c.Resolved(str(doc["audio"]), filepath.Dir(target)) != audio || exists(src) {
		t.Fatal(doc, err)
	}
	b, _ := os.ReadFile(target)
	if !strings.Contains(string(b), "# keep this note") {
		t.Fatal("migration lost comments")
	}
	if !exists(strings.TrimSuffix(target, ".yaml") + ".transcript.yaml") {
		t.Fatal("transcript lost")
	}
}
func TestCachedIngestPreservesCuratedFieldsAndExistingName(t *testing.T) {
	c := testConfig(t)
	audio := filepath.Join(c.Root, "input.mp3")
	putFile(t, audio, bytes.Repeat([]byte("audio"), 100))
	source := filepath.Join(c.Sources, "input.yaml")
	putRecord(t, source, Record{"audio": audio, "title": "Source Title", "author": "Creator", "description": "Creator's own description.", "tags": []string{"Hypnosis", "unregistered"}})
	e := NewEngine(c)
	defer e.Close()
	sources, err := LoadSources(c.Sources)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := PlanSources(c, sources, e.Index, e.Fingerprints, PlanOptions{})
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	j := jobs[0]
	fp, err := e.fingerprint(j)
	if err != nil {
		t.Fatal(err)
	}
	text := "A transcript. With evidence."
	if err = writeJSON(filepath.Join(c.Transcripts(), fp+".json"), Record{"text": text, "duration": 30, "model": "fixture", "segments": []any{}}); err != nil {
		t.Fatal(err)
	}
	if err = e.Store.Put(TranscriptKey(text), Record{"analysis": Record{"summary": "analysis"}, "final": Record{"summary": "Generated summary.", "description": "Model description.", "tags": []string{"Relaxation"}}}, fp, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err = PlaceMedia(context.Background(), c, j.Source, j.Stem); err != nil {
		t.Fatal(err)
	}
	if err = e.Emit(context.Background(), j, false, false, false); err != nil {
		t.Fatal(err)
	}
	r, err := readYAML(j.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(str(r["description"]), "Creator's own") || contains(texts(r["tags"]), "unregistered") || str(r["summary"]) != "Generated summary." {
		t.Fatal(r)
	}
	r["id"] = "public-forever"
	r["title"] = "Curated Title"
	r["custom"] = Record{"keep": true}
	putRecord(t, j.Path, r)
	renamed := filepath.Join(filepath.Dir(j.Path), "curated-name.yaml")
	if err = os.Rename(j.Path, renamed); err != nil {
		t.Fatal(err)
	}
	jobs, err = PlanSources(c, sources, e.Index, e.Fingerprints, PlanOptions{Redo: true})
	if err != nil || len(jobs) != 1 || jobs[0].Path != renamed {
		t.Fatal("renamed item not adopted", jobs, err)
	}
	j = jobs[0]
	if err = e.Emit(context.Background(), j, false, true, false); err != nil {
		t.Fatal(err)
	}
	r, _ = readYAML(renamed)
	if str(r["id"]) != "public-forever" || str(r["title"]) != "Curated Title" || !truth(record(r["custom"])["keep"]) || !strings.Contains(str(r["description"]), "Creator's own") {
		t.Fatal("curated fields changed", r)
	}
	before, _ := os.Stat(renamed)
	if err = e.Emit(context.Background(), j, false, true, false); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(renamed)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("identical rerun changed item mtime")
	}
}
func TestCheckDoesNotCreateCache(t *testing.T) {
	c := testConfig(t)
	putRecord(t, filepath.Join(c.Sources, "one.yaml"), Record{"title": "One", "author": "A", "audio": "missing.mp3"})
	var out, errs bytes.Buffer
	if code := Main(context.Background(), []string{"-r", c.Root, "check"}, &out, &errs); code != 0 {
		t.Fatal(code, errs.String())
	}
	if exists(c.Cache) {
		t.Fatal("check created disposable cache")
	}
}
func TestASelfPointingLinkDoesNotBlockTheWriteUnderIt(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "cover.png")
	// Exactly the shape a bad placement left behind: a link whose target, once
	// the ".." are folded away, is the link itself.
	if err := os.Symlink(filepath.Join(dir, "sub", "..", "cover.png"), dest); err != nil {
		t.Fatal(err)
	}
	if _, err := AtomicWrite(dest, []byte("drawn"), 0644); err == nil {
		t.Fatal("expected the write through a self-pointing link to fail; " +
			"if this stops failing, ClearLink's reason for existing has changed")
	}
	if err := ClearLink(dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatal("ClearLink left the link in place")
	}
	if _, err := AtomicWrite(dest, []byte("drawn"), 0644); err != nil {
		t.Fatal("the write should land once the link is off:", err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "drawn" {
		t.Fatalf("wrong content after the redraw: %q", b)
	}
}
func TestClearLinkLeavesARealFileAlone(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "cover.png")
	if _, err := AtomicWrite(dest, []byte("a drawn cover"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ClearLink(dest); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "a drawn cover" {
		t.Fatal("ClearLink removed a real file")
	}
}
