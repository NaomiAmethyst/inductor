// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// putAudio writes a real, short, decodable file. Fixtures used to be a handful
// of bytes named .mp3, which was fine until the pipeline started refusing audio
// that does not decode -- a check whose whole purpose is to reject exactly that.
func putAudio(t *testing.T, p string, seconds float64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	// A tone per path, so two fixtures are never the same recording: some tests
	// lean on distinct fingerprints and a shared 440Hz would collapse them.
	tone := 200
	for _, r := range p {
		tone = (tone*31 + int(r)) % 4000
	}
	out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi",
		"-i", fmt.Sprintf("sine=frequency=%d:duration=%g", 200+tone, seconds), p).CombinedOutput()
	if err != nil {
		t.Skipf("no ffmpeg for audio fixtures: %v %s", err, out)
	}
}

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

func TestPlanningTakesEveryAuthorNamedAndAllOfThemWhenNoneIs(t *testing.T) {
	c := testConfig(t)
	for _, who := range []string{"Alpha", "Beta", "Gamma"} {
		audio := filepath.Join(c.Root, who+".mp3")
		putFile(t, audio, bytes.Repeat([]byte(who), 100))
		putRecord(t, filepath.Join(c.Sources, who+".yaml"),
			Record{"audio": audio, "title": who + " Title", "author": who})
	}
	e := NewEngine(c)
	defer e.Close()
	sources, err := LoadSources(c.Sources)
	if err != nil {
		t.Fatal(err)
	}
	planned := func(who ...string) []string {
		jobs, err := PlanSources(c, sources, e.Index, e.Fingerprints, PlanOptions{Authors: who})
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, j := range jobs {
			out = append(out, j.Source.AuthorID())
		}
		return out
	}
	if got := planned(); len(got) != 3 {
		t.Fatal("naming nobody should plan everybody, got", got)
	}
	if got := planned("alpha"); !equivalent(got, []string{"alpha"}) {
		t.Fatal("naming one should plan only that one, got", got)
	}
	if got := planned("alpha", "gamma"); !equivalent(got, []string{"alpha", "gamma"}) {
		t.Fatal("naming two should plan both, got", got)
	}
	if got := planned("nobody"); len(got) != 0 {
		t.Fatal("naming an unknown creator should plan nothing, got", got)
	}
}

func TestArtIsStaleWhenItsPromptIsRestated(t *testing.T) {
	made := Record{"tagged": "violet smoke, tarot", "natural": "A violet room.", "font": "heavy-sans"}
	// The picture on disk was drawn from exactly these words, so nothing to do.
	if !SamePrompt(Record{"tagged": "violet smoke, tarot", "natural": "A violet room.", "font": "heavy-sans"}, made) {
		t.Fatal("an unchanged prompt should not call the art stale")
	}
	// Whitespace is not a different instruction.
	if !SamePrompt(Record{"tagged": " violet smoke, tarot ", "natural": "A violet room.\n", "font": "heavy-sans"}, made) {
		t.Fatal("whitespace should not call the art stale")
	}
	// Any of the three fields the renderer reads is enough to change the picture.
	for _, changed := range []Record{
		{"tagged": "green smoke, tarot", "natural": "A violet room.", "font": "heavy-sans"},
		{"tagged": "violet smoke, tarot", "natural": "A green room.", "font": "heavy-sans"},
		{"tagged": "violet smoke, tarot", "natural": "A violet room.", "font": "slab"},
	} {
		if SamePrompt(changed, made) {
			t.Fatalf("a restated prompt should call the art stale: %v", changed)
		}
	}
	// A creator who has never had a prompt has nothing the picture could match.
	if SamePrompt(Record{}, made) || SamePrompt(nil, made) {
		t.Fatal("a missing prompt should call the art stale")
	}
}

