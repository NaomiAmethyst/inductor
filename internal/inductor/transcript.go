// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Sentence struct {
	ID    string  `json:"id" yaml:"id"`
	Start float64 `json:"start" yaml:"start"`
	End   float64 `json:"end" yaml:"end"`
	Text  string  `json:"text" yaml:"text"`
}

func rounded(n float64, d int) float64 { p := math.Pow10(d); return math.RoundToEven(n*p) / p }
func SplitSentences(text string) []string {
	r := []rune(text)
	parts := []string{}
	start := 0
	for i := 0; i < len(r); i++ {
		if !strings.ContainsRune(".!?", r[i]) {
			continue
		}
		j := i + 1
		for j < len(r) && strings.ContainsRune("\"')]", r[j]) {
			j++
		}
		k := j
		for k < len(r) && unicode.IsSpace(r[k]) {
			k++
		}
		if k == j {
			continue
		}
		l := k
		for l < len(r) && strings.ContainsRune("\"'([", r[l]) {
			l++
		}
		ellipsis := i >= 2 && r[i-1] == '.' && r[i-2] == '.' && j == i+1
		capital := l < len(r) && ((r[l] >= 'A' && r[l] <= 'Z') || (r[l] >= '0' && r[l] <= '9'))
		if ellipsis || capital {
			if s := strings.TrimSpace(string(r[start : i+1])); s != "" {
				parts = append(parts, s)
			}
			start = k
			i = k - 1
		}
	}
	if s := strings.TrimSpace(string(r[start:])); s != "" {
		parts = append(parts, s)
	}
	out := []string{}
	for _, p := range parts {
		if utf8.RuneCountInString(p) <= 320 {
			out = append(out, p)
			continue
		}
		buf := []string{}
		n := 0
		for _, w := range pythonFields(p) {
			buf = append(buf, w)
			n += utf8.RuneCountInString(w) + 1
			if n > 240 {
				out = append(out, strings.Join(buf, " "))
				buf = nil
				n = 0
			}
		}
		if len(buf) > 0 {
			out = append(out, strings.Join(buf, " "))
		}
	}
	return out
}
func Sentences(payload Record) []Sentence {
	out := []Sentence{}
	for _, v := range array(payload["segments"]) {
		s := record(v)
		text := strings.TrimSpace(str(s["text"]))
		if text == "" {
			continue
		}
		start := number(s["start"])
		end := number(first(s["end"], start))
		span := math.Max(end-start, .01)
		pieces := SplitSentences(text)
		total := 0
		for _, p := range pieces {
			total += utf8.RuneCountInString(p)
		}
		offset := 0
		for _, p := range pieces {
			a := start + span*float64(offset)/float64(total)
			offset += utf8.RuneCountInString(p)
			b := start + span*float64(offset)/float64(total)
			out = append(out, Sentence{"", rounded(a, 2), rounded(b, 2), p})
		}
	}
	width := max(3, len(fmt.Sprint(len(out))))
	for i := range out {
		out[i].ID = fmt.Sprintf("s%0*d", width, i+1)
	}
	return out
}
func Timecode(seconds float64) string {
	s := max(0, int(seconds))
	return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
}
func Numbered(s []Sentence) string {
	lines := []string{}
	for _, v := range s {
		lines = append(lines, "["+v.ID+"] "+v.Text)
	}
	return strings.Join(lines, "\n")
}
func Delivery(p Record) Record {
	segments := array(p["segments"])
	if len(segments) < 3 {
		return Record{}
	}
	spoken := 0.
	words := 0
	gaps := []float64{}
	last := -1.
	for _, v := range segments {
		s := record(v)
		if s["start"] == nil || s["end"] == nil {
			continue
		}
		a, b := number(s["start"]), number(s["end"])
		if b <= a {
			continue
		}
		spoken += b - a
		words += len(pythonFields(str(s["text"])))
		if last >= 0 && a > last {
			gaps = append(gaps, a-last)
		}
		last = b
	}
	if spoken <= 0 || words < 20 {
		return Record{}
	}
	duration := number(first(p["duration"], last, spoken))
	r := Record{"words_per_minute": rounded(float64(words)*60/spoken, 1), "speaking_share": rounded(math.Min(spoken/duration, 1), 3)}
	if len(gaps) > 0 {
		sort.Float64s(gaps)
		r["median_pause_s"] = rounded(gaps[len(gaps)/2], 2)
		r["longest_pause_s"] = rounded(gaps[len(gaps)-1], 1)
	}
	return r
}

var sentenceID = regexp.MustCompile(`^s[0-9]+$`)

func PruneCitations(r Record, sents []Sentence) Record {
	valid := map[string]bool{}
	for _, s := range sents {
		valid[s.ID] = true
	}
	dropped := []string{}
	var clean func(any) any
	clean = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			o := Record{}
			for k, v := range x {
				o[k] = clean(v)
			}
			return o
		case []any:
			o := []any{}
			for _, v := range x {
				if s, ok := v.(string); ok && sentenceID.MatchString(s) && !valid[s] {
					dropped = append(dropped, s)
					continue
				}
				o = append(o, clean(v))
			}
			return o
		case string:
			if sentenceID.MatchString(x) && !valid[x] {
				dropped = append(dropped, x)
				return ""
			}
		}
		return v
	}
	out := record(clean(r))
	if len(dropped) > 0 {
		ids := uniqueStrings(dropped)
		sort.Strings(ids)
		out["_warnings"] = append(array(out["_warnings"]), fmt.Sprintf("%d cited sentence ids were not in the transcript: %s", len(dropped), strings.Join(ids[:min(20, len(ids))], ", ")))
	}
	return out
}

var quoteMarker = regexp.MustCompile(`\(?\b[0-9]{1,2}[:.][0-9]{2}([:.][0-9]{2})?\)?`)

func CleanQuote(s string) string {
	return strings.Join(pythonFields(quoteMarker.ReplaceAllString(s, " ")), " ")
}
func Block(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"), "\t", "    ")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRightFunc(lines[i], unicode.IsSpace)
	}
	return strings.TrimSpace(strings.Join(lines, "\n")) + "\n"
}
func SpoilersFrom(final Record) []any {
	out := []any{}
	for _, v := range array(final["spoilers"]) {
		f := record(v)
		r := Record{"severity": first(f["severity"], "medium"), "disclosure": first(f["disclosure"], "undisclosed"), "confidence": first(f["confidence"], "medium")}
		if truth(f["timestamp"]) {
			r["timestamp"] = str(f["timestamp"])
		}
		for _, k := range []string{"trigger", "effect", "quote", "note"} {
			if truth(f[k]) {
				s := str(f[k])
				if k == "quote" {
					s = CleanQuote(s)
				}
				r[k] = Block(s)
			}
		}
		out = append(out, r)
	}
	return out
}
