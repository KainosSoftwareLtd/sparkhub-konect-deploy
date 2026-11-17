# Shared helpers of scripts/azure.sh and scripts/aws.sh.
#
#   - DRY RUN BY DEFAULT: without --apply the wrappers only preview (Azure what-if, an AWS change set left
#     unexecuted). Commands that change something go through `run`, which prints them in a dry run.
#   - The install token comes from KONECT_INSTALL_TOKEN or a hidden prompt, never from argv. It reaches the cloud CLI
#     through a 0600 parameters file in a private temp directory that is removed on exit.
#
# shellcheck shell=bash
# shellcheck disable=SC2034  # APPLY / NON_INTERACTIVE are set by the callers

set -euo pipefail
umask 077   # parameter files hold the install token

APPLY=0
NON_INTERACTIVE=${NON_INTERACTIVE:-0}
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

say() { printf '%s\n' "$*" >&2; }
die() { say "error: $*"; exit 2; }
hr() { say "────────────────────────────────────────────────────────────────"; }
need() { local c; for c in "$@"; do command -v "$c" >/dev/null 2>&1 || die "$c is not installed (needed by this script)"; done; }

# A command that CHANGES something: printed in a dry run, run with --apply.
run() {
  if [ "$APPLY" = 1 ]; then "$@"; else say "  (dry run) ${*/#azs/az}"; fi
}

# secret VAR "prompt": $VAR, else a hidden prompt. Never printed.
secret() {
  local var=$1 prompt=$2 v
  [ -z "${!var:-}" ] || return 0
  [ "$NON_INTERACTIVE" = 0 ] || die "$var is not set (export it, or run without --non-interactive)"
  printf '%s: ' "$prompt" >&2
  IFS= read -rs v
  printf '\n' >&2
  [ -n "$v" ] || die "$var cannot be empty"
  printf -v "$var" '%s' "$v"
}

check_token() {
  case "${KONECT_INSTALL_TOKEN:-}" in
    kit_*) ;;
    *) die "KONECT_INSTALL_TOKEN must start with kit_ (SparkHub → Konect → Install → Create token)" ;;
  esac
}

check_version() {
  printf '%s' "$1" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$' || die "--version must be X.Y.Z or X.Y.Z-rc.N (got $1)"
}

# A JSON string literal.
json_str() {
  local s=$1
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  printf '"%s"' "$s"
}

# A private temp directory for parameter files (0700, removed on exit).
private_tmp() {
  TMPD=$(mktemp -d)
  chmod 700 "$TMPD"
  trap 'rm -rf "$TMPD"' EXIT
}

# Template parameters as two parallel arrays (no associative arrays: macOS ships bash 3.2).
PK=()
PV=()
pset() {
  local i=0
  while [ "$i" -lt "${#PK[@]}" ]; do
    if [ "${PK[$i]}" = "$1" ]; then PV[i]=$2; return 0; fi
    i=$((i + 1))
  done
  PK[${#PK[@]}]=$1
  PV[${#PV[@]}]=$2
}
pget() {
  local i=0
  while [ "$i" -lt "${#PK[@]}" ]; do
    if [ "${PK[$i]}" = "$1" ]; then printf '%s' "${PV[$i]}"; return 0; fi
    i=$((i + 1))
  done
  return 1
}
phas() { pget "$1" >/dev/null; }
