// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"fmt"
	"io"
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
	// Watch follows the item index, Match the sources being placed against it.
	// Planning a large library is minutes of otherwise silent work.
	Watch, Match []func(done, total int)
	// Stop lets an interrupt land mid-plan rather than after it.
	Stop        func() error
	Authors     []string
	Limit       int
	Needs       []string
	Redo, Fresh bool
	Only        []string
}

func PlanSources(c Config, report SourceReport, index *ItemIndex, fp *FingerprintCache, o PlanOptions) ([]Planned, error) {
	entries, e := index.Entries(c.Content, o.Watch...)
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
	for n, s := range report.Sources {
		if o.Stop != nil && o.Stop() != nil {
			return nil, o.Stop()
		}
		for _, w := range o.Match {
			w(n, len(report.Sources))
		}
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
	Soundness    *SoundnessCache
	Index        *ItemIndex
	Store        *AnalysisStore
	Acoustic     AcousticStore
	Sounds       SoundStore
	Box          *GPUBox
	// Say is called from every goroutine a run starts -- phases, the workers
	// under them, the progress reporter -- so whatever is plugged in here has
	// to serialise its own writes.
	Say         func(string, ...any)
	Colour      bool
	Out         io.Writer
	boxOnce     sync.Once
	boxErr      error
	noProvision bool
	locks       sync.Map
}

func NewEngine(c Config) *Engine {
	return &Engine{Config: c, API: NewAPIClient(c), Fingerprints: NewFingerprintCache(filepath.Join(c.Cache, "fingerprints.json")), Soundness: NewSoundnessCache(filepath.Join(c.Cache, "soundness.json")), Index: NewItemIndex(filepath.Join(c.Cache, "item-index.json")), Store: &AnalysisStore{Root: c.Analysis()}, Acoustic: AcousticStore{filepath.Join(c.Cache, "acoustic")}, Sounds: SoundStore{filepath.Join(c.Cache, "sound")}, Say: func(string, ...any) {}}
}
func (e *Engine) Close() error {
	if err := e.Fingerprints.Save(); err != nil {
		return err
	}
	if err := e.Soundness.Save(); err != nil {
		return err
	}
	return e.Index.Save()
}

// soundEnough refuses audio that does not decode from end to end.
//
// Half a file is worse than none: a damaged recording still transcribes, still
// gets analysed, and still becomes an item -- one that reads as complete and
// plays as silence. Better to fail the job and say why.
// audioTrouble reports what the decoder made of a file: a complaint if it
// grumbled, and whether it failed outright. The two call for different things
// -- a file that will not open is lost, one that decodes with malformed frames
// only needs rewriting -- and conflating them had this refusing to process
// recordings that were entirely intact.
// Below this, what came out is too much less than what was promised for a
// re-encode to be a repair: it would only launder the loss into a file that
// looks healthy. Frame damage that the decoder skips costs a few milliseconds
// and lands far above it.
const recoveryFloor = 0.99

func (e *Engine) audioTrouble(ctx context.Context, path string) (complaint string, recoverable, dead bool) {
	complaint, recovered, err := e.Soundness.Of(ctx, path)
	if err != nil {
		return err.Error(), false, true
	}
	if complaint == "" {
		return "", false, false
	}
	if recovered >= recoveryFloor {
		return complaint, true, false
	}
	return fmt.Sprintf("%s (only %.0f%% of the audio decodes)", complaint, recovered*100), false, true
}

func (e *Engine) soundEnough(ctx context.Context, j Planned) error {
	// Whatever will actually be decoded. Checking only the source missed the
	// whole library: media is skipped for anything already placed, so the check
	// guarded new imports and nothing else, and damaged recordings already on
	// disk went on failing transcription and voiceprinting run after run.
	p := PlacedAudio(e.Config, j.Source, j.Stem)
	if p == "" {
		p = j.Source.AudioPath(e.Config.Sources)
	}
	if p == "" {
		return fmt.Errorf("audio not found: %s", j.Source.Audio)
	}
	complaint, _, dead := e.audioTrouble(ctx, p)
	if dead {
		return fmt.Errorf("audio is damaged past repair, refusing to process it: %s", complaint)
	}
	return nil
}

// loadSources is LoadSources with something to look at. Parsing the records is
// the one stretch of a run that used to pass in silence, and on a large library
// it is the better part of a minute.
func (e *Engine) loadSources() (SourceReport, error) {
	step, finish := startLoading(e.Out, e.Colour, e.Say, "loading source records")
	r, err := LoadSources(e.Config.Sources, step)
	finish(len(r.Sources))
	return r, err
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
func (e *Engine) meta(j Planned, payload, measured, heard Record) Record {
	author := j.Source.Author
	docs, _ := Documents(e.Config.Content, "author")
	for _, d := range docs {
		if str(d.Data["id"]) == j.Source.AuthorID() {
			author = str(first(d.Data["name"], author))
			break
		}
	}
	return Record{"title": j.Source.Title, "author_name": author, "duration": first(j.Source.Data["duration"], payload["duration"]), "series": j.Source.Data["series"], "existing_description": str(j.Source.Data["description"]), "measured": measured, "heard": heard, "sound_settings": e.Config.Transcribe.Sound}
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
	extra := Record{}
	if !embed {
		extra["model"] = e.Config.Transcribe.Model
		extra["fallback"] = e.Config.Transcribe.Fallback
	}
	r, err := e.Box.Work(ctx, id, kind, audio, landing, extra)
	if err != nil {
		return err
	}
	if embed {
		r["apiVersion"] = "inductor/v1"
		r["kind"] = "VoicePrint"
	}
	return writeJSON(dest, r)
}

// Listen asks what a recording sounds like. It runs for every recording, not
// only the ones whose transcript came back thin: the question "what is this"
// has an answer for a guided session too, and a pass that only ever looked at
// the silent ones could never say that the rest are not silent.
func (e *Engine) Listen(ctx context.Context, j Planned, redo bool) error {
	cfg := e.Config.Transcribe.Sound
	if cfg.Tagger == "" && cfg.Zeroshot == "" {
		return NothingToProduce{"no sound models configured"}
	}
	fp, err := e.fingerprint(j)
	if err != nil {
		return err
	}
	lock, _ := e.locks.LoadOrStore("sound:"+fp, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if e.Sounds.Has(fp) && !redo {
		return nil
	}
	audio := PlacedAudio(e.Config, j.Source, j.Stem)
	if audio == "" {
		audio = j.Source.AudioPath(e.Config.Sources)
	}
	// The spectrum is arithmetic: no model, no GPU, no network. It is measured
	// here rather than on the box so that a library with nowhere to run the
	// taggers still learns what its tone tracks are made of.
	r := Record{}
	seconds := number(first(j.Source.Data["duration"], e.Acoustic.Measurements(fp)["seconds"]))
	if t, e2 := Tones(ctx, audio, seconds); e2 == nil {
		r["tones"] = t
	}
	if err = e.ensureBox(ctx); err != nil {
		return err
	}
	id := "sound-" + fp
	landing := filepath.Join(e.Config.Cache, "remote-out")
	if redo {
		_ = os.Remove(filepath.Join(landing, id+".json"))
	}
	heard, err := e.Box.Work(ctx, id, "sound", audio, landing, Record{
		"tagger": cfg.Tagger, "zeroshot": cfg.Zeroshot, "labels": cfg.Labels,
		"voice": cfg.Voice, "threshold": cfg.Threshold, "floor": cfg.Floor})
	if err != nil {
		return err
	}
	merge(r, heard)
	return e.Sounds.Put(fp, r)
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
	key := TranscriptKey(str(p["text"]))
	// Declining has to be remembered, or it is not a decision, it is a question
	// asked again on every run for ever. Nothing about this recording will
	// change until its transcript does -- and when that happens the key changes
	// with it, so the refusal expires exactly when it should.
	decline := func(why string) error {
		if !truth(e.Store.Peek(key, fp)["analysis_declined"]) {
			_ = e.Store.Put(key, Record{"analysis_declined": why}, fp, "")
		}
		return NothingToProduce{why}
	}
	if str(p["speech"]) == "none" {
		return decline("no speech in the recording")
	}
	s := Sentences(p)
	spoken := 0
	for _, v := range s {
		spoken += len([]rune(v.Text))
	}
	if spoken < 200 {
		return decline(fmt.Sprintf("%d characters of transcript, too little to analyse", spoken))
	}
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
	result, err := e.API.ChatJSON(ctx, e.Config.Enrich.AnalysisModel, AnalysisMessages(s, e.meta(j, p, nil, nil), reg), TokenCeiling, .15)
	if err != nil {
		return err
	}
	return e.Store.Put(key, Record{"analysis": PruneCitations(result, s)}, fp, e.Config.Enrich.AnalysisModel)
}

// reviewRoute decides how a recording with no first pass should be reviewed,
// and says so rather than leaving the caller to infer it from an empty result.
//
// The speech field only exists on transcripts taken since the transcriber
// learned to report one, so an older transcript answers "unknown" and the
// length of what it heard has to stand in.
func (e *Engine) reviewRoute(p, heard Record) (string, string) {
	text := strings.TrimSpace(str(p["text"]))
	said := len([]rune(text))
	if str(p["speech"]) != "none" && said >= 40 {
		return "solo", ""
	}
	if truth(heard) {
		return "wordless", ""
	}
	return "", fmt.Sprintf("nothing to review: %d characters transcribed and nothing heard of the audio", said)
}

// ReviewJobs builds the reviews still wanted, and reports what it could not
// build and why. A recording it declines is not a failure -- it is one nobody
// can write an entry for from what is on disk -- and calling it one buries the
// batches that really did go wrong.
func (e *Engine) ReviewJobs(jobs []Planned, redo bool) ([]ReviewJob, map[string]string, error) {
	reg, err := LoadRegistry(e.Config.RegistryPath())
	if err != nil {
		return nil, nil, err
	}
	out := []ReviewJob{}
	skipped := map[string]string{}
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
		if !redo && truth(r["final"]) {
			continue
		}
		heard := e.Sounds.Get(fp)
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
		meta := e.meta(j, p, m, heard)
		msgs := ReviewMessages(record(r["analysis"]), s, meta, reg)
		if !truth(r["analysis"]) {
			// No first pass, for one of two reasons, and they want different
			// prompts. A recording with a few words in it has something to read,
			// just not enough to have been worth analysing; one with none at all
			// can only be described from what it sounds like. Handing the second
			// prompt to the first kind would tell a model to describe as
			// wordless a recording that plainly says something.
			switch which, why := e.reviewRoute(p, heard); which {
			case "solo":
				msgs = SoloMessages(s, meta, reg)
			case "wordless":
				msgs = WordlessMessages(meta, reg)
			default:
				skipped[id] = why
				continue
			}
		}
		out = append(out, ReviewJob{id, msgs, fp, key, s})
	}
	return out, skipped, nil
}
func (e *Engine) batchJournal(id string) string {
	return filepath.Join(e.Config.Cache, "batches", id+".json")
}

// markBatch records what became of a submitted batch. Until this existed the
// journal said "submitted" forever, so a batch that landed hours ago was
// indistinguishable from one nobody ever read -- which is the whole question a
// resume has to answer.
func (e *Engine) markBatch(id, status string) {
	j := readJSON(e.batchJournal(id))
	if len(j) == 0 {
		return
	}
	j["status"] = status
	_ = writeJSON(e.batchJournal(id), j)
}

// UnreadBatches finds batches that were submitted and whose results were never
// taken up, limited to those covering work in hand. A killed process does not
// cancel a batch and there is no cancel endpoint, so the reviews are paid for
// either way: submitting them again buys the same answers twice.
func (e *Engine) UnreadBatches(wanted map[string]ReviewJob) []Record {
	paths, _ := filepath.Glob(filepath.Join(e.Config.Cache, "batches", "*.json"))
	sort.Strings(paths)
	out := []Record{}
	for _, p := range paths {
		j := readJSON(p)
		if str(j["status"]) != "submitted" {
			continue
		}
		for _, v := range array(j["jobs"]) {
			b, _ := jsonBytes(v)
			var rj ReviewJob
			if decodeJSON(b, &rj) == nil && rj.ID != "" {
				if _, ok := wanted[rj.ID]; ok {
					out = append(out, j)
					break
				}
			}
		}
	}
	return out
}

// offerToFallbacks re-asks for the reviews nobody answered. A model that will
// not describe the material returns prose, an empty body or an error, and all
// three arrive as a review that does not parse -- indistinguishable here from
// a mangled one, and not worth distinguishing, because the answer is the same:
// ask somebody else. The batch suffix is dropped because these are the leftovers,
// wanted now rather than at batch latency, and there are few enough to pay for.
func (e *Engine) offerToFallbacks(ctx context.Context, jobs []ReviewJob, landed map[string]bool, land func(map[string]string, string) error) error {
	for _, fallback := range e.Config.Enrich.ReviewFallback {
		pending := []ReviewJob{}
		for _, j := range jobs {
			if !landed[j.ID] {
				pending = append(pending, j)
			}
		}
		if len(pending) == 0 {
			return nil
		}
		fallback = strings.TrimSuffix(fallback, ":batch")
		e.Say("%d review(s) unanswered; offering them to %s", len(pending), fallback)
		results, errs := parallelMap(ctx, pending, max(1, e.Config.Enrich.Workers), func(ctx context.Context, j ReviewJob) (string, error) {
			complete := startRunWork(ctx, "review retry "+j.ID)
			text, _, err := e.API.Chat(ctx, fallback, j.Messages, TokenCeiling, .2)
			complete(err)
			return text, err
		})
		for i, text := range results {
			if errs[i] != nil {
				e.Say("FAIL review %s on %s: %v", pending[i].ID, fallback, errs[i])
				continue
			}
			if err := land(map[string]string{pending[i].ID: text}, fallback); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) RunReviews(ctx context.Context, jobs []ReviewJob, inFlight, batchSize int, poll time.Duration, recover []string, wait bool) (int, error) {
	model := e.Config.Enrich.ReviewModel
	byID := map[string]ReviewJob{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	done := 0
	landed := map[string]bool{}
	// answered records which model actually produced the entry, which is not
	// always the one first asked: a review offered to a fallback is still that
	// fallback's work, and the library should say so.
	land := func(results map[string]string, answered string) error {
		for _, id := range sortedKeys(results) {
			j, ok := byID[id]
			if !ok || landed[id] {
				continue
			}
			final, err := ExtractJSON(results[id])
			if err != nil {
				e.Say("FAIL review %s: %v", id, err)
				continue
			}
			if err = e.Store.Put(j.Key, Record{"final": final, "review_model": answered, "sentences": j.Sentences}, j.Fingerprint, ""); err != nil {
				return err
			}
			landed[id] = true
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
		if err = land(results, model); err != nil {
			return done, err
		}
		e.markBatch(id, "landed")
	}
	if len(recover) > 0 {
		return done, nil
	}
	if len(jobs) == 0 {
		return 0, nil
	}
	// Anything the journal still calls submitted that covers work in hand was
	// paid for and never read. Take it up before buying the same answers twice.
	for _, pending := range e.UnreadBatches(byID) {
		id := str(pending["id"])
		// Collected, not waited on. Naming a batch with --recover is a request
		// to wait for it; finding one in the journal is not, and BatchWait sits
		// for up to a day. A run that stumbles on somebody's unfinished batch
		// should take what is ready and get on with the rest.
		info, err := e.API.BatchStatus(ctx, id)
		if err != nil {
			e.Say("batch %s could not be read: %v", id, err)
			e.markBatch(id, "unreadable")
			continue
		}
		switch str(info["status"]) {
		case "completed", "ended", "finalized":
			results, err := e.API.BatchResults(ctx, id, info)
			if err != nil {
				e.Say("batch %s could not be collected: %v", id, err)
				e.markBatch(id, "unreadable")
				continue
			}
			e.Say("taking up batch %s, submitted earlier and never read", id)
			if err = land(results, model); err != nil {
				return done, err
			}
			e.markBatch(id, "landed")
		case "failed", "cancelled", "expired":
			e.Say("batch %s ended as %s; nothing to take up", id, str(info["status"]))
			e.markBatch(id, "unreadable")
		default:
			if !wait {
				e.Say("batch %s is still running; leaving it in the journal", id)
				continue
			}
			e.Say("batch %s is still running; waiting for it", id)
			results, err := e.API.BatchWait(ctx, id, poll)
			if err != nil {
				e.Say("batch %s could not be taken up: %v", id, err)
				e.markBatch(id, "unreadable")
				continue
			}
			if err = land(results, model); err != nil {
				return done, err
			}
			e.markBatch(id, "landed")
		}
	}
	if len(landed) > 0 {
		kept := make([]ReviewJob, 0, len(jobs))
		for _, j := range jobs {
			if !landed[j.ID] {
				kept = append(kept, j)
			}
		}
		e.Say("%d review(s) recovered from an earlier batch, %d still to submit", len(jobs)-len(kept), len(kept))
		jobs = kept
		if len(jobs) == 0 {
			return done, nil
		}
	}
	if !strings.HasSuffix(model, ":batch") {
		results, errs := parallelMap(ctx, jobs, max(1, e.Config.Enrich.Workers), func(ctx context.Context, j ReviewJob) (string, error) {
			complete := startRunWork(ctx, "review request "+j.ID)
			text, _, err := e.API.Chat(ctx, model, j.Messages, TokenCeiling, .2)
			complete(err)
			return text, err
		})
		for i, result := range results {
			if errs[i] != nil {
				e.Say("FAIL review %s: %v", jobs[i].ID, errs[i])
				continue
			}
			if err := land(map[string]string{jobs[i].ID: result}, model); err != nil {
				return done, err
			}
		}
		if err := e.offerToFallbacks(ctx, jobs, landed, land); err != nil {
			return done, err
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
		id      string
		results map[string]string
		err     error
	}
	results, errs := parallelMap(ctx, chunks, max(1, inFlight), func(ctx context.Context, chunk []ReviewJob) (result batchResult, workErr error) {
		complete := startRunWork(ctx, fmt.Sprintf("review batch (%d recordings, first=%s)", len(chunk), chunk[0].ID))
		defer func() {
			if workErr != nil {
				complete(workErr)
			} else {
				complete(result.err)
			}
		}()
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
		return batchResult{id, r, err}, nil
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
		if err = land(result.results, model); err != nil {
			return done, err
		}
		if result.id != "" {
			e.markBatch(result.id, "landed")
		}
	}
	if err := e.offerToFallbacks(ctx, jobs, landed, land); err != nil {
		return done, err
	}
	if len(failures) > 0 && done != len(jobs) {
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
	// Inductor says so of its own records, and says nothing of anyone else's.
	// Ownership is claimed, never inferred: an entry written by hand carries no
	// claim, so nothing here will ever remove it, whatever else is true of it.
	prov[ManagedBy] = ManagedByInductor
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
	return writeYAML(authorPath, NewAuthorPage(s.AuthorID(), s.Author, e.sourceAuthor(s.AuthorID())), authorOrder)
}

// sourceAuthor is whatever a parser recorded about this creator, if anything.
// LoadSources memoises on the file stamps, so asking once per recording costs a
// map lookup rather than a re-parse.
func (e *Engine) sourceAuthor(id string) Record {
	r, err := LoadSources(e.Config.Sources)
	if err != nil {
		return nil
	}
	return r.Authors[id]
}

// NewAuthorPage builds a creator page, using whatever the parser learned about
// them and asking for the rest.
//
// `needs` is the whole mechanism: a field named there is one a later pass will
// fill in, so anything the source supplied must *not* be listed, or the model
// writes over the creator's own words on the next run. Fields that arrive this
// way are never marked generated either, because they were not.
func NewAuthorPage(id, name string, supplied Record) Record {
	page := Record{"apiVersion": "hypnotica/v1", "kind": "Author", "id": id, "name": name}
	for _, f := range []string{"url", "links", "language", "explicit", "image", "description", "summary"} {
		if truth(supplied[f]) {
			page[f] = supplied[f]
		}
	}
	if truth(supplied["name"]) {
		page["name"] = str(supplied["name"])
	}
	want := []string{}
	for _, f := range []string{"summary", "description"} {
		if !truth(page[f]) {
			want = append(want, f)
		}
	}
	if len(want) > 0 {
		page["needs"] = want
	}
	if len(supplied) > 0 {
		prov := nested(page, "provenance")
		prov["metadata_source"] = str(first(record(supplied["provenance"])["metadata_source"],
			"the creator's own page"))
		if truth(page["description"]) {
			prov["description_from_source"] = true
		}
	}
	return page
}

// ManagedBy marks an entry as Inductor's own, so that removing the source
// record it came from removes the entry too. Absence of the mark is a hard stop
// on deletion -- a hand-written entry, or one older than the mark, is reported
// and left alone.
const (
	ManagedBy         = "managed_by"
	ManagedByInductor = "inductor"
)

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

// firstReason names one of a set of declined recordings, for a line that says
// how many there were without printing all of them.
func firstReason(skipped map[string]string) string {
	for _, id := range sortedKeys(skipped) {
		return id + " — " + skipped[id]
	}
	return ""
}
