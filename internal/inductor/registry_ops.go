// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Editing the registry by hand, with the rest of the library kept in step.
//
// The registry is the one list of tags that may reach an item, so changing it
// is never only a change to `content/tags.yaml`. A tag lives in four other
// places: on the items that carry it, in the `tags_added` a run recorded, in
// the proposals an item is still holding, and in the `to:` of every creator's
// map. A person editing the YAML directly moves one of the five and leaves the
// other four disagreeing with it -- and every one of those disagreements is
// silent, because a tag that no longer resolves is simply dropped.
//
// So an edit is planned in full against the registry in memory and then
// committed in one pass over the library. That is what makes a bulk edit
// possible at all: `Add X` followed by `Merge Y into X` has to see the X that
// the first change created, which it cannot do if each change reloads the
// registry from disk and walks the tree on its own.
//
// Every edit leaves the same trail an adjudication leaves: a ruling in the
// ledger saying what was decided and why. Without that the next `adjudicate`
// re-proposes what a person has already settled.

// tagEdit is a planned change to the vocabulary, not yet written anywhere.
type tagEdit struct {
	// existing tag -> what it becomes; "" means it is being removed.
	changes map[string]string
	// tag -> why, for a removal: written onto the items as a proposal and onto
	// any map row that has to be retired.
	notes map[string]string
	// folded spelling -> the tag to grant, for proposals an addition answers.
	accept  map[string]string
	rulings []Record
	steps   []Record
}

func newEdit() *tagEdit {
	return &tagEdit{changes: map[string]string{}, notes: map[string]string{},
		accept: map[string]string{}}
}

// retarget records old -> new, and carries any change that already pointed at
// `old` through to `new`. Without it, `rename A B` followed by `merge B C`
// leaves everything that held A sitting on a B that no longer exists.
func (e *tagEdit) retarget(from, to string) {
	for was, becomes := range e.changes {
		if Fold(becomes) == Fold(from) {
			e.changes[was] = to
		}
	}
	e.changes[from] = to
	for folded, granted := range e.accept {
		if Fold(granted) == Fold(from) {
			e.accept[folded] = to
		}
	}
}

// placeIn says which namespace block a name belongs to and under what key, and
// refuses a name whose prefix only looks like one.
//
// `TagKindOf` answers "content" for anything it does not recognise, which is
// right when reading a tag somebody already accepted and wrong when creating
// one: `CWW: Blood` would become a content tag *named* "CWW: Blood", sitting in
// the wrong block under a name nobody will type again. A colon in a name is
// legitimate -- ten trigger entries in one library read `Slave Mode : Sybian` --
// so what is refused is specifically a prefix that is not one of the eight.
func placeIn(reg *Registry, name string) (string, string, error) {
	prefix, bare := SplitTag(name)
	kind := reg.KindOf(name)
	key := name
	if p := reg.PrefixOf(kind); p != "" && p == prefix {
		key = bare
	} else if prefix != "" && kind == reg.ContentKey() {
		known := []string{}
		for _, k := range reg.Kinds {
			if k.Prefix != "" {
				known = append(known, k.Prefix+":")
			}
		}
		return "", "", fmt.Errorf("%q starts with %q, which is not a namespace; use one of %s, or no prefix for a content tag",
			name, prefix+":", strings.Join(known, " "))
	}
	if _, ok := reg.Data[kind]; !ok {
		// One of the eight known blocks, simply absent from this registry.
		reg.Data[kind] = Record{}
	}
	return kind, key, nil
}

// registryEntry says where an existing tag physically lives.
func registryEntry(reg *Registry, tag string) (string, string, bool) {
	canonical := reg.Spelling[Fold(tag)]
	if canonical == "" {
		return "", "", false
	}
	kind, key, err := placeIn(reg, canonical)
	if err != nil {
		return "", "", false
	}
	return kind, key, true
}

func ruling(tag, verdict, into, why string) Record {
	r := Record{"apiVersion": "inductor/v1", "kind": "TagRuling",
		"when": time.Now().UTC().Format("2006-01-02"), "tag": tag,
		"verdict": verdict, "model": "by hand"}
	if into != "" {
		r["into"] = into
	}
	if why != "" {
		r["why"] = why
	}
	return r
}

// ---------------------------------------------------------------- the changes

