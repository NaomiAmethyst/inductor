# Contracts and architecture

## Compatibility boundary

The Go executable preserves command names and options, configuration keys, YAML
schemas, media conventions, persistent cache keys, tag rulings, and model request
shapes. The Python package under `reference/python` is an offline test oracle,
not a runtime dependency or supported Python import API. Terminal output now
includes JSON summaries alongside progress messages.

- Documents remain `hypnotica/v1` `Item`, `Author`, and `Transcript` records.
  Existing IDs and filenames survive title edits and ordinary ingest reruns.
- New slugs use ASCII transliteration and hyphens. Collisions start at `-0`.
  Item IDs are author-prefixed.
- Audio fingerprints are BLAKE2b-128 over bytes excluding ID3v2 headers and
  ID3v1 trailers. Renaming or retagging does not invalidate transcription.
- Transcript keys collapse Python-compatible Unicode whitespace, casefold, and
  hash with BLAKE2b-128. Identical transcripts share analysis and review.
- Durable state uses `state/transcripts`, `state/enrichment`, and `state/decisions`.
  Existing `content/.hypnotica/transcripts` and `content/.hypnotica/analysis`
  directories are used when newer directories are absent. Legacy audio-keyed
  analysis is adopted when read.
- Unknown YAML fields and comments survive edits. Unchanged documents keep their
  bytes and modification times. Writes use temporary files and atomic replacement.
- `provenance.generated` records generated fields. Creator descriptions survive
  `--overwrite`. Proposed tags need a registry entry or decision before publication.
- Duplicate survivors retain IDs and absorbed source keys, preventing recreation
  on later ingest. Relative asset paths are rebased across directories.
- Migration stages original bytes and a manifest before installing destinations.
  On failure, the reported backup directory retains originals for recovery.

Acoustic fingerprints use 11,025 Hz mono PCM, 2,048-sample frames, 1,024-sample
hops, 33 bands, and little-endian uint32 storage. Go implements the FFT, pitch
estimation, measurements and matching. FFmpeg replaces PyAV at the local decode
boundary; decoder versions can affect damaged files and numerical rounding.

## Code map

| Files | Responsibility |
| --- | --- |
| `cmd/inductor/main.go` | Signal-aware process entry point |
| `cli.go`, `cli_schema.json`, `commands.go` | CLI contract and dispatch |
| `config.go`, `source.go`, `data.go` | Settings, validation, YAML and atomic files |
| `cache.go`, `naming.go` | Stable identities and persistent caches |
| `pipeline.go`, `ingest.go` | Scheduling and processing stages |
| `api.go`, `enrichment.go`, `prompts/` | HTTP, evidence and embedded prompts |
| `media.go`, `acoustic.go`, `voice.go` | Media, DSP and speaker comparison |
| `remote.go`, `workers/` | Persistent local/SSH queue and Python inference |
| `tags.go`, `tag_workflows.go` | Vocabulary, mapping, adjudication and ledger |
| `titles.go`, `authors.go`, `art.go` | Editorial fields and artwork |
| `duplicates.go`, `maintenance.go`, `import.go` | Maintenance and import/export |
| `compare.go`, `random.go` | Scoring and Python-compatible seeded samples |

One internal Go package shares document and storage contracts. Settings,
sentences, jobs, and scheduling records are typed; document maps preserve the
extension fields that are part of the public file contract.

## Scheduling

Media precedes transcripts, measurements, and voiceprints. Transcripts feed
analysis; analysis and measurements feed review; reviewed results feed entries
and covers. Separate limits govern disk, CPU, GPU submission, API, batch and art.

One goroutine owns scheduling state. Workers return outcomes through a channel.
Required failures block descendants; optional failures leave unrelated work
runnable. Cancellation reaches HTTP, subprocess and queue waits. Completed work
remains on disk.

## Validation scope

Offline tests compare thousands of string, transcript, title and numerical cases
against Python. Integration tests cover metadata preservation, cache adoption,
migration, media conversion, tag decisions, HTTP failures, batch recovery, queue
settlement, nameplates and graph execution. The race detector checks shared state.
Worker tests exercise real queue files and audio decoding without loading models.

Live GPU inference, OpenRouter availability and populated ComfyUI workflows need
verification in the deployment environment. File and request compatibility does
not imply identical stochastic model output or pixel-identical font rendering.
