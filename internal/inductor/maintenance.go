// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var htmlTag = regexp.MustCompile(`<[^>]+>`)

func prose(s string) string {
	return strings.Join(pythonFields(htmlTag.ReplaceAllString(s, " ")), " ")
}
func InferGenerated(item, source Record) []string {
	p := record(item["provenance"])
	out := []string{}
	if truth(p["titled_by"]) || truth(p["original_title"]) {
		out = append(out, "title")
	}
	if truth(item["spoilers"]) {
		out = append(out, "spoilers")
	}
	if truth(record(p["enriched"])["tags_added"]) {
		out = append(out, "tags")
	}
	if truth(item["cover"]) && source != nil && !truth(source["cover"]) && !truth(source["image"]) {
		out = append(out, "cover")
	}
	published := truth(item["source_url"]) || truth(p["source_url"]) || truth(p["metadata_source"]) || truth(p["description_from_source"]) || truth(source["source_url"])
	for _, f := range []string{"summary", "description"} {
		mine, theirs := prose(str(item[f])), prose(str(source[f]))
		if mine == "" {
			continue
		}
		if (theirs == "" && !published) || (f == "summary" && theirs != "" && mine != theirs) {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return uniqueStrings(out)
}
func AttributeTree(c Config, redo, write bool) (Record, error) {
	sources, err := LoadSources(c.Sources)
	if err != nil {
		return nil, err
	}
	byKey := map[string]Record{}
	for _, s := range sources.Sources {
		byKey[s.Audio] = s.Data
		byKey[c.Portable(s.Audio, "")] = s.Data
	}
	docs, err := Documents(c.Content, "item", "author")
	if err != nil {
		return nil, err
	}
	out := Record{"items": 0, "authors": 0, "unmatched": 0, "fields": 0, "claimed": 0}
	for _, d := range docs {
		p := nested(d.Data, "provenance")
		// Claim first, and for every entry: an item that already knows what was
		// generated would otherwise skip the rest of this loop and never be
		// claimed at all. Only entries whose source record is still there can be
		// claimed -- that is the proof they are Inductor's. One whose source has
		// already gone cannot be told from a hand-written entry, so it is left
		// unclaimed and nothing will ever delete it.
		if d.Kind == "item" && !truth(p[ManagedBy]) && byKey[str(p["source_key"])] != nil {
			p[ManagedBy] = ManagedByInductor
			out["claimed"] = integer(out["claimed"]) + 1
			if write {
				if _, err = SaveDocument(d.Path, d.Data); err != nil {
					return out, err
				}
			}
		}
		if truth(p["generated"]) && !redo {
			continue
		}
		if redo {
			delete(p, "generated")
		}
		var found []string
		if d.Kind == "item" {
			source := byKey[str(p["source_key"])]
			if source == nil {
				out["unmatched"] = integer(out["unmatched"]) + 1
			}
			found = InferGenerated(d.Data, source)
		} else {
			if truth(d.Data["synopsis"]) {
				found = append(found, "synopsis")
			}
			if truth(d.Data["image"]) && truth(d.Data["cover_prompts"]) {
				found = append(found, "image")
			}
		}
		MarkGenerated(p, found...)
		if len(found) > 0 {
			key := d.Kind + "s"
			out[key] = integer(out[key]) + 1
			out["fields"] = integer(out["fields"]) + len(found)
		}
		if write {
			if _, err = SaveDocument(d.Path, d.Data); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}
func PortableTree(c Config, write bool) (Record, error) {
	docs, e := Documents(c.Content, "item", "author")
	if e != nil {
		return nil, e
	}
	count := 0
	fields := Record{}
	for _, d := range docs {
		before := clone(d.Data)
		c.PortablePaths(d.Data, filepath.Dir(d.Path))
		if equivalent(before, d.Data) {
			continue
		}
		count++
		for _, f := range []string{"audio", "video", "cover", "image"} {
			if !equivalent(before[f], d.Data[f]) {
				fields[f] = integer(fields[f]) + 1
			}
		}
		for _, f := range []string{"source_key", "merged_source_keys"} {
			if !equivalent(record(before["provenance"])[f], record(d.Data["provenance"])[f]) {
				fields[f] = integer(fields[f]) + 1
			}
		}
		if write {
			if _, e = SaveDocument(d.Path, d.Data); e != nil {
				return nil, e
			}
		}
	}
	return Record{"documents": count, "fields": fields}, nil
}
func Orphans(c Config, write bool) (Record, error) {
	files, e := yamlFiles(c.Content, false)
	if e != nil {
		return nil, e
	}
	r := Record{"items": 0, "authors": 0, "transcripts": 0, "unreadable": []string{}, "undeclared": []string{}, "empty_authors": []string{}, "stray_transcripts": []string{}, "unrecorded_items": []string{}, "orphaned_items": []string{}, "no_source_key": []string{}, "uningested": []string{}}
	docs := []Document{}
	ids, madeBy, claimed, known := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	sr, e := LoadSources(c.Sources)
	if e != nil {
		return nil, e
	}
	for _, s := range sr.Sources {
		known[s.Audio] = true
		known[c.Portable(s.Audio, "")] = true
	}
	for _, p := range files {
		d, e := readYAML(p)
		if e != nil {
			r["unreadable"] = append(texts(r["unreadable"]), p)
			continue
		}
		k := GuessKind(d, p)
		if k == "" {
			r["undeclared"] = append(texts(r["undeclared"]), p)
			continue
		}
		docs = append(docs, Document{p, d, k})
		if contains([]string{"item", "author", "transcript"}, k) {
			r[k+"s"] = integer(r[k+"s"]) + 1
		}
		if k == "item" {
			ids[str(d["id"])] = true
			madeBy[str(d["author"])] = true
			prov := record(d["provenance"])
			key := str(prov["source_key"])
			claimed[key] = true
			for _, v := range texts(prov["merged_source_keys"]) {
				claimed[v] = true
			}
			if key == "" {
				r["no_source_key"] = append(texts(r["no_source_key"]), p)
			} else if !known[key] {
				// The source that made this is gone. If Inductor made it, it
				// goes too, so that deleting a source entry deletes the entry
				// from the site. If it carries no claim, it is somebody else's
				// and only gets reported.
				if str(prov[ManagedBy]) == ManagedByInductor {
					r["orphaned_items"] = append(texts(r["orphaned_items"]), p)
				} else {
					r["unrecorded_items"] = append(texts(r["unrecorded_items"]), p)
				}
			}
		}
	}
	orphaned := map[string]bool{}
	for _, p := range texts(r["orphaned_items"]) {
		orphaned[p] = true
	}
	for _, d := range docs {
		remove := false
		if d.Kind == "item" && orphaned[d.Path] {
			// Inductor's own entry, whose source record is gone. Its transcript
			// goes with it: left behind it would only be reported as stray on
			// the next pass.
			remove = true
			if write {
				_ = os.Remove(strings.TrimSuffix(d.Path, filepath.Ext(d.Path)) + ".transcript.yaml")
			}
		}
		if d.Kind == "author" && !madeBy[str(first(d.Data["id"], filepath.Base(filepath.Dir(d.Path))))] {
			r["empty_authors"] = append(texts(r["empty_authors"]), d.Path)
			remove = true
		}
		if d.Kind == "transcript" && !ids[str(d.Data["item"])] {
			r["stray_transcripts"] = append(texts(r["stray_transcripts"]), d.Path)
			remove = true
		}
		if remove && write {
			if e = os.Remove(d.Path); e != nil {
				return nil, e
			}
			_ = os.Remove(filepath.Dir(d.Path))
		}
	}
	for _, k := range sortedKeys(known) {
		if k != "" && !claimed[k] && !strings.HasPrefix(k, "media/") && !strings.HasPrefix(k, "derived/") {
			r["uningested"] = append(texts(r["uningested"]), k)
		}
	}
	return r, nil
}
func ExportRecord(item Record, author string) Record {
	if !truth(item["audio"]) {
		return nil
	}
	p := record(item["provenance"])
	en := record(p["enriched"])
	outside := false
	for _, prefix := range []string{"site:", "pack:", "wayback:", "author's own notes", "id3"} {
		outside = outside || strings.HasPrefix(strings.ToLower(str(p["metadata_source"])), prefix)
	}
	r := Record{"apiVersion": "inductor/v1", "kind": "Source", "audio": str(item["audio"]), "title": strings.TrimSpace(str(item["title"])), "author": author}
	for _, f := range []string{"date", "series", "series_index", "variant", "source_url", "cover", "explicit", "categories", "duration"} {
		if item[f] != nil && str(item[f]) != "" {
			r[f] = item[f]
		}
	}
	added := texts(en["tags_added"])
	tags := []string{}
	for _, t := range texts(item["tags"]) {
		if !contains(added, t) {
			tags = append(tags, t)
		}
	}
	if len(tags) > 0 && (outside || len(en) == 0) {
		r["tags"] = tags
	}
	if truth(item["description"]) && (truth(p["description_from_source"]) || (outside && len(en) == 0)) {
		r["description"] = item["description"]
		if truth(item["summary"]) {
			r["summary"] = item["summary"]
		}
	}
	for _, f := range []string{"summary", "description", "cover"} {
		if contains(texts(p["generated"]), f) {
			delete(r, f)
		}
	}
	keep := Record{}
	for _, f := range []string{"metadata_source", "identified_by", "slug", "wordpress_id", "from_video", "unlinked_release"} {
		if v, ok := p[f]; ok {
			keep[f] = v
		}
	}
	if len(keep) > 0 {
		r["provenance"] = keep
	}
	return r
}
func ExportTree(c Config, content, out, snapshot, assets string) (Record, error) {
	docs, e := Documents(content, "item", "author")
	if e != nil {
		return nil, e
	}
	authors := map[string]string{}
	was := map[string][]string{}
	if snapshot != "" {
		old, e := Documents(snapshot, "item")
		if e != nil {
			return nil, e
		}
		for _, d := range old {
			fp := str(record(d.Data["provenance"])["fingerprint"])
			if fp != "" {
				was[fp] = texts(d.Data["tags"])
			}
		}
	}
	for _, d := range docs {
		if d.Kind == "author" {
			authors[str(d.Data["id"])] = str(first(d.Data["name"], d.Data["id"]))
		}
	}
	groups := map[string][]Record{}
	report := Record{"authors": 0, "records": 0, "skipped_no_audio": 0, "with_description": 0, "with_tags": 0, "unresolved_audio": 0, "tags_from_snapshot": len(was) > 0}
	for _, d := range docs {
		if d.Kind != "item" || !truth(d.Data["title"]) || !truth(d.Data["author"]) {
			continue
		}
		author := str(d.Data["author"])
		r := ExportRecord(d.Data, str(first(authors[author], author)))
		if r == nil {
			report["skipped_no_audio"] = integer(report["skipped_no_audio"]) + 1
			continue
		}
		if snapshot != "" {
			delete(r, "tags")
			if tags := was[str(record(d.Data["provenance"])["fingerprint"])]; len(tags) > 0 {
				r["tags"] = tags
			}
		}
		audio := expandHome(str(r["audio"]))
		if !filepath.IsAbs(audio) {
			for _, base := range []string{filepath.Dir(d.Path), assets, content, c.Root} {
				if base != "" && exists(filepath.Join(base, audio)) {
					audio = absolute(filepath.Join(base, audio))
					break
				}
			}
		}
		r["audio"] = audio
		groups[author] = append(groups[author], r)
		report["records"] = integer(report["records"]) + 1
		for _, f := range []string{"description", "tags"} {
			if truth(r[f]) {
				report["with_"+f] = integer(report["with_"+f]) + 1
			}
		}
		if !filepath.IsAbs(audio) {
			report["unresolved_audio"] = integer(report["unresolved_audio"]) + 1
		}
	}
	for _, author := range sortedKeys(groups) {
		var b bytes.Buffer
		for _, r := range groups[author] {
			part, e := marshalYAML(r, []string{"apiVersion", "kind", "audio", "title", "author"})
			if e != nil {
				return nil, e
			}
			b.WriteString("---\n")
			b.Write(part)
		}
		if _, e = AtomicWrite(filepath.Join(out, author+".yaml"), b.Bytes(), 0644); e != nil {
			return nil, e
		}
	}
	report["authors"] = len(groups)
	return report, nil
}

// Migrate stages every move before installing destinations, so a naming cycle
// cannot overwrite a document. Asset paths are rebased while IDs stay unchanged.
func Migrate(c Config, dry bool) (Record, error) {
	docs, e := Documents(c.Content, "item", "author", "transcript")
	if e != nil {
		return nil, e
	}
	taken := map[string]map[string]bool{}
	targets := map[string]string{}
	byID := map[string]string{}
	for _, d := range docs {
		if d.Kind == "transcript" {
			continue
		}
		author := str(first(d.Data["author"], "unknown"))
		target := ""
		if d.Kind == "author" {
			author = str(first(d.Data["id"], stemOf(d.Path)))
			target = filepath.Join(c.Content, author, "_author.yaml")
		} else {
			if taken[author] == nil {
				taken[author] = map[string]bool{}
			}
			stem, err := Unique(Slug(str(first(d.Data["title"], d.Data["id"], stemOf(d.Path)))), taken[author])
			if err != nil {
				return nil, err
			}
			target = filepath.Join(c.Content, author, stem+".yaml")
			byID[str(d.Data["id"])] = target
		}
		targets[d.Path] = target
	}
	for _, d := range docs {
		if d.Kind == "transcript" {
			if item := byID[str(d.Data["item"])]; item != "" {
				targets[d.Path] = strings.TrimSuffix(item, ".yaml") + ".transcript.yaml"
			}
		}
	}
	destSeen := map[string]bool{}
	for source, target := range targets {
		if destSeen[target] {
			return nil, fmt.Errorf("migration collision at %s", target)
		}
		destSeen[target] = true
		if source != target && exists(target) {
			if _, moving := targets[target]; !moving {
				return nil, fmt.Errorf("migration would overwrite %s", target)
			}
		}
	}
	moves := []any{}
	for i, d := range docs {
		target := targets[d.Path]
		if target != "" && target != d.Path {
			moves = append(moves, Record{"from": d.Path, "to": target, "original": fmt.Sprint(i) + ".original"})
		}
	}
	report := Record{"moved": len(moves), "moves": moves}
	if dry {
		return report, nil
	}
	stage, e := os.MkdirTemp(c.Content, ".migration-*")
	if e != nil {
		return nil, e
	}
	// Keep originals until every destination has been installed. A failed migration
	// leaves its backup directory and manifest available for recovery.
	report["backup"] = stage
	if e = writeJSON(filepath.Join(stage, "manifest.json"), report); e != nil {
		return report, e
	}
	for i, d := range docs {
		target := targets[d.Path]
		if target == "" || target == d.Path {
			continue
		}
		original, err := os.ReadFile(d.Path)
		if err != nil {
			return report, err
		}
		if _, err = AtomicWrite(filepath.Join(stage, fmt.Sprint(i)+".original"), original, 0644); err != nil {
			return report, err
		}
		for _, f := range []string{"audio", "cover", "image", "video"} {
			if truth(d.Data[f]) {
				d.Data[f] = c.Portable(c.Resolved(str(d.Data[f]), filepath.Dir(d.Path)), filepath.Dir(target))
			}
		}
		staged := filepath.Join(stage, fmt.Sprint(i)+".yaml")
		if _, err = AtomicWrite(staged, original, 0644); err != nil {
			return report, err
		}
		if _, err = SaveDocument(staged, d.Data); err != nil {
			return report, err
		}
	}
	for i, d := range docs {
		target := targets[d.Path]
		if target == "" || target == d.Path {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stage, fmt.Sprint(i)+".yaml"))
		if err != nil {
			return report, err
		}
		if _, err = AtomicWrite(target, data, 0644); err != nil {
			return report, fmt.Errorf("migration failed; originals retained in %s: %w", stage, err)
		}
	}
	for _, d := range docs {
		if target := targets[d.Path]; target != "" && target != d.Path && !destSeen[d.Path] {
			if e = os.Remove(d.Path); e != nil {
				return report, e
			}
		}
	}
	if e = os.RemoveAll(stage); e != nil {
		return report, e
	}
	delete(report, "backup")
	return report, nil
}
