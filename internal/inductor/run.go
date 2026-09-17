// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

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
	step := func(name string, work func() (Record, error)) bool {
		if ctx.Err() != nil {
			return false
		}
		e.Say("%s: starting", name)
		complete := startRunWork(ctx, name)
		report, err := work()
		complete(err)
		counts[name] = report
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
			e.Say("FAIL %s: %v", name, err)
			return false
		}
		return true
	}
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
	if !a.Bool("no_tagmaps") && (!filtered || len(authors) > 0) {
		ok := step("tagmaps", func() (Record, error) {
			return e.BuildTagmaps(ctx, authors, c.Enrich.AdjudicatorModel, dry)
		})
		if ok && !dry {
			step("tagmap_items", func() (Record, error) { return reconcileRunTags(c, authors) })
		}
	}
	counts["recordings"] = len(jobs)
	if ctx.Err() == nil {
		e.noProvision = a.Bool("no_provision")
		e.Box = NewGPUBox(c, a.String("remote_dir"))
		complete := startRunWork(ctx, "recording pipeline")
		graph, err := e.RunGraph(ctx, jobs, RunOptions{Redo: a.Strings("redo"), Overwrite: a.Bool("overwrite"), Covers: !a.Bool("no_covers"), DryRun: dry, Batch: a.Int("batch"), RefreshEntries: true})
		complete(err)
		for k, v := range graph {
			counts[k] = v
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	if !a.Bool("no_transcribe_audit") {
		step("transcribe_audit", func() (Record, error) {
			audit, err := ParseArgs([]string{"transcribe", "audit"})
			if err != nil {
				return nil, err
			}
			return e.transcribeCommand(ctx, audit)
		})
	}
	if !a.Bool("no_acoustic_apply") {
		step("acoustic_apply", func() (Record, error) { return e.AcousticApply(dry) })
	}
	adjudicationOK := true
	if !a.Bool("no_adjudicate") {
		adjudicationOK = step("adjudicate", func() (Record, error) { return e.adjudicateRun(ctx, a) })
		if adjudicationOK && !dry && !a.Bool("no_adjudicate_apply") && !a.Bool("no_adjudicate_write") {
			// Applying a queued target settles its author's mapping. Resolve
			// the original source spellings on entries using those new rows.
			step("adjudicate_items", func() (Record, error) { return reconcileRunTags(c, nil) })
		}
	}
	if !a.Bool("no_backfill") && adjudicationOK {
		step("backfill", func() (Record, error) {
			write := !dry && !a.Bool("no_adjudicate_apply") && !a.Bool("no_adjudicate_write")
			return backfill(c, nil, write, true)
		})
	}
	if !a.Bool("no_covers") && !a.Bool("no_artwork_repair") && c.Enrich.Covers {
		if !a.Bool("no_cover_prompts") {
			step("cover_prompts", func() (Record, error) {
				if !exists(c.Analysis()) {
					return Record{"pending": 0}, nil
				}
				return e.CoverPrompts(ctx, c.Enrich.AnalysisModel, max(1, c.Enrich.Workers), 0, dry)
			})
		}
		step("cover_prompt_items", func() (Record, error) { return e.syncRunCoverPrompts(dry) })
		step("artwork", func() (Record, error) { return e.Artwork(ctx, "", 0, 2, false, !dry) })
	}
	if !dry && !a.Bool("no_pages") {
		step("pages", func() (Record, error) {
			return e.Authors(ctx, AuthorOptions{Model: c.Enrich.AnalysisModel, Render: !a.Bool("no_covers"), Write: true, Workers: 4})
		})
	}
	if !a.Bool("no_similar") {
		step("similar", func() (Record, error) { return SimilarVoices(c, .55, 8, !dry) })
	}
	if !a.Bool("no_voiceprint_verify") || !a.Bool("no_cameos") {
		name := "voiceprint_verify"
		if a.Bool("no_voiceprint_verify") {
			name = "cameos"
		}
		step(name, func() (Record, error) {
			r, err := VoiceReport(c, SameVoice, !a.Bool("no_cameos"))
			if a.Bool("no_voiceprint_verify") {
				return Record{"cameos": r["cameos"]}, err
			}
			if !a.Bool("no_cameos") {
				counts["cameos"] = Record{"cameos": r["cameos"]}
			}
			return r, err
		})
	}
	if !a.Bool("no_duplicates") {
		step("duplicates", func() (Record, error) { return e.Dispatch(ctx, Arguments{Command: "duplicates"}) })
	}
	if !a.Bool("no_orphans") {
		step("orphans", func() (Record, error) { return Orphans(c, false) })
	}
	return counts, errors.Join(append(failures, ctx.Err())...)
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
