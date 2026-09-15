# Inductor

Inductor turns audio and source metadata into a Hypnotica content library. It
transcribes recordings, measures audio, reviews transcript evidence, manages a
controlled tag vocabulary, and writes portable YAML entries and artwork.

The application is written in Go. A Python worker runs transcription and speaker
embeddings locally or over SSH. Existing `inductor.yaml` configuration, command
names and flags, `hypnotica/v1` documents, fingerprints, and stored enrichment
results remain usable.

## Build

Requires Go 1.24 or later on Linux. FFmpeg and ffprobe are needed for audio
inspection, conversion, and measurements. SSH and SCP are needed for a remote
worker. Python 3.10 or later is needed on the worker machine.

```sh
go build -o bin/inductor ./cmd/inductor
bin/inductor --help
```

No Python installation is needed to run metadata, tagging, cache, or document
maintenance commands. Worker scripts and model prompts are embedded in the Go
binary.

## Start a library

Copy `examples/library` into your library directory and edit `inductor.yaml` and
`sources/example.yaml`. Each source needs `audio`, `title`, and `author`. Supply
local audio paths; URL-only source records are reported but do not download audio.
The example tag registry is deliberately small: extend it to fit your library.

```sh
bin/inductor -r /path/to/library check
bin/inductor -r /path/to/library run --dry-run
bin/inductor -r /path/to/library ingest --stage media --stage emit --no-covers
```

For transcription, configure `transcribe.remote` with an SSH host, or leave it
empty to use a local CPU worker. The first worker start creates its virtualenv,
installs dependencies, and copies the embedded scripts. Models download on first
use. Set `OPENROUTER_API_KEY` for analysis and review. Configure `enrich.comfy_url`
and the appropriate ComfyUI models for artwork.

```sh
bin/inductor -r /path/to/library run --no-covers --no-pages
```

This processes missing artifacts and writes entries. `ingest` also supports
individual stages: `media`, `transcribe`, `analyse`, `review`, and `emit`. Use
`--redo` to include finished items; `--overwrite` permits replacing existing
summaries and spoilers. Creator descriptions, existing IDs, and extension fields
are preserved.

## Commands

| Area | Commands |
| --- | --- |
| Import and processing | `check`, `add`, `ingest`, `run`, `transcribe` |
| Tags and decisions | `retag`, `fold`, `tagmap`, `adjudicate`, `backfill`, `reconsider` |
| Metadata and artwork | `retitle`, `authors`, `cover-prompts`, `artwork`, `attribute` |
| Audio and speakers | `acoustic`, `acoustic-apply`, `voiceprint`, `similar` |
| Library maintenance | `paths`, `orphans`, `migrate`, `export`, `duplicates` |
| Model evaluation | `compare` |

Every command accepts `--help`. Maintenance commands retain their original
`--write` or `--dry-run` behavior; read their help before use. Output includes
progress messages and a JSON summary. Exit status is 0 on success, 1 on an
operation failure, and 2 for invalid arguments.

## Library layout

```text
inductor.yaml
sources/                         original metadata
content/tags.yaml                vocabulary and definitions
content/<creator>/_author.yaml
content/<creator>/<title>.yaml
content/<creator>/<title>.transcript.yaml
media/                           linked, copied, or converted audio
media/cover/                     artwork
state/transcripts/               reusable audio-keyed transcripts
state/enrichment/                reusable transcript-keyed analysis and review
state/decisions/                 durable tag decisions
.inductor/                       disposable indexes, measurements, queues, journals
```

Legacy caches in `content/.hypnotica` remain readable. Keep `state/` with the
library: it contains expensive results and editorial decisions.

See [configuration](docs/configuration.md), [contracts and architecture](docs/contracts.md),
and [development](CONTRIBUTING.md) for details.

## Testing

```sh
make test
make vet
make test-worker
```

Tests include thousands of comparisons against the frozen Python implementation,
numerical audio fixtures, filesystem workflows, HTTP protocol tests, and race
checks. Tests make no external API requests and do not download inference models.
GPU inference and live provider integrations require a configured environment;
the offline suite does not establish model quality or provider availability.

## License

Copyright © 2026 Naomi Persephone Amethyst <naomi@amethyst.name>.

Inductor is licensed under **GNU GPL version 3 only**. See [LICENSE](LICENSE),
[NOTICE](NOTICE), and [third-party notices](docs/third-party.md).
