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

var Graph = []Artifact{{"media", nil, "disk", "audio", 0, true}, {"transcript", []string{"media"}, "gpu", "audio", 0, true}, {"measurements", []string{"media"}, "cpu", "audio", 0, true}, {"voiceprint", []string{"media"}, "gpu", "audio", 0, false}, {"sound", []string{"media"}, "gpu", "audio", 0, false}, {"analysis", []string{"transcript"}, "api", "text", 0, true}, {"review", []string{"analysis", "measurements", "sound"}, "batch", "text", 150, true}, {"cover", []string{"review"}, "art", "text", 0, false}, {"entry", []string{"review", "measurements", "media"}, "disk", "audio", 0, true}}

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

// artifactProbe is what every artefact check needs and none of them should
// fetch for itself: the fingerprint, the transcript, and the analysis record.
//
// ArtifactExists is asked about eight artefacts per recording, and four of
// those answers each re-read the transcript, re-hashed its whole text and
// re-read the analysis beside it. Over a library of nine thousand that is some
// gigabytes of JSON parsed to answer the same question eight times. Resolve it
// once per recording and hand it round.
type artifactProbe struct {
	e          *Engine
	j          Planned
	fp         string
	fpErr      error
	transcript Record
	store      Record
	read       bool
}

func (e *Engine) probe(j Planned) *artifactProbe { return &artifactProbe{e: e, j: j} }

func (p *artifactProbe) fingerprint() (string, error) {
	if p.fp == "" && p.fpErr == nil {
		p.fp, p.fpErr = p.e.fingerprint(p.j)
	}
	return p.fp, p.fpErr
}

// enrichment is the transcript and whatever the models made of it, fetched at
// most once however many artefacts ask.
func (p *artifactProbe) enrichment() (Record, Record) {
	if p.read {
		return p.transcript, p.store
	}
	p.read = true
	fp, err := p.fingerprint()
	if err != nil {
		return nil, nil
	}
	p.transcript = p.e.transcript(fp)
	if p.transcript != nil {
		p.store = p.e.Store.Peek(TranscriptKey(str(p.transcript["text"])), fp)
	}
	return p.transcript, p.store
}

func (e *Engine) ArtifactExists(name string, j Planned, redo []string, overwrite bool) bool {
	return e.probe(j).exists(name, redo, overwrite)
}

