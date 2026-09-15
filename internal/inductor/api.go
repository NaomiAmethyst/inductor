// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const TokenCeiling = 128000

// BatchCeiling is what a *batch* asks for, which is a different question. On a
// synchronous call a generous cap costs nothing: it is a limit, not an order,
// and only the tokens actually produced are billed. A batch is pre-authorised
// against the account the moment it is submitted, so an over-generous cap on a
// batch is money that cannot be spent on anything else until it lands.
//
// Measured over 600 reviews: 17,580 prompt and 2,346 completion tokens each, at
// $0.0105 a review, or $1.40 for a tray of 150. Under TokenCeiling that same
// tray was priced at $37.09 -- fifty-five times the output it used -- and a run
// with $61 in hand stalled after four trays with the money still there but
// spoken for.
//
// Seven times the observed mean, which leaves room for a long reply and for the
// reasoning billed alongside it, and still asks for eight times less. Not
// smaller: a budget that truncates is how a tagmap lost three of eight turns to
// finish_reason=length, and that failure is silent where this one is loud.
const BatchCeiling = 16000
const defaultAPI = "https://openrouter.ai/api"

type APIClient struct {
	Config     Config
	HTTP       *http.Client
	BaseURL    string
	mu         sync.Mutex
	usage      Record
	prices     Record
	pricesOnce sync.Once
}

func NewAPIClient(c Config) *APIClient {
	return &APIClient{Config: c, HTTP: &http.Client{Timeout: 15 * time.Minute}, BaseURL: defaultAPI, usage: Record{}}
}

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("API HTTP %d: %s", e.Status, e.Message) }
func (a *APIClient) request(ctx context.Context, method, path string, payload any) (Record, error) {
	key, e := a.Config.APIKey()
	if e != nil {
		return nil, e
	}
	var body io.Reader
	if payload != nil {
		b, e := json.Marshal(payload)
		if e != nil {
			return nil, e
		}
		body = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.BaseURL, "/")+path, body)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Title", "Inductor")
	res, e := a.HTTP.Do(req)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, 128<<20))
	if e != nil {
		return nil, e
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, &APIError{res.StatusCode, string(b[:min(len(b), 800)])}
	}
	var r Record
	if len(b) == 0 {
		return Record{}, nil
	}
	if e = json.Unmarshal(b, &r); e != nil {
		return nil, e
	}
	if err := r["error"]; err != nil && !truth(r["choices"]) {
		return nil, &APIError{res.StatusCode, str(err)}
	}
	return r, nil
}
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (a *APIClient) Chat(ctx context.Context, model string, messages []any, maxTokens int, temperature float64) (string, Record, error) {
	model = strings.TrimSuffix(model, ":batch")
	body := Record{"model": model, "messages": messages, "max_tokens": maxTokens, "temperature": temperature}
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			if e := pause(ctx, time.Duration(min(60, 4<<(attempt-1)))*time.Second); e != nil {
				return "", nil, e
			}
		}
		r, e := a.request(ctx, "POST", "/v1/chat/completions", body)
		if e != nil {
			last = e
			if x, ok := e.(*APIError); ok && contains([]string{"400", "401", "403", "404"}, fmt.Sprint(x.Status)) {
				return "", nil, e
			}
			if ctx.Err() != nil {
				return "", nil, ctx.Err()
			}
			continue
		}
		choices := array(r["choices"])
		if len(choices) == 0 {
			last = fmt.Errorf("%s: no choices in response", model)
			continue
		}
		choice := record(choices[0])
		msg := record(choice["message"])
		a.recordUsage(model, a.pricedUsage(ctx, model, record(r["usage"])), 1)
		text := str(msg["content"])
		if strings.TrimSpace(text) == "" {
			u := record(r["usage"])
			thought := number(record(u["completion_tokens_details"])["reasoning_tokens"])
			spent := number(u["completion_tokens"])
			last = fmt.Errorf("%s: empty content, finish_reason=%s, completion_tokens=%g, reasoning_tokens=%g, refusal=%s", model, str(choice["finish_reason"]), spent, thought, str(msg["refusal"]))
			if str(choice["finish_reason"]) == "length" && thought > 0 && spent > 0 && thought >= spent-64 {
				return "", r, last
			}
			continue
		}
		return text, r, nil
	}
	return "", nil, fmt.Errorf("%s failed after 5 tries: %w", model, last)
}
func ExtractJSON(content string) (Record, error) {
	s := strings.TrimSpace(content)
	if strings.HasPrefix(s, "```") {
		_, s, _ = strings.Cut(s, "\n")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	var r Record
	if e := json.Unmarshal([]byte(s), &r); e == nil && r != nil {
		return r, nil
	}
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		if e := json.Unmarshal([]byte(s[start:end+1]), &r); e == nil && r != nil {
			return r, nil
		}
	}
	return nil, fmt.Errorf("response is not a JSON object")
}
func (a *APIClient) ChatJSON(ctx context.Context, model string, messages []any, maxTokens int, temp float64) (Record, error) {
	attemptMessages := append([]any{}, messages...)
	var last error
	for i := 0; i < 3; i++ {
		content, _, e := a.Chat(ctx, model, attemptMessages, maxTokens, temp)
		if e != nil {
			return nil, e
		}
		r, e := ExtractJSON(content)
		if e == nil {
			return r, nil
		}
		last = e
		attemptMessages = append(append([]any{}, messages...), Record{"role": "assistant", "content": truncate(content, 2000)}, Record{"role": "user", "content": "That was not valid JSON. Reply again with the same content as a single valid JSON object and nothing else: no prose, no code fence, no trailing comma, and every newline inside a string escaped as \\n."})
	}
	return nil, fmt.Errorf("%s returned unparseable JSON 3 times: %w", model, last)
}