func (e *tagEdit) add(reg *Registry, tag, description, why string) error {
	tag, description = strings.TrimSpace(tag), strings.TrimSpace(description)
	if tag == "" || description == "" {
		return fmt.Errorf("add %q: a tag and a description are both required", tag)
	}
	if found := reg.Spelling[Fold(tag)]; found != "" {
		return fmt.Errorf("add %q: the registry already has %q; use `describe` to change its meaning", tag, found)
	}
	kind, key, err := placeIn(reg, tag)
	if err != nil {
		return err
	}
	nested(reg.Data, kind)[key] = description
	*reg = *NewRegistry(reg.Data)
	// Every recording still proposing this tag now has it. That consequence is
	// the reason to add it, and leaving it out makes a hand edit weaker than an
	// adjudication for no reason anybody chose.
	e.accept[Fold(tag)] = tag
	r := ruling(tag, "approve", "", why)
	r["name"], r["description"] = tag, description
	e.rulings = append(e.rulings, r)
	e.steps = append(e.steps, Record{"action": "add", "tag": tag})
	return nil
}

func (e *tagEdit) describe(reg *Registry, tag, description string) error {
	description = strings.TrimSpace(description)
	if description == "" {
		return fmt.Errorf("describe %q: a description is required", tag)
	}
	kind, key, ok := registryEntry(reg, tag)
	if !ok {
		return fmt.Errorf("describe %q: the registry has no such tag", tag)
	}
	canonical := reg.Spelling[Fold(tag)]
	was := str(record(reg.Data[kind])[key])
	nested(reg.Data, kind)[key] = description
	*reg = *NewRegistry(reg.Data)
	e.steps = append(e.steps, Record{"action": "describe", "tag": canonical, "was": was, "now": description})
	return nil
}

func (e *tagEdit) remove(reg *Registry, tag, why string) error {
	kind, key, ok := registryEntry(reg, tag)
	if !ok {
		return fmt.Errorf("remove %q: the registry has no such tag", tag)
	}
	canonical := reg.Spelling[Fold(tag)]
	note := "removed from the registry on " + time.Now().UTC().Format("2006-01-02")
	if why != "" {
		note += ": " + why
	}
	delete(record(reg.Data[kind]), key)
	*reg = *NewRegistry(reg.Data)
	e.retarget(canonical, "")
	e.notes[canonical] = note
	e.rulings = append(e.rulings, ruling(canonical, "omit", "", why))
	e.steps = append(e.steps, Record{"action": "remove", "tag": canonical})
	return nil
}

func (e *tagEdit) rename(reg *Registry, from, to, description string) error {
	to = strings.TrimSpace(to)
	if to == "" {
		return fmt.Errorf("rename %q: a new name is required", from)
	}
	kind, key, ok := registryEntry(reg, from)
	if !ok {
		return fmt.Errorf("rename %q: the registry has no such tag", from)
	}
	canonical := reg.Spelling[Fold(from)]
	if found := reg.Spelling[Fold(to)]; found != "" && Fold(found) != Fold(canonical) {
		return fmt.Errorf("rename %q: the registry already has %q; use `merge` to fold one into the other", from, found)
	}
	meaning := str(record(reg.Data[kind])[key])
	if description != "" {
		meaning = strings.TrimSpace(description)
	}
	newKind, newKey, err := placeIn(reg, to)
	if err != nil {
		return err
	}
	delete(record(reg.Data[kind]), key)
	nested(reg.Data, newKind)[newKey] = meaning
	*reg = *NewRegistry(reg.Data)
	e.retarget(canonical, to)
	e.notes[canonical] = "renamed to " + to
	e.rulings = append(e.rulings, ruling(canonical, "rework", to, description))
	e.steps = append(e.steps, Record{"action": "rename", "tag": canonical, "target": to})
	return nil
}

func (e *tagEdit) merge(reg *Registry, from, into string) error {
	kind, key, ok := registryEntry(reg, from)
	if !ok {
		return fmt.Errorf("merge %q: the registry has no such tag", from)
	}
	survivor := reg.Spelling[Fold(into)]
	if survivor == "" {
		return fmt.Errorf("merge %q into %q: the registry has no %q; use `rename` to give it that name instead", from, into, into)
	}
	canonical := reg.Spelling[Fold(from)]
	if Fold(canonical) == Fold(survivor) {
		return fmt.Errorf("merge %q into %q: they are the same tag", from, into)
	}
	delete(record(reg.Data[kind]), key)
	*reg = *NewRegistry(reg.Data)
	e.retarget(canonical, survivor)
	e.notes[canonical] = "merged into " + survivor
	e.rulings = append(e.rulings, ruling(canonical, "merge", survivor, ""))
	e.steps = append(e.steps, Record{"action": "merge", "tag": canonical, "target": survivor})
	return nil
}

