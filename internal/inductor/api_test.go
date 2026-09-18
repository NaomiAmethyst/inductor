// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testAPI(t *testing.T, h http.HandlerFunc) *APIClient {
	t.Helper()
	t.Setenv("OPENROUTER_API_KEY", "fixture-key")
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	c := NewAPIClient(Config{})
	c.BaseURL = server.URL
	return c
}
func replyJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Error(err)
	}
}
func chatReply(text string) Record {
	return Record{"choices": []any{Record{"message": Record{"content": text}, "finish_reason": "stop"}}, "usage": Record{"prompt_tokens": 10, "completion_tokens": 5, "cost": .002}}
}
func TestChatJSONCorrectsInvalidOutputAndPreservesProtocol(t *testing.T) {
	var calls atomic.Int32
	c := testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Error("wrong request", r.URL, r.Header)
		}
		var body Record
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if str(body["model"]) != "model" || integer(body["max_tokens"]) != 128000 {
			t.Error(body)
		}
		if calls.Add(1) == 1 {
			replyJSON(t, w, chatReply("not JSON"))
			return
		}
		if len(array(body["messages"])) != 3 {
			t.Error("missing correction prompt", body)
		}
		replyJSON(t, w, chatReply("```json\n{\"summary\":\"done\"}\n```"))
	})
	got, err := c.ChatJSON(context.Background(), "model:batch", []any{Record{"role": "user", "content": "test"}}, TokenCeiling, .2)
	if err != nil || str(got["summary"]) != "done" || calls.Load() != 2 {
		t.Fatal(got, err, calls.Load())
	}
	if number(record(c.Usage()["model"])["cost"]) != .004 {
		t.Fatal(c.Usage())
	}
}
func TestChatStopsOnAuthenticationAndReasoningExhaustion(t *testing.T) {
	for _, which := range []string{"auth", "reasoning"} {
		t.Run(which, func(t *testing.T) {
			var calls atomic.Int32
			c := testAPI(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/models" {
					replyJSON(t, w, Record{"data": []any{}})
					return
				}
				calls.Add(1)
				if which == "auth" {
					http.Error(w, "no", 401)
					return
				}
				replyJSON(t, w, Record{"choices": []any{Record{"message": Record{"content": ""}, "finish_reason": "length"}}, "usage": Record{"completion_tokens": 1000, "completion_tokens_details": Record{"reasoning_tokens": 999}}})
			})
			_, _, err := c.Chat(context.Background(), "model", nil, 1000, .2)
			if err == nil || calls.Load() != 1 {
				t.Fatal("failure retried", err, calls.Load())
			}
		})
	}
}
func TestChatRetryHonorsCancellation(t *testing.T) {
	c := testAPI(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "retry", 429) })
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := c.Chat(ctx, "model", nil, 100, .2)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal("retry ignored cancellation", err)
	}
}
func TestBatchSubmissionResultsAndFailure(t *testing.T) {
	var submitted atomic.Int32
	c := testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/beta/batches":
			submitted.Add(1)
			var b Record
			if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
				t.Error(err)
			}
			rows := array(b["requests"])
			if str(b["model"]) != "model" || len(rows) != 1 || str(record(rows[0])["custom_id"]) != "one" {
				t.Error(b)
			}
			// A batch is pre-authorised against the account from this number,
			// so asking for the synchronous ceiling prices a $1.40 tray at $37
			// and stalls a run that has the money to pay for it.
			if got := integer(record(record(rows[0])["body"])["max_tokens"]); got != BatchCeiling {
				t.Errorf("batch asked for %d tokens, want BatchCeiling %d", got, BatchCeiling)
			}
			replyJSON(t, w, Record{"id": "batch-1"})
		case "/beta/batches/batch-1":
			replyJSON(t, w, Record{"status": "completed", "model": "model"})
		case "/beta/batches/batch-1/results":
			replyJSON(t, w, Record{"results": []any{Record{"custom_id": "one", "response": Record{"body": chatReply(`{"summary":"ready"}`)}}}})
		default:
			replyJSON(t, w, Record{"status": "failed"})
		}
	})
	if _, err := c.BatchSubmit(context.Background(), "model", []ReviewJob{{ID: "duplicate"}, {ID: "duplicate"}}); err == nil || submitted.Load() != 0 {
		t.Fatal("duplicate IDs submitted")
	}
	id, err := c.BatchSubmit(context.Background(), "model:batch", []ReviewJob{{ID: "one"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.BatchWait(context.Background(), id, time.Millisecond)
	if err != nil || !strings.Contains(got["one"], "ready") {
		t.Fatal(got, err)
	}
	if _, err = c.BatchWait(context.Background(), "failed", time.Millisecond); err == nil {
		t.Fatal("failed batch accepted")
	}
}
func TestReviewRecoveryUsesJournalWithoutResubmission(t *testing.T) {
	c := testConfig(t)
	engine := NewEngine(c)
	var submits atomic.Int32
	engine.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			submits.Add(1)
			t.Error("recovery resubmitted")
		}
		replyJSON(t, w, Record{"status": "completed", "results": []any{Record{"custom_id": "one", "body": chatReply(`{"summary":"recovered"}`)}}})
	})
	job := ReviewJob{ID: "one", Key: "text-key", Fingerprint: "audio-key", Sentences: []Sentence{{ID: "s1", Start: 0, End: 1, Text: "Evidence."}}}
	if err := writeJSON(filepath.Join(c.Cache, "batches", "old.json"), Record{"jobs": []ReviewJob{job}}); err != nil {
		t.Fatal(err)
	}
	done, err := engine.RunReviews(context.Background(), nil, 1, 1, time.Millisecond, []string{"old"}, false)
	if err != nil || done != 1 || submits.Load() != 0 {
		t.Fatal(done, err)
	}
	got := engine.Store.Get("text-key", "")
	if str(record(got["final"])["summary"]) != "recovered" || len(array(got["sentences"])) != 1 {
		t.Fatal(got)
	}
}
func TestReviewFailureIsReported(t *testing.T) {
	c := testConfig(t)
	c.Enrich.ReviewModel = "model"
	engine := NewEngine(c)
	engine.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "forbidden", 403) })
	done, err := engine.RunReviews(context.Background(), []ReviewJob{{ID: "one", Key: "key"}}, 1, 1, time.Millisecond, nil, false)
	if err == nil || done != 0 {
		t.Fatal(done, err)
	}
}

