# Contributing

Build with Go 1.24 or later. Run `make test vet build`. Install FFmpeg for media
integration tests. `make test-worker` runs Python worker tests; install `av` and
`numpy` to include actual decoder tests.

The reference suite needs `pytest`, `pyyaml`, `mutagen`, `numpy`, `av`, and
optionally `pillow`. Run `make test-reference` in an isolated Python environment.
`make parity` regenerates checked-in text and acoustic fixtures. Go tests consume
them without Python.

Keep changes focused and describe observable behavior. Test identity, data-loss,
concurrency, retries, and compatibility bugs. Use temporary libraries and local
HTTP servers: normal tests must not use personal libraries, credentials, billable
requests, or downloaded models.

Preserve unknown fields, curated metadata, immutable IDs, merged source keys,
and no-op modification times. Keep prompts as readable assets. Use contexts for
blocking work and atomic writes for persistent state.

The module is named `inductor` until its public hosting location is chosen.
Update `go.mod` and the executable import together when assigning that location.
Contributions use GPL-3.0-only; include SPDX headers in new source files.
Contact Naomi Persephone Amethyst at <naomi@amethyst.name>.

## Publishing builds

The workflow in `.github/workflows/test.yml` runs the offline test suite, native
Windows/macOS file-operation checks, and cross-compiles six release archives.
To produce one locally, run `bash scripts/build-release.sh linux amd64 dev`;
archives and their checksums go in `dist/`. The script requires Go, Bash, tar,
`sha256sum`, and zip for Windows archives.

Container publication uses the repository's `GITHUB_TOKEN` with `packages: write`;
tagged releases use `contents: write`. Publication is restricted to push events
in `NaomiAmethyst/inductor`. If anonymous pulls are desired, set the GHCR package
visibility to public after its first publication. No registry password is needed.
Both Docker targets embed the same version as the matching binary archives.
