// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAPartialFailureOrdersWithoutBlocking(t *testing.T) {
	var ran []string
	var mu sync.Mutex
	note := func(name string) func(context.Context) (Record, error) {
		return func(context.Context) (Record, error) {
			mu.Lock()
			ran = append(ran, name)
			mu.Unlock()
			return Record{}, nil
		}
	}
	phases := []phase{
		// Finished with failures: the recordings pass reports errors while
		// still leaving thousands of usable artefacts behind it.
		{name: "recordings", lane: "graph", when: true,
			run: func(ctx context.Context) (Record, error) {
				return Record{"entry": 2066}, errors.New("66 artefacts failed")
			}},
		// Reads what the recordings left, whatever else went wrong.
		{name: "audit", lane: "local", after: []string{"recordings"}, when: true, run: note("audit")},
		// Needs a ruling to have actually happened.
		{name: "ruling", lane: "api", when: true,
			run: func(ctx context.Context) (Record, error) { return nil, errors.New("no key") }},
		{name: "applies-ruling", lane: "items", needs: []string{"ruling"}, when: true, run: note("applies-ruling")},
	}
	e := NewEngine(testConfig(t))
	counts := Record{}
	failures := e.RunPhases(context.Background(), phases, counts)
	if len(failures) != 2 {
		t.Fatalf("both failures should be reported: %v", failures)
	}
	mu.Lock()
	defer mu.Unlock()
	if !contains(ran, "audit") {
		t.Fatal("a pass that only needed the recordings to finish was blocked by their partial failure")
	}
	if contains(ran, "applies-ruling") {
		t.Fatal("a pass that needed a ruling ran without one")
	}
	if integer(record(counts["recordings"])["entry"]) != 2066 {
		t.Fatal("the work a failed pass completed was not kept", counts["recordings"])
	}
}

func TestPhasesRunTogetherWaitForWhatTheyNeedAndStopAtAFailure(t *testing.T) {
	var mu sync.Mutex
	var order []string
	var live, mostAtOnce atomic.Int32
	note := func(name string) func(context.Context) (Record, error) {
		return func(context.Context) (Record, error) {
			n := live.Add(1)
			for {
				top := mostAtOnce.Load()
				if n <= top || mostAtOnce.CompareAndSwap(top, n) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			live.Add(-1)
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return Record{"did": name}, nil
		}
	}
	phases := []phase{
		{name: "root", lane: "local", when: true, run: note("root")},
		// Three with nothing between them: they should overlap.
		{name: "a", lane: "local", needs: []string{"root"}, when: true, run: note("a")},
		{name: "b", lane: "local", needs: []string{"root"}, when: true, run: note("b")},
		{name: "c", lane: "local", needs: []string{"root"}, when: true, run: note("c")},
		// One that fails, and one that needs it.
		{name: "breaks", lane: "api", needs: []string{"root"}, when: true,
			run: func(ctx context.Context) (Record, error) { return nil, errors.New("no") }},
		{name: "after-breaks", lane: "local", needs: []string{"breaks"}, when: true, run: note("after-breaks")},
		// One that depends on something nobody asked for: not a reason to wait.
		{name: "skipped", lane: "local", when: false, run: note("skipped")},
		{name: "after-skipped", lane: "local", needs: []string{"skipped"}, when: true, run: note("after-skipped")},
	}
	e := NewEngine(testConfig(t))
	counts := Record{}
	failures := e.RunPhases(context.Background(), phases, counts)

	if len(failures) != 1 {
		t.Fatalf("expected the one failure, got %v", failures)
	}
	if mostAtOnce.Load() < 2 {
		t.Fatalf("phases with nothing between them ran one at a time (%d at once)", mostAtOnce.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	// root must precede what needs it. It need not be first overall: a phase
	// whose only dependency was never enabled is ready from the start.
	at := func(name string) int {
		for i, n := range order {
			if n == name {
				return i
			}
		}
		return -1
	}
	for _, after := range []string{"a", "b", "c"} {
		if at("root") < 0 || at(after) < at("root") {
			t.Fatalf("%s did not wait for what it needed: %v", after, order)
		}
	}
	if contains(order, "after-breaks") {
		t.Fatal("a phase ran although what it needed had failed")
	}
	if blocked := texts(record(counts["after-breaks"])["blocked_by"]); !contains(blocked, "breaks") {
		t.Fatalf("a blocked phase did not say what held it: %v", counts["after-breaks"])
	}
	if !contains(order, "after-skipped") {
		t.Fatal("a phase waited on something nobody asked to run")
	}
	if str(record(counts["a"])["did"]) != "a" {
		t.Fatalf("a phase's report was not kept: %v", counts["a"])
	}
}
