#!/bin/sh
# Konect laptop installer (macOS, Linux). Design #1702 §5.5.
#
#   curl -fsSL https://sparkhub.studio/konect/install.sh | sh -s -- --org <org> --version 0.7.0
#
# What it does:
#   1. downloads the Konect bootstrap for this computer from the public GitHub Release of
#      KainosSoftwareLtd/sparkhub-konect-deploy and checks its SHA-256 against the value SparkHub filled in below;
#   2. reads the install token from KONECT_INSTALL_TOKEN or asks for it (never from the command line);
#   3. runs `konect-bootstrap fetch`, which gets the signed Konect release from SparkHub and verifies the signature
#      and every hash before anything is written to --dir (default ~/.konect/bin);
#   4. clears the macOS quarantine flag and deletes the bootstrap.
#
# SparkHub serves this file with the block between the markers filled in from its bootstrap catalog (the copy in
# the repository has it empty and refuses to run). Nothing else in the file changes.

set -eu

# >>> sparkhub-filled (SparkHub replaces this block when it serves the file)
SPARKHUB_ENV=""
BOOTSTRAP_VERSION=""
BOOTSTRAP_URL_LINUX_X64=""
BOOTSTRAP_SHA256_LINUX_X64=""
BOOTSTRAP_URL_LINUX_ARM64=""
BOOTSTRAP_SHA256_LINUX_ARM64=""
BOOTSTRAP_URL_MACOS_ARM64=""
BOOTSTRAP_SHA256_MACOS_ARM64=""
BOOTSTRAP_URL_MACOS_X64=""
BOOTSTRAP_SHA256_MACOS_X64=""
# <<< sparkhub-filled

say() { printf '%s\n' "$*" >&2; }
die() { say "install.sh: $*"; exit 1; }

usage() {
  say "usage: install.sh --org <org> --version <X.Y.Z> [--dir <dir>]"
  say "  The install token comes from KONECT_INSTALL_TOKEN or a prompt (create one in SparkHub → Konect → Install)."
}

org=""
version=""
dir="${HOME:?HOME is not set}/.konect/bin"
while [ $# -gt 0 ]; do
  case "$1" in
    --org) [ $# -ge 2 ] || die "--org needs a value"; org="$2"; shift 2 ;;
    --org=*) org="${1#--org=}"; shift ;;
    --version) [ $# -ge 2 ] || die "--version needs a value"; version="$2"; shift 2 ;;
    --version=*) version="${1#--version=}"; shift ;;
    --dir) [ $# -ge 2 ] || die "--dir needs a value"; dir="$2"; shift 2 ;;
    --dir=*) dir="${1#--dir=}"; shift ;;
    --token|--token=*) die "the token is never passed on the command line: set KONECT_INSTALL_TOKEN or answer the prompt" ;;
    -h|--help) usage; exit 0 ;;
    *) usage; die "unknown argument: $1" ;;
  esac
done

[ -n "$SPARKHUB_ENV" ] && [ -n "$BOOTSTRAP_VERSION" ] ||
  die "this copy was not served by SparkHub; use: curl -fsSL https://sparkhub.studio/konect/install.sh | sh -s -- --org <org> --version <X.Y.Z>"
[ -n "$org" ] || { usage; die "--org is required"; }
[ -n "$version" ] || { usage; die "--version is required (an exact version, see SparkHub → Konect → Install)"; }
printf '%s' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || die "--version must be an exact version such as 0.7.0"
printf '%s' "$org" | grep -Eq '^[a-z0-9]([a-z0-9-]*[a-z0-9])?$' || die "--org must be the organization code (lower case)"

os="$(uname -s)"
arch="$(uname -m)"
case "$os/$arch" in
  Linux/x86_64|Linux/amd64) url="$BOOTSTRAP_URL_LINUX_X64"; sum="$BOOTSTRAP_SHA256_LINUX_X64" ;;
  Linux/aarch64|Linux/arm64) url="$BOOTSTRAP_URL_LINUX_ARM64"; sum="$BOOTSTRAP_SHA256_LINUX_ARM64" ;;
  Darwin/arm64) url="$BOOTSTRAP_URL_MACOS_ARM64"; sum="$BOOTSTRAP_SHA256_MACOS_ARM64" ;;
  Darwin/x86_64) url="$BOOTSTRAP_URL_MACOS_X64"; sum="$BOOTSTRAP_SHA256_MACOS_X64" ;;
  *) die "no Konect build for $os/$arch (Windows: download Konect from SparkHub → Konect → Install)" ;;
esac
[ -n "$url" ] && [ -n "$sum" ] || die "SparkHub has no bootstrap for $os/$arch"

command -v curl >/dev/null 2>&1 || die "curl is required"
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "sha256sum or shasum is required"
fi

# The token: from the environment, else a prompt on the terminal (not echoed). Never from argv.
token="${KONECT_INSTALL_TOKEN:-}"
if [ -z "$token" ]; then
  [ -r /dev/tty ] || die "set KONECT_INSTALL_TOKEN (no terminal to ask on)"
  printf 'Install token for %s (kit_…, from SparkHub → Konect → Install): ' "$org" >/dev/tty
  stty -echo </dev/tty 2>/dev/null || true
  IFS= read -r token </dev/tty || true
  stty echo </dev/tty 2>/dev/null || true
  printf '\n' >/dev/tty
fi
[ -n "$token" ] || die "no install token"
case "$token" in
  "kit_$org."*) ;;
  kit_*) die "this install token belongs to another organization than $org" ;;
  *) die "that is not an install token (it starts with kit_)" ;;
esac

umask 077
tmp="$(mktemp -d 2>/dev/null || mktemp -d -t konect-install)"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading the Konect bootstrap $BOOTSTRAP_VERSION for $os/$arch…"
curl -fsSL --proto '=https' --tlsv1.2 -o "$tmp/konect-bootstrap" "$url" || die "download failed: $url"
got="$(sha256 "$tmp/konect-bootstrap")"
[ "$got" = "$sum" ] || die "the bootstrap's SHA-256 is $got, SparkHub expects $sum; nothing was installed"
chmod 0700 "$tmp/konect-bootstrap"

mkdir -p "$dir"
say "Getting Konect $version from SparkHub and checking its signature…"
KONECT_INSTALL_TOKEN="$token" KONECT_VERSION="$version" KONECT_SPARKHUB="$SPARKHUB_ENV" \
  "$tmp/konect-bootstrap" fetch --out "$dir" --client install-script ||
  die "the bootstrap could not install Konect (see the message above)"
token=""

if [ "$os" = "Darwin" ]; then
  xattr -d com.apple.quarantine "$dir/konect" 2>/dev/null || true
fi

say ""
say "Konect $version is installed: $dir/konect (verified; manifest.json and manifest.sig.json beside it)."
case ":$PATH:" in
  *":$dir:"*) say "Run: konect --help" ;;
  *) say "Add it to your PATH, e.g.:  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile   then run: konect --help" ;;
esac
