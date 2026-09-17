// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"encoding/hex"
	"fmt"
	"golang.org/x/crypto/blake2b"
	"path/filepath"
	"strings"
	"time"
)

type Artifact struct {
	Name        string
	Needs       []string
	Lane, Keyed string
	Batch       int
	Required    bool
}

var Graph = []Artifact{{"media", nil, "disk", "audio", 0, true}, {"transcript", []string{"media"}, "gpu", "audio", 0, true}, {"measurements", []string{"media"}, "cpu", "audio", 0, true}, {"voiceprint", []string{"media"}, "gpu", "audio", 0, false}, {"analysis", []string{"transcript"}, "api", "text", 0, true}, {"review", []string{"analysis", "measurements"}, "batch", "text", 150, true}, {"cover", []string{"review"}, "art", "text", 0, false}, {"entry", []string{"review", "measurements", "media"}, "disk", "audio", 0, true}}

func GraphOrder(graph []Artifact) ([]string, error) {
	known := map[string]Artifact{}
	for _, a := range graph {
		if _, ok := known[a.Name]; ok {
			return nil, fmt.Errorf("duplicate artefact %s", a.Name)
		}
		known[a.Name] = a
	}
	state := map[string]int{}
	out := []string{}
	var visit func(string) error
	visit = func(name string) error {
		a, ok := known[name]
		if !ok {
			return fmt.Errorf("unknown artefact %s", name)
		}
		if state[name] == 1 {
			return fmt.Errorf("dependency cycle at %s", name)
		}
		if state[name] == 2 {
			return nil
		}
		state[name] = 1
		for _, dep := range a.Needs {
			if e := visit(dep); e != nil {
				return e
			}
		}
		state[name] = 2
		out = append(out, name)
		return nil
	}
	for _, a := range graph {
		if e := visit(a.Name); e != nil {
			return nil, e
		}
	}
	return out, nil
}
func GraphPlan(states map[string]map[string]bool, graph []Artifact) (Record, error) {
	if _, e := GraphOrder(graph); e != nil {
		return nil, e
	}
	todo, lanes, submissions := Record{}, Record{}, Record{}
	finished := 0
	for _, a := range graph {
		lanes[a.Lane] = 0
	}
	for _, have := range states {
		complete := true
		for _, a := range graph {
			if !have[a.Name] {
				complete = false
				todo[a.Name] = integer(todo[a.Name]) + 1
				lanes[a.Lane] = integer(lanes[a.Lane]) + 1
			}
		}
		if complete {
			finished++
		}
	}
	for _, a := range graph {
		if a.Batch > 0 && integer(todo[a.Name]) > 0 {
			submissions[a.Name] = (integer(todo[a.Name]) + a.Batch - 1) / a.Batch
		}
	}
	return Record{"recordings": len(states), "finished": finished, "artefacts": todo, "lanes": lanes, "submissions": submissions}, nil
}
func (e *Engine) ArtifactExists(name string, j Planned, redo []string, overwrite bool) bool {
	if contains(redo, name) {
		return false
	}
	if name == "media" {
		return PlacedAudio(e.Config, j.Source, j.Stem) != ""
	}
	fp, err := e.fingerprint(j)
	if err != nil {
		return false
	}
	if name == "transcript" {
		return truth(e.transcript(fp))
	}
	if name == "measurements" {
		return e.Acoustic.Has(fp)
	}
	if name == "voiceprint" {
		return exists(filepath.Join(e.Config.Cache, "voiceprints", fp+".json"))
	}
	p := e.transcript(fp)
	if p == nil {
		return false
	}
	r := e.Store.Peek(TranscriptKey(str(p["text"])), fp)
	switch name {
	case "analysis":
		return truth(r["analysis"])
	case "review":
		return truth(r["final"])
	case "cover":
		return exists(filepath.Join(e.Config.Covers, j.Source.AuthorID(), ItemID(j.Source.AuthorID(), j.Stem)+".png"))
	case "entry":
		return exists(j.Path) && !overwrite
	}
	return false
}

type RunOptions struct {
	Redo                      []string
	Overwrite, Covers, DryRun bool
	Batch                     int
	RefreshEntries            bool
	WaitBatches               bool
}

