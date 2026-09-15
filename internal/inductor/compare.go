// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"golang.org/x/text/unicode/norm"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var compareNonWord = regexp.MustCompile(`[^a-z0-9 ]+`)
var digitsPattern = regexp.MustCompile(`\d+`)

func squash(s string) string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(s) {
		if r < 128 {
			b.WriteRune(r)
		}
	}
	return strings.Join(pythonFields(compareNonWord.ReplaceAllString(strings.ToLower(b.String()), " ")), " ")
}

// sequenceRatio uses the same recursive longest-matching-block definition as
// difflib.SequenceMatcher, including its popular-character optimization.
func sequenceRatio(a, b string) float64 {
	aa, bb := []rune(a), []rune(b)
	if len(aa)+len(bb) == 0 {
		return 1
	}
	positions := map[rune][]int{}
	for j, r := range bb {
		positions[r] = append(positions[r], j)
	}
	if len(bb) >= 200 {
		for r, js := range positions {
			if len(js) > len(bb)/100+1 {
				delete(positions, r)
			}
		}
	}
	var count func(int, int, int, int) int
	count = func(alo, ahi, blo, bhi int) int {
		besti, bestj, bestsize := alo, blo, 0
		j2len := map[int]int{}
		for i := alo; i < ahi; i++ {
			next := map[int]int{}
			for _, j := range positions[aa[i]] {
				if j < blo {
					continue
				}
				if j >= bhi {
					break
				}
				k := j2len[j-1] + 1
				next[j] = k
				if k > bestsize {
					besti, bestj, bestsize = i-k+1, j-k+1, k
				}
			}
			j2len = next
		}
		for besti > alo && bestj > blo && aa[besti-1] == bb[bestj-1] {
			besti--
			bestj--
			bestsize++
		}
		for besti+bestsize < ahi && bestj+bestsize < bhi && aa[besti+bestsize] == bb[bestj+bestsize] {
			bestsize++
		}
		if bestsize == 0 {
			return 0
		}
		n := bestsize
		if alo < besti && blo < bestj {
			n += count(alo, besti, blo, bestj)
		}
		if besti+bestsize < ahi && bestj+bestsize < bhi {
			n += count(besti+bestsize, ahi, bestj+bestsize, bhi)
		}
		return n
	}
	return 2 * float64(count(0, len(aa), 0, len(bb))) / float64(len(aa)+len(bb))
}
func ScoreReview(final Record, sents []Sentence, reg *Registry) Record {
	parts := []string{}
	byNorm := []string{}
	for _, s := range sents {
		parts = append(parts, s.Text)
		byNorm = append(byNorm, squash(s.Text))
	}
	haystack := squash(strings.Join(parts, " "))
	exact, fuzzy, missing := 0, 0, 0
	errors := []float64{}
	severities := Record{}
	for _, v := range array(final["spoilers"]) {
		s := record(v)
		severity := str(first(s["severity"], "?"))
		severities[severity] = integer(severities[severity]) + 1
		quote := squash(str(s["quote"]))
		if len(quote) < 12 {
			continue
		}
		located := -1
		if strings.Contains(haystack, quote) {
			exact++
			for i, text := range byNorm {
				if strings.Contains(text, quote) || strings.Contains(quote, text) {
					located = i
					break
				}
			}
		} else {
			best := .75
			bestText := ""
			for i, text := range byNorm {
				score := sequenceRatio(text, quote)
				if score > best || (score == best && text >= bestText) {
					best, bestText, located = score, text, i
				}
			}
			if located >= 0 {
				fuzzy++
			} else {
				missing++
			}
		}
		if located >= 0 && truth(s["timestamp"]) {
			parts := digitsPattern.FindAllString(str(s["timestamp"]), 3)
			if len(parts) > 0 {
				seconds := 0
				for _, p := range parts {
					seconds = seconds*60 + integer(p)
				}
				errors = append(errors, math.Abs(float64(seconds)-sents[located].Start))
			}
		}
	}
	tags := texts(final["tags"])
	inv := 0
	for _, t := range tags {
		if _, ok := reg.Meanings[t]; ok {
			inv++
		}
	}
	within := 0
	for _, n := range errors {
		if n <= 30 {
			within++
		}
	}
	sort.Float64s(errors)
	var median any
	if len(errors) > 0 {
		median = errors[len(errors)/2]
	}
	v := record(final["verification"])
	return Record{"spoilers": len(array(final["spoilers"])), "quotes_exact": exact, "quotes_fuzzy": fuzzy, "quotes_missing": missing, "ts_median_error": median, "ts_within_30s": within, "ts_measured": len(errors), "tags": len(tags), "tags_in_vocab": inv, "severities": severities, "claims_checked": v["claims_checked"], "claims_dropped": v["claims_dropped"], "summary_len": len([]rune(str(final["summary"]))), "description_len": len([]rune(str(final["description"]))), "has_thumbnail": strings.TrimSpace(str(final["thumbnail_prompt"])) != ""}
}

type CompareOptions struct {
	Items, Seed, Workers int
	Poll                 time.Duration
	Models, Solo         []string
	SampleFile           string
}

