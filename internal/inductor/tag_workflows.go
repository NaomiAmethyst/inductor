// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func RulingEntry(tag, verdict, why, into string, proposals int, model, source string) Record {
	r := Record{"apiVersion": "inductor/v1", "kind": "TagRuling", "when": time.Now().Format("2006-01-02"), "tag": tag, "verdict": verdict}
	for k, v := range map[string]string{"why": why, "into": into, "model": model, "source": source} {
		if v != "" {
			r[k] = v
		}
	}
	if proposals != 0 {
		r["proposals"] = proposals
	}
	return r
}
func AppendLedger(path string, rows []Record) (int, error) {
	var b bytes.Buffer
	count := 0
	for _, r := range rows {
		if !truth(r["tag"]) || !truth(r["verdict"]) {
			continue
		}
		data, e := marshalYAML(r, []string{"apiVersion", "kind", "when", "tag", "verdict", "into", "why", "proposals", "model", "source"})
		if e != nil {
			return 0, e
		}
		b.WriteString("---\n")
		b.Write(data)
		count++
	}
	if count == 0 {
		return 0, nil
	}
	if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
		return 0, e
	}
	// Windows needs read access to lock a handle opened for append-only writes.
	f, e := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
	if e != nil {
		return 0, e
	}
	defer f.Close()
	if e = lockFile(f); e != nil {
		return 0, e
	}
	defer unlockFile(f)
	if _, e = f.Write(b.Bytes()); e != nil {
		return 0, e
	}
	return count, f.Sync()
}
func ReadLedger(path string) ([]Record, error) {
	f, e := os.Open(path)
	if os.IsNotExist(e) {
		return []Record{}, nil
	}
	if e != nil {
		return nil, e
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	out := []Record{}
	for {
		var r Record
		e = dec.Decode(&r)
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		if truth(r["tag"]) {
			out = append(out, r)
		}
	}
	return out, nil
}
func Standing(path string) (map[string]Record, error) {
	rows, e := ReadLedger(path)
	if e != nil {
		return nil, e
	}
	out := map[string]Record{}
	for _, r := range rows {
		out[str(r["tag"])] = r
	}
	return out, nil
}
func wordFold(s string) string { return strings.ToLower(strings.Join(pythonFields(s), " ")) }
func Refused(path string, known []string) (map[string]Record, error) {
	standing, e := Standing(path)
	if e != nil {
		return nil, e
	}
	folded := map[string]bool{}
	for _, s := range known {
		folded[wordFold(s)] = true
	}
	out := map[string]Record{}
	for t, r := range standing {
		if contains([]string{"decline", "omit", "reject"}, str(r["verdict"])) && !folded[wordFold(t)] {
			out[t] = r
		}
	}
	return out, nil
}
func WorthRevisiting(path string, counts map[string]int, known []string) ([]Record, error) {
	refused, e := Refused(path, known)
	if e != nil {
		return nil, e
	}
	out := []Record{}
	for t, r := range refused {
		now, then := counts[t], integer(r["proposals"])
		if now < 4 || (then > 0 && now < then*2) {
			continue
		}
		out = append(out, Record{"tag": t, "was": then, "now": now, "verdict": r["verdict"], "why": first(r["why"], ""), "when": first(r["when"], "")})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return integer(out[i]["now"])-integer(out[i]["was"]) > integer(out[j]["now"])-integer(out[j]["was"])
	})
	return out, nil
}

type TagRequest struct {
	Spellings, Recordings map[string]bool
	Reasons               []string
}

func AskedFor(root string) (map[string]*TagRequest, error) {
	return askedFor(root, false)
}

