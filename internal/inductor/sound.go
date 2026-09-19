// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// What a recording sounds like, for the recordings that do not say anything.
//
// A transcript answers "what words are in this", and for a great deal of audio
// the honest answer is none: tones, drones, music, breathing, wordless voice.
// Asking a speech model harder does not help — handed audio with no words in
// it, it returns the stock phrases of its training data, and a bigger model
// returns fewer of them but still returns them. So the question has to be asked
// of the sound instead, by instruments that were built for it.
//
// Three, because they fail differently. The spectrum is arithmetic and always
// available: it needs no model, no GPU and no network, and for a tone track it
// recovers the entire design. The ontology tagger is multi-label, so its verdict
// on one class does not compete with its verdict on another — which is what
// makes it the gate for "is anybody speaking", and why a zero-shot ranking
// cannot be: asked to choose, "there is a voice" competes with "there are mouth
// sounds" and loses whenever both are true. The zero-shot model describes best,
// because it can be given this collection's own words, but it cannot decline —
// handed a vocabulary with no word for what it hears it returns the nearest
// entry and looks just as sure. Only the absolute similarity gives that away,
// so it is scored against a floor rather than believed.

const (
	// 8 kHz is ample: the carriers this finds live below 1 kHz, and halving the
	// rate doubles the resolution a transform of a given size can reach.
	soundRate = 8000
	// 32768 points at 8 kHz is 0.24 Hz per bin. Coarser than this and a pair of
	// carriers a few Hz apart -- which is the entire point of a binaural track --
	// lands in one bin and the beat disappears.
	soundFrame = 32768
	// Seconds per window, and where in the recording they are taken. Whole-file
	// decoding is not wanted here: a tone is a tone throughout, and what the
	// windows are for is catching the ones that are not.
	soundWindow = 24
)

var soundOffsets = []float64{.08, .3, .5, .7, .92}

func DefaultSound() SoundSettings {
	return SoundSettings{
		Tagger:   "MIT/ast-finetuned-audioset-10-10-0.4593",
		Zeroshot: "laion/clap-htsat-unfused",
		Labels: []string{
			"a person speaking", "whispering close to the microphone",
			"singing", "music with no voice", "a pure sine wave tone",
			"a low humming drone", "white noise or static", "silence",
			"breathing", "laughter", "rain, water or wind",
			"rustling, tapping and handling noise", "a crowd or background chatter",
			"machinery or engine noise", "birds or animals",
		},
		Voice: []string{"Speech", "Whispering", "Female speech, woman speaking",
			"Male speech, man speaking", "Conversation", "Narration, monologue",
			"Child speech, kid speaking", "Singing"},
		Threshold: .25,
		// Measured by offering deliberately wrong vocabularies: at .35 nothing
		// forced through a list with no word for the audio survived, while most
		// genuine identifications did.
		Floor: .35,
	}
}

// decodeStereo reads one window as two channels. ffmpeg is asked for the window
// rather than the file because seeking is free and decoding is not.
func decodeStereo(ctx context.Context, path string, start float64, seconds, rate int) ([]float32, []float32, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-err_detect", "ignore_err",
		"-threads", "1", "-ss", fmt.Sprintf("%.2f", math.Max(0, start)), "-t", fmt.Sprint(seconds),
		"-i", path, "-map", "0:a:0", "-vn", "-ac", "2", "-ar", fmt.Sprint(rate), "-f", "s16le", "pipe:1")
	out, e := cmd.Output()
	if e != nil {
		return nil, nil, e
	}
	n := len(out) / 4
	left, right := make([]float32, n), make([]float32, n)
	for i := 0; i < n; i++ {
		left[i] = float32(int16(uint16(out[i*4])|uint16(out[i*4+1])<<8)) / 32768
		right[i] = float32(int16(uint16(out[i*4+2])|uint16(out[i*4+3])<<8)) / 32768
	}
	return left, right, nil
}

