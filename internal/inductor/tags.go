// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type TagKind struct {
	Key, Prefix, Label string
	Spoiler            bool
}

var TagKinds = []TagKind{{"voice", "Voice", "Voice", false}, {"audience", "Audience", "Audience", false}, {"induction", "Induction", "Induction", false}, {"production", "Production", "Production", false}, {"trigger", "Trigger", "Triggers", true}, {"compulsion", "Compulsion", "Compulsions", true}, {"cw", "CW", "Content warnings", false}, {"content", "", "Content", false}}

func sortStrings(s []string) { sort.Strings(s) }
func SplitTag(t string) (string, string) {
	p, v, ok := strings.Cut(t, ":")
	if !ok {
		return "", strings.TrimSpace(t)
	}
	return strings.TrimSpace(p), strings.TrimSpace(v)
}
func TagKindOf(t string) string {
	p, _ := SplitTag(t)
	for _, k := range TagKinds {
		if k.Prefix == p {
			return k.Key
		}
	}
	return "content"
}
func tagPrefix(key string) string {
	for _, k := range TagKinds {
		if k.Key == key {
			return k.Prefix
		}
	}
	return ""
}

type Registry struct {
	Data     Record
	Spelling map[string]string
	Meanings map[string]string
}

func LoadRegistry(path string) (*Registry, error) {
	r, e := readYAML(path)
	if e != nil {
		return nil, e
	}
	return NewRegistry(r), nil
}
func NewRegistry(data Record) *Registry {
	r := &Registry{Data: data, Spelling: map[string]string{}, Meanings: map[string]string{}}
	bare := map[string][]string{}
	for _, key := range sortedKeys(data) {
		entries := record(data[key])
		prefix := tagPrefix(key)
		for _, name := range sortedKeys(entries) {
			if name == "_about" {
				continue
			}
			full := name
			if prefix != "" {
				full = prefix + ": " + name
			}
			r.Spelling[Fold(full)] = full
			bare[Fold(name)] = append(bare[Fold(name)], full)
			r.Meanings[full] = str(entries[name])
		}
	}
	for k, choices := range bare {
		if len(choices) == 1 && r.Spelling[k] == "" {
			r.Spelling[k] = choices[0]
		}
	}
	return r
}
func (r *Registry) Has(tag string) bool { _, ok := r.Meanings[tag]; return ok }
func (r *Registry) Block() string {
	var lines []string
	for _, kind := range TagKinds {
		entries := record(r.Data[kind.Key])
		if len(entries) == 0 {
			continue
		}
		head := kind.Label
		if about := str(entries["_about"]); about != "" {
			head += " — " + about
		}
		lines = append(lines, head)
		for _, n := range sortedKeys(entries) {
			if n == "_about" {
				continue
			}
			full := n
			if kind.Prefix != "" {
				full = kind.Prefix + ": " + n
			}
			lines = append(lines, "  "+full+" — "+str(entries[n]))
		}
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}
func (r *Registry) Resolve(tag string, mapping Record) (string, string) {
	raw := strings.TrimSpace(tag)
	if raw == "" {
		return "", "dropped"
	}
	prefix, value := SplitTag(raw)
	entry := record(mapping[raw])
	if len(entry) == 0 && r.Spelling[Fold(raw)] == "" {
		// The bare-word fallback is for a tag the registry does not know: a
		// creator writing "sissy" should still meet their row for it once the
		// pipeline has written it as "Audience: sissy". It must not reach a tag
		// the registry knows in full, though -- one creator's row for their own
		// word "Moans" caught the registry's "Trigger: Moans" and turned a
		// trigger into a content tag on sixteen recordings. A tag the registry
		// spells out is already resolved; only its own row may speak for it.
		entry = record(mapping[value])
	}
	if len(entry) > 0 {
		if strings.ToLower(str(entry["verdict"])) == "drop" {
			return "", "dropped"
		}
		if target := strings.TrimSpace(str(entry["to"])); target != "" {
			if found := r.Spelling[Fold(target)]; found != "" {
				return found, "mapped"
			}
			// The row names a tag the registry does not have, so it is not an
			// answer -- and a row that answers nothing must not be allowed to
			// take the tag away. The registry wins: fall through and let it rule
			// on the tag as written. Returning "unresolved" here instead put a
			// creator's map above the registry it is supposed to point into, and
			// stripped 1,029 registered tags off 675 recordings in one pass,
			// every one of them a tag the registry already had under the spelling
			// the item was using. `check` reports these rows; they are to be
			// adjudicated into the registry or dropped from the map.
		}
	}
	if prefix != "" {
		if found := r.Spelling[Fold(prefix+": "+value)]; found != "" {
			return found, "registry"
		}
	}
	if found := r.Spelling[Fold(value)]; found != "" {
		return found, "registry"
	}
	return "", "unresolved"
}

var audienceCode = regexp.MustCompile(`(?i)^[FMTNACGX]{1,4}4[FMTNACGX]{1,4}$`)

func ExpandCode(t string) []string {
	p, v := SplitTag(t)
	if (p != "" && p != "Audience") || !audienceCode.MatchString(v) {
		return nil
	}
	speakers, listeners, _ := strings.Cut(strings.ToLower(v), "4")
	voice := map[rune]string{'f': "fem", 'm': "masc", 't': "transfem", 'n': "androgynous"}
	audience := map[rune]string{'f': "woman", 'm': "man", 't': "transfem", 'n': "enby", 'c': "couple", 'a': "anyone", 'g': "man", 'x': "anyone"}
	out := []string{}
	for _, ch := range speakers {
		if s := voice[ch]; s != "" {
			out = append(out, "Voice: "+s)
		}
	}
	for _, ch := range listeners {
		if s := audience[ch]; s != "" {
			out = append(out, "Audience: "+s)
		}
	}
	return uniqueStrings(out)
}
func (r *Registry) Retag(tags []string, mapping Record) ([]string, []string) {
	out, unresolved := []string{}, []string{}
	for _, t := range tags {
		if c := ExpandCode(t); len(c) > 0 {
			out = append(out, c...)
			continue
		}
		s, why := r.Resolve(t, mapping)
		if s != "" {
			out = append(out, s)
		} else if why == "unresolved" {
			unresolved = append(unresolved, strings.TrimSpace(t))
		}
	}
	out = uniqueStrings(out)
	order := map[string]int{}
	for i, k := range TagKinds {
		order[k.Key] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := TagKindOf(out[i]), TagKindOf(out[j])
		if order[a] != order[b] {
			return order[a] < order[b]
		}
		return casefold(out[i]) < casefold(out[j])
	})
	return out, unresolved
}
func DurationTag(seconds float64) string {
	if seconds == 0 {
		return ""
	}
	m := seconds / 60
	if m < 10 {
		return "Duration: <10"
	}
	for low := 10; low < 120; low += 10 {
		if m < float64(low+10) {
			return fmt.Sprintf("Duration: %d-%d", low, low+10)
		}
	}
	return "Duration: 120+"
}

