// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRunAdjudicatesReviewsAndBackfillsCanonicalDecisions(t *testing.T) {
	e, _ := runFixture(t)
	for _, id := range []string{"kept", "rejected", "unreviewed"} {
		putRecord(t, filepath.Join(e.Config.Content, "creator", id+".yaml"), Record{"kind": "Item", "id": id, "author": "creator", "needs": []string{"tags"}, "provenance": Record{"fingerprint": id}})
		payload := Record{"analysis": Record{"tags": Record{"proposed": []string{"NotReviewed"}}}}
		if id == "kept" {
			payload["final"] = Record{"new_tags": []any{
				Record{"tag": "Dreamy", "verdict": "keep", "why": "A dream unfolds, supported by the cited passage."},
				Record{"tag": "Unwinding", "verdict": "keep"},
				Record{"tag": "Gentleness", "verdict": "keep"},
			}}
		} else if id == "rejected" {
			payload["final"] = Record{"new_tags": []any{Record{"tag": "Gentleness", "verdict": "reject"}, Record{"tag": "NeverAccepted", "verdict": "reject"}}}
		}
		if err := e.Store.Put(id, payload, id, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	e.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		for _, want := range []string{"Dreamy", "Unwinding", "Gentleness", "Calm", "A dream unfolds"} {
			if !bytes.Contains(b, []byte(want)) {
				t.Errorf("adjudication omitted %s", want)
			}
		}
		for _, forbidden := range []string{"NeverAccepted", "NotReviewed"} {
			if bytes.Contains(b, []byte(forbidden)) {
				t.Errorf("adjudication included %s", forbidden)
			}
		}
		replyJSON(t, w, chatReply(`{"rulings":[{"tag":"Calm","verdict":"approve","description":"A calm mood."},{"tag":"Dreamy","verdict":"rework","name":"Dreams","description":"Dream imagery."},{"tag":"Unwinding","verdict":"merge","merge_into":"Relaxation"},{"tag":"Gentleness","verdict":"approve","description":"Gentle delivery."}]}`))
	})
	report, err := dispatchRun(t, e)
	if err != nil || integer(record(report["backfill"])["added"]) != 3 {
		t.Fatal(report, err)
	}
	kept := optionalYAML(filepath.Join(e.Config.Content, "creator", "kept.yaml"))
	for _, tag := range []string{"Dreams", "Relaxation", "Gentleness"} {
		if !contains(texts(kept["tags"]), tag) {
			t.Fatal("missing approved tag", tag, kept)
		}
	}
	if contains(texts(kept["needs"]), "tags") {
		t.Fatal("backfill left tags outstanding", kept)
	}
	for _, id := range []string{"rejected", "unreviewed"} {
		if truth(optionalYAML(filepath.Join(e.Config.Content, "creator", id+".yaml"))["tags"]) {
			t.Fatal("unapproved tags were backfilled", id)
		}
	}
	if _, err := dispatchRun(t, e); err != nil || calls.Load() != 1 {
		t.Fatal("settled review proposals were repeated", err, calls.Load())
	}
}

func TestRunNewStepsCanBeOmittedOrPreviewed(t *testing.T) {
	for _, flag := range []string{"--no-review-proposals", "--no-backfill", "--no-adjudicate-apply", "--no-adjudicate-write", "--dry-run"} {
		t.Run(flag, func(t *testing.T) {
			e, p := runFixture(t)
			putRecord(t, p, Record{"kind": "Item", "id": "one", "author": "creator", "provenance": Record{"fingerprint": "one"}})
			if err := e.Store.Put("one", Record{"final": Record{"new_tags": []any{Record{"tag": "Dreamy", "verdict": "keep"}}}}, "one", "fixture"); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			e.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				replyJSON(t, w, chatReply(`{"rulings":[{"tag":"Dreamy","verdict":"merge","merge_into":"Relaxation"}]}`))
			})
			report, err := dispatchRun(t, e, flag)
			if err != nil {
				t.Fatal(report, err)
			}
			if truth(optionalYAML(p)["tags"]) {
				t.Fatal("opt-out changed tags", flag)
			}
			if (flag == "--dry-run" || flag == "--no-review-proposals") && calls.Load() != 0 {
				t.Fatal("unexpected adjudication", flag)
			}
			if flag == "--no-adjudicate-apply" || flag == "--no-adjudicate-write" {
				if _, err := dispatchRun(t, e, "--no-adjudicate"); err != nil {
					t.Fatal(err)
				}
				if truth(optionalYAML(p)["tags"]) {
					t.Fatal("backfill applied saved rulings despite --no-adjudicate")
				}
				if _, err := dispatchRun(t, e); err != nil {
					t.Fatal(err)
				}
				if !contains(texts(optionalYAML(p)["tags"]), "Relaxation") {
					t.Fatal("resumed ruling was not backfilled")
				}
			}
		})
	}
	e, _ := runFixture(t)
	flags := []string{"--no-adjudicate", "--no-backfill", "--no-acoustic-apply", "--no-similar", "--no-cameos", "--no-transcribe-audit", "--no-artwork-repair", "--no-cover-prompts"}
	report, err := dispatchRun(t, e, flags...)
	if err != nil {
		t.Fatal(report, err)
	}
	for _, key := range []string{"backfill", "acoustic_apply", "similar", "cameos", "transcribe_audit", "artwork", "cover_prompts"} {
		if report[key] != nil {
			t.Errorf("omitted step still ran: %s", key)
		}
	}
	if report["voiceprint_verify"] == nil {
		t.Fatal("cameo opt-out also disabled verification")
	}
}

