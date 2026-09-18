// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

func LooksMachineMade(title string) bool {
	t := strings.TrimSpace(title)
	if t == "" || strings.Contains(t, " ") || !retitleMACHINE.MatchString(t) {
		return false
	}
	digits := 0
	for _, r := range t {
		if unicode.IsDigit(r) {
			digits++
		}
	}
	return digits >= 6 && float64(digits)/float64(len([]rune(t))) >= .35
}
func innerCaps(s string) int {
	n := 0
	for i, r := range []rune(s) {
		if i > 0 && r >= 'A' && r <= 'Z' {
			n++
		}
	}
	return n
}
func trailingNumber(s string) int {
	r := []rune(s)
	i := len(r)
	for i > 0 && r[i-1] >= '0' && r[i-1] <= '9' {
		i--
	}
	if i > 0 && i < len(r) && ((r[i-1] >= 'a' && r[i-1] <= 'z') || (r[i-1] >= 'A' && r[i-1] <= 'Z')) {
		return i
	}
	return -1
}
func Unrun(title string) string {
	t := strings.Join(pythonFields(title), " ")
	if t == "" || strings.Contains(t, " ") {
		return t
	}
	if strings.Contains(t, "_") {
		return strings.Join(pythonFields(strings.ReplaceAll(t, "_", " ")), " ")
	}
	caps := innerCaps(t)
	if LooksMachineMade(t) || caps < 1 || (caps < 2 && trailingNumber(t) < 0) {
		return t
	}
	r := []rune(t)
	if at := trailingNumber(t); at >= 0 {
		t = string(r[:at]) + " " + string(r[at:])
		r = []rune(t)
	}
	var b strings.Builder
	for i, ch := range r {
		if i > 0 && ch >= 'A' && ch <= 'Z' {
			prev := r[i-1]
			split := (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9')
			split = split || (prev >= 'A' && prev <= 'Z' && i+1 < len(r) && r[i+1] >= 'a' && r[i+1] <= 'z')
			if split {
				b.WriteByte(' ')
			}
		}
		b.WriteRune(ch)
	}
	return strings.Join(pythonFields(b.String()), " ")
}
func NotReallyTitle(t string) bool {
	t = strings.TrimSpace(t)
	if t == "" || strings.Contains(t, " ") {
		return false
	}
	if LooksMachineMade(t) {
		return true
	}
	if strings.Contains(t, "_") && len([]rune(t)) >= 8 {
		return true
	}
	if !retitleRUNTOGETHER.MatchString(t) {
		return false
	}
	if innerCaps(t) >= 2 {
		return true
	}
	r := []rune(t)
	for i := 1; i < len(r); i++ {
		if unicode.IsDigit(r[i]) && unicode.IsLetter(r[i-1]) {
			return true
		}
	}
	return false
}
func TidyCase(title string) string {
	words := pythonFields(title)
	small := pythonFields("a an and as at but by for from in into nor of on onto or over the to up with")
	for i, w := range words {
		if i > 0 && i < len(words)-1 && contains(small, strings.ToLower(w)) {
			words[i] = strings.ToLower(w)
		}
	}
	return strings.Join(words, " ")
}

var trailingLower = regexp.MustCompile(`(\s+[a-z][a-z'\-]*)+$`)

func TidyTitle(title string) (string, Record) {
	text := strings.Join(pythonFields(title), " ")
	notes := Record{}
	text = retitleDURATION.ReplaceAllString(text, "")
	if m := retitleSCRIPTBY.FindStringSubmatchIndex(text); m != nil {
		notes["script_by"] = text[m[2]:m[3]]
		text = strings.TrimSpace(text[:m[0]] + " " + text[m[1]:])
	}
	text = retitleAUDIENCE.ReplaceAllString(text, "")
	if strings.HasPrefix(text, "[") {
		stop := -1
		for i := 1; i < len(text); i++ {
			if text[i] == '[' || text[i] == ']' {
				stop = i
				break
			}
		}
		head, after := strings.TrimSpace(text[1:]), ""
		if stop >= 0 {
			head = strings.TrimSpace(text[1:stop])
			if text[stop] == ']' {
				after = text[stop+1:]
			} else {
				after = text[stop:]
			}
		}
		rest := strings.Join(pythonFields(retitleGROUP.ReplaceAllString(after, " ")), " ")
		if len([]rune(rest)) >= 12 {
			text = rest
		} else {
			text = head
		}
	} else if at := strings.Index(text, "["); at >= 0 {
		text = text[:at]
	}
	if strings.HasSuffix(text, "]") && !strings.Contains(text, "[") {
		text = trailingLower.ReplaceAllString(strings.TrimSpace(strings.TrimSuffix(text, "]")), "")
	}
	text = retitleFRAGMENT.ReplaceAllString(text, "")
	for _, pattern := range []*regexp.Regexp{retitleSITE, retitleENCODE, retitleEXTENSION} {
		for i := 0; i < 4; i++ {
			shorter := strings.Trim(pattern.ReplaceAllString(text, ""), " -–—:;,._")
			if len([]rune(shorter)) < 3 || shorter == text {
				break
			}
			if pattern == retitleENCODE {
				cut := strings.Trim(strings.Join(pythonFields(strings.ReplaceAll(pattern.FindString(text), "_", " ")), " "), " -–—:;,._")
				if cut != "" && !strings.Contains(strings.ToLower(str(notes["variant"])), strings.ToLower(cut)) {
					notes["variant"] = strings.TrimSpace(str(notes["variant"]) + " " + cut)
				}
			}
			text = shorter
		}
	}
	return strings.Trim(strings.Join(pythonFields(Unrun(text)), " "), " -–—:;,"), notes
}
func (e *Engine) Retitle(ctx context.Context, author, model string, limit int, tidy, write bool) (Record, error) {
	docs, err := Documents(e.Config.Content, "item")
	if err != nil {
		return nil, err
	}
	rows := []Document{}
	for _, d := range docs {
		if author != "" && str(d.Data["author"]) != author {
			continue
		}
		if tidy {
			was := str(d.Data["title"])
			now, notes := TidyTitle(was)
			if now == was || len([]rune(now)) < 3 {
				continue
			}
			p := nested(d.Data, "provenance")
			if _, ok := p["original_title"]; !ok {
				p["original_title"] = was
			}
			if truth(notes["variant"]) && !truth(d.Data["variant"]) {
				d.Data["variant"] = notes["variant"]
			}
			delete(notes, "variant")
			merge(p, notes)
			d.Data["title"] = now
			rows = append(rows, d)
		} else if NotReallyTitle(str(d.Data["title"])) && strings.TrimSpace(str(d.Data["summary"])) != "" {
			rows = append(rows, d)
		}
	}
	if limit > 0 {
		rows = rows[:min(limit, len(rows))]
	}
	report := Record{"named": 0, "written": 0, "changed": len(rows)}
	if tidy {
		if write {
			for _, d := range rows {
				if _, err = SaveDocument(d.Path, d.Data); err != nil {
					return nil, err
				}
			}
			report["written"] = len(rows)
		}
		return report, nil
	}
	for start := 0; start < len(rows); start += 20 {
		part := rows[start:min(start+20, len(rows))]
		lines := []string{}
		for i, d := range part {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, truncate(strings.TrimSpace(str(d.Data["summary"])), 400)))
		}
		reply, err := e.API.ChatJSON(ctx, model, messages(prompt("retitle_system"), prompt("retitle_shape")+"\n\nRECORDINGS:\n"+strings.Join(lines, "\n")), TokenCeiling, .4)
		if err != nil {
			e.Say("retitle: %v", err)
			continue
		}
		for i, d := range part {
			value, ok := reply[fmt.Sprint(i+1)].(string)
			if !ok {
				continue
			}
			title := strings.TrimRight(strings.Trim(strings.Join(pythonFields(value), " "), "\""), ".")
			if len([]rune(title)) < 3 || len([]rune(title)) > 90 || NotReallyTitle(title) {
				continue
			}
			p := nested(d.Data, "provenance")
			if _, ok := p["original_title"]; !ok {
				p["original_title"] = d.Data["title"]
			}
			p["titled_by"] = model
			MarkGenerated(p, "title")
			d.Data["title"] = TidyCase(title)
			report["named"] = integer(report["named"]) + 1
			if write {
				if _, err = SaveDocument(d.Path, d.Data); err != nil {
					return nil, err
				}
				report["written"] = integer(report["written"]) + 1
			}
		}
	}
	report["missed"] = len(rows) - integer(report["named"])
	return report, nil
}
func stripQualifier(text string, p *regexp.Regexp) string {
	for i := 0; i < 4; i++ {
		s := strings.TrimSpace(p.ReplaceAllString(text, ""))
		if s == "" || s == text {
			break
		}
		text = s
	}
	return text
}
func pythonTitle(s string) string {
	var b strings.Builder
	previous := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			if previous {
				b.WriteRune(unicode.ToLower(r))
			} else {
				b.WriteRune(unicode.ToUpper(r))
			}
			previous = true
		} else {
			b.WriteRune(r)
			previous = false
		}
	}
	return b.String()
}
func FoldProposal(tag string, known map[string]bool) (string, string) {
	text := strings.TrimSpace(tag)
	if text == "" {
		return "", "empty"
	}
	if foldNOTATAG.MatchString(text) {
		return "", "not a tag: describes the upload, not the recording"
	}
	if foldSOFTENERS.MatchString(text) {
		bare := stripQualifier(text, foldSOFTENERS)
		for _, c := range []string{bare, pythonTitle(bare)} {
			if known[c] {
				return c, "a qualifier that only softens"
			}
		}
	}
	if foldMENTIONS.MatchString(text) {
		bare := stripQualifier(text, foldMENTIONS)
		for _, c := range []string{"CW: " + bare, "CW: " + pythonTitle(bare)} {
			if known[c] {
				return c, "mentioned rather than featured"
			}
		}
	}
	return text, ""
}
func FoldTree(c Config, write bool) (Record, error) {
	reg, err := LoadRegistry(c.RegistryPath())
	if err != nil {
		return nil, err
	}
	docs, err := Documents(c.Content, "item")
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for n := range reg.Meanings {
		known[n] = true
	}
	for _, d := range docs {
		for _, t := range texts(d.Data["tags"]) {
			known[t] = true
		}
	}
	report := Record{"items": 0, "removed": 0, "folded": 0}
	for _, d := range docs {
		before := texts(d.Data["tags"])
		after := []string{}
		changed := false
		for _, tag := range before {
			to, _ := FoldProposal(tag, known)
			if to != tag {
				changed = true
				if to == "" {
					report["removed"] = integer(report["removed"]) + 1
				} else {
					report["folded"] = integer(report["folded"]) + 1
				}
			}
			if to != "" && !contains(after, to) {
				after = append(after, to)
			}
		}
		if !changed {
			continue
		}
		d.Data["tags"] = after
		en := record(record(d.Data["provenance"])["enriched"])
		if truth(en["tags_added"]) {
			added := []string{}
			for _, t := range texts(en["tags_added"]) {
				to, _ := FoldProposal(t, known)
				if to != "" {
					added = append(added, to)
				}
			}
			en["tags_added"] = added
		}
		report["items"] = integer(report["items"]) + 1
		if write {
			if _, err = SaveDocument(d.Path, d.Data); err != nil {
				return nil, err
			}
		}
	}
	return report, nil
}

