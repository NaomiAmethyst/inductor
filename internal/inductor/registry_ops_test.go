// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"path/filepath"
	"testing"
)

// A library with one creator, one item, and a map -- enough for every place a
// tag is written down to be checked at once.
func registryFixture(t *testing.T) (Config, string) {
	t.Helper()
	c := testConfig(t)
	item := filepath.Join(c.Content, "creator", "one.yaml")
	putRecord(t, item, Record{"kind": "Item", "id": "one", "author": "creator", "title": "One",
		"tags": []string{"Hypnosis", "Relaxation"},
		"provenance": Record{
			"enriched":      Record{"tags_added": []string{"Relaxation"}},
			"proposed_tags": []any{Record{"tag": "Humour", "why": "not in the registry"}},
		}})
	putRecord(t, MappingPath(c, "creator"), Record{"author": "creator", "mapping": []any{
		Record{"tag": "calm", "verdict": "map", "to": "Relaxation"},
		Record{"tag": "hypno", "verdict": "map", "to": "Hypnosis"},
	}})
	return c, item
}

func TestRegistryAddAcceptsTheProposalsThatWereWaitingForIt(t *testing.T) {
	c, item := registryFixture(t)
	if _, err := RegistryAdd(c, "Humour", "Comedy is part of the intent.", "the library has ten of them", true); err != nil {
		t.Fatal(err)
	}
	reg, err := LoadRegistry(c.RegistryPath())
	if err != nil {
		t.Fatal(err)
	}
	if !reg.Has("Humour") {
		t.Fatal("the tag did not reach the registry")
	}
	got := optionalYAML(item)
	// The point of adding it: the recording that proposed it now has it.
	if !contains(texts(got["tags"]), "Humour") {
		t.Fatal("the proposal was not honoured:", got["tags"])
	}
	if truth(record(got["provenance"])["proposed_tags"]) {
		t.Fatal("a settled proposal should be cleared:", record(got["provenance"])["proposed_tags"])
	}
	rows, err := ReadLedger(filepath.Join(c.Decisions, "rulings.yaml"))
	if err != nil || len(rows) == 0 {
		t.Fatal("adding a tag by hand must leave a ruling behind:", rows, err)
	}
}

func TestRegistryAddRefusesADuplicateAndAnEmptyMeaning(t *testing.T) {
	c, _ := registryFixture(t)
	if _, err := RegistryAdd(c, "hypnosis", "Already there under another case.", "", true); err == nil {
		t.Fatal("a tag that folds onto an existing one is not a new tag")
	}
	if _, err := RegistryAdd(c, "Brand New", "", "", true); err == nil {
		t.Fatal("a registry entry with no meaning is not worth having")
	}
}

func TestRegistryDescribeMovesNothingElse(t *testing.T) {
	c, item := registryFixture(t)
	before := optionalYAML(item)
	if _, err := RegistryDescribe(c, "Hypnosis", "A hypnotic recording, revised.", true); err != nil {
		t.Fatal(err)
	}
	reg, _ := LoadRegistry(c.RegistryPath())
	if reg.Meanings["Hypnosis"] != "A hypnotic recording, revised." {
		t.Fatal("the meaning did not change:", reg.Meanings["Hypnosis"])
	}
	if !equivalent(optionalYAML(item)["tags"], before["tags"]) {
		t.Fatal("renaming nothing should touch no item")
	}
}