func TestRunWritesMeasurementsAndSimilarVoicesAndReportsCameos(t *testing.T) {
	e, _ := runFixture(t)
	for _, author := range []string{"alice", "bob"} {
		putRecord(t, filepath.Join(e.Config.Content, author, "_author.yaml"), Record{"kind": "Author", "id": author, "name": author})
		for i := 0; i < 2; i++ {
			id := fmt.Sprintf("%s-%d", author, i)
			putRecord(t, filepath.Join(e.Config.Content, author, id+".yaml"), Record{"kind": "Item", "id": id, "author": author, "provenance": Record{"fingerprint": id}})
			voice := []float64{1, 0}
			if author == "bob" {
				voice = []float64{.8, .6}
			}
			embedding := Record{"voice": voice}
			if id == "alice-0" {
				embedding["windows"] = [][]float64{{0, 1}, {0, 1}}
				embedding["window_seconds"] = 20
			}
			if err := writeJSON(filepath.Join(e.Config.Cache, "voiceprints", id+".json"), embedding); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writeJSON(filepath.Join(e.Acoustic.Root, "alice-0.json"), Record{"seconds": 180, "rms": .15}); err != nil {
		t.Fatal(err)
	}
	preview, err := dispatchRun(t, e, "--no-adjudicate", "--dry-run")
	if err != nil || integer(record(preview["acoustic_apply"])["written"]) != 1 || integer(record(preview["similar"])["changed"]) != 2 {
		t.Fatal(preview, err)
	}
	p := filepath.Join(e.Config.Content, "alice", "alice-0.yaml")
	if optionalYAML(p)["acoustic"] != nil {
		t.Fatal("dry run applied measurements")
	}
	report, err := dispatchRun(t, e, "--no-adjudicate", "--no-voiceprint-verify")
	if err != nil || report["voiceprint_verify"] != nil {
		t.Fatal(report, err)
	}
	item := optionalYAML(p)
	if number(item["duration"]) != 180 || number(record(item["acoustic"])["rms"]) != .15 {
		t.Fatal(item)
	}
	author := optionalYAML(filepath.Join(e.Config.Content, "alice", "_author.yaml"))
	if rows := array(author["similar"]); len(rows) != 1 || str(record(rows[0])["id"]) != "bob" {
		t.Fatal(author)
	}
	if guests, _ := record(report["cameos"])["cameos"].([]Record); len(guests) == 0 {
		t.Fatal("cameos disabled along with verification", report)
	}
}

func TestRunAuditsTranscriptCoverageWithoutRetranscribing(t *testing.T) {
	for _, binary := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skip(binary + " not installed")
		}
	}
	e, p := runFixture(t)
	audio := filepath.Join(e.Config.Root, "long.wav")
	if _, err := command(context.Background(), "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=220:duration=130", "-ar", "8000", "-y", audio); err != nil {
		t.Fatal(err)
	}
	fp, err := Fingerprint(audio)
	if err != nil {
		t.Fatal(err)
	}
	putRecord(t, p, Record{"kind": "Item", "id": "one", "title": "Partial transcript", "author": "creator", "audio": audio, "provenance": Record{"fingerprint": fp}})
	path := filepath.Join(e.Config.Transcripts(), fp+".json")
	if err := writeJSON(path, Record{"text": "Partial.", "covered": 30}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	report, err := dispatchRun(t, e, "--no-adjudicate")
	if err != nil {
		t.Fatal(report, err)
	}
	short := array(record(report["transcribe_audit"])["short"])
	if len(short) != 1 || number(record(short[0])["covered"]) != 30 {
		t.Fatal(report)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("audit changed the transcript")
	}
}

func TestRunRepairsCoverPromptsAndArtworkAcrossExistingEntries(t *testing.T) {
	e, p := runFixture(t)
	e.Config.Enrich.Covers = true
	e.Config.Enrich.CoverWidth, e.Config.Enrich.CoverHeight = 64, 64
	e.Config.Enrich.CoverEngine = "flux"
	tagged := "violet smoke, a quiet room, soft lighting"
	natural := "Violet smoke drifts across a quiet room in soft evening light."
	putRecord(t, p, Record{"kind": "Item", "id": "one", "title": "One", "author": "creator", "provenance": Record{"fingerprint": "one", "enriched": Record{"transcript": "one"}}})
	if err := e.Store.Put("one", Record{"final": Record{"thumbnail_prompt": tagged}}, "one", "fixture"); err != nil {
		t.Fatal(err)
	}
	manual := filepath.Join(e.Config.Content, "creator", "manual.yaml")
	putRecord(t, manual, Record{"kind": "Item", "id": "manual", "author": "creator", "cover": "curated.png", "cover_prompts": Record{"tagged": tagged}})
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	var translations, renders atomic.Int32
	e.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/chat/completions":
			translations.Add(1)
			b, _ := json.Marshal(Record{"1": natural})
			replyJSON(t, w, chatReply(string(b)))
		case r.URL.Path == "/prompt":
			renders.Add(1)
			b, _ := io.ReadAll(r.Body)
			if !bytes.Contains(b, []byte(natural)) {
				t.Error("render did not use the translated prompt")
			}
			replyJSON(t, w, Record{"prompt_id": "fixture"})
		case strings.HasPrefix(r.URL.Path, "/history/"):
			replyJSON(t, w, Record{"fixture": Record{"outputs": Record{"1": Record{"images": []any{Record{"filename": "image.png"}}}}}})
		case r.URL.Path == "/view":
			w.Write(pngBytes.Bytes())
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.Error(w, "unexpected", 400)
		}
	})
	e.Config.Enrich.ComfyURL = e.API.BaseURL
	run := func(flags ...string) Record {
		a, err := ParseArgs(append([]string{"run", "--no-pages", "--no-adjudicate"}, flags...))
		if err != nil {
			t.Fatal(err)
		}
		report, err := e.Dispatch(context.Background(), a)
		if err != nil {
			t.Fatal(report, err)
		}
		return report
	}
	run("--dry-run")
	if translations.Load() != 0 || renders.Load() != 0 || optionalYAML(p)["cover_prompts"] != nil {
		t.Fatal("dry run performed artwork work")
	}
	if r := run("--dry-run", "--no-cover-prompts"); r["cover_prompts"] != nil || r["artwork"] == nil {
		t.Fatal("prompt translation opt-out also disabled artwork repair", r)
	}
	run("--no-artwork-repair")
	if translations.Load() != 0 || renders.Load() != 0 {
		t.Fatal("repair opt-out made requests")
	}
	report := run()
	if translations.Load() != 1 || renders.Load() != 1 || integer(record(report["artwork"])["drawn"]) != 1 {
		t.Fatal(report, translations.Load(), renders.Load())
	}
	item := optionalYAML(p)
	if str(record(item["cover_prompts"])["natural"]) != natural || !exists(e.Config.Resolved(str(item["cover"]), filepath.Dir(p))) {
		t.Fatal(item)
	}
	if str(optionalYAML(manual)["cover"]) != "curated.png" {
		t.Fatal("manual cover was changed")
	}
	run()
	if translations.Load() != 1 || renders.Load() != 1 {
		t.Fatal("valid artwork was regenerated")
	}
	// A changed prompt makes otherwise correctly sized generated art stale.
	StampArt(nested(item, "provenance"), "old prompt", "", "flux", str(item["title"]))
	putRecord(t, p, item)
	run()
	if renders.Load() != 2 {
		t.Fatal("stale artwork was not repaired")
	}
	cover := e.Config.Resolved(str(item["cover"]), filepath.Dir(p))
	var wrongSize bytes.Buffer
	if err := png.Encode(&wrongSize, image.NewRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}
	putFile(t, cover, wrongSize.Bytes())
	run()
	if renders.Load() != 3 {
		t.Fatal("incorrectly sized artwork was not repaired")
	}
	if err := os.Remove(cover); err != nil {
		t.Fatal(err)
	}
	run()
	if renders.Load() != 4 || !exists(cover) {
		t.Fatal("missing generated artwork was not repaired")
	}
}

