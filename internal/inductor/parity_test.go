// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"testing"
)

func TestPythonParity(t *testing.T) {
	data, err := os.ReadFile("testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Op    string `json:"op"`
		Input any    `json:"input"`
		Want  any    `json:"want"`
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, c := range cases {
		var got any
		s := str(c.Input)
		switch c.Op {
		case "slug":
			got = Slug(s)
		case "fold":
			got = Fold(s)
		case "unrun":
			got = Unrun(s)
		case "machine":
			got = LooksMachineMade(s)
		case "not_title":
			got = NotReallyTitle(s)
		case "tidy_case":
			got = TidyCase(s)
		case "title_quality":
			q := TitleQuality(s)
			got = []any{q[0] != 0, q[1] != 0, q[2] != 0, q[3] != 0, q[4]}
		case "split":
			got = SplitSentences(s)
		case "transcript_key":
			got = TranscriptKey(s)
		case "tidy":
			title, notes := TidyTitle(s)
			got = Record{"title": title, "notes": notes}
		case "sentences":
			got = Sentences(record(c.Input))
		case "delivery":
			got = Delivery(record(c.Input))
		case "measured_block":
			got = MeasuredBlock(record(c.Input))
			if got == nil {
				got = []string{}
			}
		default:
			t.Fatalf("unknown oracle operation %s", c.Op)
		}
		if !equivalent(got, c.Want) {
			failed++
			if failed <= 20 {
				t.Errorf("%s(%q)\n got: %v\nwant: %v", c.Op, s, got, c.Want)
			}
		}
	}
	if failed > 20 {
		t.Errorf("%d total parity mismatches", failed)
	}
}
func TestAcousticParity(t *testing.T) {
	data, err := os.ReadFile("testdata/signal.f32")
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]float32, len(data)/4)
	for i := range samples {
		samples[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
	}
	got := Measure(samples)
	pitch, timbre := PitchTimbre(samples)
	merge(got, pitch)
	merge(got, timbre)
	want := readJSON("testdata/acoustic.json")
	for k, v := range want {
		if _, ok := v.(bool); ok {
			if got[k] != v {
				t.Errorf("%s: got %v want %v", k, got[k], v)
			}
			continue
		}
		tolerance := .11
		if k == "rms" || k == "peak" || k == "silence_ratio" || k == "level_variation" || k == "voiced_share" {
			tolerance = .0011
		}
		if got[k] == nil || math.Abs(number(got[k])-number(v)) > tolerance {
			t.Errorf("%s: got %v want %v", k, got[k], v)
		}
	}
	prints := AcousticFingerprint(samples)
	raw, err := os.ReadFile("testdata/signal.u32")
	if err != nil {
		t.Fatal(err)
	}
	if len(prints) != len(raw)/4 {
		t.Fatalf("fingerprint frame count: got %d want %d", len(prints), len(raw)/4)
	}
	different := 0
	for i, v := range prints {
		if v != binary.LittleEndian.Uint32(raw[i*4:]) {
			different++
		}
	}
	if different > len(prints)/100 {
		t.Errorf("%d/%d fingerprint frames differ", different, len(prints))
	}
}

func TestPythonShuffleParity(t *testing.T) {
	data, err := os.ReadFile("testdata/shuffle.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Seed int64
		N    int
		Want []int
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		got := make([]int, c.N)
		for i := range got {
			got[i] = i
		}
		newPythonRandom(c.Seed).shuffle(c.N, func(i, j int) { got[i], got[j] = got[j], got[i] })
		if !equivalent(got, c.Want) {
			t.Errorf("shuffle differs: seed %d, n %d", c.Seed, c.N)
		}
	}
}
