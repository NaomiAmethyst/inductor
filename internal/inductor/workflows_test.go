// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRulingsPreserveColonDefinitionsAndRefuseUnknownTargets(t *testing.T) {
	c := testConfig(t)
	p := filepath.Join(c.Content, "creator", "one.yaml")
	putRecord(t, p, Record{"kind": "Item", "id": "one", "author": "creator", "tags": []string{"Hypnosis"}, "provenance": Record{"proposed_tags": []any{Record{"tag": "Calm"}, Record{"tag": "Voice: fem"}}}})
	rulings := []any{Record{"tag": "Calm", "verdict": "approve", "description": "Calm: a sustained relaxed mood."}, Record{"tag": "Voice: fem", "verdict": "merge", "merge_into": "Voice: masc"}}
	r, err := ApplyRulings(c, rulings, false)
	if err != nil || integer(r["tagged"]) != 2 {
		t.Fatal(r, err)
	}
	before, _ := readYAML(p)
	if contains(texts(before["tags"]), "Calm") {
		t.Fatal("dry ruling mutated item")
	}
	if _, err = ApplyRulings(c, rulings, true); err != nil {
		t.Fatal(err)
	}
	registry, err := LoadRegistry(c.RegistryPath())
	if err != nil || registry.Meanings["Calm"] != "Calm: a sustained relaxed mood." {
		t.Fatal(registry, err)
	}
	got, _ := readYAML(p)
	if !contains(texts(got["tags"]), "Calm") || !contains(texts(got["tags"]), "Voice: masc") || truth(record(got["provenance"])["proposed_tags"]) {
		t.Fatal(got)
	}
}
func TestBackfillHonorsReviewerRejections(t *testing.T) {
	c := testConfig(t)
	store := AnalysisStore{Root: c.Analysis()}
	for _, id := range []string{"kept", "rejected"} {
		putRecord(t, filepath.Join(c.Content, "creator", id+".yaml"), Record{"kind": "Item", "id": id, "author": "creator", "provenance": Record{"fingerprint": id}})
		verdict := "keep"
		if id == "rejected" {
			verdict = "reject"
		}
		if err := store.Put(id, Record{"analysis": Record{"tags": Record{"proposed": []any{Record{"tag": "Relaxation"}}}}, "final": Record{"new_tags": []any{Record{"tag": "Relaxation", "verdict": verdict}}}}, id, ""); err != nil {
			t.Fatal(err)
		}
	}
	report, err := Backfill(c, nil, true)
	if err != nil || integer(report["added"]) != 1 {
		t.Fatal(report, err)
	}
	kept := optionalYAML(filepath.Join(c.Content, "creator", "kept.yaml"))
	rejected := optionalYAML(filepath.Join(c.Content, "creator", "rejected.yaml"))
	if !contains(texts(kept["tags"]), "Relaxation") || truth(rejected["tags"]) {
		t.Fatal(kept, rejected)
	}
}
func TestDuplicateMergeRetainsSurvivorAndAbsorbedSources(t *testing.T) {
	c := testConfig(t)
	audio := filepath.Join(c.Root, "audio.mp3")
	putFile(t, audio, []byte("shared recording"))
	fp, err := Fingerprint(audio)
	if err != nil {
		t.Fatal(err)
	}
	keepPath := filepath.Join(c.Content, "creator", "title.yaml")
	dropPath := filepath.Join(c.Content, "creator", "title-0.yaml")
	keep := Record{"kind": "Item", "id": "public-id", "author": "creator", "title": "Curated Title", "audio": audio, "provenance": Record{"fingerprint": fp, "source_key": "first"}}
	drop := Record{"kind": "Item", "id": "drop-id", "author": "creator", "title": "Curated Title", "audio": audio, "description": "A richer description.", "tags": []string{"Hypnosis"}, "provenance": Record{"fingerprint": fp, "source_key": "second"}}
	putRecord(t, keepPath, keep)
	putRecord(t, dropPath, drop)
	groups, err := FindDuplicates(c.Content)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ResolveDuplicates(groups[SameTitle], nil, c.Content, true, true); err != nil {
		t.Fatal(err)
	}
	got := optionalYAML(keepPath)
	if str(got["id"]) != "public-id" || str(got["description"]) != "A richer description." || !contains(texts(record(got["provenance"])["merged_source_keys"]), "second") || exists(dropPath) {
		t.Fatal(got)
	}
	s := &Source{Path: "source.yaml", Audio: "second", Author: "Creator", Title: "Curated Title", Data: Record{}}
	engine := NewEngine(c)
	jobs, err := PlanSources(c, SourceReport{Sources: []*Source{s}}, engine.Index, engine.Fingerprints, PlanOptions{Redo: true})
	if err != nil || len(jobs) != 0 {
		t.Fatal("merged source recreated", jobs, err)
	}
}
func TestExportDropsGeneratedProseAndTags(t *testing.T) {
	r := ExportRecord(Record{"id": "id", "title": "Creator title", "audio": "audio.mp3", "description": "Own description", "summary": "Generated", "tags": []string{"Hypnosis", "Relaxation"}, "spoilers": []any{Record{"quote": "model"}}, "provenance": Record{"description_from_source": true, "metadata_source": "site:fixture", "generated": []string{"summary", "tags", "spoilers"}, "enriched": Record{"tags_added": []string{"Relaxation"}}}}, "Creator")
	if truth(r["summary"]) || truth(r["spoilers"]) || str(r["description"]) != "Own description" || contains(texts(r["tags"]), "Relaxation") {
		t.Fatal(r)
	}
}
func TestOrphansNeverDeletesUnmatchedItemsOrAudio(t *testing.T) {
	c := testConfig(t)
	if err := os.MkdirAll(c.Sources, 0755); err != nil {
		t.Fatal(err)
	}
	audio := filepath.Join(c.Root, "audio.mp3")
	putFile(t, audio, []byte("audio"))
	item := filepath.Join(c.Content, "creator", "one.yaml")
	putRecord(t, item, Record{"kind": "Item", "id": "one", "author": "creator", "audio": audio})
	author := filepath.Join(c.Content, "empty", "_author.yaml")
	putRecord(t, author, Record{"kind": "Author", "id": "empty"})
	stray := filepath.Join(c.Content, "stray.transcript.yaml")
	putRecord(t, stray, Record{"kind": "Transcript", "item": "missing"})
	if _, err := Orphans(c, true); err != nil {
		t.Fatal(err)
	}
	if !exists(item) || !exists(audio) || exists(author) || exists(stray) {
		t.Fatal("incorrect orphan cleanup")
	}
}
func TestAddAndExportRoundTrip(t *testing.T) {
	c := testConfig(t)
	source := filepath.Join(c.Root, "recording.mp3")
	putFile(t, source, []byte("audio fixture"))
	engine := NewEngine(c)
	report, err := engine.Add(context.Background(), AddOptions{Paths: []string{source}, Author: "creator", AuthorName: "Creator", Tags: []string{"Hypnosis"}})
	if err != nil {
		t.Fatal(report, err)
	}
	docs, err := Documents(c.Content, "item")
	if err != nil || len(docs) != 1 {
		t.Fatal(docs, err)
	}
	export := filepath.Join(c.Root, "exported")
	report, err = ExportTree(c, c.Content, export, "", "")
	if err != nil {
		t.Fatal(report, err)
	}
	sources, err := LoadSources(export)
	if err != nil || len(sources.Sources) != 1 || sources.Sources[0].AudioPath(export) == "" {
		t.Fatal(sources, err)
	}
}
func TestDuplicateMergeKeepsTheSurvivorsOwnProvenance(t *testing.T) {
	c := testConfig(t)
	audio := filepath.Join(c.Root, "audio.mp3")
	putFile(t, audio, []byte("shared recording"))
	fp, err := Fingerprint(audio)
	if err != nil {
		t.Fatal(err)
	}
	keepPath := filepath.Join(c.Content, "creator", "title.yaml")
	dropPath := filepath.Join(c.Content, "creator", "title-0.yaml")
	// The survivor knows things about itself, including that it has already
	// absorbed something once. The other document is *richer*, so the fold seeds
	// the merged record from it -- which is exactly when the survivor's own
	// provenance used to be dropped on the floor.
	putRecord(t, keepPath, Record{"kind": "Item", "id": "public-id", "author": "creator",
		"title": "Curated Title", "audio": audio, "description": "Written by the creator.",
		"provenance": Record{"fingerprint": fp, "source_key": "first",
			"archive_path": "Collection/MP3/one.mp3", "original_title": "One (mp3)",
			"merged_source_keys": []string{"absorbed-earlier"}}})
	putRecord(t, dropPath, Record{"kind": "Item", "id": "drop-id", "author": "creator",
		"title": "Curated Title", "audio": audio, "source_url": "https://example.test/one/",
		"summary": "A machine summary.", "tags": []string{"Hypnosis"},
		"provenance": Record{"fingerprint": fp, "source_key": "second",
			"generated": []string{"summary", "tags"}}})
	groups, err := FindDuplicates(c.Content)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ResolveDuplicates(groups[SameTitle], nil, c.Content, true, true); err != nil {
		t.Fatal(err)
	}
	got := optionalYAML(keepPath)
	p := record(got["provenance"])
	if str(p["archive_path"]) != "Collection/MP3/one.mp3" {
		t.Fatal("the survivor's archive_path went with the seed:", p)
	}
	if str(p["original_title"]) != "One (mp3)" {
		t.Fatal("the survivor's original_title went with the seed:", p)
	}
	for _, want := range []string{"absorbed-earlier", "second"} {
		if !contains(texts(p["merged_source_keys"]), want) {
			t.Fatalf("merged_source_keys lost %q: %v", want, p["merged_source_keys"])
		}
	}
	// The summary came from the other document, which said it wrote it.
	if !contains(texts(p["generated"]), "summary") {
		t.Fatal("a generated field that survived the merge is unmarked:", p["generated"])
	}
	// The description came from the survivor, which did not.
	if contains(texts(p["generated"]), "description") {
		t.Fatal("the creator's own description was marked machine-made:", p["generated"])
	}
}
func TestAMergeWillNotMarkAValueTwoDocumentsDisagreeAbout(t *testing.T) {
	c := testConfig(t)
	audio := filepath.Join(c.Root, "audio.mp3")
	putFile(t, audio, []byte("shared recording"))
	fp, err := Fingerprint(audio)
	if err != nil {
		t.Fatal(err)
	}
	keepPath := filepath.Join(c.Content, "creator", "title.yaml")
	dropPath := filepath.Join(c.Content, "creator", "title-0.yaml")
	// Both hold the same description. One says it generated it; the other, which
	// is the survivor, does not. Marking it would tell the creator their own
	// writing was machine-made, so the disagreement resolves to "unmarked".
	shared := "The same words in both documents."
	putRecord(t, keepPath, Record{"kind": "Item", "id": "public-id", "author": "creator",
		"title": "Curated Title", "audio": audio, "description": shared,
		"provenance": Record{"fingerprint": fp, "source_key": "first"}})
	putRecord(t, dropPath, Record{"kind": "Item", "id": "drop-id", "author": "creator",
		"title": "Curated Title", "audio": audio, "description": shared,
		"source_url": "https://example.test/one/", "tags": []string{"Hypnosis"},
		"provenance": Record{"fingerprint": fp, "source_key": "second",
			"generated": []string{"description"}}})
	groups, err := FindDuplicates(c.Content)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ResolveDuplicates(groups[SameTitle], nil, c.Content, true, true); err != nil {
		t.Fatal(err)
	}
	if g := texts(record(optionalYAML(keepPath)["provenance"])["generated"]); contains(g, "description") {
		t.Fatal("marked a description the survivor claims as its own:", g)
	}
}
func TestAUnionFieldStaysMarkedWhenBothDocumentsWroteIt(t *testing.T) {
	c := testConfig(t)
	audio := filepath.Join(c.Root, "audio.mp3")
	putFile(t, audio, []byte("shared recording"))
	fp, err := Fingerprint(audio)
	if err != nil {
		t.Fatal(err)
	}
	keepPath := filepath.Join(c.Content, "creator", "title.yaml")
	dropPath := filepath.Join(c.Content, "creator", "title-0.yaml")
	// Tags are merged as a union, so the survivor's tags match neither document's
	// exactly. A rule that asks "which document held this value" therefore finds
	// nobody and unmarks a field both of them said a model wrote.
	putRecord(t, keepPath, Record{"kind": "Item", "id": "public-id", "author": "creator",
		"title": "Curated Title", "audio": audio, "tags": []string{"Hypnosis"},
		"provenance": Record{"fingerprint": fp, "source_key": "first",
			"generated": []string{"tags"}}})
	putRecord(t, dropPath, Record{"kind": "Item", "id": "drop-id", "author": "creator",
		"title": "Curated Title", "audio": audio, "tags": []string{"Relaxation"},
		"source_url": "https://example.test/one/",
		"provenance": Record{"fingerprint": fp, "source_key": "second",
			"generated": []string{"tags"}}})
	groups, err := FindDuplicates(c.Content)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ResolveDuplicates(groups[SameTitle], nil, c.Content, true, true); err != nil {
		t.Fatal(err)
	}
	got := optionalYAML(keepPath)
	for _, want := range []string{"Hypnosis", "Relaxation"} {
		if !contains(texts(got["tags"]), want) {
			t.Fatalf("the union lost %q: %v", want, got["tags"])
		}
	}
	if g := texts(record(got["provenance"])["generated"]); !contains(g, "tags") {
		t.Fatal("both documents wrote their tags; the merge unmarked them:", g)
	}
}
func TestAUnionFieldOneCreatorCuratedIsNotMarked(t *testing.T) {
	c := testConfig(t)
	audio := filepath.Join(c.Root, "audio.mp3")
	putFile(t, audio, []byte("shared recording"))
	fp, err := Fingerprint(audio)
	if err != nil {
		t.Fatal(err)
	}
	keepPath := filepath.Join(c.Content, "creator", "title.yaml")
	dropPath := filepath.Join(c.Content, "creator", "title-0.yaml")
	// The survivor's tags are the creator's own. Merging a model's tags in
	// alongside them must not relabel the creator's as machine-made.
	putRecord(t, keepPath, Record{"kind": "Item", "id": "public-id", "author": "creator",
		"title": "Curated Title", "audio": audio, "tags": []string{"Hypnosis"},
		"provenance": Record{"fingerprint": fp, "source_key": "first"}})
	putRecord(t, dropPath, Record{"kind": "Item", "id": "drop-id", "author": "creator",
		"title": "Curated Title", "audio": audio, "tags": []string{"Relaxation"},
		"source_url": "https://example.test/one/",
		"provenance": Record{"fingerprint": fp, "source_key": "second",
			"generated": []string{"tags"}}})
	groups, err := FindDuplicates(c.Content)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ResolveDuplicates(groups[SameTitle], nil, c.Content, true, true); err != nil {
		t.Fatal(err)
	}
	if g := texts(record(optionalYAML(keepPath)["provenance"])["generated"]); contains(g, "tags") {
		t.Fatal("the creator's own tags were marked machine-made:", g)
	}
}
func TestAMappingToATagTheRegistryLacksIsStillPending(t *testing.T) {
	c := testConfig(t)
	// The creator's map answers "humor" with "Humour", which the registry does
	// not have. `retag` therefore drops the tag and proposes it; the adjudicator
	// has to see it, or the proposal sits on the item for ever.
	putRecord(t, MappingPath(c, "creator"), Record{"author": "creator", "mapping": []any{
		Record{"tag": "humor", "verdict": "new", "to": "Humour"},
		Record{"tag": "banter", "verdict": "drop"},
		Record{"tag": "calm", "verdict": "map", "to": "Relaxation"},
	}})
	putRecord(t, filepath.Join(c.Content, "creator", "one.yaml"), Record{
		"kind": "Item", "id": "one", "author": "creator", "title": "One",
		"provenance": Record{"proposed_tags": []any{
			Record{"tag": "humor", "why": "not in the registry"},
			Record{"tag": "banter", "why": "not in the registry"},
			Record{"tag": "calm", "why": "not in the registry"},
		}}})
	pending, err := PendingTags(c, false)
	if err != nil {
		t.Fatal(err)
	}
	if pending["humor"] == nil {
		t.Fatal("a mapping onto a tag the registry lacks was treated as settled:", sortedKeys(pending))
	}
	if pending["banter"] != nil {
		t.Fatal("a ruling to drop is an answer; it should not be asked again")
	}
	if pending["calm"] != nil {
		t.Fatal("a mapping onto a registry tag is an answer; it should not be asked again")
	}
}
func TestAlreadyInTheRegistryPutsTheTagBackOnTheItem(t *testing.T) {
	c := testConfig(t)
	// The map sent "Hypnosis" at a spelling the registry lacks, so retag took it
	// off and proposed it. The adjudicator replies that the registry already has
	// it -- which has to mean the item gets it back, not that the proposal is
	// quietly binned.
	putRecord(t, filepath.Join(c.Content, "creator", "one.yaml"), Record{
		"kind": "Item", "id": "one", "author": "creator", "title": "One",
		"tags": []string{"Relaxation"},
		"provenance": Record{"proposed_tags": []any{
			Record{"tag": "Hypnosis", "why": "not in the registry"},
			Record{"tag": "Nonsense Word", "why": "not in the registry"},
		}}})
	rulings := []any{
		Record{"tag": "Hypnosis", "verdict": "omit", "why": "Already in the registry."},
		Record{"tag": "Nonsense Word", "verdict": "omit", "why": "Not a subject anyone browses by."},
	}
	if _, err := ApplyRulings(c, rulings, true); err != nil {
		t.Fatal(err)
	}
	got := optionalYAML(filepath.Join(c.Content, "creator", "one.yaml"))
	if !contains(texts(got["tags"]), "Hypnosis") {
		t.Fatal("a tag ruled already-registered was not restored:", got["tags"])
	}
	if contains(texts(got["tags"]), "Nonsense Word") {
		t.Fatal("a rejected tag reached the item:", got["tags"])
	}
	if truth(record(got["provenance"])["proposed_tags"]) {
		t.Fatal("settled proposals should be cleared:", record(got["provenance"])["proposed_tags"])
	}
}
func TestAQueuedTargetReachesTheAdjudicator(t *testing.T) {
	c := testConfig(t)
	// What a tagmap run leaves behind when the model wants a name the registry
	// does not have: held outside `mapping`, so nothing resolves onto it, and
	// picked up as a question rather than sitting in the file for ever.
	putRecord(t, MappingPath(c, "creator"), Record{"author": "creator",
		"mapping": []any{Record{"tag": "calm", "verdict": "map", "to": "Relaxation"}},
		"pending": []any{Record{"tag": "Humour", "from": "humor", "count": 9,
			"why": "comedy is the intent, not the tone"}}})
	if m := LoadMapping(c, "creator"); m["Humour"] != nil || m["humor"] != nil {
		t.Fatal("a queued target must not act as a mapping")
	}
	pending, err := PendingTags(c, false)
	if err != nil {
		t.Fatal(err)
	}
	if pending["Humour"] == nil {
		t.Fatal("a queued target never reached the adjudicator:", sortedKeys(pending))
	}
	if n := integer(pending["Humour"]["count"]); n != 9 {
		t.Fatal("the creator's usage count did not carry:", n)
	}
	if pending["Relaxation"] != nil {
		t.Fatal("a target the registry already has is not a question")
	}
}
func TestTagmapRowsMustPointIntoTheRegistry(t *testing.T) {
	c := testConfig(t)
	putRecord(t, MappingPath(c, "creator"), Record{"author": "creator", "mapping": []any{
		Record{"tag": "calm", "verdict": "map", "to": "Relaxation"},
		Record{"tag": "giggly", "verdict": "new", "to": "Humour"},
		Record{"tag": "blues", "verdict": "drop"},
	}})
	got, err := DanglingMapTargets(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || str(got[0]["tag"]) != "giggly" || str(got[0]["to"]) != "Humour" {
		t.Fatal("check should name exactly the row pointing outside the registry:", got)
	}
}
func TestARulingRetiresTheQueueItAnswered(t *testing.T) {
	c := testConfig(t)
	// A ruling that reached the registry and the items but left the creator map
	// alone was the gap: the queue went on asking, and the creator's own
	// spelling went on resolving to nothing, so the decision had no effect on
	// what their recordings were tagged.
	putRecord(t, MappingPath(c, "creator"), Record{"author": "creator",
		"mapping": []any{Record{"tag": "calm", "verdict": "map", "to": "Relaxation"}},
		"pending": []any{
			Record{"tag": "Humour", "from": "humor", "count": 9, "why": "comedy is the intent"},
			Record{"tag": "Gilding", "from": "aurification", "count": 1, "why": "turned to gold"},
			Record{"tag": "Headphones", "from": "headphones on", "count": 2, "why": "listening advice"},
			Record{"tag": "Unanswered", "from": "unanswered", "count": 1, "why": "nobody has looked"},
		}})
	rulings := []any{
		Record{"tag": "Humour", "verdict": "approve", "description": "Comedy is the intent.",
			"why": "recurs, and no existing tag says it"},
		Record{"tag": "Gilding", "verdict": "merge", "merge_into": "Relaxation",
			"why": "one recording; the broader tag already covers it"},
		Record{"tag": "Headphones", "verdict": "omit", "why": "listening advice, not content"},
	}
	if _, err := ApplyRulings(c, rulings, true); err != nil {
		t.Fatal(err)
	}
	doc := optionalYAML(MappingPath(c, "creator"))
	rows := map[string]Record{}
	for _, v := range array(doc["mapping"]) {
		rows[str(record(v)["tag"])] = record(v)
	}
	// Keyed on what the creator wrote, not on the name the run proposed for it.
	if got := rows["humor"]; str(got["to"]) != "Humour" || str(got["verdict"]) != "map" {
		t.Fatal("an approved tag did not become a mapping row:", got)
	}
	if got := rows["aurification"]; str(got["to"]) != "Relaxation" || str(got["verdict"]) != "map" {
		t.Fatal("a merge did not point the creator's spelling at the target:", got)
	}
	if got := rows["headphones on"]; str(got["verdict"]) != "drop" || str(got["to"]) != "" {
		t.Fatal("an omission should be recorded as a drop, not forgotten:", got)
	}
	left := array(doc["pending"])
	if len(left) != 1 || str(record(left[0])["tag"]) != "Unanswered" {
		t.Fatal("exactly the unruled entry should still be queued:", left)
	}
	// Every row a ruling wrote has to name something the registry actually has.
	dangling, err := DanglingMapTargets(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(dangling) != 0 {
		t.Fatal("a retired row points outside the registry:", dangling)
	}
}

func TestARulingIsMatchedDespiteOddWhitespace(t *testing.T) {
	// The tag as it sits in the library, with a non-breaking space in it.
	asked := "Almost \u00a0Too Professional To Give In"
	// The same tag as a model hands it back, spaced ordinarily.
	replied := "Almost Too Professional To Give In"
	if looseTag(asked) != looseTag(replied) {
		t.Fatalf("a ruling on the same tag did not match: %q vs %q", looseTag(asked), looseTag(replied))
	}
	// Different tags must still be told apart.
	if looseTag("Deep Trance") == looseTag("Deep Trance Training") {
		t.Fatal("two different tags were folded together")
	}
	// Narrow and figure spaces are whitespace too.
	if looseTag("Cum\u202fCommand") != looseTag("cum command") {
		t.Fatal("a narrow space defeated the match")
	}
}

// A variant group shares one title across its members -- that is what makes it
// a group. Warning on the repeat told somebody to go and fix the thing the
// library was deliberately doing, and buried the repeats that are duplicate
// imports among hundreds that are not.
func TestRepeatedTitleWarnsOnlyWhenTheVariantAlsoRepeats(t *testing.T) {
	c := testConfig(t)
	if err := os.MkdirAll(c.Sources, 0755); err != nil {
		t.Fatal(err)
	}
	one := func(title, variant string, n int) string {
		v := ""
		if variant != "" {
			v = "\nvariant: " + variant
		}
		return fmt.Sprintf("---\napiVersion: inductor/v1\nkind: Source\naudio: /tmp/a%d.mp3\ntitle: %s\nauthor: Creator%s\n", n, title, v)
	}
	body := one("Deepener", "For Men", 1) + one("Deepener", "For Sissies", 2) +
		one("Clone", "", 3) + one("Clone", "", 4) +
		one("Twice Over", "Long Cut", 5) + one("Twice Over", "Long Cut", 6)
	if err := os.WriteFile(filepath.Join(c.Sources, "creator.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	r, err := LoadSources(c.Sources)
	if err != nil {
		t.Fatal(err)
	}
	var plain, sameVariant, distinct int
	for _, w := range r.Warnings {
		switch {
		case strings.Contains(w, `"Deepener"`):
			distinct++
		case strings.Contains(w, `"Clone"`):
			plain++
		case strings.Contains(w, `"Twice Over"`):
			sameVariant++
		}
	}
	if distinct != 0 {
		t.Errorf("warned about a title two distinct variants share: %v", r.Warnings)
	}
	if plain != 1 {
		t.Errorf("want one warning for the unvarianted repeat, got %d: %v", plain, r.Warnings)
	}
	if sameVariant != 1 {
		t.Errorf("want one warning for the repeated title+variant, got %d: %v", sameVariant, r.Warnings)
	}
}
