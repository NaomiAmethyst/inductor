#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-only
set -euo pipefail

# Run from the repository root: bash scripts/build-release.sh linux amd64 VERSION
os=${1:?target OS required}
arch=${2:?target architecture required}
version=${3:?version required}
case "$os" in linux|windows|darwin) ;; *) echo "Unsupported OS: $os" >&2; exit 1 ;; esac
case "$arch" in amd64|arm64) ;; *) echo "Unsupported architecture: $arch" >&2; exit 1 ;; esac
if [[ ! "$version" =~ ^[a-zA-Z0-9][a-zA-Z0-9._+-]*$ ]]; then
    echo "Invalid version: $version" >&2
    exit 1
fi

platform=$os
[[ "$os" != darwin ]] || platform=macos
name="inductor_${version}_${platform}_${arch}"
out=${DIST_DIR:-dist}
mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/$name/docs"
exe=inductor
[[ "$os" != windows ]] || exe=inductor.exe
CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags="-s -w -X inductor/internal/inductor.Version=$version" \
    -o "$work/$name/$exe" ./cmd/inductor
cp LICENSE NOTICE README.md go.mod go.sum "$work/$name/"
cp docs/third-party.md "$work/$name/docs/"
cp -R docs/licenses "$work/$name/docs/"
if [[ "$os" == windows ]]; then
    archive="$name.zip"
    (cd "$work" && zip -qr "$out/$archive" "$name")
else
    archive="$name.tar.gz"
    tar -czf "$out/$archive" -C "$work" "$name"
fi
(cd "$out" && sha256sum "$archive" > "$archive.sha256")