// QueuedTargets returns the registry entries creator maps are waiting on.
//
// `tagmap` puts a target it cannot find in the registry here instead of into
// `mapping`, so the row cannot answer for a tag before anybody has ruled on it.
// The queue is only half a rule, though: a proposal nothing ever reads is the
// same silence as a dangling row. This is the half that reads it.
func QueuedTargets(c Config) ([]Record, error) {
	entries, e := os.ReadDir(c.Decisions)
	if e != nil {
		if os.IsNotExist(e) {
			return nil, nil
		}
		return nil, e
	}
	out := []Record{}
	for _, f := range entries {
		if f.IsDir() || filepath.Ext(f.Name()) != ".yaml" {
			continue
		}
		author := strings.TrimSuffix(f.Name(), ".yaml")
		for _, v := range array(optionalYAML(filepath.Join(c.Decisions, f.Name()))["pending"]) {
			r := clone(record(v))
			if str(r["tag"]) == "" {
				continue
			}
			if str(r["author"]) == "" {
				r["author"] = author
			}
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return str(out[i]["tag"]) < str(out[j]["tag"]) })
	return out, nil
}

// DanglingMapTargets finds every mapping row that points somewhere the registry
// does not go.
//
// A creator's map exists to say what their vocabulary becomes *in the registry*.
// A row whose `to:` names something the registry has never heard of therefore
// answers nothing, and the resolver treats it as silence -- but silence in a
// decision file is the kind of fault that hides: the row reads as settled, and
// the tag it was meant to place goes on being proposed for ever. So they are
// reported, and each one is to be adjudicated into the registry or dropped from
// the map.
//
// `redundant` says the row's own tag is already registered, which makes the row
// pure loss: delete it and the registry answers correctly on its own.
func DanglingMapTargets(c Config) ([]Record, error) {
	reg, e := LoadRegistry(c.RegistryPath())
	if e != nil {
		return nil, e
	}
	entries, e := os.ReadDir(c.Decisions)
	if e != nil {
		if os.IsNotExist(e) {
			return nil, nil
		}
		return nil, e
	}
	out := []Record{}
	for _, f := range entries {
		if f.IsDir() || filepath.Ext(f.Name()) != ".yaml" {
			continue
		}
		author := strings.TrimSuffix(f.Name(), ".yaml")
		for _, v := range array(optionalYAML(filepath.Join(c.Decisions, f.Name()))["mapping"]) {
			r := record(v)
			tag := strings.TrimSpace(str(r["tag"]))
			target := strings.TrimSpace(str(r["to"]))
			if tag == "" || target == "" || strings.ToLower(str(r["verdict"])) == "drop" {
				continue
			}
			if reg.Spelling[Fold(target)] != "" {
				continue
			}
			out = append(out, Record{"author": author, "tag": tag, "to": target,
				"verdict": str(r["verdict"]), "why": str(r["why"]),
				"redundant": reg.Spelling[Fold(tag)] != ""})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if str(out[i]["author"]) != str(out[j]["author"]) {
			return str(out[i]["author"]) < str(out[j]["author"])
		}
		return str(out[i]["tag"]) < str(out[j]["tag"])
	})
	return out, nil
}

