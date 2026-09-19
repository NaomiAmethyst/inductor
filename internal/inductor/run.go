// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// filedFindings are the phases that look rather than change. What a run *did*
// -- the rulings it applied, the tags it mapped -- is the result and stays put.
var filedFindings = []string{"check", "duplicates", "orphans", "similar",
	"cameos", "voiceprint_verify", "transcribe_audit"}

// fileRunReport puts the maintenance phases' full findings in the cache and
// leaves a line about each in their place.
//
// check, duplicates and orphans each return a nested document -- every warning,
// every stray path -- and printing all of it buries the part of a run somebody
// actually watched for. The detail is worth keeping and worth reading; it is
// just not worth scrolling past.
func (e *Engine) fileRunReport(counts Record, dry bool) Record {
	if dry {
		// A dry run writes nothing, the cache included. It is also the mode
		// somebody uses to read the findings in full, so leave them in place.
		return counts
	}
	verbose := Record{}
	// Only the phases that look rather than change. What a run *did* -- the
	// rulings it applied, the tags it mapped, the covers it repaired -- stays in
	// the report itself, because that is the result, not a finding about it.
	for _, name := range filedFindings {
		if r, ok := counts[name].(map[string]any); ok {
			verbose[name] = r
		}
	}
	if len(verbose) == 0 {
		return counts
	}
	path := filepath.Join(e.Config.Cache, "runs",
		fmt.Sprintf("run-%s.json", time.Now().UTC().Format("20060102-150405")))
	if err := writeJSON(path, Record{"apiVersion": "inductor/v1", "kind": "RunReport",
		"written": time.Now().UTC().Format(time.RFC3339), "phases": verbose}); err != nil {
		e.Say("could not file the run report: %v", err)
		return counts
	}
	// The findings stay in the report as well as the file. They are the
	// machine-readable result of the run and other things read them; the file
	// is so a person has somewhere to look rather than somewhere to scroll.
	counts["report"] = path
	e.Say("full findings: %s", path)
	return counts
}