func TestRegistryRemoveLeavesTheTagProposedRatherThanForgotten(t *testing.T) {
	c, item := registryFixture(t)
	if _, err := RegistryRemove(c, "Relaxation", "too vague to browse by", true); err != nil {
		t.Fatal(err)
	}
	reg, _ := LoadRegistry(c.RegistryPath())
	if reg.Has("Relaxation") {
		t.Fatal("the tag is still in the registry")
	}
	got := optionalYAML(item)
	if contains(texts(got["tags"]), "Relaxation") {
		t.Fatal("the tag is still on the item:", got["tags"])
	}
	// The item said this about itself once; a registry edit does not erase that.
	p := record(got["provenance"])
	found := false
	for _, v := range array(p["proposed_tags"]) {
		if str(record(v)["tag"]) == "Relaxation" {
			found = true
			if str(record(v)["why"]) == "" {
				t.Fatal("a proposal with no reason is the thing this is meant to avoid")
			}
		}
	}
	if !found {
		t.Fatal("the tag was dropped without being proposed back:", p["proposed_tags"])
	}
	if contains(texts(record(p["enriched"])["tags_added"]), "Relaxation") {
		t.Fatal("the item still claims a run added a tag it no longer has")
	}
	// A map may never point at a tag the registry lacks.
	m := optionalYAML(MappingPath(c, "creator"))
	for _, v := range array(m["mapping"]) {
		if str(record(v)["to"]) == "Relaxation" {
			t.Fatal("a map row was left pointing at the removed tag")
		}
	}
	if len(array(m["retired"])) != 1 {
		t.Fatal("the row should be retired with its reasoning, not deleted:", m["retired"])
	}
}

func TestRegistryRenameFollowsTheTagEverywhere(t *testing.T) {
	c, item := registryFixture(t)
	if _, err := RegistryRename(c, "Relaxation", "CW: Relaxation", "", true); err != nil {
		t.Fatal(err)
	}
	reg, _ := LoadRegistry(c.RegistryPath())
	if reg.Has("Relaxation") || !reg.Has("CW: Relaxation") {
		t.Fatal("the rename did not land in the registry")
	}
	// It crossed namespaces, so it must have moved block as well as name.
	if truth(record(reg.Data["content"])["Relaxation"]) {
		t.Fatal("the old entry is still in the content block")
	}
	got := optionalYAML(item)
	if !contains(texts(got["tags"]), "CW: Relaxation") || contains(texts(got["tags"]), "Relaxation") {
		t.Fatal("the item kept the old name:", got["tags"])
	}
	if !contains(texts(record(record(got["provenance"])["enriched"])["tags_added"]), "CW: Relaxation") {
		t.Fatal("tags_added did not follow the rename")
	}
	m := optionalYAML(MappingPath(c, "creator"))
	for _, v := range array(m["mapping"]) {
		if str(record(v)["tag"]) == "calm" && str(record(v)["to"]) != "CW: Relaxation" {
			t.Fatal("the map still points at the old name:", record(v)["to"])
		}
	}
}

func TestRegistryMergeElidesTheDuplicateItWouldCreate(t *testing.T) {
	c, item := registryFixture(t)
	if _, err := RegistryMerge(c, "Relaxation", "Hypnosis", true); err != nil {
		t.Fatal(err)
	}
	got := optionalYAML(item)
	// The item held both, so the merge must leave exactly one.
	n := 0
	for _, tag := range texts(got["tags"]) {
		if tag == "Hypnosis" {
			n++
		}
	}
	if n != 1 || contains(texts(got["tags"]), "Relaxation") {
		t.Fatal("merge left a duplicate or the absorbed tag:", got["tags"])
	}
	reg, _ := LoadRegistry(c.RegistryPath())
	if reg.Has("Relaxation") {
		t.Fatal("the absorbed tag is still registered")
	}
	m := optionalYAML(MappingPath(c, "creator"))
	for _, v := range array(m["mapping"]) {
		if str(record(v)["to"]) == "Relaxation" {
			t.Fatal("a map row still points at the absorbed tag")
		}
	}
}

func TestRegistryMergeAndRenameRefuseTheImpossible(t *testing.T) {
	c, _ := registryFixture(t)
	if _, err := RegistryMerge(c, "Relaxation", "Nowhere", true); err == nil {
		t.Fatal("merging into a tag the registry lacks would break the invariant")
	}
	if _, err := RegistryRename(c, "Relaxation", "Hypnosis", "", true); err == nil {
		t.Fatal("renaming onto an existing tag is a merge, and should say so")
	}
	if _, err := RegistryRemove(c, "Nowhere", "", true); err == nil {
		t.Fatal("removing what is not there should be an error, not a no-op")
	}
}