func askedFor(root string, reviewedOnly bool) (map[string]*TagRequest, error) {
	if !isDir(root) {
		if reviewedOnly && !exists(root) {
			return map[string]*TagRequest{}, nil
		}
		return nil, fmt.Errorf("expected stored enrichments at %s", root)
	}
	files, e := filepath.Glob(filepath.Join(root, "*.json"))
	if e != nil {
		return nil, e
	}
	out := map[string]*TagRequest{}
	for _, p := range files {
		doc := readJSON(p)
		prints := texts(doc["audio"])
		if s, ok := doc["audio"].(string); ok {
			prints = []string{s}
		}
		if len(prints) == 0 {
			continue
		}
		wanted := []string{}
		reasons := map[string]string{}
		final := record(doc["final"])
		if len(final) > 0 {
			for _, v := range array(final["new_tags"]) {
				r := record(v)
				if truth(r["tag"]) && contains([]string{"keep", "accept", "approve", "new"}, wordFold(str(r["verdict"]))) {
					wanted = append(wanted, str(r["tag"]))
					reasons[str(r["tag"])] = str(r["why"])
				}
			}
		} else if !reviewedOnly {
			for _, v := range array(record(record(doc["analysis"])["tags"])["proposed"]) {
				name, ok := v.(string)
				if !ok {
					r := record(v)
					name = str(first(r["tag"], r["name"]))
				}
				if name != "" {
					wanted = append(wanted, name)
				}
			}
		}
		for _, name := range wanted {
			key := wordFold(name)
			if out[key] == nil {
				out[key] = &TagRequest{Spellings: map[string]bool{}, Recordings: map[string]bool{}}
			}
			out[key].Spellings[name] = true
			if why := reasons[name]; why != "" && !contains(out[key].Reasons, why) && len(out[key].Reasons) < 4 {
				out[key].Reasons = append(out[key].Reasons, why)
			}
			for _, fp := range prints {
				out[key].Recordings[fp] = true
			}
		}
	}
	return out, nil
}
func Backfill(c Config, tags []string, write bool) (Record, error) {
	return backfill(c, tags, write, false)
}

