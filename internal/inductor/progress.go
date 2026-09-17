// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const runProgressInterval = time.Minute

type progressContextKey struct{}

type graphProgress struct {
	Total, Cached, Completed, Running, Pending, Failed, Blocked, Skipped int
	PerArtifact                                                          []artifactTally
}

// artifactTally is one row of the progress report: how far one artefact has got
// across the whole run, and what is holding the rest up.
type artifactTally struct {
	Name                                           string
	Done, Total, Running, Blocked, Failed, Skipped int
}

// Colour is written only to a terminal. Anything redirected to a file or a pipe
// -- a log, a CI capture, this project's own task output -- gets plain text, and
// NO_COLOR is honoured whatever the destination.
func colourAvailable(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

type paint struct{ on bool }

func (p paint) in(code, text string) string {
	if !p.on || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (p paint) dim(t string) string    { return p.in("2", t) }
func (p paint) green(t string) string  { return p.in("32", t) }
func (p paint) cyan(t string) string   { return p.in("36", t) }
func (p paint) yellow(t string) string { return p.in("33", t) }
func (p paint) red(t string) string    { return p.in("31", t) }

type runProgress struct {
	mu                      sync.Mutex
	say                     func(string, ...any)
	verbose                 bool
	started                 time.Time
	next, completed, failed int
	skipped                 int
	active                  map[int]string
	graph                   *graphProgress
	ink                     paint
}

func progressFrom(ctx context.Context) *runProgress {
	p, _ := ctx.Value(progressContextKey{}).(*runProgress)
	return p
}

func startRunProgress(ctx context.Context, say func(string, ...any), noProgress, verbose, colour bool) (context.Context, func()) {
	p := &runProgress{say: say, verbose: verbose, started: time.Now(), active: map[int]string{}, ink: paint{on: colour}}
	ctx = context.WithValue(ctx, progressContextKey{}, p)
	if noProgress {
		return ctx, func() {}
	}
	ticker := time.NewTicker(runProgressInterval)
	watchCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		p.watch(watchCtx, ticker.C)
	}()
	return ctx, func() {
		ticker.Stop()
		cancel()
		<-stopped
	}
}

func (p *runProgress) watch(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok || ctx.Err() != nil {
				return
			}
			p.print(now)
		}
	}
}

func (p *runProgress) print(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ink := p.ink

	active := map[string]int{}
	for _, label := range p.active {
		active[label]++
	}
	labels := sortedKeys(active)
	shown := append([]string{}, labels[:min(5, len(labels))]...)
	if len(labels) > len(shown) {
		shown = append(shown, fmt.Sprintf("+%d more", len(labels)-len(shown)))
	}
	current := strings.Join(shown, ", ")
	if current == "" {
		current = "preparing next work"
	}

	head := fmt.Sprintf("%s  %s elapsed · %s · %s",
		ink.dim(now.Format("15:04:05")),
		now.Sub(p.started).Round(time.Second),
		ink.cyan(fmt.Sprintf("%d in flight", len(p.active))),
		ink.dim(current))
	if p.completed+p.failed+p.skipped > 0 {
		head += fmt.Sprintf(" · %d done", p.completed)
		if p.failed > 0 {
			head += ink.red(fmt.Sprintf(", %d failed", p.failed))
		}
		if p.skipped > 0 {
			head += ink.dim(fmt.Sprintf(", %d skipped", p.skipped))
		}
	}
	p.say("%s", head)

	g := p.graph
	if g == nil {
		return
	}
	if len(g.PerArtifact) == 0 {
		// Commands that report progress without running the graph still have
		// their overall shape worth saying.
		p.say("  %s", ink.dim(fmt.Sprintf("artefacts: %d total, %d cached, %d completed, %d running, %d pending, %d failed, %d blocked, %d skipped",
			g.Total, g.Cached, g.Completed, g.Running, g.Pending, g.Failed, g.Blocked, g.Skipped)))
		return
	}
	width, digits := 0, 0
	for _, a := range g.PerArtifact {
		if len(a.Name) > width {
			width = len(a.Name)
		}
		if d := len(fmt.Sprint(a.Total)); d > digits {
			digits = d
		}
	}
	for _, a := range g.PerArtifact {
		if a.Total == 0 {
			continue
		}
		count := fmt.Sprintf("%*d/%d", digits, a.Done, a.Total)
		if a.Done == a.Total {
			count = ink.green(count)
		}
		notes := []string{}
		for _, n := range []struct {
			n    int
			word string
			tint func(string) string
		}{
			{a.Running, "running", ink.cyan},
			{a.Skipped, "skipped", ink.dim},
			{a.Blocked, "blocked", ink.yellow},
			{a.Failed, "failed", ink.red},
		} {
			if n.n > 0 {
				notes = append(notes, n.tint(fmt.Sprintf("%d %s", n.n, n.word)))
			}
		}
		line := fmt.Sprintf("  %-*s  %s", width, a.Name, count)
		if len(notes) > 0 {
			line += "  " + strings.Join(notes, ink.dim(" · "))
		}
		p.say("%s", line)
	}
}

// A context carries the observer through worker goroutines without changing
// engine state or enabling run-specific output for standalone commands.
func startRunWork(ctx context.Context, label string) func(error) {
	p := progressFrom(ctx)
	if p == nil {
		return func(error) {}
	}
	started := time.Now()
	p.mu.Lock()
	p.next++
	id := p.next
	p.active[id] = label
	if p.verbose {
		p.say("dispatch: %s", label)
	}
	p.mu.Unlock()
	return func(err error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if _, ok := p.active[id]; !ok {
			return
		}
		delete(p.active, id)
		status := "completed"
		if _, nothing := err.(NothingToProduce); nothing {
			status = "skipped"
			p.skipped++
		} else if err != nil {
			status = "failed"
			p.failed++
		} else {
			p.completed++
		}
		if p.verbose {
			detail := ""
			if err != nil {
				detail = ": " + err.Error()
			}
			p.say("%s: %s (%s)%s", status, label, time.Since(started).Round(time.Millisecond), detail)
		}
	}
}

func updateGraphProgress(ctx context.Context, g graphProgress) {
	if p := progressFrom(ctx); p != nil {
		p.mu.Lock()
		p.graph = &g
		p.mu.Unlock()
	}
}