func MappingPath(c Config, author string) string {
	now := filepath.Join(c.Decisions, author+".yaml")
	if exists(now) {
		return now
	}
	old := filepath.Join(c.Root, "tagmaps", author+".yaml")
	if exists(old) {
		return old
	}
	return now
}
func LoadMapping(c Config, author string) Record {
	out := Record{}
	for _, v := range array(optionalYAML(MappingPath(c, author))["mapping"]) {
		r := record(v)
		if truth(r["tag"]) {
			out[str(r["tag"])] = r
		}
	}
	return out
}
func addProposals(item Record, tags []string, why string) {
	p := nested(item, "provenance")
	entries := array(p["proposed_tags"])
	seen := map[string]bool{}
	for _, x := range entries {
		seen[str(record(x)["tag"])] = true
	}
	for _, t := range tags {
		if !seen[t] {
			entries = append(entries, Record{"tag": t, "why": why})
			seen[t] = true
		}
	}
	if len(entries) > 0 {
		p["proposed_tags"] = entries
	}
}
func RetagTree(c Config, author string, write bool) (Record, error) {
	reg, e := LoadRegistry(c.RegistryPath())
	if e != nil {
		return nil, e
	}
	docs, e := Documents(c.Content, "item")
	if e != nil {
		return nil, e
	}
	out := Record{"scanned": 0, "changed": 0, "proposed": 0, "unresolved": Record{}}
	for _, d := range docs {
		if author != "" && str(d.Data["author"]) != author {
			continue
		}
		if _, ok := d.Data["tags"]; !ok {
			continue
		}
		out["scanned"] = integer(out["scanned"]) + 1
		want, unresolved := reg.Retag(texts(d.Data["tags"]), LoadMapping(c, str(d.Data["author"])))
		for _, t := range unresolved {
			m := record(out["unresolved"])
			m[t] = integer(m[t]) + 1
		}
		if equivalent(texts(d.Data["tags"]), want) {
			continue
		}
		out["changed"] = integer(out["changed"]) + 1
		d.Data["tags"] = want
		addProposals(d.Data, unresolved, "was on the item; not in the registry and no ruling covers it")
		out["proposed"] = integer(out["proposed"]) + len(unresolved)
		if write {
			if _, e = SaveDocument(d.Path, d.Data); e != nil {
				return out, e
			}
		}
	}
	return out, nil
}
func Decisions(rulings []any) map[string]string {
	out := map[string]string{}
	for _, x := range rulings {
		r := record(x)
		tag := strings.TrimSpace(str(r["tag"]))
		if tag == "" {
			continue
		}
		switch str(r["verdict"]) {
		case "approve", "rework":
			out[tag] = strings.TrimSpace(str(first(r["name"], tag)))
		case "merge":
			out[tag] = strings.TrimSpace(str(r["merge_into"]))
		default:
			out[tag] = ""
		}
	}
	return out
}
func ApplyRulings(c Config, rulings []any, write bool) (Record, error) {
	reg, e := LoadRegistry(c.RegistryPath())
	if e != nil {
		return nil, e
	}
	decided := Decisions(rulings)
	report := Record{"registry": []any{}, "tagged": 0, "removed": 0, "items": 0, "refused": []string{}}
	for _, x := range rulings {
		r := record(x)
		if !contains([]string{"approve", "rework"}, str(r["verdict"])) {
			continue
		}
		name := strings.TrimSpace(str(first(r["name"], r["tag"])))
		meaning := strings.TrimSpace(str(r["description"]))
		if name == "" || meaning == "" || reg.Spelling[Fold(name)] != "" {
			continue
		}
		kind, key, err := placeIn(reg, name)
		if err != nil {
			// One unusable ruling is not a reason to drop ninety good ones on
			// the floor, but it must be said out loud rather than skipped
			// quietly: the tag it names goes on being proposed until somebody
			// rules on it properly.
			report["refused"] = append(texts(report["refused"]), name+": "+err.Error())
			continue
		}
		nested(reg.Data, kind)[key] = meaning
		reg = NewRegistry(reg.Data)
		report["registry"] = append(array(report["registry"]), []string{name, meaning})
	}
	for raw, target := range decided {
		if target == "" {
			// "Already in the registry" is a ruling *for* the tag, not against it.
			// It reaches here because a creator's map sent the tag at a spelling
			// the registry does not have, so `retag` took it off the item and
			// proposed it; the adjudicator's answer is that the item was right.
			// Resolving it to the registry's own spelling puts it back. Treating
			// it as a bare drop instead cleared the proposal and left the
			// recording without a tag the registry sanctions -- 66 of the 93
			// rulings in one run here, and silent every time, because a tag that
			// is never proposed again is a tag nobody is told about.
			if canonical := reg.Spelling[Fold(raw)]; canonical != "" {
				decided[raw] = canonical
			}
			continue
		}
		canonical := reg.Spelling[Fold(target)]
		if canonical == "" {
			return report, fmt.Errorf("ruling for %q names an unregistered target %q", raw, target)
		}
		decided[raw] = canonical
	}
	docs, e := Documents(c.Content, "item")
	if e != nil {
		return nil, e
	}
	changes := []Document{}
	for _, d := range docs {
		p := record(d.Data["provenance"])
		left := []any{}
		tags := texts(d.Data["tags"])
		gained, dropped := []string{}, []string{}
		ruled := false
		for _, x := range array(p["proposed_tags"]) {
			was := strings.TrimSpace(str(record(x)["tag"]))
			becomes, ok := decided[was]
			if !ok {
				left = append(left, x)
				continue
			}
			ruled = true
			if becomes != "" && !contains(tags, becomes) {
				tags = append(tags, becomes)
				gained = append(gained, becomes)
			}
			if was != becomes && contains(tags, was) && !(becomes == "" && reg.Has(was)) {
				tags = without(tags, was)
				dropped = append(dropped, was)
			}
		}
		if !ruled {
			continue
		}
		if len(gained)+len(dropped) > 0 {
			d.Data["tags"] = tags
			en := record(p["enriched"])
			if len(en) > 0 {
				added := uniqueStrings(append(texts(en["tags_added"]), gained...))
				for _, s := range dropped {
					added = without(added, s)
				}
				sort.Strings(added)
				en["tags_added"] = added
			}
		}
		if len(left) > 0 {
			p["proposed_tags"] = left
		} else {
			delete(p, "proposed_tags")
		}
		report["items"] = integer(report["items"]) + 1
		report["tagged"] = integer(report["tagged"]) + len(gained)
		report["removed"] = integer(report["removed"]) + len(dropped)
		changes = append(changes, d)
	}
	if write {
		if len(array(report["registry"])) > 0 {
			if _, e = SaveDocument(c.RegistryPath(), reg.Data); e != nil {
				return report, e
			}
		}
		for _, d := range changes {
			if _, e = SaveDocument(d.Path, d.Data); e != nil {
				return report, e
			}
		}
	}
	return report, nil
}
func without(a []string, s string) []string {
	out := []string{}
	for _, v := range a {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}
