// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"os"
	"path/filepath"
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
