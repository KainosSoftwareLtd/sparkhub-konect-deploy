#!/usr/bin/env bash
# Build azure/azuredeploy.json from azure/main.bicep (the Deploy to Azure button takes ARM JSON, not Bicep).
#   scripts/build-azure.sh           write azure/azuredeploy.json
#   scripts/build-azure.sh --check   fail when the committed azure/azuredeploy.json is not what main.bicep builds to
# Uses `bicep` when on the PATH, else `az bicep`. CI installs the Bicep version recorded in the committed JSON
# (metadata._generator.version), so the check compares like with like; rebuild with that version or bump it on purpose.
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
SRC="$ROOT/azure/main.bicep"
OUT="$ROOT/azure/azuredeploy.json"

build() {
  if command -v bicep >/dev/null 2>&1; then bicep build "$SRC" --outfile "$1"; else az bicep build --file "$SRC" --outfile "$1"; fi
}

if [ "${1:-}" = --check ]; then
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  build "$tmp/azuredeploy.json"
  if ! diff -u "$OUT" "$tmp/azuredeploy.json"; then
    printf 'azure/azuredeploy.json is stale: run scripts/build-azure.sh and commit the result\n' >&2
    exit 1
  fi
  printf 'azure/azuredeploy.json matches azure/main.bicep\n' >&2
else
  build "$OUT"
  printf 'wrote %s\n' "$OUT" >&2
fi