// welch averages the periodograms of overlapping Hann-windowed segments. One
// transform of the whole window would have the same resolution and far more
// variance; averaging is what makes a peak stand out from the noise around it.
func welch(x []float32) []float64 {
	if len(x) < soundFrame {
		return nil
	}
	power := make([]float64, soundFrame/2+1)
	buf := make([]complex128, soundFrame)
	hann := make([]float64, soundFrame)
	for i := range hann {
		hann[i] = .5 - .5*math.Cos(2*math.Pi*float64(i)/float64(soundFrame-1))
	}
	segments := 0
	for start := 0; start+soundFrame <= len(x); start += soundFrame / 2 {
		mean := 0.
		for _, v := range x[start : start+soundFrame] {
			mean += float64(v)
		}
		mean /= float64(soundFrame)
		for i := 0; i < soundFrame; i++ {
			buf[i] = complex((float64(x[start+i])-mean)*hann[i], 0)
		}
		fft(buf, false)
		for i := range power {
			power[i] += real(buf[i])*real(buf[i]) + imag(buf[i])*imag(buf[i])
		}
		segments++
	}
	if segments == 0 {
		return nil
	}
	for i := range power {
		power[i] /= float64(segments)
	}
	return power
}

func binHz(i int) float64 { return float64(i) * soundRate / soundFrame }

type peak struct{ Hz, Share float64 }

// peaks returns the strongest partials, each at least `apart` Hz from the ones
// already taken, so a single broad peak is not reported as several.
func peaks(power []float64, want int, apart float64) ([]peak, float64) {
	lo := int(math.Ceil(8 * soundFrame / float64(soundRate)))
	total := 0.
	for i := lo; i < len(power); i++ {
		total += power[i]
	}
	if total <= 0 {
		return nil, 0
	}
	order := make([]int, 0, len(power)-lo)
	for i := lo; i < len(power); i++ {
		order = append(order, i)
	}
	sort.SliceStable(order, func(a, b int) bool { return power[order[a]] > power[order[b]] })
	out := []peak{}
	for _, i := range order {
		if len(out) >= want {
			break
		}
		ok := true
		for _, p := range out {
			if math.Abs(binHz(i)-p.Hz) <= apart {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, peak{rounded(binHz(i), 2), power[i] / total})
		}
	}
	return out, total
}

// modulation finds the rate at which the level rises and falls -- an isochronic
// pulse, which is a real amplitude change in the file, as against a binaural
// beat, which is not in the file at all and happens in the listener.
//
// It must therefore be measured in one channel. Summing the two to mono
// manufactures exactly the modulation a binaural pair does not have: two tones
// eight Hertz apart, added together, beat at eight Hertz on paper -- so a
// downmix reports every binaural track as pulsing, which is the one thing it
// certainly is not doing.
//
// The envelope is also decimated, which folds everything above half the
// envelope rate back into the band being searched, a carrier's own second
// harmonic included. Both guards below are for that fold.
func modulation(x []float32, carrier float64) (float64, float64) {
	const envRate = 100
	step := soundRate / envRate
	n := len(x) / step
	if n < 256 {
		return 0, 0
	}
	env := make([]float32, n)
	mean := 0.
	for i := 0; i < n; i++ {
		s := 0.
		for _, v := range x[i*step : (i+1)*step] {
			s += math.Abs(float64(v))
		}
		env[i] = float32(s / float64(step))
		mean += float64(env[i])
	}
	mean /= float64(n)
	size := 1
	for size*2 <= n {
		size *= 2
	}
	if size < 256 {
		return 0, 0
	}
	buf := make([]complex128, size)
	for i := 0; i < size; i++ {
		buf[i] = complex(float64(env[i])-mean, 0)
	}
	fft(buf, false)
	best, at, total := 0., 0, 0.
	for i := 1; i < size/2; i++ {
		hz := float64(i) * envRate / float64(size)
		if hz < .3 || hz > 40 {
			continue
		}
		p := real(buf[i])*real(buf[i]) + imag(buf[i])*imag(buf[i])
		total += p
		if p > best {
			best, at = p, i
		}
	}
	if total <= 0 || best <= 0 {
		return 0, 0
	}
	hz := float64(at) * envRate / float64(size)
	if carrier > 0 && hz*4 > carrier {
		return 0, 0
	}
	// A rectified carrier puts energy at twice its frequency, which folds back
	// into the search band for any carrier worth having. A rate sitting on that
	// fold is the carrier talking about itself.
	if carrier > 0 && math.Abs(hz-foldInto(2*carrier, envRate)) < 1.5 {
		return 0, 0
	}
	return rounded(hz, 2), rounded(best/total, 3)
}

// foldInto is where a frequency lands once sampled at the given rate.
func foldInto(hz, rate float64) float64 {
	m := math.Mod(math.Abs(hz), rate)
	if m > rate/2 {
		m = rate - m
	}
	return m
}

