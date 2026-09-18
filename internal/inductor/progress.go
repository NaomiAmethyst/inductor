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

// Short enough that a person watching a long phase can see it is alive. The
// report is a few lines, so this is not noisy, and a run that finishes inside
// one interval never prints a heartbeat at all.
const runProgressInterval = 15 * time.Second

type progressContextKey struct{}

type graphProgress struct {
	Total, Cached, Completed, Running, Pending, Failed, Blocked, Skipped int
	PerArtifact                                                          []artifactTally
}

// phaseTally is one row of the maintenance report: what each pass over the
// library is doing, so the passes after the recordings are as legible as the
// recordings were.
type phaseTally struct {
	Name, State string
	Detail      string
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

// workItem is one unit of dispatched work and the pass that asked for it, so
// a phase still running can say what it is doing rather than only that it is.
type workItem struct{ label, phase string }

type phaseContextKey struct{}

// withPhase marks a context as belonging to one pass. Work started under it is
// attributed to that pass; the pass's own top-level work item deliberately is
// not, or every row would do nothing but repeat its own name.
func withPhase(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, phaseContextKey{}, name)
}

func phaseFrom(ctx context.Context) string {
	name, _ := ctx.Value(phaseContextKey{}).(string)
	return name
}

type runProgress struct {
	mu                      sync.Mutex
	say                     func(string, ...any)
	verbose                 bool
	started                 time.Time
	next, completed, failed int
	skipped                 int
	active                  map[int]workItem
	graph                   *graphProgress
	graphOver               bool
	phases                  []phaseTally
	ink                     paint
}

func progressFrom(ctx context.Context) *runProgress {
	p, _ := ctx.Value(progressContextKey{}).(*runProgress)
	return p
}

