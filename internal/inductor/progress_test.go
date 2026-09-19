// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProgressReportsMaintenanceAndGraphOnEachTick(t *testing.T) {
	lines := make(chan string, 8)
	ctx, stop := startRunProgress(context.Background(), func(f string, args ...any) {
		lines <- fmt.Sprintf(f, args...)
	}, true, false, false)
	defer stop()
	p := progressFrom(ctx)
	ticks := make(chan time.Time)
	watchCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() { defer close(stopped); p.watch(watchCtx, ticks) }()
	defer func() { cancel(); <-stopped }()
	// A tick now reports over several lines -- a heading, then a row per
	// artefact -- so gather everything it emits before asserting on it.
	read := func() string {
		t.Helper()
		var got []string
		select {
		case line := <-lines:
			got = append(got, line)
		case <-time.After(5 * time.Second):
			t.Fatal("progress tick produced no report")
			return ""
		}
		for {
			select {
			case line := <-lines:
				got = append(got, line)
			case <-time.After(50 * time.Millisecond):
				return strings.Join(got, "\n")
			}
		}
	}
	complete := startRunWork(ctx, "tagmap creator tags 1-100/250")
	select {
	case line := <-lines:
		t.Fatal("non-verbose dispatch emitted output before a tick", line)
	default:
	}
	ticks <- p.started.Add(time.Minute)
	line := read()
	if !strings.Contains(line, "1m0s elapsed") || !strings.Contains(line, "1 in flight") || !strings.Contains(line, "tagmap creator tags 1-100/250") {
		t.Fatal(line)
	}
	complete(nil)
	finish := startRunWork(ctx, "transcript creator/recording")
	updateGraphProgress(ctx, graphProgress{Total: 8, Cached: 2, Completed: 1, Running: 1, Pending: 2, Failed: 1, Blocked: 1})
	ticks <- p.started.Add(2 * time.Minute)
	line = read()
	for _, want := range []string{"2m0s elapsed", "transcript creator/recording", "8 total, 2 cached, 1 completed, 1 running, 2 pending, 1 failed, 1 blocked"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
	finish(errors.New("worker failed"))
	startRunWork(ctx, "adjudication request (3 tags)")
	ticks <- p.started.Add(3 * time.Minute)
	line = read()
	if !strings.Contains(line, "1 done, 1 failed") || !strings.Contains(line, "adjudication request (3 tags)") {
		t.Fatal(line)
	}
}

func TestProgressVerboseIsIndependentAndConcurrent(t *testing.T) {
	var out bytes.Buffer
	ctx, stop := startRunProgress(context.Background(), func(f string, args ...any) {
		fmt.Fprintf(&out, f+"\n", args...)
	}, true, true, false)
	defer stop()
	var workers sync.WaitGroup
	for i := 0; i < 30; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			complete := startRunWork(ctx, fmt.Sprintf("entry creator/%d", i))
			var err error
			switch i % 3 {
			case 1:
				err = errors.New("write failed")
			case 2:
				err = NothingToProduce{"empty transcript"}
			}
			complete(err)
			complete(err) // Deferred cleanup must not complete a task twice.
		}(i)
	}
	workers.Wait()
	output := out.String()
	for prefix, want := range map[string]int{"dispatch: entry": 30, "completed: entry": 10, "failed: entry": 10, "skipped: entry": 10, "progress:": 0} {
		if n := strings.Count(output, prefix); n != want {
			t.Errorf("%s: got %d, want %d", prefix, n, want)
		}
	}
	p := progressFrom(ctx)
	if len(p.active) != 0 || p.completed != 10 || p.failed != 10 || p.skipped != 10 {
		t.Fatal("incorrect concurrent progress counts")
	}
}

func TestProgressStopsOnReturnAndCancellation(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		_, stop := startRunProgress(ctx, func(string, ...any) { t.Error("unexpected heartbeat during short run") }, false, false, false)
		if cancelFirst {
			cancel()
		}
		stopped := make(chan struct{})
		go func() { stop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("reporter did not shut down")
		}
		cancel()
	}
}