// Brainwave band names, given for the beat frequency because that is the term
// the material itself uses. Naming it is not endorsing it.
func beatBand(hz float64) string {
	switch {
	case hz < .3:
		return ""
	case hz < 4:
		return "delta"
	case hz < 8:
		return "theta"
	case hz < 13:
		return "alpha"
	case hz < 30:
		return "beta"
	default:
		return "gamma"
	}
}

type toneWindow struct {
	At                 float64
	CarrierL, CarrierR float64
	Beat               float64
	Paired             bool
	Purity             float64
	Low, Mid, High     float64
	AMRate, AMStrength float64
}

func bandShares(power []float64, total float64) (lo, mid, hi float64) {
	for i := range power {
		hz := binHz(i)
		switch {
		case hz < 8:
		case hz < 300:
			lo += power[i]
		case hz < 3000:
			mid += power[i]
		default:
			hi += power[i]
		}
	}
	if total <= 0 {
		return 0, 0, 0
	}
	return lo / total, mid / total, hi / total
}

func analyseWindow(left, right []float32, at float64) *toneWindow {
	pl, tl := welch(left), welch(right)
	if pl == nil || tl == nil {
		return nil
	}
	lp, total := peaks(pl, 6, 3)
	rp, _ := peaks(tl, 6, 3)
	if len(lp) == 0 || len(rp) == 0 {
		return nil
	}
	w := &toneWindow{At: rounded(at, 0), CarrierL: lp[0].Hz}
	// Pair like with like. The beat is between one ear's carrier and the other
	// ear's nearest strong partial -- never its harmonic, which is what taking
	// each channel's loudest peak independently will eventually give you.
	best := -1.
	for _, p := range rp {
		if math.Abs(p.Hz-lp[0].Hz) < 40 && p.Share > best {
			best, w.CarrierR, w.Paired = p.Share, p.Hz, true
		}
	}
	if !w.Paired {
		w.CarrierR = rp[0].Hz
	}
	w.Beat = rounded(math.Abs(w.CarrierL-w.CarrierR), 2)
	for _, p := range lp[:min(3, len(lp))] {
		w.Purity += p.Share
	}
	w.Purity = rounded(w.Purity, 3)
	w.Low, w.Mid, w.High = bandShares(pl, total)
	w.Low, w.Mid, w.High = rounded(w.Low, 3), rounded(w.Mid, 3), rounded(w.High, 3)
	w.AMRate, w.AMStrength = modulation(left, w.CarrierL)
	return w
}

// Tones measures the steady content of a recording: the carriers, the beat
// between the ears, and whether either holds still.
func Tones(ctx context.Context, path string, seconds float64) (Record, error) {
	if seconds <= 0 {
		return nil, fmt.Errorf("unknown duration")
	}
	windows := []*toneWindow{}
	for _, frac := range soundOffsets {
		start := seconds*frac - soundWindow/2
		left, right, e := decodeStereo(ctx, path, start, soundWindow, soundRate)
		if e != nil || len(left) < soundFrame {
			continue
		}
		if w := analyseWindow(left, right, seconds*frac); w != nil {
			windows = append(windows, w)
		}
	}
	if len(windows) == 0 {
		return nil, fmt.Errorf("nothing measurable")
	}
	carriers, beats := []float64{}, []float64{}
	purity, low, mid, high := 0., 0., 0., 0.
	amRate, amStrength := []float64{}, 0.
	paired := 0
	for _, w := range windows {
		carriers = append(carriers, w.CarrierL)
		if w.Paired {
			beats = append(beats, w.Beat)
			paired++
		}
		purity += w.Purity
		low += w.Low
		mid += w.Mid
		high += w.High
		amStrength += w.AMStrength
		if w.AMRate > 0 {
			amRate = append(amRate, w.AMRate)
		}
	}
	n := float64(len(windows))
	r := Record{
		"windows":     len(windows),
		"purity":      rounded(purity/n, 3),
		"low_share":   rounded(low/n, 3),
		"mid_share":   rounded(mid/n, 3),
		"high_share":  rounded(high/n, 3),
		"carrier_hz":  rounded(percentile(carriers, 50), 2),
		"carrier_min": rounded(minFloat(carriers), 2),
		"carrier_max": rounded(maxFloat(carriers), 2),
	}
	// A median only describes the file when the windows agree about it. Where
	// they do not, saying so is the finding: a track whose dominant partial
	// wanders has no carrier to report, and reporting one anyway invents a
	// steadiness the recording does not have.
	r["carrier_steady"] = len(carriers) >= 3 &&
		maxFloat(carriers)-minFloat(carriers) < .25*percentile(carriers, 50)
	if len(beats) > 0 {
		median := percentile(beats, 50)
		r["beat_hz"] = rounded(median, 2)
		r["beat_band"] = beatBand(median)
		r["beat_windows"] = paired
		r["beat_steady"] = len(beats) >= 3 &&
			maxFloat(beats)-minFloat(beats) < math.Max(1.5, .25*median)
		if len(beats) >= 3 {
			r["beat_drift"] = drift(beats)
			r["beat_span"] = rounded(maxFloat(beats)-minFloat(beats), 2)
		}
	}
	if len(carriers) >= 3 {
		r["carrier_drift"] = drift(carriers)
	}
	if len(amRate) > 0 {
		r["pulse_hz"] = rounded(percentile(amRate, 50), 2)
		r["pulse_strength"] = rounded(amStrength/n, 3)
	}
	r["shape"] = toneShape(r)
	return r, nil
}