func backfill(c Config, tags []string, write, reviewedOnly bool) (Record, error) {
	reg, e := LoadRegistry(c.RegistryPath())
	if e != nil {
		return nil, e
	}
	wanted, e := askedFor(c.Analysis(), reviewedOnly)
	if e != nil {
		return nil, e
	}
	standing, e := Standing(filepath.Join(c.Decisions, "rulings.yaml"))
	if e != nil {
		return nil, e
	}
	// Decisions enter the ledger before application. A report-only run must
	// not accidentally apply a pending rename/merge through backfill instead.
	unapplied := map[string]bool{}
	if reviewedOnly {
		path := filepath.Join(c.Cache, "rulings.yaml")
		if exists(path) {
			saved, err := readYAML(path)
			if err != nil {
				return nil, err
			}
			if !truth(saved["applied"]) {
				for _, row := range array(saved["rulings"]) {
					unapplied[wordFold(str(record(row)["tag"]))] = true
				}
			}
		}
	}
	decided := map[string]string{}
	for tag, row := range standing {
		if !unapplied[wordFold(tag)] && contains([]string{"approve", "rework", "merge"}, str(row["verdict"])) {
			decided[wordFold(tag)] = str(first(row["into"], tag))
		}
	}
	docs, e := Documents(c.Content, "item")
	if e != nil {
		return nil, e
	}
	selected := map[string]bool{}
	for _, t := range tags {
		selected[wordFold(t)] = true
	}
	report := Record{"tags": Record{}, "items": 0, "added": 0}
	for _, d := range docs {
		fp := str(record(d.Data["provenance"])["fingerprint"])
		changed := false
		have := texts(d.Data["tags"])
		mapping := LoadMapping(c, str(d.Data["author"]))
		for _, folded := range sortedKeys(wanted) {
			request := wanted[folded]
			if !request.Recordings[fp] {
				continue
			}
			name := ""
			for _, raw := range sortedKeys(request.Spellings) {
				var why string
				name, why = reg.Resolve(raw, mapping)
				if name == "" && why != "dropped" {
					name = reg.Spelling[Fold(decided[folded])]
				}
				if name != "" {
					break
				}
			}
			if name == "" || (len(tags) > 0 && !selected[wordFold(name)]) {
				continue
			}
			canonical := wordFold(name)
			already := false
			for _, t := range have {
				already = already || wordFold(t) == canonical
			}
			if already {
				continue
			}
			have = append(have, name)
			changed = true
			report["added"] = integer(report["added"]) + 1
			counts := record(report["tags"])
			counts[name] = integer(counts[name]) + 1
			en := record(record(d.Data["provenance"])["enriched"])
			if len(en) > 0 {
				added := uniqueStrings(append(texts(en["tags_added"]), name))
				sort.Strings(added)
				en["tags_added"] = added
			}
		}
		if changed {
			d.Data["tags"] = have
			if contains(texts(d.Data["needs"]), "tags") {
				needs := without(texts(d.Data["needs"]), "tags")
				if len(needs) == 0 {
					delete(d.Data, "needs")
				} else {
					d.Data["needs"] = needs
				}
			}
			report["items"] = integer(report["items"]) + 1
			if write {
				if _, e = SaveDocument(d.Path, d.Data); e != nil {
					return nil, e
				}
			}
		}
	}
	return report, nil
}
func PendingTags(c Config, fromReviews bool) (map[string]Record, error) {
	reg, e := LoadRegistry(c.RegistryPath())
	if e != nil {
		return nil, e
	}
	docs, e := Documents(c.Content, "item")
	if e != nil {
		return nil, e
	}
	out := map[string]Record{}
	// Targets a tagmap run wanted but the registry does not have. They are held
	// in the creator's own file rather than in `mapping`, precisely so nothing
	// can map onto them until they have been ruled on -- which only works if
	// they are put to the adjudicator, so they are collected here.
	queued, e := QueuedTargets(c)
	if e != nil {
		return nil, e
	}
	for _, r := range queued {
		tag := str(r["tag"])
		if tag == "" || reg.Has(tag) {
			continue
		}
		row := out[tag]
		if row == nil {
			row = Record{"count": 0, "items": []string{}, "reasons": []string{}}
			out[tag] = row
		}
		row["count"] = integer(row["count"]) + integer(r["count"])
		why := strings.TrimSpace(str(r["why"]))
		if why != "" && len(texts(row["reasons"])) < 4 {
			row["reasons"] = append(texts(row["reasons"]),
				fmt.Sprintf("%s wants it for %q: %s", str(r["author"]), str(r["from"]), why))
		}
	}
	if fromReviews {
		wanted, e := AskedFor(c.Analysis())
		if e != nil {
			return nil, e
		}
		titles := map[string]string{}
		for _, d := range docs {
			fp := str(record(d.Data["provenance"])["fingerprint"])
			if titles[fp] == "" {
				titles[fp] = str(first(d.Data["title"], d.Data["id"]))
			}
		}
		known := map[string]bool{}
		for name := range reg.Meanings {
			known[wordFold(name)] = true
		}
		for folded, row := range wanted {
			if known[folded] {
				continue
			}
			name := sortedKeys(row.Spellings)[0]
			items := []string{}
			for _, fp := range sortedKeys(row.Recordings) {
				if titles[fp] != "" && len(items) < 6 {
					items = append(items, titles[fp])
				}
			}
			out[name] = Record{"count": len(row.Recordings), "items": items, "reasons": []string{}}
		}
		return out, nil
	}
	for _, d := range docs {
		mapping := LoadMapping(c, str(d.Data["author"]))
		for _, v := range array(record(d.Data["provenance"])["proposed_tags"]) {
			r := record(v)
			tag := strings.TrimSpace(str(r["tag"]))
			if tag == "" {
				continue
			}
			// Settled means something actually resolves the tag, not that a row
			// mentions it. A creator's map saying `humor -> Humour` leaves the tag
			// homeless for as long as `Humour` is absent from the registry, and
			// that is the case most in need of a ruling rather than least.
			// Counting any mapping row as an answer swallowed 34 spellings over
			// 104 recordings here: proposed on the items, reported by `retag`, and
			// never once put to the adjudicator -- which is how 874 unregistered
			// spellings accumulated the last time.
			if s, why := reg.Resolve(tag, mapping); s != "" || why == "dropped" {
				continue
			}
			row := out[tag]
			if row == nil {
				row = Record{"count": 0, "items": []string{}, "reasons": []string{}}
				out[tag] = row
			}
			row["count"] = integer(row["count"]) + 1
			if len(texts(row["items"])) < 6 {
				row["items"] = append(texts(row["items"]), str(first(d.Data["title"], d.Data["id"])))
			}
			why := strings.TrimSpace(str(r["why"]))
			if why != "" && len(texts(row["reasons"])) < 4 {
				row["reasons"] = append(texts(row["reasons"]), why)
			}
		}
	}
	return out, nil
}