func (e *Engine) Compare(ctx context.Context, o CompareOptions) (Record, error) {
	docs, err := Documents(e.Config.Content, "item")
	if err != nil {
		return nil, err
	}
	reg, err := LoadRegistry(e.Config.RegistryPath())
	if err != nil {
		return nil, err
	}
	pool := []Document{}
	for _, d := range docs {
		fp := str(record(d.Data["provenance"])["fingerprint"])
		if len(array(e.transcript(fp)["segments"])) >= 8 {
			pool = append(pool, d)
		}
	}
	sort.Slice(pool, func(i, j int) bool { return str(pool[i].Data["id"]) < str(pool[j].Data["id"]) })
	chosen := []Document{}
	if o.SampleFile != "" && exists(o.SampleFile) {
		wanted, err := readLines(o.SampleFile)
		if err != nil {
			return nil, err
		}
		for _, id := range wanted {
			for _, d := range pool {
				if str(d.Data["id"]) == id || stemOf(d.Path) == id {
					chosen = append(chosen, d)
					break
				}
			}
		}
	} else {
		newPythonRandom(int64(o.Seed)).shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
		counts := map[string]int{}
		for _, d := range pool {
			a := str(d.Data["author"])
			if counts[a] >= 2 {
				continue
			}
			counts[a]++
			chosen = append(chosen, d)
			if len(chosen) >= o.Items {
				break
			}
		}
		if o.SampleFile != "" {
			ids := []string{}
			for _, d := range chosen {
				ids = append(ids, str(d.Data["id"]))
			}
			if _, err = AtomicWrite(o.SampleFile, []byte(strings.Join(ids, "\n")+"\n"), 0644); err != nil {
				return nil, err
			}
		}
	}
	scores := Record{}
	if len(chosen) == 0 {
		return scores, nil
	}
	type contextRow struct {
		FP             string
		Sents          []Sentence
		Meta, Analysis Record
	}
	contexts := map[string]contextRow{}
	for _, d := range chosen {
		fp := str(record(d.Data["provenance"])["fingerprint"])
		p := e.transcript(fp)
		s := Sentences(p)
		meta := Record{"title": d.Data["title"], "author_name": d.Data["author"], "duration": first(d.Data["duration"], p["duration"]), "series": d.Data["series"], "existing_description": str(d.Data["description"])}
		key := TranscriptKey(str(p["text"]))
		r := e.Store.Get(key, fp)
		a := record(r["analysis"])
		if len(a) == 0 {
			a, err = e.API.ChatJSON(ctx, e.Config.Enrich.AnalysisModel, AnalysisMessages(s, meta, reg), TokenCeiling, .15)
			if err != nil {
				return nil, err
			}
			a = PruneCitations(a, s)
			if err = e.Store.Put(key, Record{"analysis": a}, fp, e.Config.Enrich.AnalysisModel); err != nil {
				return nil, err
			}
		}
		contexts[str(d.Data["id"])] = contextRow{fp, s, meta, a}
	}
	type arm struct {
		Model string
		Solo  bool
	}
	arms := []arm{}
	for _, m := range o.Models {
		arms = append(arms, arm{m, false})
	}
	for _, m := range o.Solo {
		arms = append(arms, arm{m, true})
	}
	out := filepath.Join(e.Config.State(), "model-comparison")
	for _, a := range arms {
		label := a.Model
		if a.Solo {
			label += " [solo]"
		}
		jobs := []ReviewJob{}
		for _, id := range sortedKeys(contexts) {
			c := contexts[id]
			msgs := ReviewMessages(c.Analysis, c.Sents, c.Meta, reg)
			if a.Solo {
				msgs = SoloMessages(c.Sents, c.Meta, reg)
			}
			jobs = append(jobs, ReviewJob{ID: id, Messages: msgs})
		}
		results := map[string]string{}
		if strings.HasSuffix(a.Model, ":batch") {
			id, err := e.API.BatchSubmit(ctx, a.Model, jobs)
			if err != nil {
				return scores, err
			}
			e.Say("comparison batch: %s", id)
			if err = writeJSON(filepath.Join(e.Config.Cache, "batches", id+".json"), Record{"id": id, "model": a.Model, "jobs": jobs, "comparison": true}); err != nil {
				return scores, err
			}
			results, err = e.API.BatchWait(ctx, id, o.Poll)
			if err != nil {
				return scores, err
			}
		} else {
			texts, errs := parallelMap(ctx, jobs, o.Workers, func(ctx context.Context, j ReviewJob) (string, error) {
				s, _, err := e.API.Chat(ctx, a.Model, j.Messages, TokenCeiling, .2)
				return s, err
			})
			for i, s := range texts {
				if errs[i] != nil {
					e.Say("comparison %s: %v", jobs[i].ID, errs[i])
					continue
				}
				results[jobs[i].ID] = s
			}
		}
		rows := []any{}
		for _, id := range sortedKeys(results) {
			final, err := ExtractJSON(results[id])
			if err != nil {
				continue
			}
			c, ok := contexts[id]
			if !ok {
				continue
			}
			r := ScoreReview(final, c.Sents, reg)
			r["id"] = id
			rows = append(rows, r)
			name := strings.ReplaceAll(strings.ReplaceAll(label, "/", "_"), " ", "") + "__" + id + ".json"
			if err = writeJSON(filepath.Join(out, name), final); err != nil {
				return scores, err
			}
		}
		scores[label] = rows
		e.Say("%s: %d/%d returned", label, len(rows), len(jobs))
	}
	return scores, writeJSON(filepath.Join(out, "scores.json"), scores)
}
