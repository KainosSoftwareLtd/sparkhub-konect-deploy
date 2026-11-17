#!/bin/sh
# Build the bootstrap release matrix reproducibly (design #1702 §5.1): four static binaries and SHA256SUMS.
#
#   scripts/build.sh <out dir> <version>          e.g. scripts/build.sh dist 1.0.0
#
# Pinned toolchain (GOTOOLCHAIN=local: the Go that runs this is the one CI pins), -trimpath, -buildvcs=false,
# -ldflags "-s -w -buildid=", no cgo. Go embeds no timestamps; SOURCE_DATE_EPOCH is honoured by anything else that
# reads it. Anyone with the same Go version can rebuild a release from its tag and compare the hashes.
set -eu
out="${1:?usage: scripts/build.sh <out dir> <version>}"
version="${2:?usage: scripts/build.sh <out dir> <version>}"
mkdir -p "$out"
for target in linux/amd64:linux-x64 linux/arm64:linux-arm64 darwin/arm64:macos-arm64 darwin/amd64:macos-x64; do
  goosarch="${target%%:*}"
  name="${target##*:}"
  CGO_ENABLED=0 GOOS="${goosarch%/*}" GOARCH="${goosarch#*/}" GOFLAGS=-mod=readonly GOTOOLCHAIN=local \
    go build -trimpath -buildvcs=false -ldflags "-s -w -buildid= -X main.version=$version" \
      -o "$out/konect-bootstrap-$name" ./cmd/konect-bootstrap
done
(cd "$out" && sha256sum konect-bootstrap-linux-x64 konect-bootstrap-linux-arm64 konect-bootstrap-macos-arm64 konect-bootstrap-macos-x64 > SHA256SUMS)
cat "$out/SHA256SUMS"
