// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/bits"
	"math/cmplx"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

const AudioRate = 11025
const AudioFrame = 2048
const AudioHop = 1024
const AudioBands = 33

// fft is an in-place radix-two transform. Its memory is bounded by a frame,
// independent of recording length. Inverse transforms include normalization.
func fft(a []complex128, inverse bool) {
	n := len(a)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for j&bit != 0 {
			j ^= bit
			bit >>= 1
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	sign := -1.
	if inverse {
		sign = 1
	}
	for size := 2; size <= n; size <<= 1 {
		wlen := cmplx.Exp(complex(0, sign*2*math.Pi/float64(size)))
		for start := 0; start < n; start += size {
			w := complex(1., 0)
			for j := 0; j < size/2; j++ {
				u, v := a[start+j], a[start+j+size/2]*w
				a[start+j] = u + v
				a[start+j+size/2] = u - v
				w *= wlen
			}
		}
	}
	if inverse {
		for i := range a {
			a[i] /= complex(float64(n), 0)
		}
	}
}
func DecodeAudio(ctx context.Context, path string, rate int) ([]float32, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-err_detect", "ignore_err", "-threads", "1", "-i", path, "-map", "0:a:0", "-vn", "-ac", "1", "-ar", fmt.Sprint(rate), "-f", "s16le", "pipe:1")
	pipe, e := cmd.StdoutPipe()
	if e != nil {
		return nil, e
	}
	if e = cmd.Start(); e != nil {
		return nil, e
	}
	samples := []float32{}
	buf := make([]byte, 64<<10)
	var pending byte
	hasPending := false
	for {
		n, err := pipe.Read(buf)
		start := 0
		if hasPending && n > 0 {
			samples = append(samples, float32(int16(uint16(pending)|uint16(buf[0])<<8))/32768)
			start = 1
			hasPending = false
		}
		for i := start; i+1 < n; i += 2 {
			samples = append(samples, float32(int16(binary.LittleEndian.Uint16(buf[i:i+2])))/32768)
		}
		if (n-start)%2 == 1 {
			pending = buf[n-1]
			hasPending = true
		}
		if err != nil {
			if err != io.EOF {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return nil, err
			}
			break
		}
	}
	e = cmd.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(samples) == 0 && e != nil {
		return nil, fmt.Errorf("decode %s: %w", path, e)
	}
	return samples, nil
}
func bandEdges() []int {
	e := make([]int, AudioBands+1)
	for i := range e {
		e[i] = int(math.Floor(300 * math.Pow(2000./300, float64(i)/AudioBands) / (float64(AudioRate) / AudioFrame)))
	}
	return e
}
func spectra(samples []float32) [][AudioBands]float32 {
	if len(samples) < AudioFrame {
		return nil
	}
	count := 1 + (len(samples)-AudioFrame)/AudioHop
	out := make([][AudioBands]float32, count)
	edges := bandEdges()
	win := make([]float32, AudioFrame)
	for i := range win {
		win[i] = float32(.5 - .5*math.Cos(2*math.Pi*float64(i)/float64(AudioFrame-1)))
	}
	a := make([]complex128, AudioFrame)
	for i := range out {
		for j := range a {
			a[j] = complex(float64(samples[i*AudioHop+j]*win[j]), 0)
		}
		fft(a, false)
		for b := 0; b < AudioBands; b++ {
			sum := 0.
			for k := edges[b]; k < max(edges[b+1], edges[b]+1); k++ {
				sum += real(a[k])*real(a[k]) + imag(a[k])*imag(a[k])
			}
			out[i][b] = float32(sum)
		}
	}
	return out
}
func printsFromSpectra(frames [][AudioBands]float32) []uint32 {
	if len(frames) < 2 {
		return []uint32{}
	}
	out := make([]uint32, len(frames)-1)
	var previous [AudioBands - 1]float32
	for i, frame := range frames {
		var logs [AudioBands]float32
		for j, e := range frame {
			logs[j] = float32(math.Log1p(float64(e)))
		}
		for j := 0; j < AudioBands-1; j++ {
			across := logs[j+1] - logs[j]
			if i > 0 && across-previous[j] > 0 {
				out[i-1] |= 1 << j
			}
			previous[j] = across
		}
	}
	return out
}
func AcousticFingerprint(samples []float32) []uint32 { return printsFromSpectra(spectra(samples)) }
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	a := append([]float64{}, values...)
	sort.Float64s(a)
	x := p / 100 * float64(len(a)-1)
	i := int(x)
	if i+1 == len(a) {
		return a[i]
	}
	return a[i] + (a[i+1]-a[i])*(x-float64(i))
}
func meanFloat(a []float64) float64 {
	if len(a) == 0 {
		return 0
	}
	s := 0.
	for _, v := range a {
		s += v
	}
	return s / float64(len(a))
}
func ReliablePitch(f0, spread float64) bool {
	return !(spread > math.Max(40, f0*.35) || spread < 8 || f0 < 69 || f0 > 360)
}
func VoiceFromPitch(f0 float64, reliable bool) string {
	if f0 <= 0 || !reliable {
		return ""
	}
	if f0 < 135 {
		return "masc"
	}
	if f0 > 175 {
		return "fem"
	}
	return ""
}
func Measure(samples []float32) Record { return measureSpectra(samples, spectra(samples)) }
func measureSpectra(samples []float32, frames [][AudioBands]float32) Record {
	if len(samples) < AudioRate {
		return Record{}
	}
	sum, peak := 0., 0.
	for _, v := range samples {
		sum += float64(v * v)
		peak = math.Max(peak, math.Abs(float64(v)))
	}
	rms := math.Sqrt(sum / float64(len(samples)))
	energies := make([]float64, len(frames))
	centroid := 0.
	edges := bandEdges()
	for i, f := range frames {
		total, weighted := 0., 0.
		for b, e := range f {
			total += float64(e)
			centre := float64(edges[b]+max(edges[b+1], edges[b]+1)-1) / 2 * AudioRate / AudioFrame
			weighted += float64(e) * centre
		}
		energies[i] = total
		if total > 0 {
			centroid += weighted / total
		}
	}
	quiet := 0
	threshold := percentile(energies, 10) * 1.5
	for _, e := range energies {
		if e < threshold {
			quiet++
		}
	}
	r := Record{"rms": rounded(rms, 5), "peak": rounded(peak, 4), "crest_db": nil, "silence_ratio": rounded(float64(quiet)/float64(len(frames)), 3), "spectral_centroid_hz": rounded(centroid/float64(len(frames)), 1)}
	if rms > 0 {
		r["crest_db"] = rounded(20*math.Log10(peak/rms), 2)
	}
	return r
}
func PitchTimbre(samples []float32) (Record, Record) {
	size := 4096
	if len(samples) < size*2 {
		return Record{}, Record{}
	}
	minLag, maxLag := AudioRate/400, AudioRate/60
	found, harmonic, levels, brightness := []float64{}, []float64{}, []float64{}, []float64{}
	spectrum := make([]complex128, size*2)
	for i := 0; i < 240; i++ {
		start := int(float64(i) * float64(len(samples)-size-1) / 239)
		frame := samples[start : start+size]
		sum, sq := 0., 0.
		for _, v := range frame {
			sum += float64(v)
			sq += float64(v * v)
		}
		level := math.Sqrt(sq / float64(size))
		if level < .01 {
			continue
		}
		mean := float32(sum / float64(size))
		for j := range spectrum {
			spectrum[j] = 0
		}
		for j, v := range frame {
			spectrum[j] = complex(float64(v-mean), 0)
		}
		fft(spectrum, false)
		for j, v := range spectrum {
			spectrum[j] = complex(real(v)*real(v)+imag(v)*imag(v), 0)
		}
		fft(spectrum, true)
		zero := real(spectrum[0])
		if zero <= 0 {
			continue
		}
		best, bestLag := real(spectrum[minLag]), minLag
		for lag := minLag + 1; lag < maxLag; lag++ {
			if v := real(spectrum[lag]); v > best {
				best = v
				bestLag = lag
			}
		}
		if best <= 0 {
			continue
		}
		lag := bestLag
		for j := minLag + 1; j < maxLag-1; j++ {
			if real(spectrum[j]) >= best*.85 && real(spectrum[j]) >= real(spectrum[j-1]) {
				lag = j
				break
			}
		}
		if real(spectrum[lag])/zero > .3 {
			found = append(found, float64(AudioRate)/float64(lag))
		}
		periodicity := math.Min(.999, math.Max(0, best/zero))
		if periodicity <= .05 {
			continue
		}
		harmonic = append(harmonic, periodicity)
		levels = append(levels, level)
		a := make([]complex128, AudioFrame)
		for j := range a {
			a[j] = complex(float64(frame[j]-mean)*(.5-.5*math.Cos(2*math.Pi*float64(j)/(AudioFrame-1))), 0)
		}
		fft(a, false)
		total, weight := 0., 0.
		for j := 0; j <= AudioFrame/2; j++ {
			p := real(a[j])*real(a[j]) + imag(a[j])*imag(a[j])
			total += p
			weight += p * float64(j) * AudioRate / AudioFrame
		}
		if total > 0 {
			brightness = append(brightness, weight/total)
		}
	}
	pitch, timbre := Record{}, Record{}
	if len(found) >= 8 {
		median := percentile(found, 50)
		spread := percentile(found, 75) - percentile(found, 25)
		pitch = Record{"f0_median_hz": rounded(median, 1), "f0_spread_hz": rounded(spread, 1), "f0_reliable": ReliablePitch(median, spread), "voiced_frames": len(found)}
	}
	if len(harmonic) >= 8 {
		ratio := percentile(harmonic, 50)
		mean := meanFloat(levels)
		variance := 0.
		for _, v := range levels {
			variance += (v - mean) * (v - mean)
		}
		timbre = Record{"hnr_db": rounded(10*math.Log10(ratio/(1-ratio)), 1), "level_variation": rounded(math.Sqrt(variance/float64(len(levels)))/math.Max(mean, 1e-9), 3), "voiced_share": rounded(float64(len(harmonic))/240, 3)}
		if len(brightness) > 0 {
			timbre["brightness_hz"] = rounded(percentile(brightness, 50), 1)
			timbre["brightness_spread_hz"] = rounded(percentile(brightness, 75)-percentile(brightness, 25), 1)
		}
	}
	return pitch, timbre
}

