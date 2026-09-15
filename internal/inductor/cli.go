// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const Version = "0.1.0"

//go:embed cli_schema.json
var cliSchema []byte

type optionSpec struct {
	Names   []string `json:"names"`
	Action  string   `json:"action"`
	Default any      `json:"default"`
	Help    string   `json:"help"`
	Choices []string `json:"choices"`
	Type    string   `json:"type"`
	Nargs   any      `json:"nargs"`
}
type commandSpec struct {
	Options []optionSpec `json:"options"`
}
type Arguments struct {
	Root, Command string
	Values        Record
	Present       map[string]bool
	Positionals   []string
	Help, Version bool
}

func (a Arguments) String(k string) string    { return str(a.Values[k]) }
func (a Arguments) Bool(k string) bool        { return truth(a.Values[k]) }
func (a Arguments) Int(k string) int          { return integer(a.Values[k]) }
func (a Arguments) Float(k string) float64    { return number(a.Values[k]) }
func (a Arguments) Strings(k string) []string { return texts(a.Values[k]) }
func optionKey(s optionSpec) string {
	for _, name := range s.Names {
		if strings.HasPrefix(name, "--") {
			return strings.ReplaceAll(strings.TrimPrefix(name, "--"), "-", "_")
		}
	}
	return strings.ReplaceAll(strings.TrimLeft(s.Names[0], "-"), "-", "_")
}
func schema() map[string]commandSpec {
	var s map[string]commandSpec
	if err := json.Unmarshal(cliSchema, &s); err != nil {
		panic(err)
	}
	return s
}
func ParseArgs(argv []string) (Arguments, error) {
	a := Arguments{Root: ".", Values: Record{}, Present: map[string]bool{}}
	rest := []string{}
	for i := 0; i < len(argv); i++ {
		v := argv[i]
		if v == "--root" || v == "-r" {
			i++
			if i == len(argv) {
				return a, fmt.Errorf("%s requires a path", v)
			}
			a.Root = argv[i]
		} else if strings.HasPrefix(v, "--root=") {
			a.Root = strings.TrimPrefix(v, "--root=")
		} else {
			rest = append(rest, v)
		}
	}
	if len(rest) == 0 {
		return a, fmt.Errorf("a command is required; use --help")
	}
	if rest[0] == "--version" {
		a.Version = true
		return a, nil
	}
	if rest[0] == "--help" || rest[0] == "-h" {
		a.Help = true
		return a, nil
	}
	a.Command = rest[0]
	rest = rest[1:]
	s := schema()
	spec, ok := s[a.Command]
	if !ok {
		return a, fmt.Errorf("unknown command %q", a.Command)
	}
	if a.Command == "transcribe" {
		for i, v := range rest {
			if contains([]string{"run", "status", "audit", "worker"}, v) {
				a.Command += "/" + v
				spec.Options = append(spec.Options, s[a.Command].Options...)
				rest = append(rest[:i], rest[i+1:]...)
				break
			}
		}
		if a.Command == "transcribe" && !contains(rest, "--help") && !contains(rest, "-h") {
			return a, fmt.Errorf("transcribe requires run, status, audit, or worker")
		}
	}
	byName := map[string]optionSpec{}
	for _, o := range spec.Options {
		k := optionKey(o)
		if o.Default != nil {
			a.Values[k] = o.Default
		}
		if o.Action == "append" && a.Values[k] == nil {
			a.Values[k] = []string{}
		}
		for _, n := range o.Names {
			if strings.HasPrefix(n, "-") {
				byName[n] = o
			}
		}
	}
	literal := false
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		if literal {
			a.Positionals = append(a.Positionals, arg)
			continue
		}
		if arg == "--" {
			literal = true
			continue
		}
		if arg == "--help" || arg == "-h" {
			a.Help = true
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			a.Positionals = append(a.Positionals, arg)
			continue
		}
		name, value, inline := strings.Cut(arg, "=")
		o, ok := byName[name]
		if !ok {
			return a, fmt.Errorf("unrecognized argument: %s", arg)
		}
		k := optionKey(o)
		a.Present[k] = true
		if o.Action == "store_true" || o.Action == "store_false" {
			if inline {
				return a, fmt.Errorf("%s does not take a value", name)
			}
			a.Values[k] = o.Action == "store_true"
			continue
		}
		if !inline {
			i++
			if i == len(rest) {
				return a, fmt.Errorf("%s requires a value", name)
			}
			value = rest[i]
		}
		if len(o.Choices) > 0 && !contains(o.Choices, value) {
			return a, fmt.Errorf("%s: choose from %s", name, strings.Join(o.Choices, ", "))
		}
		var v any = value
		switch o.Type {
		case "int":
			n, err := strconv.Atoi(value)
			if err != nil {
				return a, fmt.Errorf("%s requires an integer", name)
			}
			v = n
		case "float":
			n, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return a, fmt.Errorf("%s requires a number", name)
			}
			v = n
		}
		if o.Action == "append" {
			a.Values[k] = append(array(a.Values[k]), v)
		} else {
			a.Values[k] = v
		}
	}
	if len(a.Positionals) > 0 && a.Command != "add" && a.Command != "transcribe/worker" {
		return a, fmt.Errorf("unexpected arguments: %s", strings.Join(a.Positionals, " "))
	}
	if a.Command == "transcribe/worker" && !a.Help && (len(a.Positionals) != 1 || !contains([]string{"start", "stop", "kill", "log", "failures"}, a.Positionals[0])) {
		return a, fmt.Errorf("worker requires start, stop, kill, log, or failures")
	}
	return a, nil
}
func Help(w io.Writer, command string) {
	fmt.Fprintln(w, "Inductor — build a Hypnotica content tree from audio.")
	if command == "" {
		fmt.Fprintln(w, "\nUsage: inductor [-r ROOT] COMMAND [OPTIONS]\n\nCommands:")
		for _, n := range sortedKeys(schema()) {
			if !strings.Contains(n, "/") {
				fmt.Fprintln(w, "  "+n)
			}
		}
		fmt.Fprintln(w, "\nUse inductor COMMAND --help for options.")
		return
	}
	fmt.Fprintf(w, "\nUsage: inductor [-r ROOT] %s [OPTIONS]\n\n", strings.ReplaceAll(command, "/", " "))
	spec := schema()[command]
	if command == "transcribe" {
		fmt.Fprintln(w, "Subcommands: run, status, audit, worker")
	}
	for _, o := range spec.Options {
		fmt.Fprintf(w, "  %-24s %s\n", strings.Join(o.Names, ", "), o.Help)
	}
}
func Main(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	a, err := ParseArgs(argv)
	if err != nil {
		fmt.Fprintln(stderr, "inductor:", err)
		return 2
	}
	if a.Version {
		fmt.Fprintln(stdout, Version)
		return 0
	}
	if a.Help {
		Help(stdout, a.Command)
		return 0
	}
	c, err := LoadConfig(a.Root)
	if err != nil {
		fmt.Fprintln(stderr, "inductor:", err)
		return 1
	}
	engine := NewEngine(c)
	var outputMu sync.Mutex
	engine.Say = func(format string, v ...any) {
		outputMu.Lock()
		defer outputMu.Unlock()
		fmt.Fprintf(stdout, format+"\n", v...)
	}
	report, err := engine.Dispatch(ctx, a)
	if usage := engine.API.Usage(); len(usage) > 0 {
		if report == nil {
			report = Record{}
		}
		report["usage"] = usage
	}
	if report != nil {
		b, _ := json.MarshalIndent(report, "", "  ")
		fmt.Fprintln(stdout, string(b))
	}
	closeErr := engine.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		fmt.Fprintln(stderr, "inductor:", err)
		return 1
	}
	return 0
}
func (e *Engine) Dispatch(ctx context.Context, a Arguments) (Record, error) {
	c := e.Config
	model := a.String("model")
	switch a.Command {
	case "check":
		r, err := LoadSources(c.Sources)
		if err != nil {
			return nil, err
		}
		authors := map[string]int{}
		missing, remote := 0, 0
		for _, s := range r.Sources {
			authors[s.AuthorID()]++
			if strings.HasPrefix(s.Audio, "http://") || strings.HasPrefix(s.Audio, "https://") {
				remote++
			} else if s.AudioPath(c.Sources) == "" {
				missing++
			}
		}
		out := Record{"sources": len(r.Sources), "authors": authors, "audio_missing": missing, "audio_remote": remote, "errors": r.Errors, "warnings": r.Warnings}
		if len(r.Errors) > 0 {
			return out, fmt.Errorf("%d source error(s)", len(r.Errors))
		}
		return out, nil
	case "ingest", "run":
		return e.ingestCommand(ctx, a)
	case "retag":
		return RetagTree(c, a.String("author"), a.Bool("write"))
	case "fold":
		return FoldTree(c, a.Bool("write"))
	case "paths":
		return PortableTree(c, a.Bool("write"))
	case "orphans":
		return Orphans(c, a.Bool("write"))
	case "attribute":
		return AttributeTree(c, a.Bool("redo"), a.Bool("write"))
	case "migrate":
		return Migrate(c, a.Bool("dry_run"))
	case "export":
		return ExportTree(c, str(first(a.String("content"), c.Content)), str(first(a.String("out"), c.Sources)), a.String("snapshot"), a.String("assets"))
	case "duplicates":
		groups, err := FindDuplicates(c.Content)
		if err != nil {
			return nil, err
		}
		report := Record{}
		all := [][]Document{}
		for _, k := range []string{SameTitle, SameCreator, CrossCreator} {
			report[k] = len(groups[k])
			all = append(all, groups[k]...)
		}
		decisions := map[string]bool{}
		if a.Bool("resolve") {
			p := a.String("decisions")
			if p != "" {
				for _, v := range array(optionalYAML(p)["keep"]) {
					decisions[str(record(v)["path"])] = true
				}
			}
		} else {
			all = groups[SameTitle]
		}
		lines, err := ResolveDuplicates(all, decisions, c.Content, !a.Bool("resolve"), a.Bool("merge") || (a.Bool("resolve") && a.Bool("write")))
		report["merges"] = lines
		return report, err
	case "tagmap":
		if a.Bool("adopt") {
			rows, err := AdoptTagmaps(c, a.Strings("author"), a.Bool("write"))
			return Record{"added": rows}, err
		}
		if model == "" {
			model = c.Enrich.AdjudicatorModel
		}
		return e.BuildTagmaps(ctx, a.Strings("author"), model, a.Bool("dry_run"))
	case "adjudicate":
		p := str(first(a.String("rulings"), filepath.Join(c.Cache, "rulings.yaml")))
		if a.Bool("apply") {
			r, err := readYAML(p)
			if err != nil {
				return nil, err
			}
			return ApplyRulings(c, array(r["rulings"]), a.Bool("write"))
		}
		if model == "" {
			model = c.Enrich.AdjudicatorModel
		}
		return e.Adjudicate(ctx, a.Bool("from_reviews"), a.Bool("collect"), model, p)
	case "backfill":
		return Backfill(c, a.Strings("tag"), a.Bool("write"))
	case "reconsider":
		if model == "" {
			model = c.Enrich.ReviewModel
		}
		tags := a.Strings("tag")
		if len(tags) == 0 {
			return nil, fmt.Errorf("give at least one --tag")
		}
		return e.Reconsider(ctx, tags, model, a.Bool("write"))
	case "retitle":
		if model == "" {
			model = c.Enrich.AnalysisModel
		}
		return e.Retitle(ctx, a.String("author"), model, a.Int("limit"), a.Bool("tidy"), a.Bool("write"))
	case "cover-prompts":
		if model == "" {
			model = c.Enrich.AnalysisModel
		}
		return e.CoverPrompts(ctx, model, a.Int("workers"), a.Int("limit"), a.Bool("dry_run"))
	case "authors":
		if model == "" {
			model = c.Enrich.AnalysisModel
		}
		return e.Authors(ctx, AuthorOptions{Model: model, Limit: a.Int("limit"), Redo: a.Bool("redo"), KeepSynopsis: a.Bool("keep_synopsis"), Render: !a.Bool("no_render"), Write: a.Bool("write"), Workers: 4})
	case "artwork":
		return e.Artwork(ctx, a.String("author"), a.Int("limit"), a.Int("workers"), a.Bool("redo"), a.Bool("write"))
	case "similar":
		floor := a.Float("floor")
		if !a.Present["floor"] {
			floor = .55
		}
		return SimilarVoices(c, floor, a.Int("most"), a.Bool("write"))
	case "voiceprint":
		return e.voiceCommand(ctx, a)
	case "acoustic":
		sources, err := LoadSources(c.Sources)
		if err != nil {
			return nil, err
		}
		jobs := []*Source{}
		for _, s := range sources.Sources {
			if s.AudioPath(c.Sources) != "" {
				jobs = append(jobs, s)
			}
		}
		if n := a.Int("limit"); n > 0 {
			jobs = jobs[:min(n, len(jobs))]
		}
		workers := a.Int("workers")
		if workers <= 0 {
			workers = runtime.NumCPU()
		}
		results, errs := parallelMap(ctx, jobs, workers, func(ctx context.Context, s *Source) (bool, error) {
			fp, err := e.Fingerprints.Of(s.AudioPath(c.Sources))
			if err != nil {
				return false, err
			}
			if e.Acoustic.Has(fp) && !a.Bool("force") {
				return true, nil
			}
			_, err = e.Acoustic.Analyze(ctx, fp, s.AudioPath(c.Sources), a.Bool("force"))
			return false, err
		})
		cached, failed := 0, 0
		for i, r := range results {
			if r {
				cached++
			}
			if errs[i] != nil {
				failed++
				e.Say("acoustic %s: %v", jobs[i].Title, errs[i])
			}
		}
		return Record{"analysed": len(jobs) - cached - failed, "cached": cached, "failed": failed}, nil
	case "acoustic-apply":
		return e.AcousticApply(a.Bool("dry_run"))
	case "add":
		return e.Add(ctx, AddOptions{Paths: a.Positionals, Author: a.String("author"), AuthorName: a.String("author_name"), URL: a.String("url"), Series: a.String("series"), Date: a.String("date"), Tags: a.Strings("tag"), NotExplicit: a.Bool("not_explicit"), DryRun: a.Bool("dry_run")})
	case "compare":
		return e.Compare(ctx, CompareOptions{Items: a.Int("items"), Seed: a.Int("seed"), Workers: a.Int("workers"), Poll: time.Duration(a.Int("poll")) * time.Second, Models: a.Strings("model"), Solo: a.Strings("solo"), SampleFile: a.String("sample_file")})
	case "transcribe/run", "transcribe/status", "transcribe/audit", "transcribe/worker":
		return e.transcribeCommand(ctx, a)
	}
	return nil, fmt.Errorf("unknown command %s", a.Command)
}
func readLines(path string) ([]string, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	out := []string{}
	for _, line := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(line); s != "" && !strings.HasPrefix(s, "#") {
			out = append(out, s)
		}
	}
	return out, nil
}
