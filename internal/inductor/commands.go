// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

func (e *Engine) adoptItem(d Document) Planned {
	c := e.Config
	p := record(d.Data["provenance"])
	audio := c.Resolved(str(d.Data["audio"]), filepath.Dir(d.Path))
	data := clone(d.Data)
	data["author_id"] = str(d.Data["author"])
	s := &Source{Path: d.Path, Audio: str(first(p["source_key"], audio)), Title: str(d.Data["title"]), Author: str(d.Data["author"]), Data: data, resolved: audio, checked: true, fingerprint: str(p["fingerprint"])}
	return Planned{s, stemOf(d.Path), d.Path}
}
func (e *Engine) ingestCommand(ctx context.Context, a Arguments) (out Record, commandErr error) {
	c := e.Config
	counts := Record{}
	if a.Command == "run" {
		var stop func()
		ctx, stop = startRunProgress(ctx, e.Say, a.Bool("no_progress") || a.Bool("dry_run"), a.Bool("verbose"), e.Colour)
		defer stop()
	}
	if a.Command == "run" && !a.Bool("no_check") {
		e.Say("check: checking sources and mapping targets")
		complete := startRunWork(ctx, "check")
		check, err := e.Dispatch(ctx, Arguments{Command: "check"})
		complete(err)
		counts["check"] = check
		if err != nil && !(a.Bool("force") && len(array(check["errors"])) > 0) {
			return counts, err
		}
	}
	planned := startRunWork(ctx, "planning recordings")
	defer func() { planned(commandErr) }()
	r, err := e.loadSources()
	if err != nil {
		if a.String("only") == "" {
			return nil, err
		}
		r = SourceReport{}
	}
	if len(r.Errors) > 0 && !a.Bool("force") {
		return Record{"errors": r.Errors}, fmt.Errorf("fix source errors or pass --force")
	}
	only := []string(nil)
	if p := a.String("only"); p != "" {
		only, err = readLines(p)
		if err != nil {
			return nil, err
		}
		if only == nil {
			only = []string{}
		}
	}
	// run checks individual artifacts even on entries marked finished. This
	// widens selection without forcing any cached artifact to be regenerated.
	if id := a.String("resume"); id != "" {
		only, err = e.LoadResume(id)
		if err != nil {
			return nil, err
		}
		e.Say("resuming %d recording(s) from %s", len(only), id)
	}
	redo := a.Command == "run" || a.Bool("redo") || a.Bool("redo_analysis") || a.Bool("overwrite")
	planLimit := a.Int("limit")
	if a.Command == "run" {
		planLimit = 0
	}
	indexed := 0
	readIndexRaw, doneIndex := startLoading(e.Out, e.Colour, e.Say, "reading the item index")
	readIndex := func(n, total int) { indexed = total; readIndexRaw(n, total) }
	matchSources, doneMatching := startLoading(e.Out, e.Colour, e.Say, "placing sources")
	chosen, err := PlanSources(c, r, e.Index, e.Fingerprints, PlanOptions{
		Watch: []func(int, int){readIndex}, Match: []func(int, int){func(n, total int) {
			if n == 0 {
				// Matching has begun, so the index is read. Closing it here
				// rather than on every source, which printed its summary 9,824
				// times before finish learned to say things once.
				doneIndex(indexed)
			}
			matchSources(n, total)
		}},
		Stop: ctx.Err, Authors: a.Strings("author"), Limit: planLimit, Needs: a.Strings("needs"), Redo: redo, Fresh: a.Bool("new"), Only: only})
	doneIndex(0)
	doneMatching(len(chosen))
	// Planning is over the moment it returns, whatever the command does next.
	// Left to the deferred call this label stayed in flight for the whole run,
	// so a heartbeat an hour in still said "planning recordings".
	planned(err)
	if err != nil {
		return nil, err
	}
	if only != nil {
		matched := map[string]bool{}
		for _, j := range chosen {
			item := optionalYAML(j.Path)
			matched[str(item["id"])] = true
		}
		docs, err := Documents(c.Content, "item")
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			id := str(d.Data["id"])
			if contains(only, id) && !matched[id] {
				j := e.adoptItem(d)
				if exists(j.Source.resolved) {
					chosen = append(chosen, j)
					matched[id] = true
				}
			}
		}
		missing := []string{}
		for _, id := range only {
			if !matched[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 && !a.Bool("force") {
			return Record{"unmatched": missing}, fmt.Errorf("%d named item(s) could not be reached; --force to run without them", len(missing))
		}
	}
	if a.Command == "run" {
		// Read once for the whole sweep, not once per recording.
		reg, regErr := LoadRegistry(c.RegistryPath())
		if regErr != nil {
			reg = nil
		}
		maps := map[string]Record{}
		sift, doneSifting := startLoading(e.Out, e.Colour, e.Say, "checking what each recording still needs")
		pending := []Planned{}
		for n, j := range chosen {
			// Ctrl+C is answered here, not after the sweep. Cancellation only
			// takes effect where something looks for it, and a loop over every
			// recording in the library is long enough to feel ignored.
			if ctx.Err() != nil {
				doneSifting(len(pending))
				planned(ctx.Err())
				return counts, ctx.Err()
			}
			sift(n, len(chosen))
			if only != nil || len(a.Strings("redo")) > 0 || a.Bool("overwrite") || e.runJobOutstanding(j, a, reg, maps) {
				pending = append(pending, j)
			}
		}
		doneSifting(len(pending))
		chosen = pending
		if n := a.Int("limit"); n > 0 {
			chosen = chosen[:min(n, len(chosen))]
		}
	}
	planned(nil)
	if len(chosen) == 0 && a.Command != "run" {
		return Record{"recordings": 0, "message": "nothing outstanding"}, nil
	}
	e.Say("%d recording(s) selected", len(chosen))
	if a.Command == "run" {
		return e.runSelected(ctx, a, chosen, counts)
	}
	stages := a.Strings("stage")
	if len(stages) == 0 {
		stages = []string{"media", "transcribe", "analyse", "review", "emit"}
	}
	failures := 0
	for _, stage := range []string{"media", "transcribe", "analyse", "review", "emit"} {
		if !contains(stages, stage) {
			continue
		}
		if stage == "review" {
			jobs, skipped, err := e.ReviewJobs(chosen, len(a.Strings("recover")) > 0)
			if err != nil {
				return counts, err
			}
			if len(skipped) > 0 {
				e.Say("%d recording(s) have nothing to review from; the first: %s",
					len(skipped), firstReason(skipped))
			}
			done, err := e.RunReviews(ctx, jobs, max(1, a.Int("in_flight")), max(1, c.Enrich.BatchSize), time.Duration(max(1, a.Int("poll")))*time.Second, a.Strings("recover"), a.Bool("wait_unfinished"))
			counts[stage] = done
			if err != nil {
				failures++
				e.Say("review: %v", err)
			}
			continue
		}
		workers := 1
		if stage == "analyse" {
			workers = max(1, c.Enrich.Workers)
		} else if stage == "transcribe" {
			workers = max(1, c.Transcribe.Workers)
		}
		_, errs := parallelMap(ctx, chosen, workers, func(ctx context.Context, j Planned) (bool, error) {
			var err error
			switch stage {
			case "media":
				_, err = PlaceMedia(ctx, c, j.Source, j.Stem, false)
			case "transcribe":
				err = e.Transcribe(ctx, j, false, false)
			case "analyse":
				err = e.Analyze(ctx, j, a.Bool("redo_analysis"))
			case "emit":
				err = e.Emit(ctx, j, !a.Bool("no_covers"), a.Bool("overwrite"), false)
			}
			return err == nil, err
		})
		done, skipped := 0, 0
		for i, err := range errs {
			if err == nil {
				done++
				continue
			}
			if _, ok := err.(NothingToProduce); ok {
				skipped++
				continue
			}
			failures++
			e.Say("FAIL %s %s: %v", stage, chosen[i].Stem, err)
		}
		counts[stage] = Record{"done": done, "skipped": skipped}
		e.Say("%s: %d done, %d skipped", stage, done, skipped)
		if ctx.Err() != nil {
			return counts, ctx.Err()
		}
	}
	counts["failures"] = failures
	if failures > 0 {
		return counts, fmt.Errorf("%d operation(s) failed; completed work was retained", failures)
	}
	return counts, nil
}
func (e *Engine) voiceCommand(ctx context.Context, a Arguments) (Record, error) {
	if a.Bool("embed") {
		docs, err := Documents(e.Config.Content, "item")
		if err != nil {
			return nil, err
		}
		if host := a.String("host"); host != "" {
			e.Config.Transcribe.Remote = host
		}
		e.Box = NewGPUBox(e.Config, a.String("remote_dir"))
		jobs := []Planned{}
		for _, d := range docs {
			if a.String("author") != "" && str(d.Data["author"]) != a.String("author") {
				continue
			}
			j := e.adoptItem(d)
			fp := j.Source.fingerprint
			if fp == "" || (!a.Bool("redo") && exists(filepath.Join(e.Config.Cache, "voiceprints", fp+".json"))) || !exists(j.Source.resolved) {
				continue
			}
			jobs = append(jobs, j)
		}
		if n := a.Int("limit"); n > 0 {
			jobs = jobs[:min(n, len(jobs))]
		}
		_, errs := parallelMap(ctx, jobs, max(1, e.Config.Transcribe.Workers), func(ctx context.Context, j Planned) (bool, error) {
			err := e.Transcribe(ctx, j, true, a.Bool("redo"))
			return err == nil, err
		})
		failed := 0
		for i, err := range errs {
			if err != nil {
				failed++
				e.Say("embed %s: %v", jobs[i].Stem, err)
			}
		}
		if !a.Bool("verify") && !a.Bool("cameos") {
			return Record{"embedded": len(jobs) - failed, "failed": failed}, nil
		}
	}
	floor := SameVoice
	if a.Present["floor"] {
		floor = a.Float("floor")
	}
	return VoiceReport(e.Config, floor, a.Bool("cameos"))
}
func (e *Engine) transcribeCommand(ctx context.Context, a Arguments) (Record, error) {
	if a.Present["model"] {
		e.Config.Transcribe.Model = a.String("model")
	}
	e.Box = NewGPUBox(e.Config, e.Config.Transcribe.RemoteDir)
	if a.Int("interval") > 0 {
		e.Box.Poll = time.Duration(a.Int("interval")) * time.Second
	}
	if a.Command == "transcribe/worker" {
		action := a.Positionals[0]
		switch action {
		case "start":
			return Record{"worker": "started"}, e.ensureBox(ctx)
		case "stop":
			return Record{"worker": "stopping"}, e.Box.Stop(ctx)
		case "kill":
			return Record{"worker": "killed"}, e.Box.Kill(ctx)
		case "log":
			b, err := e.Box.run(ctx, fmt.Sprintf("tail -n %d %s", max(1, a.Int("lines")), shellQuote(e.Box.Directory+"/worker.log")))
			return Record{"log": string(b)}, err
		case "failures":
			b, err := e.Box.run(ctx, "ls -1 "+shellQuote(e.Box.Directory+"/failed"))
			return Record{"failures": pythonFields(string(b))}, err
		}
	}
	jobs := []Planned{}
	if paths := a.Strings("path"); len(paths) > 0 {
		files, err := looseAudio(paths)
		if err != nil {
			return nil, err
		}
		for _, p := range files {
			s := &Source{Path: p, Audio: p, Title: stemOf(p), Data: Record{}}
			jobs = append(jobs, Planned{s, stemOf(p), ""})
		}
	} else if list := a.String("from_list"); list != "" {
		files, err := readLines(list)
		if err != nil {
			return nil, err
		}
		for _, p := range files {
			if exists(p) {
				s := &Source{Path: p, Audio: p, Title: stemOf(p), Data: Record{}}
				jobs = append(jobs, Planned{s, stemOf(p), ""})
			}
		}
	} else {
		docs, err := Documents(e.Config.Content, "item")
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			if a.String("author") != "" && str(d.Data["author"]) != a.String("author") {
				continue
			}
			j := e.adoptItem(d)
			if exists(j.Source.resolved) {
				jobs = append(jobs, j)
			}
		}
	}
	if a.Command == "transcribe/audit" {
		short := []any{}
		keys := []string{}
		for _, j := range jobs {
			fp, err := e.fingerprint(j)
			if err != nil {
				continue
			}
			p := e.transcript(fp)
			if p == nil {
				continue
			}
			// The duration is already recorded, on the source or on the
			// transcript. Asking ffprobe again -- once per recording, in turn --
			// was the whole cost of this pass.
			length := number(first(j.Source.Data["duration"], p["duration"]))
			if length == 0 {
				complete := startRunWork(ctx, "audit transcript "+j.Source.AuthorID()+"/"+j.Stem)
				length = number(AudioMetadata(ctx, j.Source.AudioPath(e.Config.Sources))["duration"])
				complete(ctx.Err())
			}
			covered := number(p["covered"])
			if p["covered"] == nil {
				segments := array(p["segments"])
				if len(segments) > 0 {
					covered = number(record(segments[len(segments)-1])["end"])
				}
				if covered == 0 {
					covered = number(j.Source.Data["duration"])
				}
			}
			if length > 120 && covered < length*a.Float("coverage") {
				short = append(short, Record{"fingerprint": fp, "title": j.Source.Title, "duration": length, "covered": covered})
				keys = append(keys, fp)
			}
		}
		if path := a.String("out"); path != "" {
			if _, err := AtomicWrite(path, []byte(strings.Join(keys, "\n")+"\n"), 0644); err != nil {
				return nil, err
			}
		}
		return Record{"short": short}, nil
	}
	pending := []Planned{}
	for _, j := range jobs {
		fp, err := e.fingerprint(j)
		if err != nil {
			continue
		}
		if e.transcript(fp) == nil {
			pending = append(pending, j)
		}
	}
	if a.Command == "transcribe/status" {
		r := Record{"files": len(jobs), "cached": len(jobs) - len(pending), "pending": len(pending)}
		if e.Box.Host != "" {
			b, err := e.Box.run(ctx, "ls -1 "+shellQuote(e.Box.Directory+"/jobs"))
			if err == nil {
				r["queued"] = len(pythonFields(string(b)))
			}
		}
		return r, nil
	}
	if n := a.Int("limit"); n > 0 {
		pending = pending[:min(n, len(pending))]
	}
	_, errs := parallelMap(ctx, pending, max(1, a.Int("readers")), func(ctx context.Context, j Planned) (bool, error) {
		err := e.Transcribe(ctx, j, false, false)
		return err == nil, err
	})
	failed := 0
	for i, err := range errs {
		if err != nil {
			failed++
			e.Say("transcribe %s: %v", pending[i].Stem, err)
		}
	}
	if a.Bool("stop_worker") {
		if err := e.Box.Stop(ctx); err != nil {
			return nil, err
		}
	}
	report := Record{"transcribed": len(pending) - failed, "failed": failed}
	if failed > 0 {
		return report, fmt.Errorf("%d transcription(s) failed", failed)
	}
	return report, nil
}
