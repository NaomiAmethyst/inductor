// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func BodyOfWork(docs []Document, author string) Record {
	tags, voices, audiences := Record{}, Record{}, Record{}
	titles, summaries := []string{}, []string{}
	count := 0
	for _, d := range docs {
		if str(d.Data["author"]) != author {
			continue
		}
		count++
		for _, t := range texts(d.Data["tags"]) {
			target, key := tags, t
			if strings.HasPrefix(t, "Voice: ") {
				target, key = voices, t[7:]
			} else if strings.HasPrefix(t, "Audience: ") {
				target, key = audiences, t[10:]
			} else if strings.HasPrefix(t, "CW:") {
				continue
			}
			target[key] = integer(target[key]) + 1
		}
		if truth(d.Data["title"]) {
			titles = append(titles, str(d.Data["title"]))
		}
		if s := strings.TrimSpace(str(d.Data["summary"])); s != "" {
			summaries = append(summaries, s)
		}
	}
	return Record{"count": count, "tags": tags, "voices": voices, "audiences": audiences, "titles": titles, "summaries": summaries}
}
func common(m Record, n int) []string {
	keys := sortedKeys(m)
	sort.SliceStable(keys, func(i, j int) bool { return integer(m[keys[i]]) > integer(m[keys[j]]) })
	return keys[:min(n, len(keys))]
}
func AuthorMaterial(author string, doc, work Record) string {
	summaries := texts(work["summaries"])
	if len(summaries) > 40 {
		sample := []string{}
		for i := 0; i < 40; i++ {
			sample = append(sample, summaries[i*len(summaries)/40])
		}
		summaries = sample
	}
	parts := []string{"Name: " + str(first(doc["name"], author)), fmt.Sprintf("They have %d recording(s) in the library.", integer(work["count"]))}
	if blurb := prose(str(doc["description"])); blurb != "" {
		parts = append(parts, "Their own description: "+truncate(blurb, 500))
	}
	if truth(work["tags"]) {
		parts = append(parts, "Recurring tags: "+strings.Join(common(record(work["tags"]), 20), ", "))
	}
	titles := texts(work["titles"])
	if len(titles) > 0 {
		parts = append(parts, "Titles: "+strings.Join(titles[:min(20, len(titles))], "; "))
	}
	if len(summaries) > 0 {
		for i := range summaries {
			summaries[i] = truncate(summaries[i], 180)
		}
		parts = append(parts, "What the recordings do:\n  - "+strings.Join(summaries, "\n  - "))
	}
	return strings.Join(parts, "\n")
}

// SamePrompt says whether two author prompts would draw the same picture. Only
// the three fields the renderer reads count: a prompt that differs anywhere in
// them is a different instruction, and the art made from the old one is stale.
func SamePrompt(was, now Record) bool {
	if len(was) == 0 {
		return false
	}
	for _, f := range []string{"tagged", "natural", "font"} {
		if strings.TrimSpace(str(was[f])) != strings.TrimSpace(str(now[f])) {
			return false
		}
	}
	return true
}

func SynopsisStale(doc Record, count int) bool {
	if strings.TrimSpace(str(doc["synopsis"])) == "" {
		return true
	}
	was := integer(record(doc["synopsis_from"])["items"])
	return was == 0 || math.Abs(float64(count-was)) >= float64(max(5, was/10))
}

var clauses = regexp.MustCompile(`[,;\n]`)

func EchoesTags(text string, work Record) bool {
	parts := []string{}
	for _, c := range clauses.Split(text, -1) {
		if c = strings.ToLower(strings.TrimSpace(c)); c != "" {
			parts = append(parts, c)
		}
	}
	if len(parts) == 0 {
		return true
	}
	theirs := map[string]bool{}
	for _, k := range []string{"tags", "audiences"} {
		for t := range record(work[k]) {
			theirs[strings.ToLower(t)] = true
		}
	}
	for v := range record(work["voices"]) {
		theirs[strings.ToLower(v)+" voice"] = true
	}
	hits := 0
	for _, c := range parts {
		if theirs[c] {
			hits++
		}
	}
	return hits >= max(3, len(parts)/4)
}
func UsableAuthorPrompt(p, work Record) bool {
	tagged, natural := str(p["tagged"]), str(p["natural"])
	return len([]rune(tagged)) >= 40 && len([]rune(natural)) >= 40 && !authorsEXPLICIT.MatchString(tagged) && !authorsEXPLICIT.MatchString(natural) && !EchoesTags(tagged, work)
}
func VoiceAndTags(work Record) string {
	lines := []string{}
	for _, v := range []struct {
		key, label string
		n          int
	}{{"voices", "Voice: ", 3}, {"audiences", "Speaks to: ", 3}, {"tags", "Most common tags: ", 16}} {
		if truth(work[v.key]) {
			lines = append(lines, v.label+strings.Join(common(record(work[v.key]), v.n), ", "))
		}
	}
	return strings.Join(lines, "\n")
}