func (e *Engine) produce(ctx context.Context, name string, jobs []Planned, o RunOptions) []error {
	errs := make([]error, len(jobs))
	if name == "review" {
		review, err := e.ReviewJobs(jobs, contains(o.Redo, "review"))
		if err == nil {
			_, err = e.RunReviews(ctx, review, 4, max(1, o.Batch), 60*time.Second, nil, o.WaitBatches)
		}
		for i, j := range jobs {
			if e.ArtifactExists("review", j, nil, false) {
				continue
			}
			if err == nil {
				errs[i] = fmt.Errorf("review produced no result")
			} else {
				errs[i] = err
			}
		}
		return errs
	}
	for i, j := range jobs {
		switch name {
		case "media":
			// Vetted before anything is placed: media is the root of the graph,
			// so refusing here stops a broken file reaching transcription,
			// measurement or the item writer at all.
			if errs[i] = e.soundEnough(ctx, j); errs[i] == nil {
				_, errs[i] = PlaceMedia(ctx, e.Config, j.Source, j.Stem)
			}
		case "transcript":
			errs[i] = e.Transcribe(ctx, j, false, contains(o.Redo, name))
		case "voiceprint":
			errs[i] = e.Transcribe(ctx, j, true, contains(o.Redo, name))
		case "measurements":
			fp, err := e.fingerprint(j)
			if err == nil {
				audio := PlacedAudio(e.Config, j.Source, j.Stem)
				_, err = e.Acoustic.Analyze(ctx, fp, audio, contains(o.Redo, name))
			}
			errs[i] = err
		case "analysis":
			errs[i] = e.Analyze(ctx, j, contains(o.Redo, name))
		case "entry":
			errs[i] = e.Emit(ctx, j, false, o.Overwrite, false)
		case "cover":
			if o.Covers {
				errs[i] = e.Emit(ctx, j, true, o.Overwrite, contains(o.Redo, name))
			}
		default:
			errs[i] = fmt.Errorf("no producer for %s", name)
		}
	}
	return errs
}

// RunGraph owns scheduling state on one goroutine. Workers only send outcomes;
// optional failures settle dependencies, while required failures block descendants.
// saveResume records what a run had left to do when it was interrupted.
//
// Re-running would find the finished artefacts on disk and skip them anyway,
// but it would also re-plan the whole library to get there. The snapshot keeps
// the selection, so `--resume` picks up this run rather than starting a new one
// that happens to overlap.
func (e *Engine) saveResume(jobs []Planned, states []map[string]int) (string, error) {
	left := []string{}
	for i, j := range jobs {
		for _, a := range Graph {
			if a.Required && states[i][a.Name] != 2 {
				left = append(left, ItemID(j.Source.AuthorID(), j.Stem))
				break
			}
		}
	}
	if len(left) == 0 {
		return "", nil
	}
	h, _ := blake2b.New(4, nil)
	for _, id := range left {
		_, _ = h.Write([]byte(id))
	}
	id := fmt.Sprintf("run-%d-%s", time.Now().Unix(), hex.EncodeToString(h.Sum(nil)))
	path := filepath.Join(e.Config.Cache, "resume", id+".json")
	return id, writeJSON(path, Record{"apiVersion": "inductor/v1", "kind": "ResumePoint",
		"id": id, "created": time.Now().UTC().Format(time.RFC3339), "items": left})
}

// LoadResume returns the recordings a snapshot still wanted done.
func (e *Engine) LoadResume(id string) ([]string, error) {
	path := filepath.Join(e.Config.Cache, "resume", id+".json")
	r := readJSON(path)
	if len(r) == 0 {
		return nil, fmt.Errorf("no resume point called %s (looked in %s)", id, path)
	}
	items := texts(r["items"])
	if len(items) == 0 {
		return nil, fmt.Errorf("resume point %s has nothing left to do", id)
	}
	return items, nil
}