// tagVocabulary is the half of tag-map building that is the same for every
// creator: what the registry says, which tags each creator uses and how often.
// Worked out once so a creator's map can be built on its own without paying for
// a sweep of the whole library each time.
type tagVocabulary struct {
	reg    *Registry
	counts map[string]map[string]int
	titles map[string][]string
}

// Worked out afresh on each call. It was briefly memoised on the engine, which
// was wrong: items change while a run is going -- the graph writes them, and a
// creator's tags with them -- so a vocabulary held from earlier answers about a
// library that has moved on. Shared within one sweep by passing it, not by
// remembering it.
func (e *Engine) tagVocabulary() (*tagVocabulary, error) {
	{
		c := e.Config
		report, err := e.loadSources()
		if err != nil {
			return nil, err
		}
		reg, err := LoadRegistry(c.RegistryPath())
		if err != nil {
			return nil, err
		}
		v := &tagVocabulary{reg: reg, counts: map[string]map[string]int{}, titles: map[string][]string{}}
		for _, s := range report.Sources {
			a := s.AuthorID()
			if v.counts[a] == nil {
				v.counts[a] = map[string]int{}
			}
			v.titles[a] = append(v.titles[a], s.Title)
			for _, tag := range texts(s.Data["tags"]) {
				v.counts[a][strings.TrimSpace(tag)]++
			}
		}
		if docs, err := e.Index.Entries(c.Content); err == nil {
			for _, d := range docs {
				a := str(d.Data["author"])
				if v.counts[a] == nil {
					v.counts[a] = map[string]int{}
				}
				for _, tag := range texts(d.Data["tags"]) {
					v.counts[a][tag]++
				}
			}
		}
		return v, nil
	}
}

// looseTag is a tag with its whitespace made ordinary, for comparing what was
// asked against what came back.
func looseTag(t string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.NewReplacer(
		"\u00a0", " ", "\u2007", " ", "\u202f", " ").Replace(t)), " "))
}