type AuthorOptions struct {
	Only                              []string
	Model                             string
	Limit                             int
	Redo, KeepSynopsis, Render, Write bool
	Workers                           int
}

func (e *Engine) Authors(ctx context.Context, o AuthorOptions) (Record, error) {
	docs, err := Documents(e.Config.Content, "item", "author")
	if err != nil {
		return nil, err
	}
	type authorJob struct {
		Doc  Document
		Work Record
	}
	rows := []authorJob{}
	for _, d := range docs {
		if d.Kind != "author" {
			continue
		}
		who := str(first(d.Data["id"], filepath.Base(filepath.Dir(d.Path))))
		if len(o.Only) > 0 && !contains(o.Only, who) {
			continue
		}
		work := BodyOfWork(docs, who)
		if o.Redo || SynopsisStale(d.Data, integer(work["count"])) || !truth(d.Data["image"]) {
			rows = append(rows, authorJob{d, work})
		}
	}
	if o.Limit > 0 {
		rows = rows[:min(o.Limit, len(rows))]
	}
	report := Record{"asked": len(rows), "synopses": 0, "prompted": 0, "drawn": 0}
	for start := 0; start < len(rows); start += 8 {
		part := rows[start:min(start+8, len(rows))]
		synopses := Record{}
		if !o.KeepSynopsis {
			blocks := []string{}
			for i, j := range part {
				blocks = append(blocks, fmt.Sprintf("=== %d\n%s", i+1, AuthorMaterial(str(j.Doc.Data["id"]), j.Doc.Data, j.Work)))
			}
			complete := startRunWork(ctx, fmt.Sprintf("author synopses %d-%d/%d", start+1, start+len(part), len(rows)))
			synopses, err = e.API.ChatJSON(ctx, o.Model, messages(prompt("authors_synopsis_system"), prompt("authors_synopsis_shape")+"\n\nCREATORS:\n"+strings.Join(blocks, "\n\n")), TokenCeiling, .4)
			complete(err)
			if err != nil {
				return report, err
			}
		}
		for i, j := range part {
			if value, ok := synopses[fmt.Sprint(i+1)].(string); ok && len([]rune(value)) > 60 {
				j.Doc.Data["synopsis"] = strings.Join(pythonFields(value), " ")
				j.Doc.Data["synopsis_from"] = Record{"items": j.Work["count"], "model": o.Model}
				MarkGenerated(nested(j.Doc.Data, "provenance"), "synopsis")
				report["synopses"] = integer(report["synopses"]) + 1
			}
		}
		found := map[int]Record{}
		for turn := 0; turn < 2; turn++ {
			blocks := []string{}
			for i, j := range part {
				if found[i] != nil || !truth(j.Doc.Data["synopsis"]) {
					continue
				}
				blocks = append(blocks, fmt.Sprintf("=== %d\nName: %s\n%s\nWhat they make: %s", i+1, str(first(j.Doc.Data["name"], j.Doc.Data["id"])), VoiceAndTags(j.Work), str(j.Doc.Data["synopsis"])))
			}
			if len(blocks) == 0 {
				break
			}
			temp := .5
			if turn > 0 {
				temp = .2
			}
			complete := startRunWork(ctx, fmt.Sprintf("author prompts %d-%d/%d attempt %d", start+1, start+len(part), len(rows), turn+1))
			reply, err := e.API.ChatJSON(ctx, o.Model, messages(prompt("authors_system"), prompt("authors_shape")+"\n\nTYPEFACES to choose from:\n"+FontChoices()+"\n\nCREATORS:\n"+strings.Join(blocks, "\n\n")), TokenCeiling, temp)
			complete(err)
			if err != nil {
				return report, err
			}
			for i, j := range part {
				p := record(reply[fmt.Sprint(i+1)])
				if UsableAuthorPrompt(p, j.Work) {
					f := strings.ToLower(strings.TrimSpace(str(p["font"])))
					if _, ok := Fonts[f]; !ok {
						f = "heavy-sans"
					}
					found[i] = Record{"tagged": strings.TrimSpace(str(p["tagged"])), "natural": strings.TrimSpace(str(p["natural"])), "font": f}
				}
			}
		}
		for i, j := range part {
			if p := found[i]; p != nil {
				// The picture is made from these words, so words that have
				// changed leave the picture describing the creator this page
				// used to be about -- and provenance still calls the image ours.
				restated := !SamePrompt(record(j.Doc.Data["cover_prompts"]), p)
				j.Doc.Data["cover_prompts"] = p
				report["prompted"] = integer(report["prompted"]) + 1
				engine := e.Config.Enrich.CoverEngine
				text, _ := PromptFor(nil, engine, j.Doc.Data)
				// Two ways to know the picture is behind: the words we just
				// wrote differ from the words on the page, or the stamp says
				// the picture was drawn from something else again.
				stale := restated || ArtStale(record(j.Doc.Data["provenance"]), text, "", engine)
				if o.Render && (o.Redo || stale || !truth(j.Doc.Data["image"])) && o.Write {
					dest := filepath.Join(e.Config.Covers, str(j.Doc.Data["id"]), "_author.png")
					if o.Redo || stale || !exists(dest) {
						complete := startRunWork(ctx, "author artwork "+str(j.Doc.Data["id"]))
						err = e.Generate(ctx, text, dest, "")
						complete(err)
						if err != nil {
							e.Say("author art %s: %v", str(j.Doc.Data["id"]), err)
							if _, saveErr := SaveDocument(j.Doc.Path, j.Doc.Data); saveErr != nil {
								return report, saveErr
							}
							continue
						}
						if _, err = DrawNameplate(dest, str(first(j.Doc.Data["name"], j.Doc.Data["id"])), str(p["font"]), false); err != nil {
							return report, err
						}
						StampArt(nested(j.Doc.Data, "provenance"), text, "", engine)
					}
					j.Doc.Data["image"] = e.Config.Portable(dest, filepath.Dir(j.Doc.Path))
					MarkGenerated(nested(j.Doc.Data, "provenance"), "image")
					report["drawn"] = integer(report["drawn"]) + 1
				}
			}
			if o.Write {
				if _, err = SaveDocument(j.Doc.Path, j.Doc.Data); err != nil {
					return report, err
				}
			}
		}
		e.Say("creator pages: %d/%d", min(start+8, len(rows)), len(rows))
	}
	return report, nil
}
func (e *Engine) CoverPrompts(ctx context.Context, model string, workers, limit int, dry bool) (Record, error) {
	root := e.Config.Analysis()
	if !isDir(root) {
		return nil, fmt.Errorf("expected enrichments at %s", root)
	}
	files, _ := filepath.Glob(filepath.Join(root, "*.json"))
	found := map[string][]string{}
	for _, p := range files {
		r := readJSON(p)
		f := record(r["final"])
		tagged := strings.TrimSpace(str(f["thumbnail_prompt"]))
		if tagged != "" && strings.TrimSpace(str(f["thumbnail_prompt_natural"])) == "" {
			found[tagged] = append(found[tagged], p)
		}
	}
	promptsList := sortedKeys(found)
	if limit > 0 {
		promptsList = promptsList[:min(limit, len(promptsList))]
	}
	report := Record{"pending": len(promptsList), "translated": 0, "written": 0}
	if dry {
		return report, nil
	}
	chunks := [][]string{}
	for i := 0; i < len(promptsList); i += 20 {
		chunks = append(chunks, promptsList[i:min(i+20, len(promptsList))])
	}
	replies, errs := parallelMap(ctx, chunks, workers, func(ctx context.Context, part []string) (Record, error) {
		lines := []string{}
		for i, p := range part {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, p))
		}
		complete := startRunWork(ctx, fmt.Sprintf("cover prompt translation (%d prompts)", len(part)))
		reply, err := e.API.ChatJSON(ctx, model, messages(prompt("coverprompts_system"), prompt("coverprompts_shape")+"\n\nPROMPTS:\n"+strings.Join(lines, "\n")), TokenCeiling, .3)
		complete(err)
		return reply, err
	})
	for i, reply := range replies {
		if errs[i] != nil {
			e.Say("cover prompts: %v", errs[i])
			continue
		}
		for j, p := range chunks[i] {
			value, ok := reply[fmt.Sprint(j+1)].(string)
			if !ok || len([]rune(strings.TrimSpace(value))) <= 30 {
				continue
			}
			report["translated"] = integer(report["translated"]) + 1
			for _, path := range found[p] {
				r := readJSON(path)
				f := record(r["final"])
				if truth(f["thumbnail_prompt_natural"]) {
					continue
				}
				f["thumbnail_prompt_natural"] = strings.TrimSpace(value)
				if err := writeJSON(path, r); err != nil {
					return report, err
				}
				report["written"] = integer(report["written"]) + 1
			}
		}
	}
	report["missed"] = len(promptsList) - integer(report["translated"])
	if integer(report["missed"]) > 0 {
		return report, fmt.Errorf("%d cover prompt(s) could not be translated", integer(report["missed"]))
	}
	return report, nil
}
func (e *Engine) Artwork(ctx context.Context, author string, limit, workers int, redo, write bool) (Record, error) {
	docs, err := Documents(e.Config.Content, "item")
	if err != nil {
		return nil, err
	}
	rows := []Document{}
	for _, d := range docs {
		if author != "" && str(d.Data["author"]) != author {
			continue
		}
		if truth(d.Data["cover"]) && !contains(texts(record(d.Data["provenance"])["generated"]), "cover") {
			continue
		}
		p, _ := PromptFor(nil, e.Config.Enrich.CoverEngine, d.Data)
		if p == "" {
			continue
		}
		w, h := imageSize(e.Config.Resolved(str(d.Data["cover"]), filepath.Dir(d.Path)))
		// A cover with no width is one that could not be read: gone, unreadable,
		// or a link pointing at itself. The item says it has a cover and says
		// this toolchain drew it, so the shape is not the only thing that can be
		// wrong with it -- and the `w > 0` that used to guard this skipped
		// exactly the covers most in need of drawing. Thirty-eight of them sat
		// through every run of this command untouched, each one a warning in the
		// build and a missing picture on the site.
		negative := str(record(d.Data["cover_prompts"])["negative"])
		if redo || !truth(d.Data["cover"]) || w == 0 || w != e.Config.Enrich.CoverWidth || h != e.Config.Enrich.CoverHeight || ArtStale(record(d.Data["provenance"]), p, negative, e.Config.Enrich.CoverEngine) {
			rows = append(rows, d)
		}
	}
	if limit > 0 {
		rows = rows[:min(limit, len(rows))]
	}
	report := Record{"pending": len(rows), "drawn": 0, "failed": 0}
	if !write {
		return report, nil
	}
	_, errs := parallelMap(ctx, rows, workers, func(ctx context.Context, d Document) (changed bool, workErr error) {
		complete := startRunWork(ctx, "repair artwork "+str(d.Data["author"])+"/"+str(d.Data["id"]))
		defer func() { complete(workErr) }()
		engine := e.Config.Enrich.CoverEngine
		p, _ := PromptFor(nil, engine, d.Data)
		dest := e.Config.Resolved(str(d.Data["cover"]), filepath.Dir(d.Path))
		if !truth(d.Data["cover"]) {
			dest = filepath.Join(e.Config.Covers, str(d.Data["author"]), str(d.Data["id"])+".png")
		}
		negative := str(record(d.Data["cover_prompts"])["negative"])
		if err := e.Generate(ctx, p, dest, negative); err != nil {
			return false, err
		}
		ok, err := DrawNameplate(dest, str(d.Data["title"]), e.CoverFont(str(d.Data["author"])), true)
		if err != nil {
			return ok, err
		}
		// The picture changed even though its path did not, so the document has
		// to be written back for the stamp to survive the run.
		StampArt(nested(d.Data, "provenance"), p, negative, engine)
		MarkGenerated(nested(d.Data, "provenance"), "cover")
		d.Data["cover"] = e.Config.Portable(dest, filepath.Dir(d.Path))
		_, err = SaveDocument(d.Path, d.Data)
		return ok, err
	})
	for _, err := range errs {
		if err != nil {
			report["failed"] = integer(report["failed"]) + 1
			e.Say("artwork: %v", err)
		} else {
			report["drawn"] = integer(report["drawn"]) + 1
		}
	}
	if integer(report["failed"]) > 0 {
		return report, fmt.Errorf("%d artwork repair(s) failed", integer(report["failed"]))
	}
	return report, nil
}
