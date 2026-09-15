// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed workers/queue_worker.py workers/voice_worker.py workers/decode.py
var workerFiles embed.FS
var safeJobID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

type GPUBox struct {
	Host, Directory string
	Config          Config
	Poll            time.Duration
	mu              sync.Mutex
}

func NewGPUBox(c Config, directory string) *GPUBox {
	if directory == "" {
		directory = str(first(c.Transcribe.RemoteDir, "inductor"))
	}
	if c.Transcribe.Remote == "" {
		directory = filepath.Join(c.Cache, "gpu-worker")
	} else {
		directory = strings.TrimPrefix(directory, "~/")
	}
	return &GPUBox{Host: c.Transcribe.Remote, Directory: directory, Config: c}
}
func (b *GPUBox) run(ctx context.Context, script string) ([]byte, error) {
	if b.Host == "" {
		return command(ctx, "sh", "-c", script)
	}
	if strings.HasPrefix(b.Host, "-") {
		return nil, fmt.Errorf("invalid SSH host")
	}
	return command(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=30", b.Host, script)
}
func (b *GPUBox) send(ctx context.Context, local, name string) error {
	if b.Host == "" {
		_, e := Place(local, filepath.Join(b.Directory, name), "copy")
		return e
	}
	_, e := command(ctx, "scp", "-q", "-o", "BatchMode=yes", local, b.Host+":"+b.Directory+"/"+name)
	return e
}
func (b *GPUBox) Provision(ctx context.Context, install bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	dir := shellQuote(b.Directory)
	if _, e := b.run(ctx, "mkdir -p "+dir+"/jobs "+dir+"/out "+dir+"/failed "+dir+"/audio"); e != nil {
		return e
	}
	if install {
		script := "cd " + dir + " && (test -x venv/bin/python || python3 -m venv venv) && (venv/bin/python -c 'import faster_whisper, speechbrain, torch, av' || venv/bin/python -m pip install faster-whisper torch torchaudio speechbrain av nvidia-cublas-cu12 nvidia-cudnn-cu12)"
		if _, e := b.run(ctx, script); e != nil {
			return fmt.Errorf("GPU worker dependencies: %w", e)
		}
	}
	entries, _ := workerFiles.ReadDir("workers")
	tmp, e := os.MkdirTemp("", "inductor-workers-*")
	if e != nil {
		return e
	}
	defer os.RemoveAll(tmp)
	for _, entry := range entries {
		data, e := workerFiles.ReadFile("workers/" + entry.Name())
		if e != nil {
			return e
		}
		path := filepath.Join(tmp, entry.Name())
		if e = os.WriteFile(path, data, 0600); e != nil {
			return e
		}
		if e = b.send(ctx, path, entry.Name()); e != nil {
			return e
		}
	}
	return nil
}
func (b *GPUBox) Start(ctx context.Context) error {
	device := "cuda"
	if b.Host == "" {
		device = "cpu"
	}
	script := "cd " + shellQuote(b.Directory) + " && rm -f STOP && export LD_LIBRARY_PATH=$(find \"$PWD/venv/lib\" -path \"*/nvidia/*/lib\" -type d 2>/dev/null | paste -sd:):${LD_LIBRARY_PATH:-} && (STT_MODEL=" + shellQuote(b.Config.Transcribe.Model) + " INDUCTOR_DEVICE=" + shellQuote(device) + " nohup ./venv/bin/python -u queue_worker.py >>worker.log 2>&1 </dev/null & )"
	_, e := b.run(ctx, script)
	return e
}
func (b *GPUBox) Stop(ctx context.Context) error {
	_, e := b.run(ctx, "touch "+shellQuote(b.Directory+"/STOP"))
	return e
}
func (b *GPUBox) Enqueue(ctx context.Context, id, kind, audio string) error {
	if !safeJobID.MatchString(id) || !contains([]string{"transcribe", "embed"}, kind) {
		return fmt.Errorf("invalid GPU job")
	}
	ext := strings.ToLower(filepath.Ext(audio))
	name := "audio/" + id + ext
	spec := Record{"id": id, "kind": kind, "audio": name, "batch_size": b.Config.Transcribe.BatchSize, "language": b.Config.Transcribe.Language}
	if _, e := b.run(ctx, "test -f "+shellQuote(b.Directory+"/jobs/"+id+".json")+" || test -f "+shellQuote(b.Directory+"/out/"+id+".json")); e == nil {
		return nil
	}
	if e := b.send(ctx, audio, name); e != nil {
		return e
	}
	f, e := os.CreateTemp("", "inductor-job-*.json")
	if e != nil {
		return e
	}
	path := f.Name()
	defer os.Remove(path)
	e = json.NewEncoder(f).Encode(spec)
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = b.send(ctx, path, "jobs/."+id+".json"); e != nil {
		return e
	}
	_, e = b.run(ctx, "mv "+shellQuote(b.Directory+"/jobs/."+id+".json")+" "+shellQuote(b.Directory+"/jobs/"+id+".json"))
	return e
}
func (b *GPUBox) Collect(ctx context.Context, into string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := os.MkdirAll(into, 0755); e != nil {
		return 0, e
	}
	raw, e := b.run(ctx, "ls -1 "+shellQuote(b.Directory+"/out"))
	if e != nil {
		return 0, e
	}
	count := 0
	for _, name := range pythonFields(string(raw)) {
		if !strings.HasSuffix(name, ".json") || !safeJobID.MatchString(strings.TrimSuffix(name, ".json")) {
			continue
		}
		tmpDir, e := os.MkdirTemp(into, ".collect-*")
		if e != nil {
			return count, e
		}
		target := filepath.Join(tmpDir, name)
		if b.Host == "" {
			_, e = Place(filepath.Join(b.Directory, "out", name), target, "copy")
		} else {
			_, e = command(ctx, "scp", "-q", "-o", "BatchMode=yes", b.Host+":"+b.Directory+"/out/"+name, target)
		}
		if e == nil {
			payload := readJSON(target)
			if payload == nil {
				e = fmt.Errorf("invalid GPU result %s", name)
			} else {
				data, err := os.ReadFile(target)
				e = err
				if e == nil {
					_, e = AtomicWrite(filepath.Join(into, name), data, 0644)
				}
			}
		}
		_ = os.RemoveAll(tmpDir)
		if e != nil {
			return count, e
		}
		if _, e = b.run(ctx, "rm -f "+shellQuote(b.Directory+"/out/"+name)); e != nil {
			return count, e
		}
		count++
	}
	return count, nil
}
func (b *GPUBox) Work(ctx context.Context, id, kind, audio, landing string) (Record, error) {
	path := filepath.Join(landing, id+".json")
	if r := readJSON(path); r != nil {
		if truth(r["ok"]) {
			return record(r["result"]), nil
		}
		_ = os.Remove(path)
	}
	if e := b.Enqueue(ctx, id, kind, audio); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	for n := 0; ; n++ {
		if _, e := b.Collect(ctx, landing); e != nil {
			return nil, e
		}
		if r := readJSON(path); r != nil {
			if !truth(r["ok"]) {
				return nil, fmt.Errorf("GPU %s: %s", id, str(r["error"]))
			}
			return record(r["result"]), nil
		}
		delay := b.Poll
		if delay <= 0 {
			delay = time.Second
		}
		if n >= 20 && b.Poll <= 0 {
			delay = 5 * time.Second
		}
		if e := pause(ctx, delay); e != nil {
			return nil, e
		}
	}
}

func (b *GPUBox) Kill(ctx context.Context) error {
	script := `import os, pathlib, signal
root = pathlib.Path.cwd()
pid = int((root / "worker.pid").read_text())
proc = pathlib.Path("/proc") / str(pid)
if proc.exists():
    if (proc / "cwd").resolve() != root or b"queue_worker.py" not in (proc / "cmdline").read_bytes():
        raise SystemExit("worker PID no longer belongs to this queue")
    os.kill(pid, signal.SIGTERM)
`
	_, e := b.run(ctx, "cd "+shellQuote(b.Directory)+" && ./venv/bin/python -c "+shellQuote(script))
	return e
}