func TestGraphProgressCountsFailuresAndBlockedWork(t *testing.T) {
	c := testConfig(t)
	e := NewEngine(c)
	var out bytes.Buffer
	ctx, stop := startRunProgress(context.Background(), func(f string, args ...any) {
		fmt.Fprintf(&out, f+"\n", args...)
	}, true, true, false)
	defer stop()
	source := &Source{Audio: filepath.Join(c.Root, "missing.mp3"), Title: "Missing", Author: "Creator", Data: Record{}}
	_, err := e.RunGraph(ctx, []Planned{{source, "missing", filepath.Join(c.Content, "creator", "missing.yaml")}}, RunOptions{Batch: 150})
	if err == nil {
		t.Fatal("missing media should fail")
	}
	p := progressFrom(ctx)
	g := p.graph
	if g.Running != 0 || g.Pending != 0 || g.Failed != 1 || g.Blocked != len(Graph)-1 || len(p.active) != 0 {
		t.Fatalf("incorrect graph progress: %+v", g)
	}
	if strings.Count(out.String(), "dispatch:") != 1 || !strings.Contains(out.String(), "failed: media creator/missing") {
		t.Fatal("blocked work should not be logged as dispatched", out.String())
	}
}

func TestRunProgressFlagsReachMaintenance(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		e, _ := runFixture(t)
		say, said := sayInto()
		e.Say = say
		flags := []string{"--no-progress", "--no-tagmaps", "--no-adjudicate"}
		if verbose {
			flags = append(flags, "--verbose")
		}
		if _, err := dispatchRun(t, e, flags...); err != nil {
			t.Fatal(err)
		}
		output := said()
		if strings.Contains(output, "progress:") {
			t.Fatal("--no-progress emitted a heartbeat", output)
		}
		for _, label := range []string{"check", "planning recordings", "recording pipeline", "voiceprint_verify", "duplicates", "orphans"} {
			for _, prefix := range []string{"dispatch: ", "completed: "} {
				if strings.Contains(output, prefix+label) != verbose {
					t.Errorf("verbose=%t, missing or unexpected %s%s: %s", verbose, prefix, label, output)
				}
			}
		}
	}
}

func TestLoadingReportsOnATerminalAndInALog(t *testing.T) {
	// On a terminal: one line, repainted in place, never scrolling.
	var screen bytes.Buffer
	step, finish := startLoading(&screen, true, func(string, ...any) { t.Error("a terminal should be repainted, not logged to") }, "loading source records")
	time.Sleep(loadingFloor + 20*time.Millisecond)
	for i := 0; i < 5; i++ {
		step(i, 5)
		time.Sleep(spinnerPaint)
	}
	painted := screen.String()
	if !strings.Contains(painted, "loading source records: ") || !strings.Contains(painted, "\r") {
		t.Fatalf("no spinner painted: %q", painted)
	}
	if strings.Contains(painted, "\n") {
		t.Fatalf("the spinner scrolled the terminal: %q", painted)
	}
	var said []string

	// Without a terminal: occasional lines, and nothing written to the stream.
	var stream bytes.Buffer
	step, finish = startLoading(&stream, false, func(f string, v ...any) { said = append(said, fmt.Sprintf(f, v...)) }, "loading source records")
	time.Sleep(loadingFloor + 20*time.Millisecond)
	step(1, 9824)
	finish(9824)
	if stream.Len() != 0 {
		t.Fatalf("escape codes reached a non-terminal: %q", stream.String())
	}
	if len(said) == 0 || !strings.Contains(said[len(said)-1], "9824") {
		t.Fatalf("no summary logged: %v", said)
	}
}

func TestAQuickLoadSaysNothing(t *testing.T) {
	var screen bytes.Buffer
	var said []string
	step, finish := startLoading(&screen, true, func(f string, v ...any) { said = append(said, fmt.Sprintf(f, v...)) }, "loading source records")
	step(1, 2)
	finish(2)
	if screen.Len() != 0 || len(said) != 0 {
		t.Fatalf("a fast phase should pass in silence: %q %v", screen.String(), said)
	}
}

