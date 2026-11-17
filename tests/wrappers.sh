#!/usr/bin/env bash
# shellcheck disable=SC2016,SC2054  # check() evals single-quoted conditions; subnet lists are comma-separated values
# Tests of scripts/azure.sh and scripts/aws.sh against stub `az` and `aws` CLIs (no cloud): the parameters each wrapper
# hands to the CLI, the redeploy rules, and that the install token never reaches a command line.
#   tests/wrappers.sh        (needs bash and jq)
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin"
export STUB="$WORK" PATH="$WORK/bin:$PATH"
unset KONECT_SPARKHUB KONECT_INSTALL_TOKEN
TOKEN=kit_test_0123456789abcdef
FAILS=0
ok() { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; FAILS=$((FAILS + 1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

# Stub az: logs argv, answers the lookups from files in $STUB, copies the parameters file it is handed.
cat >"$WORK/bin/az" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB/argv.log"
case "$1 $2" in
  "group exists") cat "$STUB/rg-exists" 2>/dev/null || echo false ;;
  "deployment group")
    case $3 in
      list) cat "$STUB/prev.json" 2>/dev/null || echo null ;;
      what-if|create)
        while [ $# -gt 0 ]; do [ "$1" = -p ] && { f=${2#@}; cp "$f" "$STUB/params.json"; ls -l "$f" | cut -c1-10 >"$STUB/params.mode"; }; shift; done
        ;;
    esac ;;
esac
EOF
# Stub aws: logs argv, a stack exists when $STUB/stack-status exists, copies the parameter overrides file.
cat >"$WORK/bin/aws" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB/argv.log"
for a in "$@"; do case $a in file://*) f=${a#file://}; cp "$f" "$STUB/params.json"; [ -f "$STUB/first-params.json" ] || cp "$f" "$STUB/first-params.json"; ls -l "$f" | cut -c1-10 >"$STUB/params.mode" ;; esac; done
case " $* " in
  *" describe-stacks "*) if [ -f "$STUB/stack-status" ]; then cat "$STUB/stack-status"; else echo "Stack does not exist" >&2; exit 254; fi ;;
  *" list-change-sets "*) echo arn:aws:cloudformation:eu-west-2:111111111111:changeSet/x/1 ;;
esac
EOF
chmod +x "$WORK/bin/az" "$WORK/bin/aws"
reset() { rm -f "$WORK"/first-params.json "$WORK"/argv.log "$WORK"/params.json "$WORK"/params.mode "$WORK"/prev.json "$WORK"/rg-exists "$WORK"/stack-status; }
param() { jq -r --arg k "$1" '.parameters[$k] | if . == null then "<none>" else (.value | tostring) end' "$WORK/params.json"; }

# ── azure.sh ────────────────────────────────────────────────────────────────
reset; echo true >"$WORK/rg-exists"
KONECT_INSTALL_TOKEN=$TOKEN "$BASH" "$ROOT/scripts/azure.sh" --resource-group rg1 --version 0.7.0 --sparkhub preprod \
  --no-purge-protection --subscription "Sub One" --non-interactive 2>"$WORK/err" || true
check "azure: first deploy runs what-if" 'grep -q "deployment group what-if -g rg1 -n konect" "$WORK/argv.log"'
check "azure: every az call names the subscription" '! grep -v -- "--subscription Sub One" "$WORK/argv.log" | grep -q .'
check "azure: parameters" '[ "$(param name)/$(param konectVersion)/$(param sparkhub)/$(param purgeProtection)/$(param installToken)" = "konect/0.7.0/preprod/false/$TOKEN" ]'
check "azure: parameters file is 0600" 'grep -q "^-rw-------" "$WORK/params.mode"'
check "azure: token never on argv" '! grep -q kit_ "$WORK/argv.log"'
check "azure: token never printed" '! grep -q kit_ "$WORK/err"'

reset; echo true >"$WORK/rg-exists"
echo '{"name":{"value":"konect2"},"konectVersion":{"value":"0.7.0"},"sparkhub":{"value":"preprod"},"purgeProtection":{"value":false},"createRoleAssignments":{"value":false}}' >"$WORK/prev.json"
KONECT_INSTALL_TOKEN=$TOKEN "$BASH" "$ROOT/scripts/azure.sh" --resource-group rg1 --version 0.7.1 --non-interactive 2>/dev/null || true
check "azure: redeploy keeps the previous settings" '[ "$(param name)/$(param konectVersion)/$(param sparkhub)/$(param purgeProtection)/$(param createRoleAssignments)" = "konect2/0.7.1/preprod/false/false" ]'
if KONECT_INSTALL_TOKEN=$TOKEN "$BASH" "$ROOT/scripts/azure.sh" --resource-group rg1 --name other --version 0.7.1 --non-interactive 2>/dev/null; then
  bad "azure: refuses a changed --name"; else ok "azure: refuses a changed --name"; fi