// TitleRulings is a reviewed verdict per recording, applied by `retitle --apply`.
// A review of a whole library is somebody's judgement over thousands of titles;
// it arrives as a file rather than a flag because it cannot be expressed as a
// rule, and it is kept because it cost something to obtain.
//
//	apiVersion: inductor/v1
//	kind: TitleRulings
//	rulings:
//	  - id: creator-some-recording          # the item's declared id
//	    title: Some Recording               # optional: the corrected title
//	    series: {name: Some Series, index: 2}
//	    variant: {of: Base Title, distinguisher: Long Cut}
//
// A variant sets `title` to its base as well as `variant` to the distinguisher,
// because that is what makes a group cohere: siblings share a title and differ
// by variant. Ids and filenames never change -- ids are stable by convention,
// and media and covers are named after the file stem, so renaming either would
// strand them.
func (e *Engine) ApplyTitleRulings(path, author string, write, noSeries, noVariants bool) (Record, error) {
	doc, err := readYAML(path)
	if err != nil {
		return nil, err
	}
	if k := str(doc["kind"]); k != "TitleRulings" {
		return nil, fmt.Errorf("%s is a %q, not a TitleRulings", path, k)
	}
	rulings := map[string]Record{}
	for _, row := range array(doc["rulings"]) {
		r := record(row)
		if id := str(r["id"]); id != "" {
			rulings[id] = r
		}
	}
	docs, err := Documents(e.Config.Content, "item")
	if err != nil {
		return nil, err
	}
	report := Record{"rulings": len(rulings), "retitled": 0, "series": 0, "variant": 0, "written": 0, "unmatched": 0}
	seen := map[string]bool{}
	for _, d := range docs {
		id := str(d.Data["id"])
		r, ok := rulings[id]
		if !ok {
			continue
		}
		// Seen means the library holds it, which is what unmatched reports on.
		// Narrowing to one creator must not make the other 3,000 rulings look
		// like they name recordings nobody has.
		seen[id] = true
		if author != "" && str(d.Data["author"]) != author {
			continue
		}
		title := str(r["title"])
		if v := record(r["variant"]); len(v) > 0 && !noVariants {
			if base := str(v["of"]); base != "" {
				title = base
			}
			if dist := str(v["distinguisher"]); dist != "" {
				d.Data["variant"] = dist
				report["variant"] = integer(report["variant"]) + 1
			}
		}
		if s := record(r["series"]); len(s) > 0 && !noSeries {
			if name := str(s["name"]); name != "" {
				d.Data["series"] = name
				if s["index"] != nil {
					d.Data["series_index"] = s["index"]
				}
				report["series"] = integer(report["series"]) + 1
			}
		}
		if title != "" && title != str(d.Data["title"]) {
			p := nested(d.Data, "provenance")
			if _, ok := p["original_title"]; !ok {
				p["original_title"] = d.Data["title"]
			}
			// Not MarkGenerated: these are the creator's own titles with an
			// export label or a filename artefact taken off, the same as
			// --tidy. Claiming a person's words were machine-written is the
			// one error the generated list exists to prevent.
			p["retitled_by"] = "title rulings"
			d.Data["title"] = title
			report["retitled"] = integer(report["retitled"]) + 1
		}
		if write {
			changed, err := SaveDocument(d.Path, d.Data)
			if err != nil {
				return report, err
			}
			if changed {
				report["written"] = integer(report["written"]) + 1
			}
		}
	}
	for id := range rulings {
		if !seen[id] {
			report["unmatched"] = integer(report["unmatched"]) + 1
		}
	}
	return report, nil
}
