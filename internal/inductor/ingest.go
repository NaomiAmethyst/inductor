// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Planned struct {
	Source     *Source
	Stem, Path string
}
type PlanOptions struct {
	Authors     []string
	Limit       int
	Needs       []string
	Redo, Fresh bool
	Only        []string
}

func PlanSources(c Config, report SourceReport, index *ItemIndex, fp *FingerprintCache, o PlanOptions) ([]Planned, error) {
	entries, e := index.Entries(c.Content)
	if e != nil {
		return nil, e
	}
	taken := map[string]map[string]bool{}
	known := map[string]Document{}
	byAudio := map[string]Document{}
	answered, claimed, finished, wanted := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	byID := map[string]string{}
	for _, d := range entries {
		f := d.Data
		a := str(f["author"])
		stem := strings.TrimSuffix(filepath.Base(d.Path), filepath.Ext(d.Path))
		if taken[a] == nil {
			taken[a] = map[string]bool{}
		}
		taken[a][stem] = true
		byID[str(f["id"])] = a + "\x00" + stem
		if truth(f["fingerprint"]) {
			k := a + "\x00" + str(f["fingerprint"])
			if _, ok := byAudio[k]; !ok {
				byAudio[k] = d
			}
		}
		for _, k := range texts(f["merged_source_keys"]) {
			answered[k] = true
		}
		for _, field := range []string{"source_key", "audio"} {
			key := str(f[field])
			if key == "" {
				continue
			}
			k := a + "\x00" + key
			if _, ok := known[k]; !ok {
				known[k] = d
			}
			claimed[key] = true
			for _, need := range o.Needs {
				if contains(texts(f["needs"]), need) {
					wanted[key] = true
				}
			}
			if !truth(f["needs"]) && (truth(f["complete"]) || truth(f["enriched"])) {
				finished[key] = true
			}
		}
	}
	picked := map[string]bool{}
	for _, id := range o.Only {
		if pair := byID[id]; pair != "" {
			picked[pair] = true
		}
	}
	out := []Planned{}
	seen := map[string]bool{}
	for _, s := range report.Sources {
		a := s.AuthorID()
		if len(o.Authors) > 0 && !contains(o.Authors, a) {
			continue
		}
		spellings := []string{s.Audio, c.Portable(s.Audio, "")}
		skip := false
		matchedNeed := false
		var old Document
		for _, k := range spellings {
			if answered[k] || (o.Fresh && claimed[k]) {
				skip = true
			}
			if wanted[k] {
				matchedNeed = true
			}
			if !o.Redo && o.Only == nil && len(o.Needs) == 0 && finished[k] {
				skip = true
			}
			if old.Path == "" {
				old = known[a+"\x00"+k]
			}
		}
		if skip || (len(o.Needs) > 0 && !matchedNeed) {
			continue
		}
		if old.Path == "" {
			if audio := s.AudioPath(c.Sources); audio != "" {
				key, e := fp.Of(audio)
				if e == nil {
					old = byAudio[a+"\x00"+key]
				}
			}
		}
		stem := ""
		path := ""
		if old.Path != "" {
			path = old.Path
			stem = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		} else {
			if taken[a] == nil {
				taken[a] = map[string]bool{}
			}
			stem, e = Unique(Slug(s.Title), taken[a])
			if e != nil {
				return nil, e
			}
			path = filepath.Join(c.Content, a, stem+".yaml")
		}
		pair := a + "\x00" + stem
		if o.Only != nil && !picked[pair] {
			continue
		}
		if seen[pair] {
			continue
		}
		seen[pair] = true
		out = append(out, Planned{s, stem, path})
		if o.Limit > 0 && len(out) >= o.Limit {
			break
		}
	}
	return out, nil
}

type Engine struct {
	Config       Config
	API          *APIClient
	Fingerprints *FingerprintCache
	Index        *ItemIndex
	Store        *AnalysisStore
	Acoustic     AcousticStore
	Box          *GPUBox
	Say          func(string, ...any)
	boxOnce      sync.Once
	boxErr       error
	noProvision  bool
	locks        sync.Map
}