func (e *Engine) BuildTagmaps(ctx context.Context, authors []string, model string, dry bool) (Record, error) {
	c := e.Config
	v, err := e.tagVocabulary()
	if err != nil {
		return nil, err
	}
	reg, counts, titles := v.reg, v.counts, v.titles
	if len(authors) == 0 {
		authors = sortedKeys(counts)
	}
	total := Record{"asked": 0, "ruled": 0, "pending": 0}
	var failures []error
	for _, a := range authors {
		known := LoadMapping(c, a)
		waiting := map[string]Record{}
		for _, v := range array(optionalYAML(MappingPath(c, a))["pending"]) {
			row := record(v)
			waiting[str(first(row["from"], row["tag"]))] = row
		}
		total["pending"] = integer(total["pending"]) + len(waiting)
		todo := []string{}
		for t := range counts[a] {
			if known[t] == nil && waiting[t] == nil {
				if _, settled := reg.Meanings[t]; !settled {
					todo = append(todo, t)
				}
			}
		}
		sort.Slice(todo, func(i, j int) bool {
			if counts[a][todo[i]] != counts[a][todo[j]] {
				return counts[a][todo[i]] > counts[a][todo[j]]
			}
			return todo[i] < todo[j]
		})
		total["asked"] = integer(total["asked"]) + len(todo)
		if len(todo) > 0 {
			e.Say("%s: %d tag(s) to rule on", a, len(todo))
		}
		if dry {
			continue
		}
		missing := []string{}
		for start := 0; start < len(todo); start += 100 {
			part := todo[start:min(start+100, len(todo))]
			lines := []string{}
			for _, t := range part {
				lines = append(lines, fmt.Sprintf("%5d  %s", counts[a][t], t))
			}
			context := "\nSome of their recordings, for a sense of what they make:\n  " + strings.Join(titles[a][:min(12, len(titles[a]))], "\n  ") + "\n"
			if len(known) > 0 {
				context += "\nAlready decided for this creator, for consistency:\n"
				keys := sortedKeys(known)
				for _, t := range keys[:min(60, len(keys))] {
					r := record(known[t])
					context += "  " + t + " -> " + str(first(r["to"], r["verdict"])) + "\n"
				}
			}
			user := "THE REGISTRY — each tag with what it means.\n" + reg.Block() + fmt.Sprintf("\n\n\nTAGS USED BY '%s', with how many of their recordings carry each. Rule on every one.\n", a) + strings.Join(lines, "\n") + "\n" + context + "\n" + prompt("tagmap_shape")
			complete := startRunWork(ctx, fmt.Sprintf("tagmap %s tags %d-%d/%d", a, start+1, start+len(part), len(todo)))
			reply, err := e.API.ChatJSON(ctx, model, messages(prompt("tagmap_system"), user), TokenCeiling, .2)
			complete(err)
			if err != nil {
				e.Say("tagmap %s: %v", a, err)
				failures = append(failures, fmt.Errorf("tagmap %s: %w", a, err))
				missing = append(missing, part...)
				continue
			}
			// Match on the words, not the bytes. A tag carrying a non-breaking
			// space comes back with an ordinary one, and comparing exactly then
			// reads a perfectly good ruling as a missing one.
			asked := map[string]string{}
			for _, t := range part {
				asked[looseTag(t)] = t
			}
			fresh := map[string]bool{}
			for _, v := range array(reply["mapping"]) {
				r := record(v)
				t, ok := asked[looseTag(str(r["tag"]))]
				if !ok {
					continue
				}
				fresh[t] = true
				total["ruled"] = integer(total["ruled"]) + 1
				// A map may only point into the registry. Where the model wants a
				// name the registry does not have, that name is a *proposal* and
				// has to be adjudicated before anything may map onto it -- writing
				// it as a settled row instead produces a decision file that reads
				// as answered and resolves to nothing. That is how 100 rows across
				// five creators came to point at tags like "Humour" and "Exercise"
				// that were never added, each one quietly costing an item the tag
				// the registry did have.
				target := strings.TrimSpace(str(r["to"]))
				if target != "" && strings.ToLower(str(r["verdict"])) != "drop" &&
					reg.Spelling[Fold(target)] == "" {
					waiting[t] = Record{"tag": target, "from": t, "author": a,
						"count": counts[a][t], "description": str(r["description"]),
						"why": str(r["why"])}
					total["pending"] = integer(total["pending"]) + 1
					continue
				}
				known[t] = r
			}
			unruled := []string{}
			for _, t := range part {
				if !fresh[t] {
					missing = append(missing, t)
					unruled = append(unruled, t)
				}
			}
			// Left for the next pass rather than failing this one: a tag nobody
			// ruled on stays in the queue, and the creators after this one still
			// get their maps built.
			if len(unruled) > 0 {
				e.Say("%s: %d tag(s) came back unruled, still queued", a, len(unruled))
			}
		}
		if len(todo) > 0 {
			mapping := []any{}
			for _, t := range sortedKeys(known) {
				mapping = append(mapping, known[t])
			}
			body := Record{"author": a, "model": model, "mapping": mapping}
			if len(missing) > 0 {
				sort.Strings(missing)
				body["unruled"] = missing
			}
			if len(waiting) > 0 {
				// Not a mapping: a queue. `LoadMapping` reads only `mapping`, so
				// nothing here can answer for a tag until the adjudicator has put
				// the target in the registry.
				rows := []any{}
				for _, k := range sortedKeys(waiting) {
					rows = append(rows, waiting[k])
				}
				body["pending"] = rows
			}
			if err = writeYAML(MappingPath(c, a), body, []string{"author", "model", "unruled", "pending", "mapping"}); err != nil {
				return total, err
			}
		}
	}
	return total, errors.Join(failures...)
}
func AdoptTagmaps(c Config, authors []string, write bool) ([]Record, error) {
	reg, e := LoadRegistry(c.RegistryPath())
	if e != nil {
		return nil, e
	}
	maps := map[string]string{}
	for _, dir := range []string{c.Decisions, filepath.Join(c.Root, "tagmaps")} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
		for _, p := range files {
			a := stemOf(p)
			if maps[a] == "" {
				maps[a] = p
			}
		}
	}
	added := []Record{}
	for _, a := range sortedKeys(maps) {
		if len(authors) > 0 && !contains(authors, a) {
			continue
		}
		doc := optionalYAML(maps[a])
		for _, v := range array(doc["mapping"]) {
			r := record(v)
			if str(r["verdict"]) != "new" {
				continue
			}
			name := strings.TrimSpace(str(first(r["to"], r["name"], r["tag"])))
			meaning := strings.TrimSpace(str(r["description"]))
			if name == "" || meaning == "" || reg.Spelling[Fold(name)] != "" {
				continue
			}
			kind := reg.KindOf(name)
			if _, ok := reg.Data[kind]; !ok {
				return nil, fmt.Errorf("no %q block in registry", kind)
			}
			_, bare := SplitTag(name)
			nested(reg.Data, kind)[bare] = meaning
			reg = NewRegistry(reg.Data)
			added = append(added, Record{"name": name, "description": meaning, "author": a})
		}
	}
	if write && len(added) > 0 {
		if _, e = SaveDocument(c.RegistryPath(), reg.Data); e != nil {
			return nil, e
		}
		rows := []Record{}
		for _, r := range added {
			rows = append(rows, RulingEntry(str(r["name"]), "approve", str(r["description"]), "", 0, c.Enrich.AdjudicatorModel, "tagmap: "+str(r["author"])))
		}
		_, e = AppendLedger(filepath.Join(c.Decisions, "rulings.yaml"), rows)
	}
	return added, e
}
func (e *Engine) Adjudicate(ctx context.Context, fromReviews, collect bool, model, path string) (Record, error) {
	rows, err := PendingTags(e.Config, fromReviews)
	if err != nil {
		return nil, err
	}
	return e.adjudicatePending(ctx, rows, collect, model, path)
}

