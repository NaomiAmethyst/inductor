// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
)

type TranscribeSettings struct {
	Model string `yaml:"model"`
	// Fallback names stronger models to hand a thin transcript to. The first
	// model runs with the voice gate on; when what comes back is too sparse for
	// the running time these are tried in turn, and the transcript records
	// whichever one actually answered.
	Fallback  []string      `yaml:"fallback"`
	Remote    string        `yaml:"remote"`
	RemoteDir string        `yaml:"remote_dir"`
	BatchSize int           `yaml:"batch_size"`
	Language  string        `yaml:"language"`
	Workers   int           `yaml:"workers"`
	Sound     SoundSettings `yaml:"sound"`
}

// SoundSettings configures the pass that asks what a recording sounds like
// rather than what it says. Two models, because they answer different
// questions: the tagger is multi-label over a fixed ontology, so its "there is
// a voice here" is an absolute reading that stands whatever else is true of the
// audio; the zero-shot model ranks whatever phrases it is handed, which makes
// it the one worth giving a vocabulary to and the one that cannot say "none of
// these". Either may be left empty to skip it.
type SoundSettings struct {
	Tagger   string `yaml:"tagger"`
	Zeroshot string `yaml:"zeroshot"`
	// Labels is the vocabulary the zero-shot model ranks. The defaults describe
	// sound in general; a collection with its own subject matter should say so
	// here, because a label that is never offered is never returned.
	Labels []string `yaml:"labels"`
	// Voice is the ontology labels that mean somebody is speaking, and Threshold
	// how sure the tagger has to be. This is the gate that decides whether a
	// thin transcript is a failure to hear or a recording with no words in it.
	Voice     []string `yaml:"voice"`
	Threshold float64  `yaml:"threshold"`
	// Floor is how close a zero-shot label must actually be before it counts as
	// an identification rather than the nearest thing on a list the model was
	// not allowed to refuse. Below it the labels are recorded and not believed.
	Floor float64 `yaml:"floor"`
}
type EnrichSettings struct {
	AnalysisModel string `yaml:"analysis_model"`
	ReviewModel   string `yaml:"review_model"`
	// ReviewFallback names models to offer a review to when the first one will
	// not answer. A refusal is a fact about the model, not about the recording.
	ReviewFallback   []string `yaml:"review_fallback"`
	AdjudicatorModel string   `yaml:"adjudicator_model"`
	Workers          int      `yaml:"workers"`
	BatchSize        int      `yaml:"batch_size"`
	KeyFile          string   `yaml:"key_file"`
	ComfyURL         string   `yaml:"comfy_url"`
	Covers           bool     `yaml:"covers"`
	CoverEngine      string   `yaml:"cover_engine"`
	CoverWidth       int      `yaml:"cover_width"`
	CoverHeight      int      `yaml:"cover_height"`
}
type MediaSettings struct {
	Mode      string `yaml:"mode"`
	Transcode string `yaml:"transcode"`
}
type Config struct {
	Root, Sources, Content, Media, Covers, Cache, Decisions string
	Transcribe                                              TranscribeSettings
	Enrich                                                  EnrichSettings
	MediaSettings                                           MediaSettings
}

