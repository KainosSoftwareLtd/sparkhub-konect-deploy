#!/usr/bin/env bash
# Boot the bootstrap image and check what it answers without SparkHub (design #1702 §5.3):
#   - runs as 65532, read-only root, /data writable;
#   - `version` prints the version;
#   - `health` exits 1 while nothing is cached;
#   - the default command without a token exits 78 (configuration), naming the variable;
#   - the default command with a malformed token exits 78 and never echoes it.
#
#   scripts/image-smoke.sh <image> <version>
set -euo pipefail
image="${1:?image}"
version="${2:?version}"

user="$(docker image inspect --format '{{.Config.User}}' "$image")"
[ "$user" = "65532:65532" ] || { echo "image user is '$user', want 65532:65532"; exit 1; }

out="$(docker run --rm --read-only "$image" version)"
echo "$out"
grep -q "konect-bootstrap $version" <<<"$out" || { echo "version: unexpected output"; exit 1; }

set +e
docker run --rm --read-only -v konect-smoke:/data -e KONECT_VERSION=0.7.0 "$image" health
code=$?
[ "$code" = 1 ] || { echo "health without a cache exited $code, want 1"; exit 1; }

err="$(docker run --rm --read-only -v konect-smoke:/data -e KONECT_VERSION=0.7.0 "$image" 2>&1)"
code=$?
[ "$code" = 78 ] || { echo "no token: exit $code, want 78: $err"; exit 1; }
grep -q KONECT_INSTALL_TOKEN <<<"$err" || { echo "no token: message does not name KONECT_INSTALL_TOKEN: $err"; exit 1; }

err="$(docker run --rm --read-only -v konect-smoke:/data -e KONECT_VERSION=0.7.0 -e KONECT_INSTALL_TOKEN=kit_acme.not-a-real-token-SMOKESECRET "$image" 2>&1)"
code=$?
[ "$code" = 78 ] || { echo "bad token: exit $code, want 78: $err"; exit 1; }
if grep -q SMOKESECRET <<<"$err"; then echo "the token was echoed: $err"; exit 1; fi
set -e

docker volume rm konect-smoke >/dev/null
echo "image smoke: ok"
