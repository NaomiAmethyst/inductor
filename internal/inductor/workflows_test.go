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