// RegistryAbout sets what a namespace is for.
//
// `_about` is the line the adjudicator is shown above that block's tags and the
// heading the site puts over them, so a namespace without one is a namespace
// nobody is told the rule for. It is not a tag -- it names no recording and
// resolves to nothing -- so it is the one part of the registry that can be set
// on its own without the rest of the library needing to move with it.
func RegistryAbout(c Config, namespace, about string, write bool) (Record, error) {
	about = strings.TrimSpace(about)
	reg, err := LoadRegistry(c.RegistryPath())
	if err != nil {
		return nil, err
	}
	key := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(namespace, ":")))
	known := []string{}
	for _, k := range reg.Kinds {
		known = append(known, k.Key)
	}
	if !contains(known, key) {
		return nil, fmt.Errorf("%q is not a namespace; use one of %s", namespace, strings.Join(known, ", "))
	}
	if _, ok := reg.Data[key]; !ok {
		reg.Data[key] = Record{}
	}
	was := str(record(reg.Data[key])["_about"])
	if about == "" {
		delete(record(reg.Data[key]), "_about")
	} else {
		nested(reg.Data, key)["_about"] = about
	}
	if write {
		if _, err := SaveDocument(c.RegistryPath(), reg.Data); err != nil {
			return nil, err
		}
	}
	return Record{"namespace": key, "was": was, "now": about, "written": write}, nil
}

// ---------------------------------------------------------------- committing

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if Fold(s) == Fold(want) {
			return true
		}
	}
	return false
}

