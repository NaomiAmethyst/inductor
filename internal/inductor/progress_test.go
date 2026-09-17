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
		var out bytes.Buffer
		e.Say = func(f string, args ...any) { fmt.Fprintf(&out, f+"\n", args...) }
		flags := []string{"--no-progress", "--no-tagmaps", "--no-adjudicate"}
		if verbose {
			flags = append(flags, "--verbose")
		}
		if _, err := dispatchRun(t, e, flags...); err != nil {
			t.Fatal(err)
		}
		output := out.String()
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
