# Configuration and services

Inductor loads `inductor.yaml`, falling back to `inductor.yml`, under `--root`.
Unknown settings are ignored. Paths in the `paths` section are relative to the
library root. See the [complete example](../examples/library/inductor.yaml).

## Audio

`media.mode` accepts `symlink`, `hardlink`, or `copy`. Hard links fall back to
symlinks across filesystems. `media.transcode` accepts `never`, `if-needed`, or
`always`. File signatures take precedence over misleading extensions. Video is
demultiplexed when possible; other conversions use FFmpeg MP3 encoding. Outputs
are checked for a usable audio stream before installation.

Relative source audio paths are resolved from the source record's directory and
then the source tree. Existing paths supplied directly are also accepted. Output
asset references inside the library are relative to their YAML document.

## Transcription and speaker embeddings

The Python worker uses faster-whisper and SpeechBrain ECAPA. `transcribe.remote`
is an SSH host, or empty for a local CPU worker. `INDUCTOR_GPU_HOST` supplies the
host when the setting is empty. `remote_dir` defaults to `~/inductor-stt`;
`model`, `language`, and `batch_size` configure inference. `workers` controls
concurrent audio readers and job submissions.

The local worker lives in `.inductor/gpu-worker`. Remote provisioning requires
Python with `venv` and `pip`, a compatible NVIDIA driver, and noninteractive SSH
access. It installs Python and CUDA dependencies. A worker lock serializes
inference within the queue. Use `run --no-provision` with a preinstalled `venv`.

```sh
inductor -r /path/to/library transcribe worker start
inductor -r /path/to/library transcribe worker log --lines 50
inductor -r /path/to/library transcribe status
inductor -r /path/to/library transcribe run --path /path/to/audio
inductor -r /path/to/library transcribe audit --out short-fingerprints.txt
inductor -r /path/to/library transcribe worker stop
```

`stop` drains accepted jobs. Failures land in both `out` and `failed`, so callers
receive errors instead of polling forever. Jobs remain until results settle;
subsequent runs collect completed work. Stop the legacy standalone transcription
daemon before starting this worker on the same GPU.

## Language models

Key lookup order is `OPENROUTER_API_KEY`, `enrich.key_file`,
`~/.config/openrouter/key`, then `~/.openrouter-key`.

`analysis_model`, `review_model`, and `adjudicator_model` select their respective
roles. Defaults are retained from Python; availability depends on the provider.
A review model ending in `:batch` uses OpenRouter batches. `workers` controls
synchronous concurrency; `batch_size` controls review requests per batch. Usage
reports use billed cost when available, otherwise model pricing estimates.

Requests contain transcript evidence and metadata. They are made when a command
runs that stage. Recover a previously submitted batch with:

```sh
inductor -r /path/to/library ingest --stage review --recover BATCH_ID
```

Journals under `.inductor/batches` retain IDs, transcript keys and sentence
evidence. Keep them while jobs are outstanding.

## Artwork

Set `enrich.comfy_url` or `COMFY_URL`; the fallback is `http://127.0.0.1:8188`.
Embedded workflows cover `turbo`, `sdxl`, and `flux`. Check their model filenames
and nodes in [the workflow templates](../internal/inductor/prompts) against your
ComfyUI installation.

Dimensions default to 1024 × 576. `covers: false` disables automatic covers;
`--no-covers` disables them for a run. `run --no-pages` skips author pages.
Nameplates use system typefaces when installed and embedded Go Bold otherwise.