func TestCostEstimateUsesModelRates(t *testing.T) {
	c := testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			replyJSON(t, w, Record{"data": []any{Record{"id": "model", "pricing": Record{"prompt": "0.001", "completion": "0.002"}}}})
			return
		}
		response := chatReply("valid")
		delete(record(response["usage"]), "cost")
		replyJSON(t, w, response)
	})
	if _, _, err := c.Chat(context.Background(), "model", nil, 100, .2); err != nil {
		t.Fatal(err)
	}
	if number(record(c.Usage()["model"])["cost"]) != .02 {
		t.Fatal(c.Usage())
	}
}

func TestRunTakesUpASubmittedBatchNobodyRead(t *testing.T) {
	c := testConfig(t)
	c.Enrich.ReviewModel = "model:batch"
	engine := NewEngine(c)
	var submits atomic.Int32
	engine.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			submits.Add(1)
			t.Error("a batch already paid for was submitted again")
		}
		replyJSON(t, w, Record{"status": "completed", "results": []any{
			Record{"custom_id": "one", "body": chatReply(`{"summary":"already paid for"}`)}}})
	})
	job := ReviewJob{ID: "one", Key: "text-key", Fingerprint: "audio-key",
		Sentences: []Sentence{{ID: "s1", Start: 0, End: 1, Text: "Evidence."}}}
	// What a killed run leaves behind: the id, the work, and no results taken up.
	if err := writeJSON(filepath.Join(c.Cache, "batches", "stranded.json"),
		Record{"id": "stranded", "model": "model:batch", "jobs": []ReviewJob{job}, "status": "submitted"}); err != nil {
		t.Fatal(err)
	}
	// The same work comes round again, with nobody naming a batch to recover.
	done, err := engine.RunReviews(context.Background(), []ReviewJob{job}, 1, 1, time.Millisecond, nil, false)
	if err != nil || done != 1 || submits.Load() != 0 {
		t.Fatal(done, err, submits.Load())
	}
	if got := engine.Store.Get("text-key", ""); str(record(got["final"])["summary"]) != "already paid for" {
		t.Fatal("the stranded results were not taken up", got)
	}
	// And the journal now says so, or the next run would wait on it all over.
	if s := str(readJSON(filepath.Join(c.Cache, "batches", "stranded.json"))["status"]); s != "landed" {
		t.Fatalf("journal still reads %q", s)
	}
}

