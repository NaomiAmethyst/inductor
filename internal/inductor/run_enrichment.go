// SPDX-License-Identifier: GPL-3.0-only
package inductor

import "path/filepath"

// Check artifacts before applying --limit, so complete entries cannot consume
// the limit and indefinitely hide later recordings with missing work.
func (e *Engine) runJobOutstanding(j Planned, a Arguments) bool {
	for _, artifact := range Graph {
		if artifact.Name == "cover" && (a.Bool("no_covers") || !e.Config.Enrich.Covers) {
			continue
		}
		if !e.ArtifactExists(artifact.Name, j, nil, false) {
			return true
		}
	}
	fp, err := e.fingerprint(j)
	if err != nil {
		return true
	}
	transcript := e.transcript(fp)
	final := record(e.Store.Peek(TranscriptKey(str(transcript["text"])), fp)["final"])
	item := optionalYAML(j.Path)
	for _, field := range []string{"summary", "description", "spoilers"} {
		if !truth(item[field]) && truth(final[field]) {
			return true
		}
	}
	reg, err := LoadRegistry(e.Config.RegistryPath())
	if err != nil {
		return true
	}
	mapping := LoadMapping(e.Config, j.Source.AuthorID())
	for _, tag := range append(texts(j.Source.Data["tags"]), texts(final["tags"])...) {
		canonical, _ := reg.Resolve(tag, mapping)
		if canonical != "" && !contains(texts(item["tags"]), canonical) {
			return true
		}
	}
	return false
}

// Include reviewed proposals that were never emitted onto an entry. Preserve
// item and tagmap evidence too; the standalone --from-reviews mode replaces it.
func pendingRunTags(c Config, reviews bool) (map[string]Record, error) {
	rows, err := PendingTags(c, false)
	if err != nil || !reviews {
		return rows, err
	}
	wanted, err := askedFor(c.Analysis(), true)
	if err != nil {
		return nil, err
	}
	reg, err := LoadRegistry(c.RegistryPath())
	if err != nil {
		return nil, err
	}
	docs, err := Documents(c.Content, "item")
	if err != nil {
		return nil, err
	}
	titles := map[string]string{}
	for _, d := range docs {
		titles[str(record(d.Data["provenance"])["fingerprint"])] = str(first(d.Data["title"], d.Data["id"]))
	}
	for _, key := range sortedKeys(wanted) {
		request := wanted[key]
		name := sortedKeys(request.Spellings)[0]
		if reg.Has(name) {
			continue
		}
		for _, existing := range sortedKeys(rows) {
			if wordFold(existing) == key {
				name = existing
				break
			}
		}
		row := rows[name]
		if row == nil {
			row = Record{}
			rows[name] = row
		}
		// Reviews and emitted proposals often refer to the same recordings.
		row["count"] = max(integer(row["count"]), len(request.Recordings))
		items := texts(row["items"])
		for _, fp := range sortedKeys(request.Recordings) {
			if title := titles[fp]; title != "" && !contains(items, title) && len(items) < 6 {
				items = append(items, title)
			}
		}
		row["items"] = items
		reasons := uniqueStrings(append(texts(row["reasons"]), request.Reasons...))
		row["reasons"] = reasons[:min(4, len(reasons))]
	}
	return rows, nil
}

// CoverPrompts repairs stored reviews. Copy missing prompt fields onto entries
// too, so Artwork can use them even when the entry is outside source selection.
func (e *Engine) syncRunCoverPrompts(dry bool) (Record, error) {
	docs, err := Documents(e.Config.Content, "item")
	if err != nil {
		return nil, err
	}
	report := Record{"changed": 0}
	for _, d := range docs {
		prov := record(d.Data["provenance"])
		if truth(d.Data["cover"]) && !contains(texts(prov["generated"]), "cover") {
			continue
		}
		fp := str(prov["fingerprint"])
		key := str(record(prov["enriched"])["transcript"])
		if key == "" {
			if transcript := e.transcript(fp); transcript != nil {
				key = TranscriptKey(str(transcript["text"]))
			}
		}
		// Read without AnalysisStore.Get's legacy-cache migration: this is
		// also used by --dry-run and must never write while inspecting.
		cached := Record{}
		if key != "" {
			cached = readJSON(filepath.Join(e.Config.Analysis(), key+".json"))
		}
		if len(cached) == 0 && fp != "" {
			cached = readJSON(filepath.Join(e.Config.Analysis(), fp+".json"))
		}
		final := record(cached["final"])
		kept := record(d.Data["cover_prompts"])
		// A different tagged prompt may be a deliberate edit. Its natural
		// counterpart must not be copied from an unrelated cached prompt.
		if truth(kept["tagged"]) && str(kept["tagged"]) != str(final["thumbnail_prompt"]) {
			continue
		}
		changed := false
		for _, pair := range [][2]string{{"tagged", "thumbnail_prompt"}, {"natural", "thumbnail_prompt_natural"}, {"negative", "thumbnail_negative"}} {
			if !truth(kept[pair[0]]) && truth(final[pair[1]]) {
				kept[pair[0]] = final[pair[1]]
				changed = true
			}
		}
		if changed {
			report["changed"] = integer(report["changed"]) + 1
			d.Data["cover_prompts"] = kept
			if !dry {
				if _, err := SaveDocument(d.Path, d.Data); err != nil {
					return report, err
				}
			}
		}
	}
	return report, nil
}
