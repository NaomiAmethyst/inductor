// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tonePair writes a stereo file with one frequency in each ear, which is what a
// binaural track is: the beat the listener hears is the difference, and it is
// nowhere in the file.
func tonePair(t *testing.T, path string, left, right float64, seconds int) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	filter := fmt.Sprintf(
		"sine=frequency=%g:duration=%d[l];sine=frequency=%g:duration=%d[r];[l][r]join=inputs=2:channel_layout=stereo",
		left, seconds, right, seconds)
	cmd := exec.Command("ffmpeg", "-v", "error", "-y", "-filter_complex", filter, path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not synthesise a test tone: %v %s", err, out)
	}
}

func TestATonePairIsMeasuredAsABinauralBeat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tones.wav")
	tonePair(t, path, 200, 208, 40)
	r, err := Tones(context.Background(), path, 40)
	if err != nil {
		t.Fatal(err)
	}
	if got := number(r["carrier_hz"]); math.Abs(got-200) > 1 {
		t.Fatalf("carrier read as %.2f Hz, wanted 200", got)
	}
	// The whole point: a beat of 8 Hz exists between the ears and in neither
	// channel, so a mono measurement could not have found it.
	if got := number(r["beat_hz"]); math.Abs(got-8) > 1 {
		t.Fatalf("beat read as %.2f Hz, wanted 8", got)
	}
	if str(r["beat_band"]) != "alpha" {
		t.Fatalf("8 Hz should be alpha, got %q", str(r["beat_band"]))
	}
	if str(r["shape"]) != "binaural pair" {
		t.Fatalf("shape %q, wanted a binaural pair", str(r["shape"]))
	}
	if !truth(r["carrier_steady"]) || !truth(r["beat_steady"]) {
		t.Fatal("a constant tone pair should read as steady", r)
	}
	if line := ToneSentence(r); !strings.Contains(line, "binaural beat") {
		t.Fatalf("sentence did not describe the beat: %q", line)
	}
}

func TestOneToneInBothEarsIsNotCalledBinaural(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "same.wav")
	tonePair(t, path, 180, 180, 40)
	r, err := Tones(context.Background(), path, 40)
	if err != nil {
		t.Fatal(err)
	}
	// A track billed as binaural whose channels carry the same frequency has no
	// beat to hear, and saying so is the finding.
	if got := number(r["beat_hz"]); got > 1 {
		t.Fatalf("identical channels reported a %.2f Hz beat", got)
	}
	if str(r["shape"]) == "binaural pair" {
		t.Fatal("one tone in both ears is not a binaural pair")
	}
	if line := ToneSentence(r); !strings.Contains(line, "no binaural beat") {
		t.Fatalf("sentence should say there is no beat: %q", line)
	}
}

func TestAZeroShotLabelBelowTheFloorIsRecordedButNotBelieved(t *testing.T) {
	cfg := DefaultSound()
	// A ranking is not an identification. Handed a vocabulary with no word for
	// what it is hearing, the model returns the nearest entry and looks exactly
	// as confident as when it is right; the margin over the runner-up does not
	// distinguish the two, so only the absolute score may.
	forced := Record{"fits": false, "voice_confidence": .02,
		"sounds": []any{Record{"label": "whispering close to the microphone", "score": .18}},
		"tags":   []any{Record{"label": "Sine wave", "score": .6}}}
	if got := Labelled(forced, "sounds", 3); len(got) != 0 {
		t.Fatalf("a label under the floor was passed on as an identification: %v", got)
	}
	if got := Labelled(forced, "tags", 3); len(got) != 1 {
		t.Fatalf("the ontology tags are scored per class and stand on their own: %v", got)
	}
	block := strings.Join(SoundBlock(forced, cfg), "\n")
	if !strings.Contains(block, "nothing in the vocabulary matched") {
		t.Fatalf("the prompt should say the vocabulary had no word for it:\n%s", block)
	}
	if strings.Contains(block, "whispering") {
		t.Fatalf("a label under the floor reached the prompt:\n%s", block)
	}

	fitted := Record{"fits": true, "voice_confidence": .7,
		"sounds": []any{Record{"label": "a person speaking", "score": .52}}}
	if got := Labelled(fitted, "sounds", 3); len(got) != 1 {
		t.Fatalf("a label over the floor should be used: %v", got)
	}
}