// Prefer the provider's billed cost; estimate only when it omitted that field.
func (a *APIClient) pricedUsage(ctx context.Context, model string, usage Record) Record {
	if usage["cost"] != nil {
		return usage
	}
	a.pricesOnce.Do(func() {
		pricingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		result, err := a.request(pricingCtx, "GET", "/v1/models", nil)
		a.prices = Record{}
		if err != nil {
			return
		}
		for _, value := range array(result["data"]) {
			row := record(value)
			a.prices[str(row["id"])] = record(row["pricing"])
		}
	})
	rates := record(a.prices[model])
	if len(rates) == 0 {
		return usage
	}
	result := clone(usage)
	result["cost"] = number(usage["prompt_tokens"])*number(rates["prompt"]) + number(usage["completion_tokens"])*number(rates["completion"])
	return result
}
func (a *APIClient) recordUsage(model string, u Record, calls int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	row := nested(a.usage, model)
	row["calls"] = integer(row["calls"]) + calls
	row["prompt"] = number(row["prompt"]) + number(u["prompt_tokens"])
	row["completion"] = number(row["completion"]) + number(u["completion_tokens"])
	if u["cost"] != nil {
		row["cost"] = number(row["cost"]) + number(u["cost"])
	}
}
func (a *APIClient) Usage() Record { a.mu.Lock(); defer a.mu.Unlock(); return clone(a.usage) }

type ReviewJob struct {
	ID          string     `json:"id"`
	Messages    []any      `json:"messages"`
	Fingerprint string     `json:"fingerprint"`
	Key         string     `json:"key"`
	Sentences   []Sentence `json:"sentences"`
}

func (a *APIClient) BatchSubmit(ctx context.Context, model string, jobs []ReviewJob) (string, error) {
	requests := []any{}
	seen := map[string]bool{}
	for _, j := range jobs {
		if seen[j.ID] {
			return "", fmt.Errorf("duplicate batch custom_id %q", j.ID)
		}
		seen[j.ID] = true
		requests = append(requests, Record{"custom_id": j.ID, "body": Record{"max_tokens": BatchCeiling, "temperature": .2, "messages": j.Messages}})
	}
	r, e := a.request(ctx, "POST", "/beta/batches", Record{"endpoint": "/v1/chat/completions", "model": strings.TrimSuffix(model, ":batch"), "requests": requests})
	if e != nil {
		return "", e
	}
	if !truth(r["id"]) {
		return "", fmt.Errorf("batch submit returned no id")
	}
	return str(r["id"]), nil
}
func (a *APIClient) BatchStatus(ctx context.Context, id string) (Record, error) {
	return a.request(ctx, "GET", "/beta/batches/"+url.PathEscape(id), nil)
}
func (a *APIClient) BatchResults(ctx context.Context, id string, info Record) (map[string]string, error) {
	rows, ok := info["results"].([]any)
	if !ok {
		alt, e := a.request(ctx, "GET", "/beta/batches/"+url.PathEscape(id)+"/results", nil)
		if e != nil {
			return nil, e
		}
		rows = array(first(alt["results"], alt["data"]))
	}
	out := map[string]string{}
	for _, v := range rows {
		row := record(v)
		cid := str(row["custom_id"])
		body := record(first(record(row["response"])["body"], row["body"]))
		choices := array(body["choices"])
		if cid != "" && len(choices) > 0 {
			content := str(record(record(choices[0])["message"])["content"])
			if strings.TrimSpace(content) != "" {
				out[cid] = content
			}
		}
	}
	a.recordUsage(str(info["model"])+" (batch)", record(info["usage"]), len(rows))
	return out, nil
}
func (a *APIClient) BatchWait(ctx context.Context, id string, poll time.Duration) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 24*time.Hour)
	defer cancel()
	failures := 0
	for {
		info, e := a.BatchStatus(ctx, id)
		if e != nil || !truth(info["status"]) {
			failures++
			if failures >= 10 {
				return nil, fmt.Errorf("batch %s: status unreadable ten times: %v", id, e)
			}
		} else {
			failures = 0
			switch str(info["status"]) {
			case "completed", "ended", "finalized":
				return a.BatchResults(ctx, id, info)
			case "failed", "cancelled", "expired":
				return nil, fmt.Errorf("batch %s ended as %s", id, str(info["status"]))
			}
		}
		if e = pause(ctx, poll); e != nil {
			return nil, e
		}
	}
}
func truncate(s string, n int) string { r := []rune(s); return string(r[:min(len(r), n)]) }