func TestRegistryWritesNothingWithoutWrite(t *testing.T) {
	c, item := registryFixture(t)
	before := optionalYAML(item)
	if _, err := RegistryRemove(c, "Relaxation", "thinking about it", false); err != nil {
		t.Fatal(err)
	}
	reg, _ := LoadRegistry(c.RegistryPath())
	if !reg.Has("Relaxation") {
		t.Fatal("a dry run changed the registry")
	}
	if !equivalent(optionalYAML(item)["tags"], before["tags"]) {
		t.Fatal("a dry run changed an item")
	}
}
func TestAPrefixThatIsNotANamespaceIsRefused(t *testing.T) {
	c, _ := registryFixture(t)
	if _, err := RegistryAdd(c, "CWW: Blood", "A typo for CW.", "", true); err == nil {
		t.Fatal("a mistyped prefix would become a content tag named after the mistake")
	}
	// A colon inside the name is not a prefix: ten trigger entries read this way.
	if _, err := RegistryAdd(c, "Trigger: Slave Mode : Sybian", "A keyed state.", "", true); err != nil {
		t.Fatal("a namespaced tag whose name contains a colon is legitimate:", err)
	}
	reg, _ := LoadRegistry(c.RegistryPath())
	if !reg.Has("Trigger: Slave Mode : Sybian") {
		t.Fatal("the entry did not land under its namespace:", reg.Data["trigger"])
	}
	if truth(record(reg.Data["trigger"])["Slave Mode : Sybian"]) == false {
		t.Fatal("the key kept its prefix instead of being stripped:", reg.Data["trigger"])
	}
}
func bulkFile(t *testing.T, dir string, changes []any) string {
	t.Helper()
	p := filepath.Join(dir, "changes.yaml")
	putRecord(t, p, Record{"apiVersion": "inductor/v1", "kind": "TagBulkUpdate", "changes": changes})
	return p
}

func TestBulkAppliesInOrderSoALaterChangeSeesAnEarlierOne(t *testing.T) {
	c, item := registryFixture(t)
	// The merge names a tag that does not exist until the add above it runs.
	// This is the whole reason a bulk edit is not five separate commands.
	file := bulkFile(t, t.TempDir(), []any{
		Record{"tag": "Humour", "action": "Add", "description": "Comedy is part of the intent."},
		Record{"tag": "Relaxation", "action": "Merge", "target": "Humour"},
		Record{"tag": "Hypnosis", "action": "Update", "description": "A hypnotic recording, revised."},
	})
	if _, err := RegistryBulk(c, file, true); err != nil {
		t.Fatal(err)
	}
	reg, _ := LoadRegistry(c.RegistryPath())
	if !reg.Has("Humour") || reg.Has("Relaxation") {
		t.Fatal("the add and the merge did not both land:", sortedKeys(reg.Meanings))
	}
	if reg.Meanings["Hypnosis"] != "A hypnotic recording, revised." {
		t.Fatal("the update did not land:", reg.Meanings["Hypnosis"])
	}
	got := optionalYAML(item)
	if !contains(texts(got["tags"]), "Humour") || contains(texts(got["tags"]), "Relaxation") {
		t.Fatal("the item did not follow the merge:", got["tags"])
	}
}

func TestBulkCarriesAChangeThroughASecondChange(t *testing.T) {
	c, item := registryFixture(t)
	// Rename A to B, then merge B into C. Anything that held A has to end on C,
	// not on the B that no longer exists.
	file := bulkFile(t, t.TempDir(), []any{
		Record{"tag": "Relaxation", "action": "Rename", "target": "Calm"},
		Record{"tag": "Calm", "action": "Merge", "target": "Hypnosis"},
	})
	if _, err := RegistryBulk(c, file, true); err != nil {
		t.Fatal(err)
	}
	got := texts(optionalYAML(item)["tags"])
	if contains(got, "Relaxation") || contains(got, "Calm") {
		t.Fatal("an intermediate name survived on the item:", got)
	}
	if len(got) != 1 || got[0] != "Hypnosis" {
		t.Fatal("the item should hold exactly the survivor:", got)
	}
	m := optionalYAML(MappingPath(c, "creator"))
	for _, v := range array(m["mapping"]) {
		if to := str(record(v)["to"]); to == "Relaxation" || to == "Calm" {
			t.Fatal("a map row was left on an intermediate name:", to)
		}
	}
}