func drift(v []float64) string {
	if len(v) < 3 {
		return "unknown"
	}
	mean := meanFloat(v)
	if mean == 0 || (maxFloat(v)-minFloat(v))/math.Abs(mean) < .05 {
		return "steady"
	}
	if v[len(v)-1] < v[0] {
		return "descending"
	}
	return "ascending"
}

// toneShape names what the numbers add up to, so that everything downstream
// agrees on the reading rather than each re-deriving it from thresholds.
func toneShape(r Record) string {
	tonal := number(r["purity"]) >= .35 && number(r["low_share"])+number(r["mid_share"]) > .9
	// Presence, not truth: a beat of exactly zero is the answer for a track
	// whose two channels carry the same tone, and testing it for truthiness
	// reads that answer as "never measured".
	measured := r["beat_hz"] != nil
	switch {
	case tonal && measured && number(r["beat_hz"]) >= .5 && number(r["beat_hz"]) <= 40 && truth(r["beat_steady"]):
		return "binaural pair"
	case tonal && measured && number(r["beat_hz"]) < .5:
		return "single tone"
	case tonal:
		return "tonal"
	case number(r["purity"]) >= .15:
		return "layered tones"
	case number(r["high_share"]) > .15:
		return "broadband"
	default:
		return "low-frequency wash"
	}
}

func minFloat(v []float64) float64 {
	out := math.Inf(1)
	for _, x := range v {
		out = math.Min(out, x)
	}
	return out
}
func maxFloat(v []float64) float64 {
	out := math.Inf(-1)
	for _, x := range v {
		out = math.Max(out, x)
	}
	return out
}

// SoundStore holds what the tagging models made of each recording, keyed by the
// audio's fingerprint like everything else derived from the audio itself.
type SoundStore struct{ Root string }

func (s SoundStore) Path(key string) string { return filepath.Join(s.Root, key+".json") }
func (s SoundStore) Has(key string) bool    { return exists(s.Path(key)) }
func (s SoundStore) Get(key string) Record  { return readJSON(s.Path(key)) }
func (s SoundStore) Put(key string, r Record) error {
	r["apiVersion"] = "inductor/v1"
	r["kind"] = "Sound"
	return writeJSON(s.Path(key), r)
}

// Labelled reads the ranked labels out of a stored sound record.
//
// The zero-shot labels are only returned when the winner actually cleared the
// floor. A ranking is not an identification: handed a vocabulary with no word
// for what it is hearing, the model returns the nearest entry with every
// appearance of confidence, and the margin over the runner-up does not give it
// away -- offering a list of wrong labels produced *larger* margins than the
// right list did. Only the absolute similarity separates the two.
func Labelled(r Record, field string, limit int) []string {
	if field == "sounds" && r["fits"] != nil && !truth(r["fits"]) {
		return nil
	}
	out := []string{}
	for _, v := range array(r[field]) {
		e := record(v)
		if !truth(e["label"]) {
			continue
		}
		out = append(out, str(e["label"]))
		if len(out) >= limit {
			break
		}
	}
	return out
}

// VoiceConfidence is the tagger's highest reading among the labels that mean
// somebody is speaking. Zero when the tagger did not run.
func VoiceConfidence(r Record, voice []string) float64 {
	best := 0.
	for _, v := range array(r["tags"]) {
		e := record(v)
		if contains(voice, str(e["label"])) {
			best = math.Max(best, number(e["score"]))
		}
	}
	return best
}