// rewriteLibrary applies the whole edit to every item in one pass.
func (e *tagEdit) rewriteLibrary(c Config, write bool) (Record, error) {
	docs, err := Documents(c.Content, "item")
	if err != nil {
		return nil, err
	}
	folded := map[string]string{}
	for from, to := range e.changes {
		folded[Fold(from)] = to
	}
	out := Record{"items": 0, "retagged": 0, "removed": 0, "granted": 0, "proposals": 0}
	for _, d := range docs {
		touched := false
		was := texts(d.Data["tags"])
		tags, seen := []string{}, map[string]bool{}
		keep := func(t string) {
			if !seen[t] {
				seen[t] = true
				tags = append(tags, t)
			}
		}
		for _, t := range was {
			to, ruled := folded[Fold(t)]
			if !ruled {
				keep(t)
				continue
			}
			touched = true
			if to == "" {
				out["removed"] = integer(out["removed"]) + 1
				continue
			}
			out["retagged"] = integer(out["retagged"]) + 1
			// Eliding duplicates is the whole of a merge: an item already
			// holding the survivor gains nothing and keeps one copy.
			keep(to)
		}
		p := record(d.Data["provenance"])
		// What a run recorded itself as having added has to follow the tag, or
		// the item claims a model wrote a tag that is no longer there.
		if en := record(p["enriched"]); len(en) > 0 {
			added, moved := []string{}, false
			for _, t := range texts(en["tags_added"]) {
				to, ruled := folded[Fold(t)]
				if !ruled {
					added = append(added, t)
					continue
				}
				moved = true
				if to != "" && !contains(added, to) {
					added = append(added, to)
				}
			}
			if moved {
				touched = true
				sortStrings(added)
				if len(added) > 0 {
					en["tags_added"] = added
				} else {
					delete(en, "tags_added")
				}
			}
		}
		proposals := []any{}
		for _, v := range array(p["proposed_tags"]) {
			r := clone(record(v))
			name := strings.TrimSpace(str(r["tag"]))
			if granted, ok := e.accept[Fold(name)]; ok {
				// The tag it was asking for now exists.
				touched = true
				if !seen[granted] {
					out["granted"] = integer(out["granted"]) + 1
					keep(granted)
				}
				continue
			}
			if to, ruled := folded[Fold(name)]; ruled && to != "" {
				touched = true
				r["tag"] = to
			}
			proposals = append(proposals, r)
		}
		// A tag taken out of the registry goes back to being proposed: the item
		// said this about itself once, and a registry edit does not erase that.
		for from, note := range e.notes {
			if e.changes[from] != "" || !containsFold(was, from) {
				continue
			}
			already := false
			for _, v := range proposals {
				if Fold(str(record(v)["tag"])) == Fold(from) {
					already = true
				}
			}
			if !already {
				proposals = append(proposals, Record{"tag": from, "why": note})
				out["proposals"] = integer(out["proposals"]) + 1
				touched = true
			}
		}
		if !touched {
			continue
		}
		d.Data["tags"] = tags
		if len(proposals) > 0 {
			nested(d.Data, "provenance")["proposed_tags"] = proposals
		} else if len(record(d.Data["provenance"])) > 0 {
			delete(record(d.Data["provenance"]), "proposed_tags")
		}
		out["items"] = integer(out["items"]) + 1
		if write {
			if _, err := SaveDocument(d.Path, d.Data); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

// rewriteMaps keeps every creator's map pointing into the registry.
//
// A map's `to:` names a registry entry, so renaming that entry renames the
// target. Removing it would leave the row pointing at nothing, which is the one
// thing a map may never do -- so the row is retired with its reasoning rather
// than deleted, the same as a row found dangling by `check`.
func (e *tagEdit) rewriteMaps(c Config, write bool) (Record, error) {
	entries, err := os.ReadDir(c.Decisions)
	if err != nil {
		if os.IsNotExist(err) {
			return Record{"maps": 0, "rows": 0, "retired": 0}, nil
		}
		return nil, err
	}
	folded := map[string]string{}
	for from, to := range e.changes {
		folded[Fold(from)] = to
	}
	out := Record{"maps": 0, "rows": 0, "retired": 0}
	for _, f := range entries {
		if f.IsDir() || filepath.Ext(f.Name()) != ".yaml" || f.Name() == "rulings.yaml" {
			continue
		}
		path := filepath.Join(c.Decisions, f.Name())
		doc := optionalYAML(path)
		if len(array(doc["mapping"])) == 0 && len(array(doc["pending"])) == 0 {
			continue
		}
		mapping, retired, changed := []any{}, array(doc["retired"]), false
		for _, v := range array(doc["mapping"]) {
			r := clone(record(v))
			target := str(r["to"])
			to, ruled := folded[Fold(target)]
			if !ruled || target == "" {
				mapping = append(mapping, r)
				continue
			}
			changed = true
			if to == "" {
				r["retired_on"] = time.Now().UTC().Format("2006-01-02")
				r["retired_why"] = e.notes[reverseLookup(e.changes, target)]
				if str(r["retired_why"]) == "" {
					r["retired_why"] = "the registry no longer has " + target
				}
				retired = append(retired, r)
				out["retired"] = integer(out["retired"]) + 1
				continue
			}
			r["to"] = to
			mapping = append(mapping, r)
			out["rows"] = integer(out["rows"]) + 1
		}
		pending := []any{}
		for _, v := range array(doc["pending"]) {
			r := clone(record(v))
			name := str(r["tag"])
			if granted, ok := e.accept[Fold(name)]; ok {
				// It is in the registry now; the queue entry has served.
				changed = true
				_ = granted
				continue
			}
			if to, ruled := folded[Fold(name)]; ruled {
				changed = true
				if to == "" {
					continue
				}
				r["tag"] = to
			}
			pending = append(pending, r)
		}
		if !changed {
			continue
		}
		doc["mapping"] = mapping
		if len(pending) > 0 {
			doc["pending"] = pending
		} else {
			delete(doc, "pending")
		}
		if len(retired) > 0 {
			doc["retired"] = retired
		}
		out["maps"] = integer(out["maps"]) + 1
		if write {
			if err := writeYAML(path, doc, []string{"author", "model", "unruled", "pending", "mapping", "retired"}); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

// reverseLookup finds which tag a change map sent to `target`, so a retired row
// can be given the reason that was recorded against the original name.
func reverseLookup(changes map[string]string, target string) string {
	for from := range changes {
		if Fold(from) == Fold(target) {
			return from
		}
	}
	return target
}

// commit writes the registry, the library, the maps and the ledger, or reports
// what it would write and touches nothing.
func (e *tagEdit) commit(c Config, reg *Registry, write bool) (Record, error) {
	items, err := e.rewriteLibrary(c, write)
	if err != nil {
		return nil, err
	}
	maps, err := e.rewriteMaps(c, write)
	if err != nil {
		return nil, err
	}
	out := Record{"changes": e.steps, "items": items, "maps": maps, "written": write}
	if !write {
		return out, nil
	}
	if _, err := SaveDocument(c.RegistryPath(), reg.Data); err != nil {
		return out, err
	}
	if len(e.rulings) > 0 {
		if _, err := AppendLedger(filepath.Join(c.Decisions, "rulings.yaml"), e.rulings); err != nil {
			return out, err
		}
	}
	return out, nil
}

// ------------------------------------------------------------------ the verbs

func oneEdit(c Config, apply func(*Registry, *tagEdit) error, write bool) (Record, error) {
	reg, err := LoadRegistry(c.RegistryPath())
	if err != nil {
		return nil, err
	}
	e := newEdit()
	if err := apply(reg, e); err != nil {
		return nil, err
	}
	return e.commit(c, reg, write)
}

func RegistryAdd(c Config, tag, description, why string, write bool) (Record, error) {
	return oneEdit(c, func(r *Registry, e *tagEdit) error { return e.add(r, tag, description, why) }, write)
}
func RegistryDescribe(c Config, tag, description string, write bool) (Record, error) {
	return oneEdit(c, func(r *Registry, e *tagEdit) error { return e.describe(r, tag, description) }, write)
}
func RegistryRemove(c Config, tag, why string, write bool) (Record, error) {
	return oneEdit(c, func(r *Registry, e *tagEdit) error { return e.remove(r, tag, why) }, write)
}
func RegistryRename(c Config, from, to, description string, write bool) (Record, error) {
	return oneEdit(c, func(r *Registry, e *tagEdit) error { return e.rename(r, from, to, description) }, write)
}
func RegistryMerge(c Config, from, into string, write bool) (Record, error) {
	return oneEdit(c, func(r *Registry, e *tagEdit) error { return e.merge(r, from, into) }, write)
}

// RegistryBulk applies a file of changes in order, or none of them.
//
// Order is the file's own, because the changes can depend on each other: `Add`
// a tag and then `Merge` two others into it, and the merge has to see what the
// addition made. The whole list is planned against the registry in memory
// first, so a mistake on the ninth line is reported before the first line has
// touched anything -- a half-applied vocabulary change is worse than none,
// since the registry, the items and the maps would then disagree with each
// other and nothing would say so.
func RegistryBulk(c Config, path string, write bool) (Record, error) {
	doc, err := readYAML(path)
	if err != nil {
		return nil, err
	}
	if k := str(doc["kind"]); k != "" && k != "TagBulkUpdate" {
		return nil, fmt.Errorf("%s declares kind %q; this command reads TagBulkUpdate", path, k)
	}
	rows := array(doc["changes"])
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s lists no changes", path)
	}
	reg, err := LoadRegistry(c.RegistryPath())
	if err != nil {
		return nil, err
	}
	e := newEdit()
	problems := []string{}
	for i, v := range rows {
		r := record(v)
		tag := strings.TrimSpace(str(r["tag"]))
		target := strings.TrimSpace(str(r["target"]))
		description := str(r["description"])
		why := str(r["why"])
		var err error
		switch strings.ToLower(strings.TrimSpace(str(r["action"]))) {
		case "add":
			err = e.add(reg, tag, description, why)
		case "update", "describe":
			err = e.describe(reg, tag, description)
		case "remove", "delete":
			err = e.remove(reg, tag, why)
		case "rename":
			err = e.rename(reg, tag, target, description)
		case "merge":
			err = e.merge(reg, tag, target)
		case "":
			err = fmt.Errorf("no action given")
		default:
			err = fmt.Errorf("unknown action %q; use add, update, remove, rename or merge", str(r["action"]))
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("change %d (%s): %v", i+1, tag, err))
		}
	}
	if len(problems) > 0 {
		return Record{"problems": problems, "written": false},
			fmt.Errorf("%d of %d change(s) cannot be applied; nothing written", len(problems), len(rows))
	}
	return e.commit(c, reg, write)
}