func (p *artifactProbe) exists(name string, redo []string, overwrite bool) bool {
	e, j := p.e, p.j
	if contains(redo, name) {
		return false
	}
	if name == "media" {
		placed := PlacedAudio(e.Config, j.Source, j.Stem)
		if placed == "" {
			return false
		}
		// Playable, so far as is already known. Only a verdict that has been
		// reached counts here: this runs eight times a recording in two hot
		// loops, and deciding it by decoding would play the whole library.
		// A file nobody has judged yet is taken as placed; the stages that
		// actually decode it will find the trouble and repair or refuse it.
		complaint, _, known := e.Soundness.Peek(placed)
		return !known || complaint == ""
	}
	fp, err := p.fingerprint()
	if err != nil {
		return false
	}
	if name == "transcript" {
		t, _ := p.enrichment()
		return truth(t)
	}
	if name == "measurements" {
		return e.Acoustic.Has(fp)
	}
	if name == "voiceprint" {
		return exists(filepath.Join(e.Config.Cache, "voiceprints", fp+".json"))
	}
	if name == "sound" {
		// Answering the producer's question, not its own. Listen declines
		// outright when no models are configured, so probing the store would
		// queue work that refuses to happen on every run for ever -- which is
		// the same fault the cover predicate had, and shows the same way: not
		// an error, a count that never falls.
		cfg := e.Config.Transcribe.Sound
		if cfg.Tagger == "" && cfg.Zeroshot == "" {
			return true
		}
		return e.Sounds.Has(fp)
	}
	t, r := p.enrichment()
	if t == nil {
		return false
	}
	switch name {
	case "analysis":
		// Either a first pass exists, or one was declined for a reason that
		// cannot change while the transcript is what it is. Both are settled;
		// only the second used to be asked again every run.
		return truth(r["analysis"]) || truth(r["analysis_declined"])
	case "review":
		return truth(r["final"])
	case "cover":
		// This predicate has to answer the *producer's* question, not its own:
		// RenderCover declines whenever the item already declares a cover, so
		// any other answer here schedules work that will refuse to happen --
		// silently, on a lane one job wide, on every run forever. The item's
		// `cover:` field is that declaration, and it is authoritative because
		// the path it names need not be `<id>.png` at all: a cover that came
		// with the source is placed under its own extension. Probing the disk
		// for a .png missed those, and missed the ids that part company with
		// the filename over an apostrophe -- "stop-don-t-listen" against
		// "stop-dont-listen". Between them, 281 jpgs and 501 apostrophes.
		item := optionalYAML(j.Path)
		if truth(item["cover"]) {
			return true
		}
		author := j.Source.AuthorID()
		if exists(filepath.Join(e.Config.Covers, author, ItemID(author, j.Stem)+".png")) {
			return true
		}
		if id := str(item["id"]); id != "" {
			return exists(filepath.Join(e.Config.Covers, author, id+".png"))
		}
		return false
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
		review, skipped, err := e.ReviewJobs(jobs, contains(o.Redo, "review"))
		if err == nil {
			_, err = e.RunReviews(ctx, review, 4, max(1, o.Batch), 60*time.Second, nil, o.WaitBatches)
		}
		for i, j := range jobs {
			if e.ArtifactExists("review", j, nil, false) {
				continue
			}
			id := ItemID(j.Source.AuthorID(), j.Stem)
			if item := optionalYAML(j.Path); truth(item["id"]) {
				id = str(item["id"])
			}
			switch {
			case err != nil:
				errs[i] = err
			case skipped[id] != "":
				// Declined, not failed. There is nothing on disk to write an
				// entry from, and saying "review produced no result" about it
				// buries the batches that genuinely did not come back.
				errs[i] = NothingToProduce{skipped[id]}
			default:
				errs[i] = fmt.Errorf("review produced no result")
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
				// A file that decodes with malformed frames is re-encoded here
				// rather than linked, which is what makes the strict decoder
				// downstream accept it.
				complaint, recoverable, _ := e.audioTrouble(ctx, j.Source.AudioPath(e.Config.Sources))
				if recoverable {
					e.Say("re-encoding %s, which decodes whole but upsets a strict decoder: %s", j.Stem, complaint)
				}
				_, errs[i] = PlaceMedia(ctx, e.Config, j.Source, j.Stem, recoverable)
			}
		case "transcript":
			// Vetted before the GPU is asked: a file that will not decode here
			// will not decode there either, and staging it only moves the
			// failure somewhere harder to read.
			if errs[i] = e.soundEnough(ctx, j); errs[i] == nil {
				errs[i] = e.Transcribe(ctx, j, false, contains(o.Redo, name))
			}
		case "voiceprint":
			if errs[i] = e.soundEnough(ctx, j); errs[i] == nil {
				errs[i] = e.Transcribe(ctx, j, true, contains(o.Redo, name))
			}
		case "sound":
			if errs[i] = e.soundEnough(ctx, j); errs[i] == nil {
				errs[i] = e.Listen(ctx, j, contains(o.Redo, name))
			}
		case "measurements":
			// The acoustics decode the file too, and a damaged one yields
			// measurements of whatever ffmpeg managed before it gave up --
			// which is worse than none, because an envelope taken from broken
			// audio still correlates against everything and matches nothing.
			err := e.soundEnough(ctx, j)
			if err == nil {
				var fp string
				if fp, err = e.fingerprint(j); err == nil {
					audio := PlacedAudio(e.Config, j.Source, j.Stem)
					_, err = e.Acoustic.Analyze(ctx, fp, audio, contains(o.Redo, name))
				}
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
				// The entry's *declared* id, because that is what a resume is
				// matched against. The computed one agrees with it only while
				// the filename has no author prefix of its own and no collision
				// forced a suffix: a file named `<author>-<title>.mp3` under
				// `<author>/` computes to `<author>-<author>-<title>`, names
				// nothing, and takes the whole resume down with it.
				id := ItemID(j.Source.AuthorID(), j.Stem)
				if item := optionalYAML(j.Path); truth(item["id"]) {
					id = str(item["id"])
				}
				left = append(left, id)
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
		probe := e.probe(j)
		for _, a := range Graph {
			if !(a.Name == "entry" && o.RefreshEntries) && probe.exists(a.Name, o.Redo, o.Overwrite) {
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
					states[i][ev.name] = 2
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
	finishGraphProgress(ctx)
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
