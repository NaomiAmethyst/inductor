// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func runFixture(t *testing.T) (*Engine, string) {
	t.Helper()
	c := testConfig(t)
	if err := os.MkdirAll(c.Sources, 0755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(c.Content, "creator", "one.yaml")
	putRecord(t, p, Record{"kind": "Item", "id": "one", "title": "One", "author": "creator", "tags": []string{"Hypnosis"}, "provenance": Record{"proposed_tags": []any{Record{"tag": "Calm"}}}})
	return NewEngine(c), p
}

func dispatchRun(t *testing.T, e *Engine, flags ...string) (Record, error) {
	t.Helper()
	a, err := ParseArgs(append([]string{"run", "--no-pages", "--no-covers"}, flags...))
	if err != nil {
		t.Fatal(err)
	}
	return e.Dispatch(context.Background(), a)
}

func TestRunMaintainsIdleLibraryAndAppliesTagmapRulings(t *testing.T) {
	e, p := runFixture(t)
	c := e.Config
	putRecord(t, p, Record{"kind": "Item", "id": "one", "title": "One", "author": "creator", "tags": []string{"calming"}})
	emptyAuthor := filepath.Join(c.Content, "unused", "author.yaml")
	strayTranscript := filepath.Join(c.Content, "stray.transcript.yaml")
	putRecord(t, emptyAuthor, Record{"kind": "Author", "id": "unused", "name": "Unused"})
	putRecord(t, strayTranscript, Record{"kind": "Transcript", "item": "missing", "text": "stray"})
	var calls atomic.Int32
	e.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			replyJSON(t, w, chatReply(`{"mapping":[{"tag":"calming","verdict":"map","to":"Calm","description":"A calm mood."}]}`))
		case 2:
			if !exists(MappingPath(c, "creator")) {
				t.Error("adjudication preceded tagmaps")
			}
			replyJSON(t, w, chatReply(`{"rulings":[{"tag":"Calm","verdict":"approve","description":"A calm mood."}]}`))
		default:
			t.Error("repeated settled work")
			http.Error(w, "unexpected call", http.StatusBadRequest)
		}
	})
	report, err := dispatchRun(t, e)
	if err != nil || integer(report["recordings"]) != 0 || calls.Load() != 2 {
		t.Fatal(report, err, calls.Load())
	}
	for _, step := range []string{"check", "tagmaps", "adjudicate", "voiceprint_verify", "duplicates", "orphans"} {
		if report[step] == nil {
			t.Errorf("missing %s report", step)
		}
	}
	item := optionalYAML(p)
	if !contains(texts(item["tags"]), "Calm") || truth(record(item["provenance"])["proposed_tags"]) {
		t.Fatal("approved mapping did not reach the entry", item)
	}
	if row := record(LoadMapping(c, "creator")["calming"]); str(row["to"]) != "Calm" {
		t.Fatal("mapping was not settled", row)
	}
	if !exists(emptyAuthor) || !exists(strayTranscript) || len(texts(record(report["orphans"])["empty_authors"])) != 1 {
		t.Fatal("orphans should be reported and retained", report)
	}
	if _, err := dispatchRun(t, e); err != nil || calls.Load() != 2 {
		t.Fatal("a completed run should be resumable without more API calls", err, calls.Load())
	}
}

func TestRunAdjudicationOptOutsPreserveRulingsForLaterApplication(t *testing.T) {
	for _, flag := range []string{"--no-adjudicate-apply", "--no-adjudicate-write"} {
		t.Run(flag, func(t *testing.T) {
			e, p := runFixture(t)
			var calls atomic.Int32
			e.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) {
				tag := "Calm"
				if calls.Add(1) == 2 {
					tag = "Dream"
				}
				replyJSON(t, w, chatReply(`{"rulings":[{"tag":"`+tag+`","verdict":"approve","description":"A mood."}]}`))
			})
			for _, tag := range []string{"Calm", "Dream"} {
				if tag == "Dream" {
					item := optionalYAML(p)
					addProposals(item, []string{tag}, "new proposal")
					putRecord(t, p, item)
				}
				report, err := dispatchRun(t, e, flag)
				if err != nil {
					t.Fatal(report, err)
				}
				adjudication := record(report["adjudicate"])
				if (adjudication["application"] != nil) != (flag == "--no-adjudicate-write") {
					t.Fatal("wrong application mode", adjudication)
				}
				reg, _ := LoadRegistry(e.Config.RegistryPath())
				if reg.Has(tag) || contains(texts(optionalYAML(p)["tags"]), tag) {
					t.Fatal("opt-out wrote an adjudication", tag)
				}
			}
			if _, err := dispatchRun(t, e); err != nil || calls.Load() != 2 {
				t.Fatal("saved rulings were not resumed", err, calls.Load())
			}
			item := optionalYAML(p)
			if !contains(texts(item["tags"]), "Calm") || !contains(texts(item["tags"]), "Dream") {
				t.Fatal("saved rulings were overwritten", item)
			}
		})
	}
}