reset
check "azure: refuses a token without kit_" '! KONECT_INSTALL_TOKEN=abc "$BASH" "$ROOT/scripts/azure.sh" --resource-group rg1 --version 0.7.0 --non-interactive 2>/dev/null'
check "azure: refuses v0.7.0" '! KONECT_INSTALL_TOKEN=$TOKEN "$BASH" "$ROOT/scripts/azure.sh" --resource-group rg1 --version v0.7.0 --non-interactive 2>/dev/null'
check "azure: needs the token without a prompt" '! env -u KONECT_INSTALL_TOKEN "$BASH" "$ROOT/scripts/azure.sh" --resource-group rg1 --version 0.7.0 --non-interactive 2>/dev/null'
reset; echo true >"$WORK/rg-exists"
env -u KONECT_INSTALL_TOKEN "$BASH" "$ROOT/scripts/azure.sh" --resource-group rg1 --version 0.7.0 --strict --image reg.example/konect@sha256:ab --non-interactive 2>/dev/null || true
check "azure: strict sends no token" '[ "$(param imageKind)/$(param installToken)" = "strict/<none>" ]'
reset; echo true >"$WORK/rg-exists"
ACR=/subscriptions/s1/resourceGroups/rg1/providers/Microsoft.ContainerRegistry/registries/acr1
KONECT_INSTALL_TOKEN=$TOKEN "$BASH" "$ROOT/scripts/azure.sh" --resource-group rg1 --version 0.7.0 --image acr1.azurecr.io/b@sha256:ab \
  --registry-id "$ACR" --non-interactive 2>/dev/null || true
check "azure: --registry-id" '[ "$(param registryId)" = "$ACR" ]'
reset; echo true >"$WORK/rg-exists"
printf '{"name":{"value":"konect"},"konectVersion":{"value":"0.7.0"},"image":{"value":"acr1.azurecr.io/b@sha256:ab"},"registryId":{"value":"%s"}}' "$ACR" >"$WORK/prev.json"
KONECT_INSTALL_TOKEN=$TOKEN "$BASH" "$ROOT/scripts/azure.sh" --resource-group rg1 --version 0.7.1 --non-interactive 2>/dev/null || true
check "azure: redeploy keeps the registry" '[ "$(param image)/$(param registryId)" = "acr1.azurecr.io/b@sha256:ab/$ACR" ]'

# ── aws.sh ──────────────────────────────────────────────────────────────────
FIRST=(--region eu-west-2 --version 0.7.0 --domain konect.example.com --certificate-arn arn:aws:acm:eu-west-2:111111111111:certificate/abc
       --vpc vpc-1 --alb-subnets subnet-a,subnet-b --db-subnets subnet-c,subnet-d --image ghcr.io/x/y@sha256:ab --profile p1)
reset
KONECT_INSTALL_TOKEN=$TOKEN "$BASH" "$ROOT/scripts/aws.sh" "${FIRST[@]}" --non-interactive 2>"$WORK/err" >/dev/null || true
check "aws: dry run leaves the change set unexecuted" 'grep -q -- "--no-execute-changeset" "$WORK/argv.log"'
check "aws: CAPABILITY_IAM" 'grep -q -- "--capabilities CAPABILITY_IAM" "$WORK/argv.log"'
check "aws: parameters" 'jq -e --arg t "InstallToken=$TOKEN" "index(\"AlbSubnetIds=subnet-a,subnet-b\") and index(\"KonectVersion=0.7.0\") and index(\$t)" "$WORK/params.json" >/dev/null'
check "aws: parameters file is 0600" 'grep -q "^-rw-------" "$WORK/params.mode"'
check "aws: token never on argv" '! grep -q kit_ "$WORK/argv.log"'
check "aws: token never printed" '! grep -q kit_ "$WORK/err"'
check "aws: every call names the region and profile" '! grep -v -- "--region eu-west-2 --profile p1" "$WORK/argv.log" | grep -q .'

reset
KONECT_INSTALL_TOKEN=$TOKEN "$BASH" "$ROOT/scripts/aws.sh" "${FIRST[@]}" --non-interactive --apply 2>/dev/null >/dev/null || true
check "aws: a first deploy creates without deletion protection" 'grep -q "DbDeletionProtection=false" "$STUB/first-params.json"'
check "aws: then turns deletion protection on" '[ "$(grep -c "cloudformation deploy" "$WORK/argv.log")" = 2 ] && grep -q "parameter-overrides DbDeletionProtection=true" "$WORK/argv.log"'

reset
check "aws: a first deploy needs the network flags" '! KONECT_INSTALL_TOKEN=$TOKEN "$BASH" "$ROOT/scripts/aws.sh" --region eu-west-2 --version 0.7.0 --non-interactive 2>/dev/null >/dev/null'
reset; echo UPDATE_COMPLETE >"$WORK/stack-status"
env -u KONECT_INSTALL_TOKEN "$BASH" "$ROOT/scripts/aws.sh" --region eu-west-2 --version 0.7.1 --non-interactive 2>/dev/null >/dev/null || true
check "aws: update sends only what changes (token kept)" '[ "$(jq -c . "$WORK/params.json")" = "[\"KonectVersion=0.7.1\"]" ]'

[ "$FAILS" = 0 ] || { echo "--- last params:"; cat "$WORK/params.json" 2>/dev/null | sed "s/kit_[A-Za-z0-9_]*/kit_*/g"; }
printf '\n%s\n' "$([ "$FAILS" = 0 ] && echo "all passed" || echo "$FAILS failed")"
[ "$FAILS" = 0 ]
