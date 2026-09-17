# Inductor

Inductor turns audio and source metadata into a Hypnotica content library. It
transcribes recordings, measures audio, reviews transcript evidence, manages a
controlled tag vocabulary, and writes portable YAML entries and artwork.

The application is written in Go. A Python worker runs transcription and speaker
embeddings locally or over SSH. Existing `inductor.yaml` configuration, command
names and flags, `hypnotica/v1` documents, fingerprints, and stored enrichment
results remain usable.

## Build

Requires Go 1.24 or later. CI builds Linux, Windows, and macOS binaries for
AMD64 and ARM64. FFmpeg and ffprobe are needed for audio
inspection, conversion, and measurements. SSH and SCP are needed for a remote
worker. Python 3.10 or later is needed on the worker machine.

```sh
go build -o bin/inductor ./cmd/inductor
bin/inductor --help
```

No Python installation is needed to run metadata, tagging, cache, or document
maintenance commands. Worker scripts and model prompts are embedded in the Go
binary.

## Downloads and containers

Every successful push or pull request build provides six binary archives under
[GitHub Actions](https://github.com/NaomiAmethyst/inductor/actions), retained for
30 days. Each archive includes license notices and comes with a SHA-256 checksum file.
Windows downloads are ZIP files; Linux and macOS downloads are `.tar.gz` files.
Pushing a `v*` tag also publishes the archives and `checksums.txt` on
[GitHub Releases](https://github.com/NaomiAmethyst/inductor/releases).

Images are published to `ghcr.io/naomiamethyst/inductor` on successful pushes.
Both variants support Linux AMD64 and ARM64:

| Tag on the default branch | Contents |
| --- | --- |
| `latest`, `ffmpeg` | Inductor, FFmpeg/ffprobe, CA certificates, SSH/SCP |
| `scratch` | Inductor and CA certificates, built from `scratch` |

Each variant also gets `<branch>-ffmpeg` / `<branch>-scratch`,
`sha-<full-commit>-ffmpeg` / `sha-<full-commit>-scratch`, and, for tag pushes,
`<tag>-ffmpeg` / `<tag>-scratch` (for example, `v1.2.3-scratch`).
Pull requests and manual workflow runs build images without publishing them.

```sh
docker run --rm -v "$PWD:/library" ghcr.io/naomiamethyst/inductor:ffmpeg check
docker run --rm -v "$PWD:/library" ghcr.io/naomiamethyst/inductor:scratch --help
```

The scratch image supports commands that need only the Go binary, including
metadata maintenance and HTTPS API calls. Media processing needs the FFmpeg
variant. Neither image includes Python or inference models: use a remote worker
with the FFmpeg variant for transcription and voice embeddings. Native Windows
builds also need a remote Unix worker; the local worker uses a POSIX shell and
Unix file locking. Standalone binaries need FFmpeg/ffprobe installed separately
for media operations.

To build containers locally, the default target includes FFmpeg:

```sh
docker build --target runtime-ffmpeg -t inductor:ffmpeg .
docker build --target runtime-scratch -t inductor:scratch .
```

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
`ingest --redo` to include finished items; `run` checks missing artifacts even on
finished entries, and `run --redo ARTIFACT` forces the named artifact to be rebuilt.
`--overwrite` permits replacing existing
summaries and spoilers. Creator descriptions, existing IDs, and extension fields
are preserved.

`run` also checks sources and mapping targets, fills missing author tag mappings
before processing, then generates and applies adjudication rulings with writes
enabled before generating author pages. Adjudication includes stored
reviewer-approved proposals, and backfill applies approved, renamed, or merged
tags to recordings whose reviews requested them. Rejected and unreviewed
proposals are not backfilled by `run`.

After processing, `run` audits transcript coverage, writes acoustic measurements
and missing durations onto entries, repairs cover prompts and missing, invalid,
or stale generated covers, and updates similar-voice relationships on author
pages. It reports voiceprint verification, guest-speaker appearances (cameos),
duplicates, and orphans. Transcript auditing reports recordings longer than two
minutes with less than 90% coverage; it does not automatically retranscribe them.
Manual covers are preserved. Duplicates and orphans are reported
without merging or deleting anything. These steps still run when no recordings
need processing.

Each additional step can be omitted:

| Flag | Effect |
| --- | --- |
| `--no-check` | Skip the preflight report; bypassing source validation still requires `--force`. |
| `--no-tagmaps` | Skip filling missing mappings and reconciling their item tags. |
| `--no-adjudicate` | Skip generating and applying rulings. |
| `--no-adjudicate-apply` | Generate and save rulings, but do not apply them. |
| `--no-adjudicate-write` | Generate and save rulings, and preview application without writing registry, mapping, or item changes. |
| `--no-review-proposals` | Omit stored review proposals from adjudication; item and tagmap proposals remain included. |
| `--no-backfill` | Skip applying registered/adjudicated tags requested by reviews. |
| `--no-acoustic-apply` | Skip writing acoustic metadata and missing durations. |
| `--no-transcribe-audit` | Skip the transcript coverage report. |
| `--no-artwork-repair` | Skip library-wide cover prompt translation and cover repairs. |
| `--no-cover-prompts` | Skip prompt translation while retaining cover repair from existing prompts. |
| `--no-similar` | Skip writing similar-voice relationships on author pages. |
| `--no-cameos` | Skip guest-speaker detection independently of voiceprint verification. |
| `--no-voiceprint-verify` | Skip the final attribution report; voiceprint generation remains part of the recording graph. |
| `--no-duplicates` | Skip the duplicate report. |
| `--no-orphans` | Skip the orphan report. |

Unapplied rulings are retained in the cache's `rulings.yaml` for a later run.
`--dry-run` reports existing checks, missing mappings, artifact work, and pending
adjudication without API calls or applying changes. It previews backfill, acoustic
application, similar voices, and artwork repairs too. The adjudication apply/write
opt-outs also prevent backfill writes. Existing `--no-pages` controls author page
generation; `--no-covers` also skips cover prompt translation and artwork repairs.
The configured `enrich.covers: false` disables these artwork repairs as well.

Tagmaps honor `--author`; with `--only` or `--limit`, they cover the selected
recordings' authors. Unrestricted runs also fill mappings for finished authors.
Adjudication, backfill, acoustic application, transcript auditing, artwork repair,
similar voices, and the final reports cover the whole library. The recording
graph honors selection flags, and `--limit` applies after checking for outstanding
artifacts so complete entries do not prevent later missing work from being found.

During a run, a progress line prints every minute with elapsed time, active work,
and artifact counts (cached, completed, running, pending, failed, blocked, and
skipped). This continues through checks, tagmaps, adjudication, and author pages.
Use `--no-progress` to disable these periodic lines; ordinary status messages,
errors, and the final report remain visible. Dry runs omit the periodic lines.
Use `--verbose` to log each dispatch and completion, including recording artifacts,
review batches, tagmap requests, and author work. Failures and skipped results are
identified explicitly. Both flags can be combined:

```sh
bin/inductor -r /path/to/library run --verbose --no-progress
```

## Commands

| Area | Commands |
| --- | --- |
| Import and processing | `check`, `add`, `ingest`, `run`, `transcribe` |
| Tags and decisions | `retag`, `fold`, `tagmap`, `adjudicate`, `registry`, `backfill`, `reconsider` |
| Metadata and artwork | `retitle`, `authors`, `cover-prompts`, `artwork`, `attribute` |
| Audio and speakers | `acoustic`, `acoustic-apply`, `voiceprint`, `similar` |
| Library maintenance | `paths`, `orphans`, `migrate`, `export`, `duplicates` |
| Model evaluation | `compare` |

`registry` is the one that edits the vocabulary directly, for the decisions a
model should not be making: `add`, `describe`, `remove`, `rename`, `merge`, and
`bulk` for a file of those applied in order. Each keeps the rest of the library
in step — the items carrying the tag, the `tags_added` a run recorded, the
proposals still waiting on it, and every creator map that points at it — and
records a ruling saying what was decided, so a later `adjudicate` does not
re-open it.

```sh
inductor registry add "Humour" --description "Comedy is part of the intent." --write
inductor registry merge "Toy" "Toys" --write
inductor registry bulk changes.yaml --write
```

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