// runSelected surrounds the recording graph with library maintenance. Reports
// still run after individual step failures, but cancellation stops further work.
func (e *Engine) runSelected(ctx context.Context, a Arguments, jobs []Planned, counts Record) (Record, error) {
	c := e.Config
	dry := a.Bool("dry_run")
	order, err := GraphOrder(Graph)
	if err != nil {
		return counts, err
	}
	for _, name := range a.Strings("redo") {
		if !contains(order, name) {
			return counts, fmt.Errorf("nothing in the graph is called %s", name)
		}
	}
	var failures []error
	// An unrestricted run also fills maps for already-finished authors. An
	// empty item selection must never turn an explicit filter into "all".
	authors := a.Strings("author")
	filtered := a.String("only") != "" || a.Int("limit") > 0
	if filtered {
		selected := map[string]bool{}
		for _, job := range jobs {
			selected[job.Source.AuthorID()] = true
		}
		authors = sortedKeys(selected)
	}
	counts["recordings"] = len(jobs)
	adjudicateWrites := !dry && !a.Bool("no_adjudicate_apply") && !a.Bool("no_adjudicate_write")
	artOK := !a.Bool("no_covers") && !a.Bool("no_artwork_repair") && c.Enrich.Covers
	voiceName := "voiceprint_verify"
	if a.Bool("no_voiceprint_verify") {
		voiceName = "cameos"
	}
	// Everything that writes into the tree, so the two passes that want a
	// settled library actually get one.
	writers := []string{"recordings", "tagmap_items", "adjudicate_items", "backfill", "artwork", "pages"}

	phases := []phase{
		{name: "tagmaps", lane: "api",
			when: !a.Bool("no_tagmaps") && (!filtered || len(authors) > 0),
			run: func(ctx context.Context) (Record, error) {
				return e.BuildTagmaps(ctx, authors, c.Enrich.AdjudicatorModel, dry)
			}},
		{name: "tagmap_items", lane: "items", needs: []string{"tagmaps"},
			when: !a.Bool("no_tagmaps") && !dry && (!filtered || len(authors) > 0),
			run:  func(ctx context.Context) (Record, error) { return reconcileRunTags(c, authors) }},

		// The recordings themselves, after their creators' vocabularies are
		// mapped, because the entry writer resolves tags through those maps.
		{name: "recordings", label: "recording pipeline", lane: "graph", after: []string{"tagmaps"}, when: true,
			run: func(ctx context.Context) (Record, error) {
				e.noProvision = a.Bool("no_provision")
				e.Box = NewGPUBox(c, a.String("remote_dir"))
				return e.RunGraph(ctx, jobs, RunOptions{Redo: a.Strings("redo"), Overwrite: a.Bool("overwrite"),
					Covers: !a.Bool("no_covers"), DryRun: dry, Batch: a.Int("batch"), RefreshEntries: true})
			}},

		{name: "transcribe_audit", lane: "local", after: []string{"recordings"},
			when: !a.Bool("no_transcribe_audit"),
			run: func(ctx context.Context) (Record, error) {
				audit, err := ParseArgs([]string{"transcribe", "audit"})
				if err != nil {
					return nil, err
				}
				return e.transcribeCommand(ctx, audit)
			}},
		// The items lane, not local: this rewrites whole item documents from a
		// snapshot it read when it started, so anything else writing items at
		// the same time loses whichever of them saves first. That is what the
		// lane is one job wide for.
		{name: "acoustic_apply", lane: "items", after: []string{"recordings"},
			when: !a.Bool("no_acoustic_apply"),
			run:  func(ctx context.Context) (Record, error) { return e.AcousticApply(dry) }},
		// Same lane and the same reason: whole documents, saved back whole.
		{name: "sound_apply", lane: "items", after: []string{"recordings"},
			when: !a.Bool("no_sound_apply"),
			run:  func(ctx context.Context) (Record, error) { return e.SoundApply(dry) }},

		// Ruling waits for every analysis, not for the audits: how many
		// recordings want a tag is the evidence, and that count is still
		// climbing until the last one lands.
		{name: "adjudicate", lane: "api", after: []string{"recordings"},
			when: !a.Bool("no_adjudicate"),
			run:  func(ctx context.Context) (Record, error) { return e.adjudicateRun(ctx, a) }},
		{name: "adjudicate_items", lane: "items", needs: []string{"adjudicate"},
			when: !a.Bool("no_adjudicate") && adjudicateWrites,
			run:  func(ctx context.Context) (Record, error) { return reconcileRunTags(c, nil) }},
		{name: "backfill", lane: "items", needs: []string{"adjudicate"},
			when: !a.Bool("no_backfill"),
			run:  func(ctx context.Context) (Record, error) { return backfill(c, nil, adjudicateWrites, true) }},

		{name: "cover_prompts", lane: "api", after: []string{"recordings"},
			when: artOK && !a.Bool("no_cover_prompts"),
			run: func(ctx context.Context) (Record, error) {
				if !exists(c.Analysis()) {
					return Record{"pending": 0}, nil
				}
				return e.CoverPrompts(ctx, c.Enrich.AnalysisModel, max(1, c.Enrich.Workers), 0, dry)
			}},
		{name: "cover_prompt_items", lane: "items", needs: []string{"cover_prompts"}, when: artOK,
			run: func(ctx context.Context) (Record, error) { return e.syncRunCoverPrompts(dry) }},
		{name: "artwork", lane: "items", after: []string{"cover_prompt_items"}, when: artOK,
			run: func(ctx context.Context) (Record, error) { return e.Artwork(ctx, "", 0, 2, false, !dry) }},
		{name: "pages", lane: "art", after: []string{"recordings"},
			when: !dry && !a.Bool("no_pages"),
			run: func(ctx context.Context) (Record, error) {
				return e.Authors(ctx, AuthorOptions{Model: c.Enrich.AnalysisModel,
					Render: !a.Bool("no_covers"), Write: true, Workers: 4})
			}},

		{name: "similar", lane: "local", after: []string{"recordings"},
			when: !a.Bool("no_similar"),
			run:  func(ctx context.Context) (Record, error) { return SimilarVoices(c, .55, 8, !dry) }},
		{name: voiceName, lane: "local", after: []string{"recordings"},
			when: !a.Bool("no_voiceprint_verify") || !a.Bool("no_cameos"),
			run: func(ctx context.Context) (Record, error) {
				r, err := VoiceReport(c, SameVoice, !a.Bool("no_cameos"))
				if a.Bool("no_voiceprint_verify") {
					return Record{"cameos": r["cameos"]}, err
				}
				return r, err
			}},

		{name: "duplicates", lane: "settled", after: writers, when: !a.Bool("no_duplicates"),
			run: func(ctx context.Context) (Record, error) { return e.Dispatch(ctx, Arguments{Command: "duplicates"}) }},
		// Removing, not just reporting. An item only goes when Inductor claimed
		// it and its source record is gone, so a hand-written entry is never at
		// risk; what a dry run still protects is the creator pages and stray
		// transcripts, which are removed whoever wrote them.
		{name: "orphans", lane: "settled", after: writers, when: !a.Bool("no_orphans"),
			run: func(ctx context.Context) (Record, error) {
				return Orphans(c, !dry && !a.Bool("no_orphans_write"))
			}},
	}
	failures = append(failures, e.RunPhases(ctx, phases, counts)...)
	// The graph reports its artefacts at the top level, as it always has, and
	// "recordings" goes back to being how many there were rather than the
	// phase's report about them.
	if g := record(counts["recordings"]); len(g) > 0 {
		for k, v := range g {
			counts[k] = v
		}
	}
	counts["recordings"] = len(jobs)
	if r := record(counts[voiceName]); truth(r["cameos"]) && !a.Bool("no_cameos") && voiceName != "cameos" {
		counts["cameos"] = Record{"cameos": r["cameos"]}
	}
	return e.fileRunReport(counts, dry), errors.Join(append(failures, ctx.Err())...)
}