func TestSoundBlockTellsAModelNotToDescribeWordsThatWereNotSpoken(t *testing.T) {
	cfg := DefaultSound()
	quiet := Record{"voice_confidence": .03, "fits": true,
		"sounds": []any{Record{"label": "a low humming drone", "score": .5}},
		"tones": Record{"shape": "binaural pair", "carrier_hz": 100.0, "beat_hz": 4.0,
			"beat_band": "theta", "beat_steady": true, "carrier_steady": true,
			"beat_drift": "steady", "purity": .6}}
	block := strings.Join(SoundBlock(quiet, cfg), "\n")
	for _, want := range []string{"none detected", "nobody speaking", "binaural beat", "theta"} {
		if !strings.Contains(block, want) {
			t.Fatalf("sound block missing %q:\n%s", want, block)
		}
	}
	if len(SoundBlock(Record{}, cfg)) != 0 {
		t.Fatal("nothing heard should render nothing")
	}
}

func TestSoundIsSatisfiedWhenThereIsNoModelToProduceIt(t *testing.T) {
	c := testConfig(t)
	e := NewEngine(c)
	audio := filepath.Join(c.Root, "audio.mp3")
	putAudio(t, audio, 1)
	j := Planned{&Source{Path: audio, Audio: audio, Author: "Creator", Title: "T", Data: Record{}},
		"t", filepath.Join(c.Content, "creator", "t.yaml")}
	// testConfig names no sound models, and Listen declines outright without
	// them. A predicate that probed the store anyway would queue work refused
	// on every run for ever, in silence, on a lane one job wide.
	if !e.ArtifactExists("sound", j, nil, false) {
		t.Fatal("sound was reported outstanding on a config that can never produce it")
	}
	if err := e.Listen(context.Background(), j, false); err == nil {
		t.Fatal("Listen should decline when no models are configured")
	} else if _, ok := err.(NothingToProduce); !ok {
		t.Fatalf("declining is not failing: %v", err)
	}

	e.Config.Transcribe.Sound = DefaultSound()
	if e.ArtifactExists("sound", j, nil, false) {
		t.Fatal("with models configured and nothing stored, sound is outstanding")
	}
	fp, err := e.fingerprint(j)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Sounds.Put(fp, Record{"voice_confidence": .8}); err != nil {
		t.Fatal(err)
	}
	if !e.ArtifactExists("sound", j, nil, false) {
		t.Fatal("a stored sound record should satisfy the artefact")
	}
}