func TestRunSelectsFinishedSourcesAndRefreshesEntries(t *testing.T) {
	c := testConfig(t)
	e := NewEngine(c)
	audio := filepath.Join(c.Root, "audio.mp3")
	putAudio(t, audio, 1)
	fp, err := Fingerprint(audio)
	if err != nil {
		t.Fatal(err)
	}
	putRecord(t, filepath.Join(c.Sources, "one.yaml"), Record{"audio": audio, "title": "One", "author": "Creator"})
	p := filepath.Join(c.Content, "creator", "one.yaml")
	putRecord(t, p, Record{"kind": "Item", "id": "stable", "title": "Curated", "author": "creator", "audio": audio, "summary": "Keep this summary", "provenance": Record{"fingerprint": fp, "source_key": audio, "enriched": Record{"model": "old"}}})
	if err := writeJSON(filepath.Join(c.Transcripts(), fp+".json"), Record{"text": "Evidence."}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.Put(TranscriptKey("Evidence."), Record{"analysis": Record{"summary": "analysis"}, "final": Record{"summary": "replacement", "description": "New description", "tags": []string{"Hypnosis"}}}, fp, "fixture"); err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(e.Acoustic.Root, fp+".json"), []byte(`{"rms":0.1}`))
	putFile(t, filepath.Join(e.Acoustic.Root, fp+".u32"), []byte{0, 0, 0, 0})
	// Preview must see the missing voiceprint on a supposedly finished entry.
	report, err := dispatchRun(t, e, "--dry-run")
	if err != nil || integer(record(report["artefacts"])["voiceprint"]) != 1 {
		t.Fatal(report, err)
	}
	putFile(t, filepath.Join(c.Cache, "voiceprints", fp+".json"), []byte(`{"voice":[1,0]}`))
	report, err = dispatchRun(t, e)
	if err != nil || integer(report["entry"]) != 1 {
		t.Fatal(report, err)
	}
	item := optionalYAML(p)
	if str(item["id"]) != "stable" || str(item["title"]) != "Curated" || str(item["summary"]) != "Keep this summary" || !strings.Contains(str(item["description"]), "New description") {
		t.Fatal(item)
	}
	// Completed entries must not consume --limit. Exercise legacy-cache
	// inspection at the same time: planning must not migrate it on disk.
	keyPath := filepath.Join(c.Analysis(), TranscriptKey("Evidence.")+".json")
	legacy, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(c.Analysis(), fp+".json"), legacy)
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	putRecord(t, filepath.Join(c.Sources, "two.yaml"), Record{"audio": "missing.mp3", "title": "Two", "author": "Creator"})
	report, err = dispatchRun(t, e, "--limit", "1", "--dry-run")
	if err != nil || integer(report["recordings"]) != 1 || integer(record(report["artefacts"])["media"]) != 1 {
		t.Fatal("completed entry consumed the processing limit", report, err)
	}
	if exists(keyPath) {
		t.Fatal("dry run migrated the legacy review cache")
	}
}