func startRunProgress(ctx context.Context, say func(string, ...any), noProgress, verbose, colour bool) (context.Context, func()) {
	p := &runProgress{say: say, verbose: verbose, started: time.Now(), active: map[int]workItem{}, ink: paint{on: colour}}
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
	clearLiveLine()
	p.mu.Lock()
	defer p.mu.Unlock()
	ink := p.ink

	active := map[string]int{}
	byPhase := map[string][]string{}
	for _, w := range p.active {
		active[w.label]++
		if w.phase != "" {
			byPhase[w.phase] = append(byPhase[w.phase], w.label)
		}
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
	if g == nil || p.graphOver {
		// Once the recordings are done the passes over the library are the
		// story, so they get the table the artefacts had.
		if len(p.phases) > 0 {
			width := 0
			for _, r := range p.phases {
				if len(r.Name) > width {
					width = len(r.Name)
				}
			}
			for _, r := range p.phases {
				tint := ink.dim
				switch r.State {
				case "running":
					tint = ink.cyan
				case "done":
					tint = ink.green
				case "failed":
					tint = ink.red
				case "blocked":
					tint = ink.yellow
				}
				line := fmt.Sprintf("  %-*s  %s", width, r.Name, tint(r.State))
				detail := r.Detail
				if detail == "" && r.State == "running" {
					detail = inFlightDetail(byPhase[r.Name])
				}
				if detail != "" {
					line += "  " + ink.dim(detail)
				}
				p.say("%s", line)
			}
		}
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
	p.active[id] = workItem{label: label, phase: phaseFrom(ctx)}
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

// finishGraphProgress says the recordings are done with. What runs afterwards is
// library maintenance, and repainting the settled artefact rows through it made
// a working run look wedged: the only thing moving was a counter in the header.
func updatePhaseProgress(ctx context.Context, rows []phaseTally) {
	if p := progressFrom(ctx); p != nil {
		p.mu.Lock()
		p.phases = rows
		p.mu.Unlock()
	}
}

func finishGraphProgress(ctx context.Context) {
	if p := progressFrom(ctx); p != nil {
		p.mu.Lock()
		p.graphOver = true
		p.mu.Unlock()
	}
}

// Loading the source records is the one stretch of a run with nothing to say
// for itself: no artefact is in flight, so the graph reporter has not started,
// and on a large library it is twenty seconds of silence. A spinner on a
// terminal, an occasional line anywhere else.
// A spinner and the heartbeat share one terminal, and the spinner leaves the
// cursor mid-line. Whoever writes next clears what is there first, or the two
// run together: "...330/927921:41:04  15s elapsed".
var liveLine struct {
	mu    sync.Mutex
	dirty bool
	out   io.Writer
}

func markLiveLine(out io.Writer) {
	liveLine.mu.Lock()
	defer liveLine.mu.Unlock()
	liveLine.dirty, liveLine.out = true, out
}

func clearLiveLine() {
	liveLine.mu.Lock()
	defer liveLine.mu.Unlock()
	if liveLine.dirty && liveLine.out != nil {
		fmt.Fprint(liveLine.out, "\r\x1b[2K")
	}
	liveLine.dirty = false
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	spinnerPaint = 80 * time.Millisecond
	loadingSay   = 3 * time.Second
	loadingFloor = 400 * time.Millisecond
)

type loading struct {
	out     io.Writer
	tty     bool
	say     func(string, ...any)
	label   string
	started time.Time
	painted time.Time
	said    time.Time
	frame   int
	drawn   bool
	over    bool
}

// startLoading reports progress through a phase that counts files. The returned
// step is called per file and finish tidies up after it; both are safe when
// there is no terminal, and neither says anything about a phase that turns out
// to be quick.
func startLoading(out io.Writer, tty bool, say func(string, ...any), label string) (step func(done, total int), finish func(total int)) {
	l := &loading{out: out, tty: tty, say: say, label: label, started: time.Now()}
	return l.step, l.finish
}

func (l *loading) step(done, total int) {
	if l.over {
		return
	}
	now := time.Now()
	if now.Sub(l.started) < loadingFloor {
		return // a fast phase says nothing at all
	}
	if l.tty && l.out != nil {
		if now.Sub(l.painted) < spinnerPaint {
			return
		}
		l.painted = now
		l.frame = (l.frame + 1) % len(spinnerFrames)
		fmt.Fprintf(l.out, "\r\x1b[2K%s %s: %d/%d", spinnerFrames[l.frame], l.label, done, total)
		l.drawn = true
		markLiveLine(l.out)
		return
	}
	if now.Sub(l.said) < loadingSay {
		return
	}
	l.said = now
	l.say("%s: %d/%d", l.label, done, total)
}

func (l *loading) finish(total int) {
	// Said once. A phase can be closed out from more than one place -- the step
	// that follows it, the caller that waited for it -- and the second caller
	// must not repaint or repeat the line.
	if l.over {
		return
	}
	l.over = true
	if l.drawn && l.out != nil {
		clearLiveLine()
	}
	if spent := time.Since(l.started); spent >= loadingFloor {
		l.say("%s: %d in %s", l.label, total, spent.Round(time.Millisecond))
	}
}

// summarise reduces a phase's report to something worth one line. Numbers that
// are zero and lists that are empty say nothing a reader needs, so they are left
// out; what remains is what happened.
func summarise(r Record) string {
	parts := []string{}
	for _, k := range sortedKeys(r) {
		switch v := r[k].(type) {
		case nil:
		case bool:
			if v {
				parts = append(parts, k)
			}
		case string:
			if v != "" && len(v) < 60 {
				parts = append(parts, fmt.Sprintf("%s %s", k, v))
			}
		case []any:
			if len(v) > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", len(v), k))
			}
		case []string:
			if len(v) > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", len(v), k))
			}
		case map[string]any:
			if inner := summarise(Record(v)); inner != "" {
				parts = append(parts, fmt.Sprintf("%s (%s)", k, inner))
			}
		default:
			if n := number(v); n != 0 {
				parts = append(parts, fmt.Sprintf("%s %s", trimNumber(n), k))
			}
		}
		if len(parts) >= 6 {
			parts = append(parts, "…")
			break
		}
	}
	return strings.Join(parts, ", ")
}

func trimNumber(n float64) string {
	if n == float64(int64(n)) {
		return fmt.Sprint(int64(n))
	}
	return fmt.Sprintf("%.1f", n)
}

// forReading replaces a run's filed findings with a line each, leaving the path
// to the full document in their place.
//
// The trim happens where the report is printed, never where it is built: the
// Record is the machine-readable result and other things read it. A person at a
// terminal wants to know that check found three hundred warnings and where to
// read them; a pipeline wants all three hundred.
func forReading(report Record) Record {
	if report == nil || !truth(report["report"]) {
		return report
	}
	out := Record{}
	for k, v := range report {
		out[k] = v
	}
	for _, name := range filedFindings {
		r, ok := out[name].(map[string]any)
		if !ok {
			continue
		}
		if found := summarise(r); found != "" {
			out[name] = found
		} else {
			out[name] = "nothing to report"
		}
	}
	return out
}

// inFlightDetail turns a pass's outstanding work into one line. A pass that
// dispatches many identical units says how many; one that dispatches a few
// distinct ones names them, because "3 running" hides which three are stuck.
func inFlightDetail(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, l := range labels {
		counts[l]++
	}
	names := sortedKeys(counts)
	parts := []string{}
	for _, n := range names[:min(3, len(names))] {
		if counts[n] > 1 {
			parts = append(parts, fmt.Sprintf("%s ×%d", n, counts[n]))
			continue
		}
		parts = append(parts, n)
	}
	if len(names) > len(parts) {
		parts = append(parts, fmt.Sprintf("+%d more", len(names)-len(parts)))
	}
	return fmt.Sprintf("%d in flight · %s", len(labels), strings.Join(parts, ", "))
}