func (e *Engine) RunGraph(ctx context.Context, jobs []Planned, o RunOptions) (Record, error) {
	if _, err := GraphOrder(Graph); err != nil {
		return nil, err
	}
	byName := map[string]Artifact{}
	for _, a := range Graph {
		byName[a.Name] = a
	}
	for _, name := range o.Redo {
		if _, ok := byName[name]; !ok {
			return nil, fmt.Errorf("nothing in the graph is called %s", name)
		}
	}
	states := make([]map[string]int, len(jobs))
	plan := map[string]map[string]bool{}
	status := graphProgress{Total: len(jobs) * len(Graph)}
	for i, j := range jobs {
		states[i] = map[string]int{}
		have := map[string]bool{}
		for _, a := range Graph {
			if !(a.Name == "entry" && o.RefreshEntries) && e.ArtifactExists(a.Name, j, o.Redo, o.Overwrite) {
				states[i][a.Name] = 2
				have[a.Name] = true
				status.Cached++
			}
		}
		plan[fmt.Sprint(i)] = have
	}
	if o.DryRun {
		return GraphPlan(plan, Graph)
	}
	status.Pending = status.Total - status.Cached
	counts := Record{}
	// One row per artefact for the progress report: how far it has got across
	// every recording, and what is holding the remainder up. Read from the same
	// state the scheduler decides on, so the report cannot drift from the truth.
	tally := func() []artifactTally {
		rows := make([]artifactTally, 0, len(Graph))
		for _, a := range Graph {
			t := artifactTally{Name: a.Name, Total: len(jobs)}
			for i := range jobs {
				switch states[i][a.Name] {
				case 1:
					t.Running++
				case 2:
					t.Done++
				}
			}
			t.Blocked = integer(counts[a.Name+":blocked"])
			t.Failed = integer(counts[a.Name+":failed"])
			t.Skipped = integer(counts[a.Name+":nothing"])
			rows = append(rows, t)
		}
		return rows
	}
	status.PerArtifact = tally()
	updateGraphProgress(ctx, status)
	limits := map[string]int{"disk": 4, "cpu": 6, "gpu": max(1, e.Config.Transcribe.Workers), "api": max(1, e.Config.Enrich.Workers), "art": 2, "batch": 4}
	running := map[string]int{}
	type event struct {
		name, lane string
		indices    []int
		errs       []error
		complete   []func(error)
	}
	events := make(chan event, 64)
	inFlight := 0
	launch := func(a Artifact, indices []int) {
		tray := make([]Planned, len(indices))
		complete := make([]func(error), len(indices))
		for n, i := range indices {
			states[i][a.Name] = 1
			tray[n] = jobs[i]
			complete[n] = startRunWork(ctx, fmt.Sprintf("%s %s/%s", a.Name, jobs[i].Source.AuthorID(), jobs[i].Stem))
		}
		status.Running += len(indices)
		status.Pending -= len(indices)
		status.PerArtifact = tally()
		updateGraphProgress(ctx, status)
		running[a.Lane]++
		inFlight++
		go func() {
			errs := e.produce(ctx, a.Name, tray, o)
			events <- event{a.Name, a.Lane, indices, errs, complete}
		}()
	}
	settle := func(ev event) {
		running[ev.lane]--
		inFlight--
		for n, i := range ev.indices {
			err := ev.errs[n]
			status.Running--
			if err == nil {
				states[i][ev.name] = 2
				counts[ev.name] = integer(counts[ev.name]) + 1
				status.Completed++
			} else {
				states[i][ev.name] = 3
				suffix := ":failed"
				if _, ok := err.(NothingToProduce); ok {
					suffix = ":nothing"
					status.Skipped++
				} else {
					status.Failed++
					e.Say("FAIL %s %s: %v", ev.name, jobs[i].Stem, err)
				}
				counts[ev.name+suffix] = integer(counts[ev.name+suffix]) + 1
			}
			ev.complete[n](err)
		}
		status.PerArtifact = tally()
		updateGraphProgress(ctx, status)
	}
	for {
		progress := false
		ready := map[string][]int{}
		for i := range jobs {
			for _, a := range Graph {
				if states[i][a.Name] != 0 {
					continue
				}
				ok, blocked := true, false
				for _, dep := range a.Needs {
					state := states[i][dep]
					if state == 3 && byName[dep].Required {
						blocked = true
					}
					if state != 2 && !(state == 3 && !byName[dep].Required) {
						ok = false
					}
				}
				if blocked {
					states[i][a.Name] = 3
					counts[a.Name+":blocked"] = integer(counts[a.Name+":blocked"]) + 1
					status.Blocked++
					status.Pending--
					progress = true
					continue
				}
				if ok {
					ready[a.Name] = append(ready[a.Name], i)
				}
			}
		}
		for _, a := range Graph {
			r := ready[a.Name]
			for len(r) > 0 && running[a.Lane] < limits[a.Lane] {
				n := 1
				if a.Batch > 0 {
					size := max(1, o.Batch)
					if len(r) < size && inFlight > 0 {
						break
					}
					n = min(size, len(r))
				}
				launch(a, r[:n])
				r = r[n:]
				progress = true
			}
		}
		status.PerArtifact = tally()
		updateGraphProgress(ctx, status)
		if inFlight == 0 {
			if progress {
				continue
			}
			break
		}
		select {
		case ev := <-events:
			settle(ev)
		case <-ctx.Done():
			for inFlight > 0 {
				settle(<-events)
			}
			// Interrupted, not abandoned: say how to pick this run back up.
			if id, err := e.saveResume(jobs, states); err == nil && id != "" {
				e.Say("interrupted with work outstanding — resume with: inductor run --resume %s", id)
			} else if err != nil {
				e.Say("could not save a resume point: %v", err)
			}
			return counts, ctx.Err()
		}
	}
	failed := 0
	for k, v := range counts {
		if strings.HasSuffix(k, ":failed") {
			failed += integer(v)
		}
	}
	if failed > 0 {
		return counts, fmt.Errorf("%d artefact(s) failed; completed work was retained", failed)
	}
	return counts, nil
}