func TestABeatThatWillNotHoldStillIsStillReported(t *testing.T) {
	// Measured on a real track whose beat wandered between 5 and 22 Hz: the
	// windows disagree, so no single figure describes it -- but "layered tones"
	// throws away the one number the recording is built on.
	r := Record{"shape": "tonal", "carrier_hz": 250.0, "carrier_steady": true,
		"beat_hz": 4.88, "beat_steady": false, "beat_span": 17.09, "purity": .5}
	line := ToneSentence(r)
	for _, want := range []string{"binaural beat", "does not hold still", "17.1"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
}

func TestOrdinarySpeechIsNotDescribedAsTones(t *testing.T) {
	// Every recording has a loudest partial. On a spoken one it is the speaker,
	// it moves because voices move, and reporting that as "tones present but not
	// fixed" announces a pitch range as though it were a design decision.
	speech := Record{"purity": .017, "shape": "low-frequency wash", "carrier_hz": 235.0,
		"carrier_min": 200.0, "carrier_max": 270.0, "carrier_steady": false,
		"low_share": .595, "mid_share": .398, "high_share": .007}
	if line := ToneSentence(speech); line != "" {
		t.Fatalf("a spoken recording was described as tonal: %q", line)
	}
	tonal := Record{"purity": .633, "shape": "binaural pair", "carrier_hz": 44.0,
		"carrier_steady": true, "beat_hz": 8.05, "beat_band": "alpha",
		"beat_steady": true, "beat_drift": "steady"}
	if line := ToneSentence(tonal); !strings.Contains(line, "binaural beat") {
		t.Fatalf("a tone track should still be described: %q", line)
	}
}

func TestAMeasuredNumberIsWrittenAsANumberAndComparesEqualNextTime(t *testing.T) {
	// The caches are decoded with UseNumber so an integer stays an integer.
	// json.Number is a string type, though, so yaml wrote these quoted, and a
	// quoted figure read back as a string never compares equal to the float
	// that produced it -- which had the same measurements rewritten across
	// every item on every run, silently, for ever.
	dir := t.TempDir()
	path := filepath.Join(dir, "item.yaml")
	cached := Record{}
	if err := writeJSON(filepath.Join(dir, "m.json"), Record{"beat_hz": 8.05, "windows": 5}); err != nil {
		t.Fatal(err)
	}
	cached = readJSON(filepath.Join(dir, "m.json"))
	doc := Record{"apiVersion": "hypnotica/v1", "kind": "Item", "id": "x",
		"sound": Record{"tones": cached}}
	if _, err := SaveDocument(path, doc); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"8.05"`) || strings.Contains(string(raw), `"5"`) {
		t.Fatalf("a measured number was written as a string:\n%s", raw)
	}
	// And the second pass must find nothing to do.
	wrote, err := SaveDocument(path, doc)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Fatal("the same block was written twice; the count would never fall")
	}

	// The comparison the apply passes actually make: the block read back off
	// the document against a fresh one built from the cache. This is the one
	// that was always false.
	stored, err := readYAML(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh := Record{"tones": readJSON(filepath.Join(dir, "m.json"))}
	if !equivalent(record(stored["sound"]), fresh) {
		t.Fatalf("a block rebuilt from the cache did not match the one on disk:\n  disk  %#v\n  cache %#v",
			record(stored["sound"])["tones"], fresh["tones"])
	}
}

func TestAFileTheBoxAlreadyHasIsNotCopiedToIt(t *testing.T) {
	c := testConfig(t)
	box := NewGPUBox(c, "")
	if err := box.Provision(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	// A local box shares everything by definition, and staging into its own
	// directory is how the no-host path works; the probe is for a real host.
	audio := filepath.Join(c.Root, "a.mp3")
	putFile(t, audio, []byte("audio bytes"))
	if got := box.shared(context.Background(), audio); got != "" {
		t.Fatalf("a local box should not claim a shared path: %q", got)
	}

	// Through a symlink, what is shared is the target: media/ belongs to the
	// workdir and the archive is what it points at.
	link := filepath.Join(c.Root, "link.mp3")
	if err := os.Symlink(audio, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	real, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	if real != audio {
		t.Fatalf("resolved to %q, wanted %q", real, audio)
	}
}

func TestAPartialArtStampComparesOnlyWhatItRecords(t *testing.T) {
	key := ArtKey("violet smoke", "blurry", "turbo")
	// A nameplate backfilled onto a picture drawn before stamping existed. It
	// knows what the picture says and nothing about the prompt, so only the
	// title may be judged -- treating the absent digest as a mismatch would
	// order every backfilled entry redrawn.
	plateOnly := Record{"image_from": Record{"nameplate": "The Quiet Room"}}
	if ArtStale(plateOnly, "violet smoke", "blurry", "turbo", "The Quiet Room") {
		t.Fatal("a matching nameplate with no prompt digest was called stale")
	}
	if !ArtStale(plateOnly, "violet smoke", "blurry", "turbo", "Renamed Since") {
		t.Fatal("a changed title must still be caught")
	}
	// The mirror case, which the library is full of: stamped before nameplates
	// were recorded, so the digest is judged and the title is not.
	digestOnly := Record{"image_from": Record{"prompt": key, "engine": "turbo"}}
	if ArtStale(digestOnly, "violet smoke", "blurry", "turbo", "Anything At All") {
		t.Fatal("a matching digest with no nameplate was called stale")
	}
	if !ArtStale(digestOnly, "different words", "blurry", "turbo", "Anything At All") {
		t.Fatal("a changed prompt must still be caught")
	}
	if ArtStale(Record{}, "violet smoke", "blurry", "turbo", "Unstamped") {
		t.Fatal("an unstamped picture is not stale")
	}
}

func TestAttributeBackfillsTheNameplateAPictureWasDrawnUnder(t *testing.T) {
	c := testConfig(t)
	putRecord(t, filepath.Join(c.Sources, "creator.yaml"),
		Record{"apiVersion": "inductor/v1", "kind": "Source", "records": []any{}})
	dir := filepath.Join(c.Content, "creator")
	// Renamed since it was drawn: what the picture shows is the title the
	// retitle set aside, so that is what the stamp must say.
	renamed := Record{"apiVersion": "hypnotica/v1", "kind": "Item", "id": "a",
		"author": "creator", "title": "Chair Bound", "cover": "../../media/cover/creator/a.png",
		"provenance": Record{"generated": []any{"cover"},
			"original_title": "Chair Bound (2580s, 44e332)", "retitled_by": "title rulings"}}
	// Never renamed: the stamp records the title it still has, which is worth
	// writing so the *next* rename is caught, and costs no redraw now.
	kept := Record{"apiVersion": "hypnotica/v1", "kind": "Item", "id": "b",
		"author": "creator", "title": "The Optometrist", "cover": "../../media/cover/creator/b.png",
		"provenance": Record{"generated": []any{"cover"},
			"image_from": Record{"prompt": ArtKey("violet smoke", "", "turbo"), "engine": "turbo"}}}
	// Not ours to stamp: the picture arrived with the source.
	theirs := Record{"apiVersion": "hypnotica/v1", "kind": "Item", "id": "d",
		"author": "creator", "title": "Supplied", "cover": "../../media/cover/creator/d.jpg",
		"provenance": Record{"generated": []any{"summary"}}}
	for name, doc := range map[string]Record{"a": renamed, "b": kept, "d": theirs} {
		putRecord(t, filepath.Join(dir, name+".yaml"), doc)
	}
	r, err := AttributeTree(c, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if integer(r["nameplates"]) != 2 || integer(r["renamed_since"]) != 1 {
		t.Fatalf("backfill counted wrong: %v", r)
	}
	got := optionalYAML(filepath.Join(dir, "a.yaml"))
	stamp := record(record(got["provenance"])["image_from"])
	if str(stamp["nameplate"]) != "Chair Bound (2580s, 44e332)" {
		t.Fatalf("renamed entry stamped with %q", str(stamp["nameplate"]))
	}
	// Stamped, so the staleness test now fires and the cover is redrawn.
	if !ArtStale(record(got["provenance"]), "prompt", "", "turbo", str(got["title"])) {
		t.Fatal("a cover showing the old title was not called stale")
	}
	kd := optionalYAML(filepath.Join(dir, "b.yaml"))
	ks := record(record(kd["provenance"])["image_from"])
	if str(ks["nameplate"]) != "The Optometrist" || str(ks["prompt"]) != ArtKey("violet smoke", "", "turbo") {
		t.Fatalf("existing stamp was not preserved alongside the nameplate: %v", ks)
	}
	if ArtStale(record(kd["provenance"]), "violet smoke", "", "turbo", "The Optometrist") {
		t.Fatal("an unrenamed entry was called stale by its own backfill")
	}
	// The digest it already had still counts for something.
	if !ArtStale(record(kd["provenance"]), "different words", "", "turbo", "The Optometrist") {
		t.Fatal("the existing prompt digest stopped being checked")
	}
	td := optionalYAML(filepath.Join(dir, "d.yaml"))
	if truth(record(record(td["provenance"])["image_from"])["nameplate"]) {
		t.Fatal("a cover that arrived with the source was stamped")
	}
	// Idempotent: a second pass finds nothing left to stamp.
	again, err := AttributeTree(c, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if integer(again["nameplates"]) != 0 {
		t.Fatalf("the backfill is not idempotent: %v", again)
	}
}

func TestAResumePointNamesTheEntryTheWayTheLibraryDoes(t *testing.T) {
	c := testConfig(t)
	e := NewEngine(c)
	// A file whose name already carries its creator: the computed id doubles
	// the prefix, the entry's own id does not, and a resume is matched against
	// the entry's own. Twelve recordings named this way failed a whole resume.
	audio := filepath.Join(c.Root, "ellechemy-bimbo-game-3.mp3")
	putAudio(t, audio, 1)
	path := filepath.Join(c.Content, "ellechemy", "ellechemy-bimbo-game-3.yaml")
	putRecord(t, path, Record{"apiVersion": "hypnotica/v1", "kind": "Item",
		"id": "ellechemy-bimbo-game-3", "author": "ellechemy", "title": "Bimbo Game 3"})
	j := Planned{&Source{Path: audio, Audio: audio, Author: "Ellechemy",
		Title: "Bimbo Game 3", Data: Record{}}, "ellechemy-bimbo-game-3", path}
	if computed := ItemID(j.Source.AuthorID(), j.Stem); computed == "ellechemy-bimbo-game-3" {
		t.Fatal("this test needs a stem whose computed id differs from the declared one")
	}
	states := []map[string]int{{"media": 0}}
	id, err := e.saveResume([]Planned{j}, states)
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	left, err := e.LoadResume(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0] != "ellechemy-bimbo-game-3" {
		t.Fatalf("resume recorded %v, which names no entry in the library", left)
	}
}

func TestNothingToReviewIsDeclinedRatherThanFailed(t *testing.T) {
	c := testConfig(t)
	e := NewEngine(c)
	// Three shapes of recording with no first pass behind them.
	brief := Record{"text": strings.Repeat("A few real words here. ", 3)}
	silent := Record{"text": "", "speech": "none"}
	empty := Record{"text": "..."}
	heard := Record{"voice_confidence": .02, "sounds": []any{Record{"label": "a low humming drone", "score": .5}}}

	if which, _ := e.reviewRoute(brief, nil); which != "solo" {
		t.Fatalf("a short but real transcript should be reviewed from itself, got %q", which)
	}
	// It has words, so it must not be told it has none -- even with sound
	// evidence sitting beside it.
	if which, _ := e.reviewRoute(brief, heard); which != "solo" {
		t.Fatalf("a recording with words was routed to the wordless prompt: %q", which)
	}
	if which, _ := e.reviewRoute(silent, heard); which != "wordless" {
		t.Fatalf("a silent recording with sound evidence should be described as sound, got %q", which)
	}
	which, why := e.reviewRoute(empty, nil)
	if which != "" || why == "" {
		t.Fatalf("with neither words nor sound there is nothing to review: %q %q", which, why)
	}
	if !strings.Contains(why, "nothing") {
		t.Fatalf("the reason should say what is missing: %q", why)
	}
}

func TestARepairReplacesThePlacementItSupersedes(t *testing.T) {
	c := testConfig(t)
	dir := filepath.Join(c.Media, "audio", "creator")
	// The shape that looped: the original placement is an .m4a, the repair is
	// always written as .mp3, and PlacedAudio answers .m4a first -- so the
	// broken file went on being the answer and the repair ran again every run.
	src := filepath.Join(c.Root, "song.m4a")
	putFile(t, src, []byte("the broken one"))
	stale := filepath.Join(dir, "song.m4a")
	fresh := filepath.Join(dir, "song.mp3")
	putFile(t, fresh, []byte("the repaired one"))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(src, stale); err != nil {
		t.Skip("symlinks unavailable")
	}
	s := &Source{Path: src, Audio: src, Author: "Creator", Title: "Song", Data: Record{}}
	if got := PlacedAudio(c, s, "song"); got != stale {
		t.Fatalf("this test needs .m4a to win the sort, got %q", got)
	}
	supersede(dir, "song", fresh, src)
	if exists(stale) {
		t.Fatal("the superseded placement is still there, so the repair is still not the answer")
	}
	if !exists(fresh) {
		t.Fatal("the repair was removed")
	}
	if got := PlacedAudio(c, s, "song"); got != fresh {
		t.Fatalf("after the sweep the repair should be the placement, got %q", got)
	}
	// Neighbours are not this stem's business.
	other := filepath.Join(dir, "song-two.m4a")
	putFile(t, other, []byte("a different recording"))
	supersede(dir, "song", fresh, src)
	if !exists(other) {
		t.Fatal("a different stem was swept")
	}
	// Nor is a file that merely shares the stem. Two source records can collide
	// on one, and the name is no evidence about which recording owns it.
	stranger := filepath.Join(dir, "song.ogg")
	putFile(t, stranger, []byte("somebody else's audio"))
	supersede(dir, "song", fresh, src)
	if !exists(stranger) {
		t.Fatal("a same-stem file that is not this recording was deleted")
	}
}

func TestADeclinedAnalysisIsNotAskedAgain(t *testing.T) {
	c := testConfig(t)
	e := NewEngine(c)
	audio := filepath.Join(c.Root, "a.mp3")
	putAudio(t, audio, 1)
	j := Planned{&Source{Path: audio, Audio: audio, Author: "Creator", Title: "T", Data: Record{}},
		"t", filepath.Join(c.Content, "creator", "t.yaml")}
	fp, err := e.fingerprint(j)
	if err != nil {
		t.Fatal(err)
	}
	// Too little transcript to analyse, and nothing about that can change while
	// the transcript is this one.
	if err = writeJSON(filepath.Join(c.Transcripts(), fp+".json"),
		Record{"text": "Too short.", "duration": 600}); err != nil {
		t.Fatal(err)
	}
	if e.ArtifactExists("analysis", j, nil, false) {
		t.Fatal("nothing has been decided yet")
	}
	err = e.Analyze(context.Background(), j, false)
	if _, ok := err.(NothingToProduce); !ok {
		t.Fatalf("expected a decline, got %v", err)
	}
	if !e.ArtifactExists("analysis", j, nil, false) {
		t.Fatal("the decline was not remembered; it would be asked again on every run")
	}
	// A better transcript is a different question, and gets asked.
	if err = writeJSON(filepath.Join(c.Transcripts(), fp+".json"),
		Record{"text": strings.Repeat("Real words in the recording. ", 20), "duration": 600}); err != nil {
		t.Fatal(err)
	}
	if e.ArtifactExists("analysis", j, nil, false) {
		t.Fatal("a new transcript should be analysed, not covered by the old refusal")
	}
}

func TestASourceCanSayWhoTheCreatorIs(t *testing.T) {
	c := testConfig(t)
	putRecord(t, filepath.Join(c.Sources, "creator.yaml"), Record{
		"apiVersion": "inductor/v1", "kind": "Author", "name": "Some Creator",
		"url": "https://example.invalid", "language": "en",
		"links":       Record{"website": "https://example.invalid"},
		"image":       "/srv/art/some-creator.jpg",
		"description": "<p>What they say about themselves.</p>",
	})
	r, err := LoadSources(c.Sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Errors) > 0 {
		t.Fatalf("an author record was read as a broken recording: %v", r.Errors)
	}
	if len(r.Warnings) > 0 {
		t.Fatalf("unexpected warnings: %v", r.Warnings)
	}
	supplied := r.Authors["some-creator"]
	if !truth(supplied) {
		t.Fatalf("author not indexed by id; got %v", sortedKeys(r.Authors))
	}

	page := NewAuthorPage("some-creator", "Some Creator", supplied)
	for _, f := range []string{"url", "links", "language", "image", "description"} {
		if !truth(page[f]) {
			t.Fatalf("%s was thrown away", f)
		}
	}
	// The point of the exercise: a field the creator supplied must not be listed
	// in `needs`, or the next run asks a model to write over their own words.
	if contains(texts(page["needs"]), "description") {
		t.Fatal("the creator's own description was queued for replacement")
	}
	if !contains(texts(page["needs"]), "summary") {
		t.Fatal("a summary nobody supplied should still be wanted")
	}
	// And it is theirs, so nothing claims the toolchain wrote it.
	prov := record(page["provenance"])
	if !truth(prov["description_from_source"]) {
		t.Fatal("a supplied description must be recorded as the creator's")
	}
	if contains(texts(prov["generated"]), "description") {
		t.Fatal("the creator's description was marked as generated")
	}

	// With nothing supplied, the page asks for both and claims nothing.
	bare := NewAuthorPage("nobody", "Nobody", nil)
	if got := texts(bare["needs"]); len(got) != 2 {
		t.Fatalf("a bare page should want a summary and a description, got %v", got)
	}
	if truth(bare["provenance"]) {
		t.Fatal("a bare page should assert no provenance at all")
	}
}

func TestANameplateFallsBackWhenTheFaceCannotDrawIt(t *testing.T) {
	// The fault this guards against reports success everywhere: the font hands
	// back a glyph index and a correct advance and then rasterises nothing, so
	// letters vanish while the ones that remain are spaced as though they had
	// not. One creator's name came out as "CesS", another's as "Ct", and no
	// error was raised anywhere along the way.
	broken := "elegant-serif"
	if _, ok := Fonts[broken]; !ok {
		t.Skip("that face is no longer configured")
	}
	if _, err := os.Stat(Fonts[broken][0]); err != nil {
		t.Skip("that font is not installed here")
	}
	got := ChosenFace(broken, "ClairesNSFW")
	if got == broken {
		t.Skip("this font renders fine on this machine; nothing to fall back from")
	}
	if got == "" {
		t.Fatal("no face at all could draw an ordinary name")
	}
	// Whatever it fell back to must actually be able to draw the thing.
	if again := ChosenFace(got, "ClairesNSFW"); again != got {
		t.Fatalf("fell back to %q, which cannot draw it either", got)
	}
	// A face that works is left alone.
	if ChosenFace("heavy-sans", "ClairesNSFW") != "heavy-sans" {
		t.Fatal("a working face was substituted for no reason")
	}
	// And the menu no longer offers the one that does not work.
	if strings.Contains(FontChoices(), broken) {
		t.Fatalf("a face that cannot be drawn is still offered:\n%s", FontChoices())
	}
}

func TestAPictureSetInAFaceThatCannotDrawItIsCalledStale(t *testing.T) {
	key := ArtKey("violet smoke", "", "turbo")
	// Same prompt, same title: only the face is in question.
	fresh := Record{"image_from": Record{"prompt": key, "engine": "turbo",
		"nameplate": "The Quiet Room", "face": "heavy-sans"}}
	if ArtStale(fresh, "violet smoke", "", "turbo", "The Quiet Room") {
		t.Fatal("a picture set in a face that still works was called stale")
	}
	// A picture stamped with a face that can no longer draw those words wants
	// redrawing -- but only if the substitution actually applies to *these*
	// words, which is why the test is per picture and not per font.
	broken := "elegant-serif"
	if _, ok := Fonts[broken]; !ok {
		t.Skip("that face is no longer configured")
	}
	if _, err := os.Stat(Fonts[broken][0]); err != nil {
		t.Skip("that font is not installed here")
	}
	spoiled := "ClairesNSFW"
	if ChosenFace(broken, spoiled) == broken {
		t.Skip("this font renders fine here; nothing to detect")
	}
	old := Record{"image_from": Record{"prompt": key, "engine": "turbo",
		"nameplate": spoiled, "face": broken}}
	if !ArtStale(old, "violet smoke", "", "turbo", spoiled) {
		t.Fatal("a nameplate with letters missing was not called stale")
	}
	// And a stamp from before faces were recorded is not called stale for
	// lacking one -- the same rule as every other field in that stamp.
	unstamped := Record{"image_from": Record{"prompt": key, "engine": "turbo",
		"nameplate": spoiled}}
	if ArtStale(unstamped, "violet smoke", "", "turbo", spoiled) {
		t.Fatal("a stamp with no face recorded was called stale for lacking one")
	}
}

func TestControlCharactersAreStrippedFromTitles(t *testing.T) {
	// The real one, from a rip: it is not whitespace so nothing trims it, not
	// punctuation so nothing cleans it, and it slugs away to nothing so the
	// filename looks right. It surfaces only where a font is asked for a glyph.
	const dirty = "Advanced Chastity: B\x04rainwashing"
	if got := StripControls(dirty); got != "Advanced Chastity: Brainwashing" {
		t.Fatalf("got %q", got)
	}
	if got := StripControls("A\ttitle\nover two lines"); got != "A title over two lines" {
		t.Fatalf("tabs and newlines should become spaces, got %q", got)
	}
	// Ordinary text, including anything above ASCII, is left exactly as it is.
	for _, keep := range []string{"Café — Morning", "♯10 LB", "naïve", "日本語", "it's"} {
		if got := StripControls(keep); got != keep {
			t.Fatalf("%q was altered to %q", keep, got)
		}
	}
	// And a source carrying one is cleaned as it is read, so nothing downstream
	// ever sees it.
	c := testConfig(t)
	audio := filepath.Join(c.Root, "a.mp3")
	putAudio(t, audio, 1)
	putRecord(t, filepath.Join(c.Sources, "creator.yaml"), Record{
		"apiVersion": "inductor/v1", "kind": "Source",
		"audio": audio, "title": dirty, "author": "Some\x01Creator"})
	r, err := LoadSources(c.Sources)
	if err != nil || len(r.Sources) != 1 {
		t.Fatal(err, len(r.Sources))
	}
	if r.Sources[0].Title != "Advanced Chastity: Brainwashing" {
		t.Fatalf("title reached the pipeline dirty: %q", r.Sources[0].Title)
	}
	if r.Sources[0].Author != "SomeCreator" {
		t.Fatalf("author reached the pipeline dirty: %q", r.Sources[0].Author)
	}
}