func (e *Engine) adjudicatePending(ctx context.Context, rows map[string]Record, collect bool, model, path string) (Record, error) {
	reg, err := LoadRegistry(e.Config.RegistryPath())
	if err != nil {
		return nil, err
	}
	log := filepath.Join(e.Config.Decisions, "rulings.yaml")
	settled, err := Standing(log)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for t, r := range rows {
		counts[t] = integer(r["count"])
	}
	reopen, err := WorthRevisiting(log, counts, sortedKeys(reg.Meanings))
	if err != nil {
		return nil, err
	}
	reopened := map[string]bool{}
	for _, r := range reopen {
		reopened[str(r["tag"])] = true
	}
	asking := map[string]Record{}
	for t, r := range rows {
		if settled[t] == nil || reopened[t] {
			asking[t] = r
		}
	}
	if collect || len(asking) == 0 {
		return Record{"pending": asking, "reopened": reopen}, nil
	}
	names := sortedKeys(asking)
	sort.SliceStable(names, func(i, j int) bool { return integer(asking[names[i]]["count"]) > integer(asking[names[j]]["count"]) })
	lines := []string{}
	for _, tag := range names {
		r := asking[tag]
		lines = append(lines, fmt.Sprintf("\n=== %s   (proposed on %d recording(s))", tag, integer(r["count"])), "  seen on: "+strings.Join(texts(r["items"]), "; "))
		for n, why := range texts(r["reasons"]) {
			lines = append(lines, fmt.Sprintf("  justification %d: %s", n+1, why))
		}
	}
	user := "THE REGISTRY AS IT STANDS — a tag already here is a reason to reject:\n" + reg.Block() + "\n\n\nTAGS PROPOSED DURING THIS RUN. Rule on every one.\n" + strings.Join(lines, "\n") + "\n\n" + prompt("adjudicate_shape")
	complete := startRunWork(ctx, fmt.Sprintf("adjudication request (%d tags)", len(names)))
	reply, err := e.API.ChatJSON(ctx, model, messages(prompt("adjudicate_system"), user), TokenCeiling, .2)
	complete(err)
	if err != nil {
		return nil, err
	}
	rulings := []any{}
	logRows := []Record{}
	for _, v := range array(reply["rulings"]) {
		r := record(v)
		tag := str(r["tag"])
		if asking[tag] == nil {
			continue
		}
		rulings = append(rulings, r)
		logRows = append(logRows, RulingEntry(tag, str(r["verdict"]), str(r["why"]), str(first(r["merge_into"], r["name"])), counts[tag], model, "adjudicate"))
	}
	if err = writeYAML(path, Record{"model": model, "rulings": rulings}, []string{"model", "rulings"}); err != nil {
		return nil, err
	}
	n, err := AppendLedger(log, logRows)
	return Record{"rulings": len(rulings), "logged": n, "path": path}, err
}
func (e *Engine) Reconsider(ctx context.Context, tags []string, model string, write bool) (Record, error) {
	reg, err := LoadRegistry(e.Config.RegistryPath())
	if err != nil {
		return nil, err
	}
	refused, err := Refused(filepath.Join(e.Config.Decisions, "rulings.yaml"), sortedKeys(reg.Meanings))
	if err != nil {
		return nil, err
	}
	found := []any{}
	logs := []Record{}
	for _, tag := range tags {
		if _, ok := reg.Meanings[tag]; !ok {
			return nil, fmt.Errorf("new tag %q is not in the registry", tag)
		}
		names := sortedKeys(refused)
		for start := 0; start < len(names); start += 60 {
			part := names[start:min(start+60, len(names))]
			lines := []string{}
			for _, t := range part {
				if wordFold(t) != wordFold(tag) {
					lines = append(lines, "- "+t+"   (refused: "+str(first(refused[t]["why"], "no reason recorded"))+")")
				}
			}
			user := "THE NEWLY ADOPTED TAG\n" + tag + ": " + reg.Meanings[tag] + "\n\nREFUSED PROPOSALS\n" + strings.Join(lines, "\n") + "\n\n" + prompt("reconsider_shape")
			reply, err := e.API.ChatJSON(ctx, model, messages(prompt("reconsider_system"), user), TokenCeiling, .1)
			if err != nil {
				return nil, err
			}
			for _, v := range array(reply["merges"]) {
				r := record(v)
				t := str(r["tag"])
				if truth(r["same"]) && contains(part, t) && wordFold(t) != wordFold(tag) {
					found = append(found, Record{"tag": t, "into": tag, "why": str(r["why"])})
					logs = append(logs, RulingEntry(t, "merge", str(r["why"]), tag, 0, model, "reconsider (reviewer-proposed)"))
				}
			}
		}
	}
	written := 0
	if write {
		written, err = AppendLedger(filepath.Join(e.Config.Decisions, "rulings.yaml"), logs)
	}
	return Record{"proposed": found, "written": written}, err
}