func TestAnUnreadableBatchDoesNotStopTheRun(t *testing.T) {
	c := testConfig(t)
	c.Enrich.ReviewModel = "model:batch"
	engine := NewEngine(c)
	engine.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			replyJSON(t, w, Record{"id": "fresh"})
			return
		}
		if strings.Contains(r.URL.Path, "gone") {
			http.Error(w, "no such batch", 404)
			return
		}
		replyJSON(t, w, Record{"status": "completed", "results": []any{
			Record{"custom_id": "one", "body": chatReply(`{"summary":"drawn fresh"}`)}}})
	})
	job := ReviewJob{ID: "one", Key: "text-key", Fingerprint: "audio-key"}
	if err := writeJSON(filepath.Join(c.Cache, "batches", "gone.json"),
		Record{"id": "gone", "model": "model:batch", "jobs": []ReviewJob{job}, "status": "submitted"}); err != nil {
		t.Fatal(err)
	}
	done, err := engine.RunReviews(context.Background(), []ReviewJob{job}, 1, 1, time.Millisecond, nil, false)
	if err != nil || done != 1 {
		t.Fatal("a stale journal stopped the run", done, err)
	}
	if s := str(readJSON(filepath.Join(c.Cache, "batches", "gone.json"))["status"]); s != "unreadable" {
		t.Fatalf("the dead journal still reads %q and will be retried forever", s)
	}
}

func TestAnUnfinishedBatchIsLeftForLaterNotWaitedOn(t *testing.T) {
	c := testConfig(t)
	c.Enrich.ReviewModel = "model:batch"
	engine := NewEngine(c)
	var polls atomic.Int32
	engine.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			replyJSON(t, w, Record{"id": "fresh"})
			return
		}
		if strings.Contains(r.URL.Path, "slow") {
			polls.Add(1)
			replyJSON(t, w, Record{"status": "in_progress"})
			return
		}
		replyJSON(t, w, Record{"status": "completed", "results": []any{
			Record{"custom_id": "one", "body": chatReply(`{"summary":"fresh"}`)}}})
	})
	job := ReviewJob{ID: "one", Key: "text-key", Fingerprint: "audio-key"}
	if err := writeJSON(filepath.Join(c.Cache, "batches", "slow.json"),
		Record{"id": "slow", "model": "model:batch", "jobs": []ReviewJob{job}, "status": "submitted"}); err != nil {
		t.Fatal(err)
	}
	done, err := engine.RunReviews(context.Background(), []ReviewJob{job}, 1, 1, time.Millisecond, nil, false)
	if err != nil || done != 1 {
		t.Fatal("an unfinished batch derailed the run", done, err)
	}
	// Asked once and moved on, rather than sitting on it for up to a day.
	if n := polls.Load(); n != 1 {
		t.Fatalf("polled an unfinished batch %d times; adoption should ask once", n)
	}
	// Still submitted, so a later run or an explicit --recover can claim it.
	if s := str(readJSON(filepath.Join(c.Cache, "batches", "slow.json"))["status"]); s != "submitted" {
		t.Fatalf("journal reads %q; an unfinished batch must stay claimable", s)
	}
}

