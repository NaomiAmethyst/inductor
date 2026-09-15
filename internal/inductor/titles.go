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
