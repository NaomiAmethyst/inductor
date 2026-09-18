// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"fmt"
	"sync"
)

// A phase is one pass over the library. Most of them want nothing from each
// other: auditing transcripts, comparing voiceprints, translating cover prompts
// and ruling on tags all read what the recordings left behind and touch
// different things. Run one after another they took four hours, most of it
// spent waiting behind work that had no bearing on the next thing.
type phase struct {
	name string
	// label is what the progress reporter calls this phase when it differs from
	// the name the other phases depend on.
	label string
	// needs must have succeeded; after need only have finished. A pass over
	// the recordings that failed sixty-six artefacts out of two thousand still
	// leaves work worth auditing, tagging and drawing -- blocking everything
	// behind it because some of it failed is worse than the serial order it
	// replaced.
	needs []string
	after []string
	lane  string
	when  bool
	run   func(context.Context) (Record, error)
}

// Lanes keep the concurrency honest: the API ones are rate-limited upstream, the
// art ones queue on a single renderer, and the last two want a tree nobody else
// is still writing to.
// "items" is one at a time on purpose. Several passes rewrite the same item
// documents -- reconciling tags, backfilling newly-resolvable ones, syncing
// cover prompts, repairing artwork -- and run together they read, edit and save
// the same file, so whichever saves last silently discards the others' work.
var phaseLanes = map[string]int{"api": 3, "art": 1, "local": 4, "items": 1, "settled": 1, "graph": 1}

// RunPhases runs what is ready, as soon as it is ready. A phase whose
// dependency failed is reported blocked rather than attempted: its inputs are
// not there, and running it anyway would report a second failure for one cause.
func (e *Engine) RunPhases(ctx context.Context, phases []phase, counts Record) []error {
	const (
		waiting = iota
		running
		done
		failed
	)
	state := map[string]int{}
	enabled := map[string]bool{}
	for _, p := range phases {
		if p.when {
			enabled[p.name] = true
			state[p.name] = waiting
		}
	}
	var mu sync.Mutex
	type finished struct {
		name   string
		report Record
		err    error
	}
	events := make(chan finished, len(phases))
	lanes := map[string]int{}
	inFlight, failures := 0, []error{}
	detail := map[string]string{}

	// What each pass is doing, published so the heartbeat can show it the way
	// it shows the artefacts.
	publish := func() {
		rows := make([]phaseTally, 0, len(phases))
		for _, p := range phases {
			if !p.when {
				continue
			}
			name := map[int]string{waiting: "waiting", running: "running", done: "done", failed: "failed"}[state[p.name]]
			if state[p.name] == waiting {
				for _, need := range p.needs {
					if enabled[need] && state[need] == failed {
						name = "blocked"
					}
				}
			}
			rows = append(rows, phaseTally{Name: p.name, State: name, Detail: detail[p.name]})
		}
		updatePhaseProgress(ctx, rows)
	}

	ready := func(p phase) bool {
		if state[p.name] != waiting || lanes[p.lane] >= max(1, phaseLanes[p.lane]) {
			return false
		}
		for _, need := range p.needs {
			if enabled[need] && state[need] != done {
				return false
			}
		}
		for _, once := range p.after {
			if enabled[once] && state[once] != done && state[once] != failed {
				return false
			}
		}
		return true
	}
	for {
		for _, p := range phases {
			if !p.when || ctx.Err() != nil || !ready(p) {
				continue
			}
			state[p.name], lanes[p.lane], inFlight = running, lanes[p.lane]+1, inFlight+1
			publish()
			e.Say("%s: starting", p.name)
			go func(p phase) {
				complete := startRunWork(ctx, str(first(p.label, p.name)))
				r, err := p.run(withPhase(ctx, p.name))
				complete(err)
				events <- finished{p.name, r, err}
			}(p)
		}
		if inFlight == 0 {
			break
		}
		ev := <-events
		inFlight--
		for _, p := range phases {
			if p.name == ev.name {
				lanes[p.lane]--
			}
		}
		mu.Lock()
		if ev.report != nil {
			counts[ev.name] = ev.report
		}
		mu.Unlock()
		if ev.err != nil {
			state[ev.name] = failed
			detail[ev.name] = firstLine(ev.err.Error())
			publish()
			failures = append(failures, fmt.Errorf("%s: %w", ev.name, ev.err))
			e.Say("FAIL %s: %v", ev.name, ev.err)
			continue
		}
		state[ev.name] = done
		detail[ev.name] = summarise(ev.report)
		publish()
		if found := summarise(ev.report); found != "" {
			e.Say("%s: %s", ev.name, found)
		} else {
			e.Say("%s: nothing to report", ev.name)
		}
	}
	// Anything still waiting lost a dependency rather than its own footing.
	for _, p := range phases {
		if p.when && state[p.name] == waiting && ctx.Err() == nil {
			held := []string{}
			for _, need := range p.needs {
				if enabled[need] && state[need] != done {
					held = append(held, need)
				}
			}
			for _, once := range p.after {
				if enabled[once] && state[once] != done && state[once] != failed {
					held = append(held, once)
				}
			}
			if len(held) > 0 {
				e.Say("%s: not run, waiting on %v", p.name, held)
				counts[p.name] = Record{"blocked_by": held}
			}
		}
	}
	return failures
}

// firstLine keeps a failure to something that fits beside a phase's name.
func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			s = s[:i]
			break
		}
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}