func TestBulkAppliesNothingWhenAnyChangeIsBad(t *testing.T) {
	c, item := registryFixture(t)
	before := optionalYAML(item)
	file := bulkFile(t, t.TempDir(), []any{
		Record{"tag": "Humour", "action": "Add", "description": "Fine."},
		Record{"tag": "Nowhere", "action": "Remove"},
		Record{"tag": "Relaxation", "action": "Sideways"},
	})
	report, err := RegistryBulk(c, file, true)
	if err == nil {
		t.Fatal("a file with two bad changes should not apply")
	}
	if n := len(texts(report["problems"])); n != 2 {
		t.Fatal("both problems should be reported at once, not just the first:", report["problems"])
	}
	reg, _ := LoadRegistry(c.RegistryPath())
	if reg.Has("Humour") {
		t.Fatal("the good change ahead of the bad one was applied anyway")
	}
	if !equivalent(optionalYAML(item)["tags"], before["tags"]) {
		t.Fatal("an item changed despite the refusal")
	}
}

func TestBulkRefusesTheWrongKindOfFile(t *testing.T) {
	c, _ := registryFixture(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "notachangelist.yaml")
	putRecord(t, p, Record{"apiVersion": "inductor/v1", "kind": "Item", "id": "one"})
	if _, err := RegistryBulk(c, p, true); err == nil {
		t.Fatal("a file that declares itself an Item is not a change list")
	}
}
func TestRegistryAboutSetsWhatANamespaceIsFor(t *testing.T) {
	c, _ := registryFixture(t)
	if _, err := RegistryAbout(c, "cw", "What a recording touches on rather than is about.", true); err != nil {
		t.Fatal(err)
	}
	reg, _ := LoadRegistry(c.RegistryPath())
	if str(record(reg.Data["cw"])["_about"]) == "" {
		t.Fatal("the namespace description did not land:", reg.Data["cw"])
	}
	// It is not a tag, and must never become one.
	if reg.Has("_about") || reg.Spelling[Fold("CW: _about")] != "" {
		t.Fatal("_about leaked into the vocabulary")
	}
	if _, err := RegistryAbout(c, "nonsense", "x", true); err == nil {
		t.Fatal("only the eight namespaces have an _about")
	}
}
func TestABareWordRowDoesNotCatchANamespacedRegistryTag(t *testing.T) {
	r := NewRegistry(Record{
		"content":  Record{"Moaning": "Moaning.", "Relaxation": "Relaxing."},
		"trigger":  Record{"Moans": "The speaker's moaning acts as the cue."},
		"audience": Record{"sissy": "Addressed to a man being feminised."}})
	m := Record{"Moans": Record{"verdict": "map", "to": "Moaning"}}
	// The creator's row is about their own bare word.
	if got, _ := r.Resolve("Moans", m); got != "Moaning" {
		t.Fatal("the row should still answer for the creator's own spelling:", got)
	}
	// It is not about the registry's trigger of the same name.
	if got, _ := r.Resolve("Trigger: Moans", m); got != "Trigger: Moans" {
		t.Fatal("a bare row rewrote a namespaced registry tag:", got)
	}
	// Nor does it reach a namespaced tag the registry does spell out. Once a tag
	// is the registry's own spelling on an item, the way to rule on it is to
	// name it in full -- which is what "the registry wins" means here.
	if got, _ := r.Resolve("Audience: sissy", Record{"sissy": Record{"verdict": "drop"}}); got != "Audience: sissy" {
		t.Fatal("a bare row reached a canonical registry tag:", got)
	}
	if got, _ := r.Resolve("Audience: sissy", Record{"Audience: sissy": Record{"verdict": "drop"}}); got != "" {
		t.Fatal("a row naming the tag in full must still rule on it:", got)
	}
	// The fallback remains for a namespaced spelling the registry never had.
	if got, _ := r.Resolve("Production: No Binaurals", Record{"No Binaurals": Record{"verdict": "map", "to": "Relaxation"}}); got != "Relaxation" {
		t.Fatal("the fallback should still place a spelling the registry lacks:", got)
	}
}