// SoundBlock renders the sound of a recording for a model that is about to
// describe it. It is deliberately prose rather than figures: the reader is a
// language model, and "no voice anywhere in it" is a stronger instruction than
// a probability it has to interpret.
func SoundBlock(s Record, cfg SoundSettings) []string {
	if len(s) == 0 {
		return nil
	}
	out := []string{"\nHeard in the audio itself, by models that identify sound rather than speech. This is evidence about the recording that no transcript can carry."}
	t := record(s["tones"])
	if truth(t) {
		if line := ToneSentence(t); line != "" {
			out = append(out, "  Steady content: "+line)
		}
	}
	if labels := Labelled(s, "sounds", 3); len(labels) > 0 {
		out = append(out, "  Sounds most like: "+strings.Join(labels, "; "))
	} else if s["fits"] != nil && !truth(s["fits"]) {
		out = append(out, "  Sounds most like: nothing in the vocabulary matched this closely. Do not guess from the classes below alone.")
	}
	if tags := Labelled(s, "tags", 5); len(tags) > 0 {
		out = append(out, "  Sound classes present: "+strings.Join(tags, ", "))
	}
	if truth(s["voice_confidence"]) {
		v := number(s["voice_confidence"])
		switch {
		case v < cfg.Threshold:
			out = append(out, fmt.Sprintf("  Speech: none detected (%.2f against a %.2f threshold). Do not describe words, a script, or anything said — there is nobody speaking in this recording. Describe what it is and what it does as sound.", v, cfg.Threshold))
		case v < .5:
			out = append(out, fmt.Sprintf("  Speech: present but sparse or quiet (%.2f). Much of the running time has no words in it.", v))
		}
	}
	if len(out) == 1 {
		return nil
	}
	return out
}

// ToneSentence says in one line what the steady content of a recording is, and
// says nothing at all when there is none.
//
// Every recording has a loudest partial, so there is always a carrier to report
// and it is almost always meaningless: a voice moves, so an ordinary spoken
// recording reads as "tones present but not fixed" and announces its own pitch
// range as though it were a design. Measured over this library the two separate
// cleanly -- tone tracks sit at .58 to .65 purity, spoken recordings at .02 to
// .03 -- so below the floor the honest output is silence.
func ToneSentence(t Record) string {
	if len(t) == 0 || number(t["purity"]) < .1 {
		return ""
	}
	shape := str(t["shape"])
	var s string
	switch {
	case !truth(t["carrier_steady"]):
		s = fmt.Sprintf("tones present but not fixed — the dominant partial moves between %.0f and %.0f Hz across the recording",
			number(t["carrier_min"]), number(t["carrier_max"]))
	case shape == "binaural pair":
		s = fmt.Sprintf("%.0f Hz to one ear and %.0f Hz to the other — a %.1f Hz binaural beat",
			number(t["carrier_hz"]), number(t["carrier_hz"])+number(t["beat_hz"]), number(t["beat_hz"]))
		if b := str(t["beat_band"]); b != "" {
			s += " (" + b + ")"
		}
		switch str(t["beat_drift"]) {
		case "ascending", "descending":
			s += fmt.Sprintf(", %s over %.1f Hz", str(t["beat_drift"]), number(t["beat_span"]))
		default:
			s += ", steady"
		}
	case shape == "single tone":
		s = fmt.Sprintf("a single %.0f Hz tone, both channels alike — no binaural beat", number(t["carrier_hz"]))
	case (shape == "tonal" || shape == "layered tones") && t["beat_hz"] != nil && number(t["beat_hz"]) >= .5:
		// A beat that will not hold still is still a beat, and saying "layered
		// tones" instead throws away the one number the track is built on.
		s = fmt.Sprintf("tones around %.0f Hz with a binaural beat that does not hold still",
			number(t["carrier_hz"]))
		if truth(t["beat_span"]) {
			s += fmt.Sprintf(", moving over %.1f Hz around %.1f Hz",
				number(t["beat_span"]), number(t["beat_hz"]))
		}
	case shape == "tonal" || shape == "layered tones":
		s = fmt.Sprintf("layered tones around %.0f Hz", number(t["carrier_hz"]))
	case shape == "broadband":
		s = fmt.Sprintf("broadband, %.0f%% of the energy above 3 kHz", 100*number(t["high_share"]))
	default:
		s = fmt.Sprintf("low-frequency, %.0f%% of the energy below 300 Hz", 100*number(t["low_share"]))
	}
	if number(t["pulse_strength"]) > .15 && truth(t["pulse_hz"]) {
		s += fmt.Sprintf("; pulsing at %.1f Hz", number(t["pulse_hz"]))
	}
	return s
}