func TestWaitUnfinishedSitsOnABatchUntilItLands(t *testing.T) {
	c := testConfig(t)
	c.Enrich.ReviewModel = "model:batch"
	engine := NewEngine(c)
	var polls atomic.Int32
	engine.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			t.Error("waiting on a paid batch should never submit it again")
			replyJSON(t, w, Record{"id": "fresh"})
			return
		}
		// Running the first time it is asked, finished the second.
		if polls.Add(1) == 1 {
			replyJSON(t, w, Record{"status": "in_progress"})
			return
		}
		replyJSON(t, w, Record{"status": "completed", "results": []any{
			Record{"custom_id": "one", "body": chatReply(`{"summary":"waited for"}`)}}})
	})
	job := ReviewJob{ID: "one", Key: "text-key", Fingerprint: "audio-key"}
	if err := writeJSON(filepath.Join(c.Cache, "batches", "slow.json"),
		Record{"id": "slow", "model": "model:batch", "jobs": []ReviewJob{job}, "status": "submitted"}); err != nil {
		t.Fatal(err)
	}
	done, err := engine.RunReviews(context.Background(), []ReviewJob{job}, 1, 1, time.Millisecond, nil, true)
	if err != nil || done != 1 {
		t.Fatal(done, err)
	}
	if got := engine.Store.Get("text-key", ""); str(record(got["final"])["summary"]) != "waited for" {
		t.Fatal("the batch was not waited for", got)
	}
	if s := str(readJSON(filepath.Join(c.Cache, "batches", "slow.json"))["status"]); s != "landed" {
		t.Fatalf("journal reads %q", s)
	}
}

// A model that will not describe the material answers with prose, an empty body
// or an error. All three reach the lander as a review that does not parse, and
// the recording is not at fault, so the work is offered to the next model
// rather than dropped. The entry has to say who actually answered.
func TestARefusedReviewIsOfferedToTheFallbackModel(t *testing.T) {
	c := testConfig(t)
	c.Enrich.ReviewModel = "declines:batch"
	c.Enrich.ReviewFallback = []string{"obliges:batch"}
	engine := NewEngine(c)
	var asked []string
	var mu sync.Mutex
	engine.API = testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			b, _ := io.ReadAll(r.Body)
			var body Record
			_ = decodeJSON(b, &body)
			model := str(body["model"])
			mu.Lock()
			asked = append(asked, model)
			mu.Unlock()
			if strings.HasPrefix(model, "obliges") {
				// The fallback is asked directly, not at batch latency.
				replyJSON(t, w, chatReply(`{"summary":"described"}`))
				return
			}
			replyJSON(t, w, Record{"id": "refused"})
			return
		}
		replyJSON(t, w, Record{"status": "completed", "results": []any{
			Record{"custom_id": "one", "body": chatReply("I cannot fulfill this request.")}}})
	})
	job := ReviewJob{ID: "one", Key: "text-key", Fingerprint: "audio-key"}
	done, err := engine.RunReviews(context.Background(), []ReviewJob{job}, 1, 1, time.Millisecond, nil, false)
	if err != nil || done != 1 {
		t.Fatal("a refusal was left unanswered instead of being offered on", done, err)
	}
	got := engine.Store.Peek("text-key", "audio-key")
	if str(record(got["final"])["summary"]) != "described" {
		t.Fatal("the fallback's answer was not stored", got)
	}
	if m := str(got["review_model"]); m != "obliges" {
		t.Fatalf("entry credits %q; the fallback wrote it", m)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 2 || !strings.HasPrefix(asked[0], "declines") || asked[1] != "obliges" {
		t.Fatalf("asked %v; expected the first model then the fallback, without its batch suffix", asked)
	}
}