func NewEngine(c Config) *Engine {
	return &Engine{Config: c, API: NewAPIClient(c), Fingerprints: NewFingerprintCache(filepath.Join(c.Cache, "fingerprints.json")), Index: NewItemIndex(filepath.Join(c.Cache, "item-index.json")), Store: &AnalysisStore{Root: c.Analysis()}, Acoustic: AcousticStore{filepath.Join(c.Cache, "acoustic")}, Say: func(string, ...any) {}}
}
func (e *Engine) Close() error {
	if err := e.Fingerprints.Save(); err != nil {
		return err
	}
	return e.Index.Save()
}
func (e *Engine) fingerprint(j Planned) (string, error) {
	if j.Source.fingerprint != "" {
		return j.Source.fingerprint, nil
	}
	p := j.Source.AudioPath(e.Config.Sources)
	if p == "" {
		return "", fmt.Errorf("audio not found: %s", j.Source.Audio)
	}
	return e.Fingerprints.Of(p)
}
func (e *Engine) transcript(fp string) Record {
	return readJSON(filepath.Join(e.Config.Transcripts(), fp+".json"))
}
func (e *Engine) meta(j Planned, payload, measured Record) Record {
	author := j.Source.Author
	docs, _ := Documents(e.Config.Content, "author")
	for _, d := range docs {
		if str(d.Data["id"]) == j.Source.AuthorID() {
			author = str(first(d.Data["name"], author))
			break
		}
	}
	return Record{"title": j.Source.Title, "author_name": author, "duration": first(j.Source.Data["duration"], payload["duration"]), "series": j.Source.Data["series"], "existing_description": str(j.Source.Data["description"]), "measured": measured}
}
func (e *Engine) ensureBox(ctx context.Context) error {
	e.boxOnce.Do(func() {
		if e.Box == nil {
			e.Box = NewGPUBox(e.Config, "")
		}
		e.boxErr = e.Box.Provision(ctx, !e.noProvision)
		if e.boxErr == nil {
			e.boxErr = e.Box.Start(ctx)
		}
	})
	return e.boxErr
}
func (e *Engine) Transcribe(ctx context.Context, j Planned, embed, redo bool) error {
	fp, err := e.fingerprint(j)
	if err != nil {
		return err
	}
	lock, _ := e.locks.LoadOrStore(fmt.Sprintf("transcribe:%t:%s", embed, fp), &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	kind := "transcribe"
	dest := filepath.Join(e.Config.Transcripts(), fp+".json")
	if embed {
		kind = "embed"
		dest = filepath.Join(e.Config.Cache, "voiceprints", fp+".json")
	}
	if exists(dest) && !redo {
		return nil
	}
	if err = e.ensureBox(ctx); err != nil {
		return err
	}
	audio := PlacedAudio(e.Config, j.Source, j.Stem)
	if audio == "" {
		audio = j.Source.AudioPath(e.Config.Sources)
	}
	id := kind + "-" + fp
	landing := filepath.Join(e.Config.Cache, "remote-out")
	if redo {
		_ = os.Remove(filepath.Join(landing, id+".json"))
	}
	r, err := e.Box.Work(ctx, id, kind, audio, landing)
	if err != nil {
		return err
	}
	if embed {
		r["apiVersion"] = "inductor/v1"
		r["kind"] = "VoicePrint"
	}
	return writeJSON(dest, r)
}

type NothingToProduce struct{ Reason string }

func (n NothingToProduce) Error() string { return n.Reason }
func (e *Engine) Analyze(ctx context.Context, j Planned, redo bool) error {
	fp, err := e.fingerprint(j)
	if err != nil {
		return err
	}
	p := e.transcript(fp)
	if p == nil {
		return fmt.Errorf("no transcript yet")
	}
	s := Sentences(p)
	spoken := 0
	for _, v := range s {
		spoken += len([]rune(v.Text))
	}
	if spoken < 200 {
		return NothingToProduce{fmt.Sprintf("%d characters of transcript, too little to analyse", spoken)}
	}
	key := TranscriptKey(str(p["text"]))
	lock, _ := e.locks.LoadOrStore("analysis:"+key, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	cached := e.Store.Get(key, fp)
	if truth(cached["analysis"]) && !redo {
		return nil
	}
	reg, err := LoadRegistry(e.Config.RegistryPath())
	if err != nil {
		return err
	}
	result, err := e.API.ChatJSON(ctx, e.Config.Enrich.AnalysisModel, AnalysisMessages(s, e.meta(j, p, nil), reg), TokenCeiling, .15)
	if err != nil {
		return err
	}
	return e.Store.Put(key, Record{"analysis": PruneCitations(result, s)}, fp, e.Config.Enrich.AnalysisModel)
}
func (e *Engine) ReviewJobs(jobs []Planned, redo bool) ([]ReviewJob, error) {
	reg, err := LoadRegistry(e.Config.RegistryPath())
	if err != nil {
		return nil, err
	}
	out := []ReviewJob{}
	seen := map[string]bool{}
	for _, j := range jobs {
		fp, err := e.fingerprint(j)
		if err != nil {
			continue
		}
		p := e.transcript(fp)
		if p == nil {
			continue
		}
		key := TranscriptKey(str(p["text"]))
		r := e.Store.Get(key, fp)
		if !truth(r["analysis"]) || (!redo && truth(r["final"])) {
			continue
		}
		s := Sentences(p)
		m := e.Acoustic.Measurements(fp)
		if m == nil {
			m = Record{}
		}
		merge(m, Delivery(p))
		id := ItemID(j.Source.AuthorID(), j.Stem)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ReviewJob{id, ReviewMessages(record(r["analysis"]), s, e.meta(j, p, m), reg), fp, key, s})
	}
	return out, nil
}
func (e *Engine) RunReviews(ctx context.Context, jobs []ReviewJob, inFlight, batchSize int, poll time.Duration, recover []string) (int, error) {
	model := e.Config.Enrich.ReviewModel
	byID := map[string]ReviewJob{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	done := 0
	land := func(results map[string]string) error {
		for _, id := range sortedKeys(results) {
			j, ok := byID[id]
			if !ok {
				continue
			}
			final, err := ExtractJSON(results[id])
			if err != nil {
				e.Say("FAIL review %s: %v", id, err)
				continue
			}
			if err = e.Store.Put(j.Key, Record{"final": final, "review_model": model, "sentences": j.Sentences}, j.Fingerprint, ""); err != nil {
				return err
			}
			done++
		}
		return nil
	}
	for _, id := range recover {
		journal := readJSON(filepath.Join(e.Config.Cache, "batches", id+".json"))
		for _, v := range array(journal["jobs"]) {
			b, _ := jsonBytes(v)
			var j ReviewJob
			_ = decodeJSON(b, &j)
			if j.ID != "" {
				byID[j.ID] = j
			}
		}
		results, err := e.API.BatchWait(ctx, id, poll)
		if err != nil {
			return done, err
		}
		if err = land(results); err != nil {
			return done, err
		}
	}
	if len(recover) > 0 {
		return done, nil
	}
	if len(jobs) == 0 {
		return 0, nil
	}
	if !strings.HasSuffix(model, ":batch") {
		results, errs := parallelMap(ctx, jobs, max(1, e.Config.Enrich.Workers), func(ctx context.Context, j ReviewJob) (string, error) {
			text, _, err := e.API.Chat(ctx, model, j.Messages, TokenCeiling, .2)
			return text, err
		})
		for i, result := range results {
			if errs[i] != nil {
				e.Say("FAIL review %s: %v", jobs[i].ID, errs[i])
				continue
			}
			if err := land(map[string]string{jobs[i].ID: result}); err != nil {
				return done, err
			}
		}
		if done != len(jobs) {
			return done, fmt.Errorf("%d of %d reviews failed", len(jobs)-done, len(jobs))
		}
		return done, nil
	}
	chunks := [][]ReviewJob{}
	batchSize = max(1, batchSize)
	for i := 0; i < len(jobs); i += batchSize {
		chunks = append(chunks, jobs[i:min(i+batchSize, len(jobs))])
	}
	type batchResult struct {
		results map[string]string
		err     error
	}
	results, errs := parallelMap(ctx, chunks, max(1, inFlight), func(ctx context.Context, chunk []ReviewJob) (batchResult, error) {
		id, err := e.API.BatchSubmit(ctx, model, chunk)
		if err != nil {
			if strings.Contains(err.Error(), "does not have a :batch endpoint") {
				r := map[string]string{}
				for _, j := range chunk {
					text, _, err := e.API.Chat(ctx, model, j.Messages, TokenCeiling, .2)
					if err != nil {
						return batchResult{}, err
					}
					r[j.ID] = text
				}
				return batchResult{results: r}, nil
			}
			return batchResult{}, err
		}
		e.Say("submitted %d reviews -> %s", len(chunk), id)
		journal := filepath.Join(e.Config.Cache, "batches", id+".json")
		if err = writeJSON(journal, Record{"id": id, "model": model, "jobs": chunk, "status": "submitted"}); err != nil {
			return batchResult{}, fmt.Errorf("batch %s submitted but journal failed: %w; recover with --recover %s", id, err, id)
		}
		r, err := e.API.BatchWait(ctx, id, poll)
		return batchResult{r, err}, nil
	})
	var failures []string
	for i, result := range results {
		err := errs[i]
		if err == nil {
			err = result.err
		}
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		if err = land(result.results); err != nil {
			return done, err
		}
	}
	if len(failures) > 0 {
		return done, fmt.Errorf("review batches: %s", strings.Join(failures, "; "))
	}
	if done != len(jobs) {
		return done, fmt.Errorf("%d of %d reviews missing or invalid", len(jobs)-done, len(jobs))
	}
	return done, nil
}
func (e *Engine) Emit(ctx context.Context, j Planned, covers, overwrite, redraw bool) error {
	lock, _ := e.locks.LoadOrStore(j.Path, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	c := e.Config
	s := j.Source
	fp, err := e.fingerprint(j)
	if err != nil {
		return err
	}
	p := e.transcript(fp)
	cached := Record{}
	if p != nil {
		cached = e.Store.Get(TranscriptKey(str(p["text"])), fp)
	}
	final := record(cached["final"])
	before := optionalYAML(j.Path)
	item := clone(before)
	item["apiVersion"] = "hypnotica/v1"
	item["kind"] = "Item"
	item["id"] = str(first(item["id"], ItemID(s.AuthorID(), j.Stem)))
	item["author"] = s.AuthorID()
	if _, ok := item["title"]; !ok {
		item["title"] = s.Title
	}
	for _, f := range []string{"date", "duration", "series", "series_index", "cover", "source_url", "explicit", "categories", "variant"} {
		if item[f] == nil && s.Data[f] != nil {
			item[f] = s.Data[f]
		}
	}
	if item["duration"] == nil && truth(p["duration"]) {
		item["duration"] = rounded(number(p["duration"]), 1)
	}
	audio := PlacedAudio(c, s, j.Stem)
	if audio == "" {
		audio = s.AudioPath(c.Sources)
	}
	item["audio"] = audio
	if video := VideoTarget(c, s, j.Stem); video != "" && exists(video) {
		item["video"] = video
	}
	wrote := []string{}
	if !truth(item["description"]) && truth(s.Data["description"]) {
		item["description"] = Block(str(s.Data["description"]))
	}
	reg, err := LoadRegistry(c.RegistryPath())
	if err != nil {
		return err
	}
	given := texts(s.Data["tags"])
	standing := []string{}
	for _, t := range texts(item["tags"]) {
		if !contains(given, t) {
			standing = append(standing, t)
		}
	}
	theirsRaw := append(append([]string{}, given...), standing...)
	raw := uniqueStrings(append(append([]string{}, theirsRaw...), texts(final["tags"])...))
	mapping := LoadMapping(c, s.AuthorID())
	resolved, unresolved := reg.Retag(raw, mapping)
	theirs, _ := reg.Retag(theirsRaw, mapping)
	added := []string{}
	for _, t := range resolved {
		if !contains(theirs, t) {
			added = append(added, t)
		}
	}
	if len(raw) > 0 {
		item["tags"] = resolved
	}
	if len(added) > 0 {
		wrote = append(wrote, "tags")
	}
	prov := nested(item, "provenance")
	if len(final) > 0 {
		if truth(final["summary"]) && (overwrite || !truth(item["summary"])) {
			item["summary"] = final["summary"]
			wrote = append(wrote, "summary")
		}
		spoilers := SpoilersFrom(final)
		if len(spoilers) > 0 && (overwrite || !truth(item["spoilers"])) {
			item["spoilers"] = spoilers
			wrote = append(wrote, "spoilers")
			if overwrite && truth(before["spoilers"]) && !truth(record(before["provenance"])["enriched"]) {
				prov["replaced_spoilers"] = len(array(before["spoilers"]))
			}
		}
		if truth(final["description"]) && (!truth(item["description"]) || (overwrite && contains(texts(prov["generated"]), "description"))) {
			item["description"] = Block(str(final["description"]))
			wrote = append(wrote, "description")
		}
	}
	prov["fingerprint"] = fp
	prov["source_record"] = filepath.Base(s.Path)
	claimed := str(prov["source_key"])
	if claimed != "" && claimed != s.Audio {
		a := uniqueStrings(append(texts(prov["merged_source_keys"]), s.Audio))
		a = without(a, claimed)
		sort.Strings(a)
		prov["merged_source_keys"] = a
	} else {
		prov["source_key"] = s.Audio
	}
	if len(unresolved) > 0 {
		proposed := []any{}
		for _, tag := range unresolved {
			proposed = append(proposed, Record{"tag": tag, "why": "not in the registry, and no ruling covers it"})
		}
		prov["proposed_tags"] = proposed
	}
	if len(final) > 0 {
		sort.Strings(added)
		prov["enriched"] = Record{"tags_added": added, "analysis_model": cached["model"], "review_model": cached["review_model"], "transcript": TranscriptKey(str(p["text"]))}
		if truth(final["thumbnail_prompt"]) || truth(final["thumbnail_prompt_natural"]) || truth(final["thumbnail_negative"]) {
			item["cover_prompts"] = Record{"tagged": str(final["thumbnail_prompt"]), "natural": str(final["thumbnail_prompt_natural"]), "negative": str(final["thumbnail_negative"])}
		}
	}
	if truth(item["cover"]) {
		source := c.Resolved(str(item["cover"]), filepath.Dir(j.Path))
		ext := filepath.Ext(source)
		if ext == "" {
			ext = ".png"
		}
		target := filepath.Join(c.Covers, s.AuthorID(), str(item["id"])+ext)
		if exists(source) {
			if _, err = Place(source, target, c.MediaSettings.Mode); err != nil {
				return err
			}
			item["cover"] = target
		} else if exists(target) {
			item["cover"] = target
		}
	}
	if covers && len(final) > 0 {
		cover, err := e.RenderCover(ctx, item, final, redraw)
		if err != nil {
			e.Say("cover %s: %v", j.Stem, err)
		} else if cover != "" {
			item["cover"] = cover
			wrote = append(wrote, "cover")
		}
	}
	MarkGenerated(prov, wrote...)
	needs := []string{}
	for _, f := range []string{"description", "summary", "tags", "spoilers"} {
		if !truth(item[f]) {
			needs = append(needs, f)
		}
	}
	if len(needs) > 0 {
		item["needs"] = needs
	} else {
		delete(item, "needs")
	}
	c.PortablePaths(item, filepath.Dir(j.Path))
	if _, err = SaveDocument(j.Path, item); err != nil {
		return err
	}
	if p != nil {
		transcript := Record{"apiVersion": "hypnotica/v1", "kind": "Transcript", "item": item["id"], "model": p["model"], "duration": p["duration"], "covered": p["covered"], "text": Block(str(p["text"])), "segments": array(p["segments"])}
		if _, err = SaveDocument(strings.TrimSuffix(j.Path, filepath.Ext(j.Path))+".transcript.yaml", transcript); err != nil {
			return err
		}
	}
	authorLock, _ := e.locks.LoadOrStore("authors", &sync.Mutex{})
	authorMu := authorLock.(*sync.Mutex)
	authorMu.Lock()
	defer authorMu.Unlock()
	authors, err := Documents(c.Content, "author")
	if err != nil {
		return err
	}
	for _, d := range authors {
		if str(d.Data["id"]) == s.AuthorID() {
			return nil
		}
	}
	authorPath := filepath.Join(c.Content, s.AuthorID(), "_author.yaml")
	return writeYAML(authorPath, Record{"apiVersion": "hypnotica/v1", "kind": "Author", "id": s.AuthorID(), "name": s.Author, "needs": []string{"summary", "description"}}, authorOrder)
}
func MarkGenerated(p Record, fields ...string) {
	if len(fields) == 0 {
		return
	}
	valid := []string{}
	for _, field := range fields {
		if contains([]string{"title", "summary", "description", "synopsis", "cover", "image", "tags", "spoilers"}, field) {
			valid = append(valid, field)
		}
	}
	a := uniqueStrings(append(texts(p["generated"]), valid...))
	if len(a) == 0 {
		return
	}
	sort.Strings(a)
	p["generated"] = a
}
