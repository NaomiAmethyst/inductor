// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCLICommandsAndValidation(t *testing.T) {
	for name := range schema() {
		args := append(strings.Split(name, "/"), "--help")
		var out, errs bytes.Buffer
		if code := Main(context.Background(), args, &out, &errs); code != 0 || out.Len() == 0 {
			t.Errorf("%s: %d %s", name, code, errs.String())
		}
	}
	for _, args := range [][]string{{"nonsense"}, {"ingest", "--stage", "invented"}, {"run", "--unknown"}, {"retag", "--write=true"}, {"transcribe", "worker", "wrong"}, {"ingest", "--limit", "bad"}} {
		if _, err := ParseArgs(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	a, err := ParseArgs([]string{"--root", "/tmp/library", "ingest", "--stage", "media", "--stage", "emit"})
	if err != nil || !equivalent(a.Strings("stage"), []string{"media", "emit"}) {
		t.Fatal(a, err)
	}
}
func TestGraphValidationAndBatchPlanning(t *testing.T) {
	for _, g := range [][]Artifact{{{Name: "a", Needs: []string{"missing"}}}, {{Name: "a", Needs: []string{"b"}}, {Name: "b", Needs: []string{"a"}}}, {{Name: "a"}, {Name: "a"}}} {
		if _, err := GraphOrder(g); err == nil {
			t.Fatal("invalid graph accepted", g)
		}
	}
	states := map[string]map[string]bool{}
	for i := 0; i < 151; i++ {
		states[string(rune(i+1))] = map[string]bool{}
	}
	r, err := GraphPlan(states, Graph)
	if err != nil || integer(record(r["submissions"])["review"]) != 2 {
		t.Fatal(r, err)
	}
}
func TestNativeNameplatePreservesDimensionsAndWords(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cover.png")
	img := image.NewRGBA(image.Rect(0, 0, 640, 360))
	for y := 0; y < 360; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, color.RGBA{20, 30, 40, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	putFile(t, p, b.Bytes())
	changed, err := DrawNameplate(p, "A Very Long Creator Name With Several Words", "missing-font", false)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	raw, _ := os.ReadFile(p)
	result, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil || result.Bounds() != img.Bounds() || bytes.Equal(raw, b.Bytes()) {
		t.Fatal("nameplate absent or resized", err)
	}
	words := pythonFields("one two three four five six seven")
	if strings.Join(wrapWords(words, 2), " ") != strings.Join(words, " ") {
		t.Fatal("nameplate dropped words")
	}
}
func TestFFmpegConversionAndNativeMeasurement(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dir := t.TempDir()
	src := filepath.Join(dir, "tone.wav")
	if _, err := command(ctx, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=220:duration=2", "-y", src); err != nil {
		t.Fatal(err)
	}
	samples, err := DecodeAudio(ctx, src, AudioRate)
	if err != nil || len(samples) < AudioRate {
		t.Fatal(len(samples), err)
	}
	m := Measure(samples)
	if number(m["rms"]) <= 0 {
		t.Fatal(m)
	}
	dest, err := ConvertMedia(ctx, src, filepath.Join(dir, "tone.mp3"), "transcode")
	if err != nil || !HoldsAudio(ctx, dest) {
		t.Fatal(dest, err)
	}
}
func TestGPUQueueCollectionIsAtomicAndFailuresArrive(t *testing.T) {
	c := testConfig(t)
	box := NewGPUBox(c, "")
	if err := box.Provision(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	audio := filepath.Join(c.Root, "a.mp3")
	putFile(t, audio, []byte("audio"))
	if err := box.Enqueue(context.Background(), "transcribe-a", "transcribe", audio); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(box.Directory, "jobs", "transcribe-a.json")) {
		t.Fatal("job not queued")
	}
	if err := box.Enqueue(context.Background(), "../escape", "transcribe", audio); err == nil {
		t.Fatal("unsafe job accepted")
	}
	if err := writeJSON(filepath.Join(box.Directory, "out", "transcribe-a.json"), Record{"id": "transcribe-a", "kind": "transcribe", "ok": false, "error": "bad audio"}); err != nil {
		t.Fatal(err)
	}
	landing := filepath.Join(c.Cache, "landing")
	_, err := box.Work(context.Background(), "transcribe-a", "transcribe", audio, landing)
	if err == nil || !strings.Contains(err.Error(), "bad audio") {
		t.Fatal("failure lost", err)
	}
	if !exists(filepath.Join(landing, "transcribe-a.json")) || exists(filepath.Join(box.Directory, "out", "transcribe-a.json")) {
		t.Fatal("result not safely collected")
	}
	bad := filepath.Join(box.Directory, "out", "broken.json")
	putFile(t, bad, []byte("{"))
	if _, err = box.Collect(context.Background(), landing); err == nil || !exists(bad) {
		t.Fatal("invalid result discarded", err)
	}
}

func TestGraphRunsCachedWorkAndBlocksFailedDependencies(t *testing.T) {
	c := testConfig(t)
	engine := NewEngine(c)
	audio := filepath.Join(c.Root, "audio.mp3")
	putFile(t, audio, []byte("audio"))
	s := &Source{Path: audio, Audio: audio, Author: "Creator", Title: "Title", Data: Record{}}
	job := Planned{s, "title", filepath.Join(c.Content, "creator", "title.yaml")}
	fp, err := engine.fingerprint(job)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(c.Transcripts(), fp+".json"), Record{"text": "Evidence.", "duration": 10}); err != nil {
		t.Fatal(err)
	}
	if err = engine.Store.Put(TranscriptKey("Evidence."), Record{"analysis": Record{"summary": "analysis"}, "final": Record{"summary": "summary", "tags": []string{"Hypnosis"}}}, fp, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(engine.Acoustic.Root, fp+".json"), Record{"rms": .1}); err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(engine.Acoustic.Root, fp+".u32"), []byte{0, 0, 0, 0})
	putFile(t, filepath.Join(c.Cache, "voiceprints", fp+".json"), []byte(`{"voice":[1,0]}`))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	report, err := engine.RunGraph(ctx, []Planned{job}, RunOptions{Batch: 150})
	if err != nil || !exists(job.Path) || integer(report["entry"]) != 1 {
		t.Fatal(report, err)
	}
	missing := &Source{Path: audio, Audio: filepath.Join(c.Root, "missing.mp3"), Title: "Missing", Author: "Creator", Data: Record{}}
	report, err = engine.RunGraph(ctx, []Planned{{missing, "missing", filepath.Join(c.Content, "creator", "missing.yaml")}}, RunOptions{Batch: 150})
	if err == nil || integer(report["media:failed"]) != 1 || integer(report["entry:blocked"]) != 1 {
		t.Fatal("required failure did not block descendants", report, err)
	}
}