func TestRunMapsBeforeGraphAndAdjudicatesEmittedProposals(t *testing.T) {
	c := testConfig(t)
	audio := filepath.Join(c.Root, "audio.mp3")
	putAudio(t, audio, 1)
	putRecord(t, filepath.Join(c.Sources, "one.yaml"), Record{"audio": audio, "title": "One", "author": "Creator", "tags": []string{"hypnotic"}})
	putRecord(t, filepath.Join(c.Sources, "other.yaml"), Record{"audio": "missing.mp3", "title": "Other", "author": "Other", "tags": []string{"unmapped"}})
	putRecord(t, MappingPath(c, "creator"), Record{"author": "creator", "mapping": []any{Record{"tag": "relaxing", "to": "Relaxation", "verdict": "map"}}})
	e := NewEngine(c)
	var output bytes.Buffer
	var outputMu sync.Mutex
	e.Say = func(f string, args ...any) {
		outputMu.Lock()
		defer outputMu.Unlock()
		fmt.Fprintf(&output, f+"\n", args...)
	}
	fp, err := Fingerprint(audio)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(c.Transcripts(), fp+".json"), Record{"text": "Evidence.", "duration": 10}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.Put(TranscriptKey("Evidence."), Record{"analysis": Record{"summary": "analysis"}, "final": Record{"summary": "summary", "tags": []string{"Calm"}}}, fp, "fixture"); err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(e.Acoustic.Root, fp+".json"), []byte(`{"rms":0.1}`))
	putFile(t, filepath.Join(e.Acoustic.Root, fp+".u32"), []byte{0, 0, 0, 0})
	putFile(t, filepath.Join(c.Cache, "voiceprints", fp+".json"), []byte(`{"voice":[1,0]}`))
	p := filepath.Join(c.Content, "creator", "one.yaml")
	var calls atomic.Int32
	e.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			if exists(p) {
				t.Error("graph ran before tagmaps")
			}
			replyJSON(t, w, chatReply(`{"mapping":[{"tag":"hypnotic","verdict":"map","to":"Hypnosis"}]}`))
		case 2:
			if !exists(p) {
				t.Error("adjudication ran before the entry was emitted")
			}
			replyJSON(t, w, chatReply(`{"rulings":[{"tag":"Calm","verdict":"approve","description":"A calm mood."}]}`))
		default:
			t.Error("unexpected API call")
			http.Error(w, "unexpected call", http.StatusBadRequest)
		}
	})
	report, err := dispatchRun(t, e, "--author", "creator", "--verbose", "--no-progress")
	if err != nil || integer(report["entry"]) != 1 || calls.Load() != 2 {
		t.Fatal(report, err, calls.Load())
	}
	item := optionalYAML(p)
	if !contains(texts(item["tags"]), "Hypnosis") || !contains(texts(item["tags"]), "Calm") {
		t.Fatal("tagmap or adjudication was not applied", item)
	}
	if exists(MappingPath(c, "other")) || record(LoadMapping(c, "creator")["relaxing"])["to"] != "Relaxation" {
		t.Fatal("tagmaps ignored author selection or lost an existing mapping")
	}
	for _, label := range []string{"entry creator/one", "tagmap creator tags 1-1/1", "adjudication request (1 tags)", "apply adjudication (1 rulings, write=true)"} {
		for _, prefix := range []string{"dispatch: ", "completed: "} {
			if !strings.Contains(output.String(), prefix+label) {
				t.Errorf("missing verbose event %s%s: %s", prefix, label, output.String())
			}
		}
	}
}

