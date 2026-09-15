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

const SameTitle = "same title, one suffixed"
const SameCreator = "same creator, different titles"
const CrossCreator = "different creators"

// Fields a merge takes from every document at once rather than from one.
var unionFields = []string{"tags", "categories"}

var numericSuffix = regexp.MustCompile(`-[0-9]+$`)
var notTitle = regexp.MustCompile(`(?i)mixdown|_h265|-h265|[-_](hq|lq)$|[\s._-]mp3$|\.(mp3|wav|m4a)\b|femdom erotic hypnosis|fdhypno|\.com\b`)
var runTogether = regexp.MustCompile(`(?i)^[a-z0-9]{10,}$`)

func stemOf(p string) string { return strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)) }
func FindDuplicates(content string) (map[string][][]Document, error) {
	docs, e := Documents(content, "item")
	if e != nil {
		return nil, e
	}
	byFP := map[string][]Document{}
	order := []string{}
	for _, d := range docs {
		fp := str(record(d.Data["provenance"])["fingerprint"])
		if fp != "" {
			if _, ok := byFP[fp]; !ok {
				order = append(order, fp)
			}
			byFP[fp] = append(byFP[fp], d)
		}
	}
	out := map[string][][]Document{SameTitle: {}, SameCreator: {}, CrossCreator: {}}
	for _, fp := range order {
		g := byFP[fp]
		if len(g) < 2 {
			continue
		}
		authors, bases := map[string]bool{}, map[string]bool{}
		for _, d := range g {
			authors[str(d.Data["author"])] = true
			bases[numericSuffix.ReplaceAllString(stemOf(d.Path), "")] = true
		}
		kind := SameCreator
		if len(authors) > 1 {
			kind = CrossCreator
		} else if len(bases) == 1 {
			kind = SameTitle
		}
		out[kind] = append(out[kind], g)
	}
	return out, nil
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func Richness(r Record) []int {
	return []int{boolInt(truth(r["source_url"])), len(array(r["spoilers"])), len([]rune(str(r["description"]))), len(array(r["tags"])), len([]rune(str(r["summary"])))}
}
func TitleQuality(title string) []int {
	t := strings.TrimSpace(title)
	return []int{boolInt(!notTitle.MatchString(t)), boolInt(!runTogether.MatchString(t)), boolInt(strings.Contains(t, " ")), boolInt(!strings.Contains(t, "_")), len([]rune(t))}
}
func TitleIsFilename(title string) bool {
	return notTitle.MatchString(title) || runTogether.MatchString(title) || (!strings.Contains(title, " ") && len([]rune(title)) > 12)
}
func ResolutionRank(d Document) []int {
	r := d.Data
	u := str(r["source_url"])
	rank := []int{boolInt(u != "" && strings.Count(strings.TrimRight(u, "/"), "/") > 2)}
	rank = append(rank, TitleQuality(str(r["title"]))...)
	return append(rank, boolInt(truth(r["series"])), boolInt(u != ""), boolInt(!numericSuffix.MatchString(stemOf(d.Path))), len(array(r["spoilers"])), len(array(r["tags"])), len([]rune(str(r["description"]))))
}
func rankGreater(a, b []int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}
func MergeDuplicate(keep, drop Record) Record {
	m := clone(keep)
	for k, v := range drop {
		if contains([]string{"id", "title", "provenance", "needs"}, k) {
			continue
		}
		if !truth(m[k]) && truth(v) {
			m[k] = v
		}
	}
	for _, k := range unionFields {
		a := uniqueStrings(append(texts(m[k]), texts(drop[k])...))
		if len(a) > 0 {
			m[k] = a
		}
	}
	other := strings.TrimSpace(str(drop["title"]))
	if other != "" && other != strings.TrimSpace(str(m["title"])) {
		m["also_titled"] = uniqueStrings(append(texts(m["also_titled"]), other))
	}
	if truth(drop["id"]) {
		p := nested(m, "provenance")
		p["merged_ids"] = uniqueStrings(append(texts(p["merged_ids"]), str(drop["id"])))
	}
	return m
}

// MergeProvenance builds the survivor's provenance from the whole group.
//
// It exists because the obvious thing does not work. `MergeDuplicate` skips
// provenance on purpose -- provenance is about where a *document* came from,
// and gap-filling it field by field between two documents would invent a
// history neither of them has. But the fold seeds the merged record from the
// richest document in the group, which is often not the survivor, and the
// survivor's identity is then stamped back on afterwards. Everything else the
// survivor's provenance knew went with the seed: `archive_path`, `demuxed`,
// `original_title`, `titled_by`, and -- worse -- `merged_source_keys`, the
// record of a fold that already happened. Folding a group twice would erase the
// first fold's evidence.
//
// So: the survivor's provenance is the base, and a key it does not have is
// taken from the others in rank order. The one key that cannot be treated that
// way is `generated`.
//
// `generated` names the fields this toolchain wrote rather than found, and the
// merged record takes its fields from several documents. A union over-marks and
// the survivor's own list under-marks, and those two errors are not equal:
// marking a field generated that a person actually wrote tells them their own
// writing was machine-made, which is the error this library rules out first. So
// a field is marked here only when the value that survived demonstrably came
// from a document that marked it -- and not when any document holding that same
// value declined to.
func MergeProvenance(winner Document, group []Document, merged Record) Record {
	out := clone(record(winner.Data["provenance"]))
	rest := []Document{}
	for _, d := range group {
		if d.Path != winner.Path {
			rest = append(rest, d)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool {
		return rankGreater(Richness(rest[i].Data), Richness(rest[j].Data))
	})
	for _, d := range rest {
		for k, v := range record(d.Data["provenance"]) {
			if k == "generated" {
				continue
			}
			if !truth(out[k]) && truth(v) {
				out[k] = v
			}
		}
	}
	// A field is marked only if every document that put something into the
	// merged value said it wrote that something. What counts as putting
	// something in depends on the field: a union field takes from everyone who
	// holds one, and every other field takes from whoever held the value that
	// survived. Two documents that disagree about a field they both hold leave
	// it unmarked, which is the direction that cannot misattribute a person's
	// writing to a machine.
	named := map[string]bool{}
	for _, d := range group {
		for _, f := range texts(record(d.Data["provenance"])["generated"]) {
			named[f] = true
		}
	}
	made := []string{}
	for f := range named {
		all, any := true, false
		for _, d := range group {
			listed := contains(texts(record(d.Data["provenance"])["generated"]), f)
			gave := sameValue(merged[f], d.Data[f])
			if contains(unionFields, f) {
				gave = truth(d.Data[f])
			}
			if !gave {
				continue
			}
			any = true
			all = all && listed
		}
		if any && all {
			made = append(made, f)
		}
	}
	sort.Strings(made)
	if len(made) > 0 {
		out["generated"] = made
	} else {
		delete(out, "generated")
	}
	return out
}

// sameValue compares two document fields without caring how YAML typed them.
func sameValue(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func ResolveDuplicates(groups [][]Document, decisions map[string]bool, content string, fold, write bool) ([]string, error) {
	winners := make([]Document, len(groups))
	manual := make([]bool, len(groups))
	for i, g := range groups {
		ranked := append([]Document{}, g...)
		sort.SliceStable(ranked, func(i, j int) bool {
			if fold {
				return rankGreater(Richness(ranked[i].Data), Richness(ranked[j].Data))
			}
			return rankGreater(ResolutionRank(ranked[i]), ResolutionRank(ranked[j]))
		})
		winners[i] = ranked[0]
		for _, d := range g {
			rel, _ := filepath.Rel(content, d.Path)
			if decisions[rel] || decisions[d.Path] {
				winners[i] = d
				manual[i] = true
				break
			}
		}
		if fold {
			for _, d := range g {
				if !numericSuffix.MatchString(stemOf(d.Path)) {
					winners[i] = d
					break
				}
			}
		}
	}
	if !fold {
		taken := map[string][]int{}
		for _, byHand := range []bool{true, false} {
			for i, w := range winners {
				if manual[i] == byHand {
					k := str(w.Data["author"]) + "\x00" + str(w.Data["title"])
					taken[k] = append(taken[k], i)
				}
			}
		}
		for _, key := range sortedKeys(taken) {
			sharing := taken[key]
			for _, i := range sharing[min(1, len(sharing)):] {
				if manual[i] {
					continue
				}
				ranked := append([]Document{}, groups[i]...)
				sort.SliceStable(ranked, func(i, j int) bool { return rankGreater(ResolutionRank(ranked[i]), ResolutionRank(ranked[j])) })
				for _, d := range ranked {
					k := str(d.Data["author"]) + "\x00" + str(d.Data["title"])
					if _, ok := taken[k]; !ok {
						winners[i] = d
						taken[k] = []int{i}
						break
					}
				}
			}
		}
	}
	lines := []string{}
	for i, g := range groups {
		winner := winners[i]
		losers := []Document{}
		for _, d := range g {
			if d.Path != winner.Path {
				losers = append(losers, d)
			}
		}
		if len(losers) == 0 {
			continue
		}
		// Rebase the fields before merging so a richer entry in another folder
		// cannot leave the survivor pointing at that folder's relative assets.
		rebase := func(d Document) Record {
			r := clone(d.Data)
			for _, f := range []string{"audio", "cover", "image", "video"} {
				v := str(r[f])
				if v == "" || filepath.IsAbs(v) || strings.Contains(v, "://") {
					continue
				}
				target := filepath.Join(filepath.Dir(d.Path), v)
				if rel, err := filepath.Rel(filepath.Dir(winner.Path), target); err == nil {
					r[f] = rel
				}
			}
			return r
		}
		merged := clone(winner.Data)
		if fold {
			ranked := append([]Document{}, g...)
			sort.SliceStable(ranked, func(i, j int) bool { return rankGreater(Richness(ranked[i].Data), Richness(ranked[j].Data)) })
			merged = rebase(ranked[0])
			for _, d := range ranked[1:] {
				merged = MergeDuplicate(merged, rebase(d))
			}
		} else {
			sort.SliceStable(losers, func(i, j int) bool { return rankGreater(ResolutionRank(losers[i]), ResolutionRank(losers[j])) })
			for _, d := range losers {
				merged = MergeDuplicate(merged, rebase(d))
				if len(array(d.Data["spoilers"])) > len(array(merged["spoilers"])) {
					merged["spoilers"] = d.Data["spoilers"]
				}
			}
		}
		identity := []string{"id", "title"}
		if !fold {
			identity = append(identity, "series", "source_url")
		}
		for _, f := range identity {
			if truth(winner.Data[f]) {
				merged[f] = winner.Data[f]
			} else if !fold {
				delete(merged, f)
			}
		}
		merged["provenance"] = MergeProvenance(winner, g, merged)
		p := nested(merged, "provenance")
		wp := record(winner.Data["provenance"])
		for _, f := range []string{"source_key", "fingerprint"} {
			if wp[f] != nil {
				p[f] = wp[f]
			} else {
				delete(p, f)
			}
		}
		absorbed := texts(p["merged_source_keys"])
		gone := texts(p["merged_ids"])
		if fold {
			gone = nil
		}
		loserNames := []string{}
		for _, d := range losers {
			lp := record(d.Data["provenance"])
			if truth(lp["source_key"]) {
				absorbed = append(absorbed, str(lp["source_key"]))
			}
			absorbed = append(absorbed, texts(lp["merged_source_keys"])...)
			if truth(d.Data["id"]) {
				gone = append(gone, str(d.Data["id"]))
			}
			loserNames = append(loserNames, filepath.Base(filepath.Dir(d.Path))+"/"+stemOf(d.Path))
		}
		absorbed = without(uniqueStrings(absorbed), str(p["source_key"]))
		gone = without(uniqueStrings(gone), str(merged["id"]))
		sort.Strings(absorbed)
		sort.Strings(gone)
		if len(absorbed) > 0 {
			p["merged_source_keys"] = absorbed
		}
		if len(gone) > 0 {
			p["merged_ids"] = gone
		} else {
			delete(p, "merged_ids")
		}
		lines = append(lines, fmt.Sprintf("%s/%s <- %s", filepath.Base(filepath.Dir(winner.Path)), stemOf(winner.Path), strings.Join(loserNames, ", ")))
		if !write {
			continue
		}
		if _, e := SaveDocument(winner.Path, merged); e != nil {
			return lines, e
		}
		for _, d := range losers {
			if e := os.Remove(d.Path); e != nil {
				return lines, e
			}
			if !fold {
				tp := strings.TrimSuffix(d.Path, filepath.Ext(d.Path)) + ".transcript.yaml"
				if exists(tp) {
					if e := os.Remove(tp); e != nil {
						return lines, e
					}
				}
			}
		}
	}
	return lines, nil
}