func (e *Engine) adjudicateRun(ctx context.Context, a Arguments) (Record, error) {
	c := e.Config
	path := filepath.Join(c.Cache, "rulings.yaml")
	saved := Record{}
	if exists(path) {
		var err error
		saved, err = readYAML(path)
		if err != nil {
			return nil, err
		}
	}
	dry := a.Bool("dry_run")
	rows, err := pendingRunTags(c, !a.Bool("no_review_proposals"))
	if err != nil {
		return nil, err
	}
	report, err := e.adjudicatePending(ctx, rows, dry, c.Enrich.AdjudicatorModel, path)
	if err != nil {
		return report, err
	}
	apply := !a.Bool("no_adjudicate_apply")
	write := apply && !a.Bool("no_adjudicate_write")
	report["apply"], report["write"] = apply, write && !dry
	if dry {
		return report, nil
	}
	// The ledger records decisions before application. Retain unapplied
	// rulings across --no-adjudicate-apply/--no-adjudicate-write and failures,
	// since the adjudicator will not ask for those decisions again.
	byTag := map[string]any{}
	if !truth(saved["applied"]) {
		for _, row := range array(saved["rulings"]) {
			byTag[str(record(row)["tag"])] = row
		}
	}
	fresh := str(report["path"]) != ""
	if fresh {
		generated, err := readYAML(path)
		if err != nil {
			return report, err
		}
		for _, row := range array(generated["rulings"]) {
			byTag[str(record(row)["tag"])] = row
		}
	}
	rulings := []any{}
	for _, tag := range sortedKeys(byTag) {
		rulings = append(rulings, byTag[tag])
	}
	if len(rulings) == 0 {
		return report, nil
	}
	doc := Record{"model": c.Enrich.AdjudicatorModel, "rulings": rulings}
	if fresh {
		if err = writeYAML(path, doc, []string{"model", "rulings", "applied"}); err != nil {
			return report, err
		}
	}
	report["path"], report["saved_rulings"] = path, len(rulings)
	if !apply {
		return report, nil
	}
	complete := startRunWork(ctx, fmt.Sprintf("apply adjudication (%d rulings, write=%t)", len(rulings), write))
	applied, err := ApplyRulings(c, rulings, write)
	complete(err)
	report["application"] = applied
	if err == nil && write {
		doc["applied"] = true
		err = writeYAML(path, doc, []string{"model", "rulings", "applied"})
	}
	return report, err
}

// Reconcile mappings without regenerating summaries, artwork or transcripts.
func reconcileRunTags(c Config, authors []string) (Record, error) {
	reg, err := LoadRegistry(c.RegistryPath())
	if err != nil {
		return nil, err
	}
	docs, err := Documents(c.Content, "item")
	if err != nil {
		return nil, err
	}
	report := Record{"changed": 0}
	for _, d := range docs {
		if len(authors) > 0 && !contains(authors, str(d.Data["author"])) {
			continue
		}
		mapping := LoadMapping(c, str(d.Data["author"]))
		resolve := func(raw []string) []string {
			out := []string{}
			for _, was := range raw {
				tag, why := reg.Resolve(was, mapping)
				if tag == "" && why != "dropped" {
					tag = was
				}
				if tag != "" && !contains(out, tag) {
					out = append(out, tag)
				}
			}
			return out
		}
		tags := resolve(texts(d.Data["tags"]))
		p := record(d.Data["provenance"])
		if enriched := record(p["enriched"]); enriched["tags_added"] != nil {
			enriched["tags_added"] = resolve(texts(enriched["tags_added"]))
		}
		left := []any{}
		for _, proposal := range array(p["proposed_tags"]) {
			tag, why := reg.Resolve(str(record(proposal)["tag"]), mapping)
			if tag != "" {
				if !contains(tags, tag) {
					tags = append(tags, tag)
				}
			} else if why != "dropped" {
				left = append(left, proposal)
			}
		}
		if len(left) > 0 {
			p["proposed_tags"] = left
		} else {
			delete(p, "proposed_tags")
		}
		if len(tags) > 0 || d.Data["tags"] != nil {
			d.Data["tags"] = tags
		}
		if len(tags) > 0 && contains(texts(d.Data["needs"]), "tags") {
			needs := without(texts(d.Data["needs"]), "tags")
			if len(needs) > 0 {
				d.Data["needs"] = needs
			} else {
				delete(d.Data, "needs")
			}
		}
		changed, err := SaveDocument(d.Path, d.Data)
		if err != nil {
			return report, err
		}
		if changed {
			report["changed"] = integer(report["changed"]) + 1
		}
	}
	return report, nil
}