func TestRunPreservesQueuedTagmapsAndReportsFailures(t *testing.T) {
	e, p := runFixture(t)
	putRecord(t, p, Record{"kind": "Item", "id": "one", "author": "creator", "tags": []string{"calming", "relaxing"}})
	putRecord(t, MappingPath(e.Config, "creator"), Record{"author": "creator", "pending": []any{Record{"tag": "Calm", "from": "calming", "count": 1}}})
	var calls atomic.Int32
	e.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		replyJSON(t, w, chatReply(`{"mapping":[{"tag":"relaxing","verdict":"map","to":"Relaxation"}]}`))
	})
	if _, err := dispatchRun(t, e, "--no-adjudicate"); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchRun(t, e, "--no-adjudicate"); err != nil || calls.Load() != 1 {
		t.Fatal("queued mappings were repeatedly requested", err, calls.Load())
	}
	queued, err := QueuedTargets(e.Config)
	if err != nil || len(queued) != 1 || str(queued[0]["from"]) != "calming" {
		t.Fatal("new mapping discarded an existing queued target", queued, err)
	}
	putRecord(t, p, Record{"kind": "Item", "id": "one", "author": "creator", "tags": []string{"new"}})
	e.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid API key", http.StatusUnauthorized)
	})
	report, err := dispatchRun(t, e, "--no-adjudicate")
	if err == nil || !strings.Contains(err.Error(), "tagmap") || report["orphans"] == nil {
		t.Fatal("tagmap failure was swallowed or prevented reports", report, err)
	}
}

func TestRunDryRunDoesNotCallAPIOrWrite(t *testing.T) {
	e, _ := runFixture(t)
	putRecord(t, filepath.Join(e.Config.Sources, "source.yaml"), Record{"audio": "missing.mp3", "title": "New", "author": "Other", "tags": []string{"unmapped"}})
	e.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("dry run called the API")
		http.Error(w, "unexpected call", http.StatusBadRequest)
	})
	snapshot := func() map[string]string {
		out := map[string]string{}
		err := filepath.WalkDir(e.Config.Root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(path)
			out[path] = string(b)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()
	report, err := dispatchRun(t, e, "--dry-run")
	if err != nil || integer(record(report["tagmaps"])["asked"]) != 1 || truth(record(report["adjudicate"])["write"]) {
		t.Fatal(report, err)
	}
	after := snapshot()
	if len(before) != len(after) {
		t.Fatal("dry run created files")
	}
	for path, b := range before {
		if after[path] != b {
			t.Errorf("dry run changed %s", path)
		}
	}
}

func TestRunMaintenanceFlagsAndFilteredEmptySelection(t *testing.T) {
	e, _ := runFixture(t)
	e.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("omitted step called the API")
		http.Error(w, "unexpected call", http.StatusBadRequest)
	})
	flags := []string{"--no-check", "--no-tagmaps", "--no-adjudicate", "--no-voiceprint-verify", "--no-duplicates", "--no-orphans"}
	report, err := dispatchRun(t, e, flags...)
	if err != nil {
		t.Fatal(report, err)
	}
	for _, key := range []string{"check", "tagmaps", "adjudicate", "voiceprint_verify", "duplicates", "orphans"} {
		if report[key] != nil {
			t.Errorf("omitted %s still ran", key)
		}
	}
	// --only with no matching jobs must not fill every author's mapping.
	putRecord(t, filepath.Join(e.Config.Content, "other.yaml"), Record{"kind": "Item", "id": "other", "author": "other", "tags": []string{"unmapped"}})
	only := filepath.Join(e.Config.Root, "only.txt")
	putFile(t, only, nil)
	report, err = dispatchRun(t, e, "--only", only, "--no-adjudicate")
	if err != nil || report["tagmaps"] != nil {
		t.Fatal("empty filter expanded to all authors", report, err)
	}
}

func TestRunCheckForceAndPostflightAfterFailure(t *testing.T) {
	e, _ := runFixture(t)
	putRecord(t, filepath.Join(e.Config.Sources, "bad.yaml"), Record{"author": "Creator"})
	report, err := dispatchRun(t, e, "--no-adjudicate")
	if err == nil || report["check"] == nil || report["orphans"] != nil {
		t.Fatal("invalid sources were not stopped at preflight", report, err)
	}
	if _, err = dispatchRun(t, e, "--no-check", "--no-adjudicate"); err == nil {
		t.Fatal("--no-check bypassed basic source validation")
	}
	report, err = dispatchRun(t, e, "--force", "--no-adjudicate")
	if err != nil || report["orphans"] == nil {
		t.Fatal("--force did not bypass source errors", report, err)
	}
	putRecord(t, filepath.Join(e.Config.Sources, "bad.yaml"), Record{"audio": "missing.mp3", "title": "Missing", "author": "Creator"})
	report, err = dispatchRun(t, e, "--no-adjudicate")
	if err == nil || !strings.Contains(err.Error(), "artefact") || report["orphans"] == nil || report["voiceprint_verify"] == nil {
		t.Fatal("graph failure prevented diagnostic reports", report, err)
	}
}