// A cover that arrived beside its source keeps the extension it arrived with,
// so the file on disk need not be <id>.png at all. The scheduler used to look
// only for that one name while RenderCover decides by whether the item declares
// a cover — so every run queued art for the mismatches and the renderer
// declined every one of them in silence, on a lane one job wide. A predicate
// and its producer have to answer the same question.
func TestCoverIsSatisfiedByADeclaredCoverOfAnyExtension(t *testing.T) {
	e, p := runFixture(t)
	const fp = "declared-cover"
	if err := writeJSON(filepath.Join(e.Config.Transcripts(), fp+".json"), Record{"text": "spoken"}); err != nil {
		t.Fatal(err)
	}
	art := filepath.Join(e.Config.Covers, "creator", "one.jpg")
	if err := os.MkdirAll(filepath.Dir(art), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(art, []byte("jpeg"), 0644); err != nil {
		t.Fatal(err)
	}
	j := Planned{Source: &Source{Author: "creator", fingerprint: fp}, Stem: "one", Path: p}
	item := Record{"kind": "Item", "id": "one", "title": "One", "author": "creator", "provenance": Record{"fingerprint": fp}}

	putRecord(t, p, item)
	if e.ArtifactExists("cover", j, nil, false) {
		t.Fatal("an item declaring no cover, with no .png of its own, read as already drawn")
	}
	item["cover"] = "../../media/cover/creator/one.jpg"
	putRecord(t, p, item)
	if !e.ArtifactExists("cover", j, nil, false) {
		t.Fatal("a declared .jpg cover read as missing, so every run redraws art the renderer refuses to draw")
	}
	if e.ArtifactExists("cover", j, []string{"cover"}, false) {
		t.Fatal("--redo cover no longer forces a redraw")
	}
}