type AcousticMatch struct {
	Offset float64 `json:"offset"`
	Error  float64 `json:"error"`
	Frames int     `json:"frames"`
}

func (m AcousticMatch) Confident() bool { return m.Error < .35 && m.Frames >= 40 }
func FindAcoustic(needle, haystack []uint32, step int) *AcousticMatch {
	if len(needle) < 40 || len(haystack) < len(needle) {
		return nil
	}
	step = max(1, step)
	best, at := math.MaxFloat64, 0
	for i := 0; i <= len(haystack)-len(needle); i += step {
		sum := 0
		for j, v := range needle {
			sum += bits.OnesCount32(v ^ haystack[i+j])
		}
		err := float64(float32(float64(sum)/float64(len(needle)))) / 32
		if err < best {
			best = err
			at = i
		}
	}
	return &AcousticMatch{float64(at) * AudioHop / AudioRate, best, len(needle)}
}

type AcousticIdentification struct {
	Best     string        `json:"best"`
	Match    AcousticMatch `json:"match"`
	RunnerUp float64       `json:"runner_up"`
	Margin   float64       `json:"margin"`
}

func (i AcousticIdentification) Confident() bool { return i.Match.Confident() && i.Margin >= .08 }
func IdentifyAcoustic(needle []uint32, candidates map[string][]uint32, step int) *AcousticIdentification {
	type scored struct {
		name  string
		match AcousticMatch
	}
	rows := []scored{}
	for _, name := range sortedKeys(candidates) {
		if m := FindAcoustic(needle, candidates[name], step); m != nil {
			rows = append(rows, scored{name, *m})
		}
	}
	if len(rows) == 0 {
		return nil
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].match.Error < rows[j].match.Error })
	runner := .5
	if len(rows) > 1 {
		runner = rows[1].match.Error
	}
	return &AcousticIdentification{rows[0].name, rows[0].match, runner, runner - rows[0].match.Error}
}