func TestTheTerminalGetsALineAndAPathWhilePipesGetEverything(t *testing.T) {
	full := Record{
		"recordings": 517,
		"check":      map[string]any{"errors": []any{}, "warnings": []any{"a", "b", "c"}, "sources": 9824},
		"duplicates": map[string]any{"merges": []any{}, "different creators": 0},
		"adjudicate": map[string]any{"application": map[string]any{"tagged": 6}},
		"report":     "/cache/runs/run-x.json",
	}
	shown := forReading(full)

	// A finding becomes a line; the path to the whole of it stays.
	if s, ok := shown["check"].(string); !ok || !strings.Contains(s, "3 warnings") || !strings.Contains(s, "9824 sources") {
		t.Fatalf("check was not reduced to a line: %#v", shown["check"])
	}
	if s, ok := shown["duplicates"].(string); !ok || s != "nothing to report" {
		t.Fatalf("an empty finding should say so plainly: %#v", shown["duplicates"])
	}
	if shown["report"] != "/cache/runs/run-x.json" {
		t.Fatal("the path to the findings was dropped")
	}
	// What the run *did* is the result, not a finding about it, so it stays whole.
	if _, ok := shown["adjudicate"].(map[string]any); !ok {
		t.Fatalf("a phase that changed things was flattened: %#v", shown["adjudicate"])
	}
	if shown["recordings"] != 517 {
		t.Fatal("unrelated keys were disturbed")
	}

	// The report other things read must be untouched by how it was displayed.
	if _, ok := full["check"].(map[string]any); !ok {
		t.Fatal("trimming for a terminal mutated the machine-readable report")
	}

	// With nothing filed there is nothing to point at, so nothing is trimmed.
	plain := Record{"check": map[string]any{"warnings": []any{"a"}}}
	if _, ok := forReading(plain)["check"].(map[string]any); !ok {
		t.Fatal("a report with no filed findings should be left alone")
	}
}

// A pass that is still running used to print the word "running" and nothing
// else, so a heartbeat could not tell work in progress from work wedged. The
// work it dispatches is already tracked; it just was not attributed to the pass
// that asked for it.
func TestARunningPhaseReportsTheWorkItHasInFlight(t *testing.T) {
	lines := make(chan string, 16)
	ctx, stop := startRunProgress(context.Background(), func(f string, args ...any) {
		lines <- fmt.Sprintf(f, args...)
	}, true, false, false)
	defer stop()
	p := progressFrom(ctx)
	ticks := make(chan time.Time)
	watchCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() { defer close(stopped); p.watch(watchCtx, ticks) }()
	defer func() { cancel(); <-stopped }()

	updatePhaseProgress(ctx, []phaseTally{
		{Name: "pages", State: "running"},
		{Name: "backfill", State: "waiting"},
	})
	// Work dispatched by the pass, as its own goroutine would start it.
	inside := withPhase(ctx, "pages")
	defer startRunWork(inside, "author prompts 1-8/44")(nil)
	defer startRunWork(inside, "author prompts 9-16/44")(nil)
	// And work belonging to no pass, which must not be attributed to one.
	defer startRunWork(ctx, "unrelated")(nil)

	ticks <- time.Now()
	var got []string
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-lines:
			got = append(got, line)
			if strings.Contains(line, "pages") {
				report := line
				if !strings.Contains(report, "2 in flight") {
					t.Fatalf("a running pass did not say what it was doing: %q", report)
				}
				if !strings.Contains(report, "author prompts 1-8/44") {
					t.Fatalf("a running pass did not name its work: %q", report)
				}
				if strings.Contains(report, "unrelated") {
					t.Fatalf("work belonging to no pass was credited to one: %q", report)
				}
				return
			}
		case <-deadline:
			t.Fatalf("no row for the running pass in:\n%s", strings.Join(got, "\n"))
		}
	}
}