func TestArtRemembersTheWordsThatDrewIt(t *testing.T) {
	prov := Record{}
	StampArt(prov, "violet smoke, tarot", "blurry", "turbo")
	if str(record(prov["image_from"])["engine"]) != "turbo" || str(record(prov["image_from"])["prompt"]) == "" {
		t.Fatal("the stamp records neither engine nor prompt", prov)
	}
	if ArtStale(prov, "violet smoke, tarot", "blurry", "turbo") {
		t.Fatal("the same instruction should not read as stale")
	}
	// Each half of the instruction, and the engine that picks the wording, is
	// enough to make a different picture.
	for _, c := range [][3]string{
		{"green smoke, tarot", "blurry", "turbo"},
		{"violet smoke, tarot", "washed out", "turbo"},
		{"violet smoke, tarot", "blurry", "flux"},
	} {
		if !ArtStale(prov, c[0], c[1], c[2]) {
			t.Fatalf("a changed instruction should read as stale: %v", c)
		}
	}
	// Every picture drawn before this was recorded carries no stamp. Calling
	// those stale would order the whole library redrawn over a missing field.
	if ArtStale(Record{}, "anything", "", "turbo") {
		t.Fatal("an unstamped picture must not read as stale")
	}
}

func TestBrokenAudioIsRefusedBeforeAnythingProcessesIt(t *testing.T) {
	c := testConfig(t)
	e := NewEngine(c)
	defer e.Close()
	sound := filepath.Join(c.Root, "sound.mp3")
	putAudio(t, sound, 1)
	broken := filepath.Join(c.Root, "broken.mp3")
	putFile(t, broken, bytes.Repeat([]byte("not audio at all"), 64))

	if complaint, err := e.Soundness.Of(context.Background(), sound); err != nil || complaint != "" {
		t.Fatal("clean audio was called broken", complaint, err)
	}
	complaint, err := e.Soundness.Of(context.Background(), broken)
	if err != nil || complaint == "" {
		t.Fatal("a file that does not decode was called sound", complaint, err)
	}
	// The verdict is kept, or every run pays to decode the library again.
	if err := e.Soundness.Save(); err != nil {
		t.Fatal(err)
	}
	again := NewSoundnessCache(filepath.Join(c.Cache, "soundness.json"))
	if got, _ := again.Of(context.Background(), broken); got != complaint {
		t.Fatalf("the verdict did not survive a reload: %q vs %q", got, complaint)
	}

	// And the graph refuses it rather than producing half an item.
	s := &Source{Path: broken, Audio: broken, Author: "Creator", Title: "Broken", Data: Record{}}
	job := Planned{s, "broken", filepath.Join(c.Content, "creator", "broken.yaml")}
	if err := e.soundEnough(context.Background(), job); err == nil {
		t.Fatal("the pipeline accepted audio that does not decode")
	} else if !strings.Contains(err.Error(), "half a file") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
}

func TestAnInterruptedRunLeavesAResumePoint(t *testing.T) {
	c := testConfig(t)
	e := NewEngine(c)
	defer e.Close()
	jobs := []Planned{}
	for _, who := range []string{"one", "two", "three"} {
		src := &Source{Path: filepath.Join(c.Sources, who+".yaml"), Audio: filepath.Join(c.Root, who+".mp3"),
			Author: "Creator", Title: who, Data: Record{}}
		jobs = append(jobs, Planned{src, who, filepath.Join(c.Content, "creator", who+".yaml")})
	}
	// The first finished every required artefact; the others did not.
	states := []map[string]int{{}, {}, {}}
	for _, a := range Graph {
		states[0][a.Name] = 2
		states[1][a.Name] = 2
	}
	for _, a := range Graph {
		if a.Required {
			states[1][a.Name] = 1 // still running when the interrupt arrived
			break
		}
	}
	id, err := e.saveResume(jobs, states)
	if err != nil || id == "" {
		t.Fatal("no resume point written", id, err)
	}
	left, err := e.LoadResume(id)
	if err != nil {
		t.Fatal(err)
	}
	// The one that finished is not asked for again; the other two are.
	if len(left) != 2 || contains(left, ItemID("creator", "one")) {
		t.Fatalf("resume point should hold only unfinished work, got %v", left)
	}
	for _, want := range []string{ItemID("creator", "two"), ItemID("creator", "three")} {
		if !contains(left, want) {
			t.Fatalf("resume point lost %s: %v", want, left)
		}
	}
	if _, err := e.LoadResume("run-0-deadbeef"); err == nil {
		t.Fatal("an unknown resume point should say so rather than run everything")
	}
	// Nothing outstanding means nothing to resume, and no stray file.
	done := []map[string]int{{}, {}, {}}
	for i := range done {
		for _, a := range Graph {
			done[i][a.Name] = 2
		}
	}
	if id, err := e.saveResume(jobs, done); err != nil || id != "" {
		t.Fatal("a finished run should leave no resume point", id, err)
	}
}