type AcousticStore struct{ Root string }

func (s AcousticStore) Has(key string) bool {
	return exists(filepath.Join(s.Root, key+".json")) && exists(filepath.Join(s.Root, key+".u32"))
}
func (s AcousticStore) Measurements(key string) Record {
	return readJSON(filepath.Join(s.Root, key+".json"))
}
func (s AcousticStore) Prints(key string) ([]uint32, error) {
	b, e := os.ReadFile(filepath.Join(s.Root, key+".u32"))
	if e != nil {
		return nil, e
	}
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("invalid acoustic fingerprint length")
	}
	out := make([]uint32, len(b)/4)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(b[i*4:])
	}
	return out, nil
}
func (s AcousticStore) Analyze(ctx context.Context, key, path string, force bool) (Record, error) {
	if s.Has(key) && !force {
		return s.Measurements(key), nil
	}
	samples, e := DecodeAudio(ctx, path, AudioRate)
	if e != nil {
		return nil, e
	}
	frames := spectra(samples)
	prints := printsFromSpectra(frames)
	r := Record{"apiVersion": "inductor/v1", "kind": "Acoustic"}
	if len(samples) == 0 {
		r["error"] = "no audio decoded"
	} else {
		r["seconds"] = rounded(float64(len(samples))/AudioRate, 1)
		merge(r, measureSpectra(samples, frames))
		p, t := PitchTimbre(samples)
		merge(r, p)
		merge(r, t)
		r["frames"] = len(prints)
	}
	b := make([]byte, len(prints)*4)
	for i, v := range prints {
		binary.LittleEndian.PutUint32(b[i*4:], v)
	}
	if _, e = AtomicWrite(filepath.Join(s.Root, key+".u32"), b, 0644); e != nil {
		return nil, e
	}
	return r, writeJSON(filepath.Join(s.Root, key+".json"), r)
}
