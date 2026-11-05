#!/bin/sh
# Build the four release archives and their checksum manifest, without publishing.
# Run from the repository root with TAG=vX.Y.Z; an optional directory replaces dist.
set -eu
: "${TAG:?set TAG to the release version}"
out=${1:-dist}
export CGO_ENABLED=0
mkdir -p "$out/package"
cp LICENSE "$out/package/"
for GOOS in linux darwin; do
    for GOARCH in amd64 arm64; do
        export GOOS GOARCH
        ./x build -trimpath -ldflags "-s -w -X main.version=${TAG}" -o "$out/package/tap" ./cmd/tap
        # BSD tar must not add macOS metadata files to the release layout.
        COPYFILE_DISABLE=1 tar -czf "$out/tap_${TAG}_${GOOS}_${GOARCH}.tar.gz" -C "$out/package" tap LICENSE
    done
done
(
    cd "$out"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum -- *.tar.gz > checksums.txt
    else
        shasum -a 256 -- *.tar.gz > checksums.txt
    fi
)