func LoadConfig(root string) (Config, error) {
	c := Config{Root: absolute(root), Transcribe: TranscribeSettings{Model: "distil-large-v3", RemoteDir: "~/inductor-stt", BatchSize: 16, Language: "en", Workers: 6, Sound: DefaultSound()}, Enrich: EnrichSettings{AnalysisModel: "deepseek/deepseek-v4-flash", ReviewModel: "google/gemini-3.8-flash:batch", ReviewFallback: []string{"z-ai/glm-5.3-flash"}, AdjudicatorModel: "anthropic/claude-opus-5", Workers: 12, BatchSize: 150, Covers: true, CoverEngine: "turbo", CoverWidth: 1024, CoverHeight: 576}, MediaSettings: MediaSettings{"symlink", "if-needed"}}
	r := Record{}
	for _, n := range []string{"inductor.yaml", "inductor.yml"} {
		p := filepath.Join(c.Root, n)
		if exists(p) {
			var e error
			r, e = readYAML(p)
			if e != nil {
				return c, e
			}
			break
		}
	}
	paths := record(r["paths"])
	at := func(k, d string) string {
		p := expandHome(str(first(paths[k], d)))
		if !filepath.IsAbs(p) {
			p = filepath.Join(c.Root, p)
		}
		return absolute(p)
	}
	c.Sources = at("sources", "sources")
	c.Content = at("content", "content")
	c.Media = at("media", "media")
	c.Covers = at("covers", "media/cover")
	c.Cache = at("cache", ".inductor")
	c.Decisions = at("decisions", "state/decisions")
	for k, d := range map[string]any{"transcribe": &c.Transcribe, "enrich": &c.Enrich, "media": &c.MediaSettings} {
		if v := r[k]; v != nil {
			b, e := yaml.Marshal(v)
			if e != nil {
				return c, e
			}
			if e = yaml.Unmarshal(b, d); e != nil {
				return c, fmt.Errorf("%s: %w", k, e)
			}
		}
	}
	if !contains([]string{"symlink", "hardlink", "copy"}, c.MediaSettings.Mode) {
		return c, fmt.Errorf("invalid media.mode %q", c.MediaSettings.Mode)
	}
	if !contains([]string{"never", "always", "if-needed"}, c.MediaSettings.Transcode) {
		return c, fmt.Errorf("invalid media.transcode %q", c.MediaSettings.Transcode)
	}
	if c.Transcribe.Workers < 1 || c.Transcribe.BatchSize < 1 || c.Enrich.Workers < 1 || c.Enrich.BatchSize < 1 {
		return c, fmt.Errorf("worker counts and batch sizes must be positive")
	}
	if c.Enrich.CoverWidth < 1 || c.Enrich.CoverHeight < 1 {
		return c, fmt.Errorf("cover dimensions must be positive")
	}
	if c.Transcribe.Remote == "" {
		c.Transcribe.Remote = os.Getenv("INDUCTOR_GPU_HOST")
	}
	return c, nil
}
func (c Config) State() string { return filepath.Join(c.Root, "state") }
func either(a, b string) string {
	if isDir(a) {
		return a
	}
	if isDir(b) {
		return b
	}
	return a
}
func (c Config) Transcripts() string {
	return either(filepath.Join(c.State(), "transcripts"), filepath.Join(c.Content, ".hypnotica", "transcripts"))
}
func (c Config) Analysis() string {
	return either(filepath.Join(c.State(), "enrichment"), filepath.Join(c.Content, ".hypnotica", "analysis"))
}
func (c Config) RegistryPath() string { return filepath.Join(c.Content, "tags.yaml") }
func (c Config) Portable(value, near string) string {
	v := strings.TrimSpace(value)
	if v == "" || !filepath.IsAbs(expandHome(v)) {
		return v
	}
	p := absolute(v)
	rel, e := filepath.Rel(c.Root, p)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	if near == "" {
		near = c.Root
	}
	rel, e = filepath.Rel(absolute(near), p)
	if e != nil {
		return p
	}
	return rel
}
func (c Config) Resolved(value, near string) string {
	v := expandHome(strings.TrimSpace(value))
	if filepath.IsAbs(v) || v == "" {
		return v
	}
	bases := []string{}
	if near != "" {
		bases = append(bases, near)
	}
	bases = append(bases, c.Root)
	for _, b := range bases {
		p := filepath.Join(b, v)
		if exists(p) {
			return p
		}
	}
	return filepath.Join(bases[0], v)
}
func (c Config) PortablePaths(r Record, near string) {
	for _, f := range []string{"audio", "video", "cover", "image"} {
		if truth(r[f]) {
			r[f] = c.Portable(str(r[f]), near)
		}
	}
	p := record(r["provenance"])
	if truth(p["source_key"]) {
		p["source_key"] = c.Portable(str(p["source_key"]), "")
	}
	if truth(p["merged_source_keys"]) {
		a := texts(p["merged_source_keys"])
		for i := range a {
			a[i] = c.Portable(a[i], "")
		}
		p["merged_source_keys"] = a
	}
}
func (c Config) APIKey() (string, error) {
	if s := os.Getenv("OPENROUTER_API_KEY"); s != "" {
		return s, nil
	}
	for _, p := range []string{c.Enrich.KeyFile, "~/.config/openrouter/key", "~/.openrouter-key"} {
		if p == "" {
			continue
		}
		if b, e := os.ReadFile(expandHome(p)); e == nil && strings.TrimSpace(string(b)) != "" {
			return strings.TrimSpace(string(b)), nil
		}
	}
	return "", fmt.Errorf("no OpenRouter key; set OPENROUTER_API_KEY or enrich.key_file")
}
