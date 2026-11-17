#!/usr/bin/env bash
# Konect on Azure: a thin wrapper around `az deployment group create` with azure/azuredeploy.json (the same template the
# Deploy to Azure button uses, so both produce the same deployment). Dry run (what-if) unless --apply.
#
#   KONECT_INSTALL_TOKEN=kit_… scripts/azure.sh --resource-group konect-rg --location westeurope --version 0.7.0 [--apply]
#
# A redeploy (update, rollback) keeps every setting of the previous deployment in the resource group unless a flag
# changes it, refuses a different --name, and needs the install token again (a secure parameter is never stored).
# Needs az (signed in) and jq. Deployer rights: Contributor on the resource group, plus Owner, User Access
# Administrator or Role Based Access Control Administrator unless --no-role-assignments.

# shellcheck source=scripts/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
TEMPLATE="$ROOT/azure/azuredeploy.json"

usage() {
  cat >&2 <<EOF
Usage: scripts/azure.sh --resource-group <rg> --version <X.Y.Z> [options]
  --location <region>          for a new resource group (default westeurope)
  --name <prefix>              resource name prefix, 3-13 chars (default konect; fixed for the life of a deployment)
  --subscription <id|name>     passed to every az call (default: the CLI's current subscription)
  --sparkhub prod|preprod      which SparkHub (default \$KONECT_SPARKHUB or prod)
  --image <ref@sha256:…>       override the template's pinned bootstrap image
  --registry-id <acr id>       the image is in this private Azure Container Registry (resource ID): pulled with the
                               managed identity, which the template grants AcrPull (unless --no-role-assignments)
  --strict                     imageKind strict: --image is your strict image; no install token anywhere
  --custom-domain <host>       KONECT_BASE_URL host (bind the domain to the Container App afterwards)
  --no-purge-protection        Key Vault without purge protection (test deployments you will delete)
  --no-role-assignments        createRoleAssignments=false (the nextStep output names the one role to grant)
  --existing-key-id <id>       restore: the old deployment's keyId output
  --postgres-server <name>     restore: use this PostgreSQL server in the resource group
  --restore-from <server> --restore-time <ISO 8601>
                               restore: point-in-time restore <server> into the resource group, then deploy onto it
  --apply                      deploy (default: az deployment group what-if)
  --non-interactive            never prompt (KONECT_INSTALL_TOKEN must be exported)
EOF
  exit 2
}

RG="" LOCATION=westeurope NAME="" SUB="" VERSION="" RESTORE_FROM="" RESTORE_TIME=""
[ -z "${KONECT_SPARKHUB:-}" ] || pset sparkhub "$KONECT_SPARKHUB"
while [ $# -gt 0 ]; do
  case $1 in
    --resource-group) RG=$2; shift ;;
    --location) LOCATION=$2; shift ;;
    --name) NAME=$2; shift ;;
    --subscription) SUB=$2; shift ;;
    --version) VERSION=$2; shift ;;
    --sparkhub) pset sparkhub "$2"; shift ;;
    --image) pset image "$2"; shift ;;
    --registry-id) pset registryId "$2"; shift ;;
    --strict) pset imageKind strict ;;
    --custom-domain) pset customDomain "$2"; shift ;;
    --purge-protection) pset purgeProtection "$2"; shift ;;
    --no-purge-protection) pset purgeProtection false ;;
    --no-role-assignments) pset createRoleAssignments false ;;
    --existing-key-id) pset existingKeyId "$2"; shift ;;
    --postgres-server) pset postgresServerName "$2"; shift ;;
    --restore-from) RESTORE_FROM=$2; shift ;;
    --restore-time) RESTORE_TIME=$2; shift ;;
    --apply) APPLY=1 ;;
    --non-interactive) export NON_INTERACTIVE=1 ;;
    -h|--help) usage ;;
    *) die "unknown option $1 (--help)" ;;
  esac
  shift
done
[ -n "$RG" ] || usage
[ -n "$VERSION" ] || die "--version is required (the Konect version, X.Y.Z)"
check_version "$VERSION"
[ -z "$RESTORE_FROM" ] || [ -n "$RESTORE_TIME" ] || die "--restore-from needs --restore-time"
need az jq
azs() { if [ -n "$SUB" ]; then az "$@" --subscription "$SUB"; else az "$@"; fi; }

# The previous Konect deployment in the group (the CLI's or the button's): its parameters are the defaults.
PREV='{}'
if [ "$(azs group exists -n "$RG")" = true ]; then
  RG_EXISTS=1
  PREV=$(azs deployment group list -g "$RG" -o json \
    --query "sort_by([?properties.parameters.konectVersion && properties.provisioningState=='Succeeded'], &properties.timestamp)[-1].properties.parameters" 2>/dev/null || true)
  [ -n "$PREV" ] && [ "$PREV" != null ] || PREV='{}'
else
  RG_EXISTS=0
fi
prev() { jq -r --arg k "$1" '.[$k] | if . == null then empty else (.value | tostring) end' <<<"$PREV"; }
PREV_NAME=$(prev name)
if [ -n "$PREV_NAME" ]; then
  [ -z "$NAME" ] || [ "$NAME" = "$PREV_NAME" ] || die "resource group $RG holds the deployment '$PREV_NAME'; --name cannot change on a redeploy"
  NAME=$PREV_NAME
  say "redeploy of '$NAME' (was $(prev konectVersion)): settings not given as flags are kept"
  for k in sparkhub image imageKind registryId customDomain createRoleAssignments purgeProtection existingKeyId postgresServerName \
           postgresSku postgresTier postgresStorageGb cpu memory; do
    phas "$k" || [ -z "$(prev "$k")" ] || pset "$k" "$(prev "$k")"
  done
fi
NAME=${NAME:-konect}

# Install token: bootstrap only, from the environment or a hidden prompt.
if [ "$(pget imageKind || echo bootstrap)" = bootstrap ]; then
  secret KONECT_INSTALL_TOKEN "Install token (kit_…, from SparkHub → Konect → Install)"
  check_token
else
  phas image || die "--strict needs --image (your image built with strict/Dockerfile)"
  KONECT_INSTALL_TOKEN=""
fi

# Restore: a point-in-time copy of the old server in this group; the template then points Konect at it.
if [ -n "$RESTORE_FROM" ]; then
  RESTORED="$NAME-pg-r$(date -u +%Y%m%d%H%M)"
  pset postgresServerName "$RESTORED"
  say "restore: $RESTORE_FROM at $RESTORE_TIME → $RESTORED"
  [ "$RG_EXISTS" = 1 ] || run azs group create -n "$RG" -l "$LOCATION" -o none
  run azs postgres flexible-server restore -g "$RG" -n "$RESTORED" --source-server "$RESTORE_FROM" --restore-time "$RESTORE_TIME" -o none
fi

private_tmp
PARAMS="$TMPD/parameters.json"
(
  umask 077
  # shellcheck disable=SC2016  # a literal "$schema" key
  printf '{"$schema":"https://schema.management.azure.com/schemas/2019-04-01/deploymentParameters.json#","contentVersion":"1.0.0.0","parameters":{'
  printf '"name":{"value":%s},"konectVersion":{"value":%s}' "$(json_str "$NAME")" "$(json_str "$VERSION")"
  [ -z "$KONECT_INSTALL_TOKEN" ] || printf ',"installToken":{"value":%s}' "$(json_str "$KONECT_INSTALL_TOKEN")"
  i=0
  while [ "$i" -lt "${#PK[@]}" ]; do
    k=${PK[$i]} val=${PV[$i]}
    i=$((i + 1))
    case $k in
      createRoleAssignments|purgeProtection)
        case $val in true|false) v=$val ;; *) die "$k must be true or false" ;; esac ;;
      postgresStorageGb)
        case $val in ''|*[!0-9]*) die "$k must be a number" ;; *) v=$val ;; esac ;;
      *) v=$(json_str "$val") ;;
    esac
    printf ',"%s":{"value":%s}' "$k" "$v"
  done
  printf '}}\n'
) >"$PARAMS"

hr
say "Konect on Azure: resource group $RG${SUB:+ (subscription $SUB)}, name $NAME, version $VERSION, $([ "$APPLY" = 1 ] && echo APPLY || echo 'DRY RUN (what-if; pass --apply)')"
i=0
while [ "$i" -lt "${#PK[@]}" ]; do say "  ${PK[$i]} = ${PV[$i]}"; i=$((i + 1)); done
hr
if [ "$APPLY" = 0 ]; then
  if [ "$RG_EXISTS" = 0 ]; then
    say "resource group $RG does not exist: --apply creates it in $LOCATION (what-if needs an existing group)."
    exit 0
  fi
  [ -z "$RESTORE_FROM" ] || { say "restore: what-if skipped (the restored server does not exist yet)."; exit 0; }
  azs deployment group what-if -g "$RG" -n konect -f "$TEMPLATE" -p @"$PARAMS"
  exit 0
fi
[ "$RG_EXISTS" = 1 ] || azs group create -n "$RG" -l "$LOCATION" -o none
if [ "$(pget createRoleAssignments || true)" = false ] && phas registryId; then
  say "note: --no-role-assignments with --registry-id: until the identity $NAME-id has AcrPull on the registry, the deploy"
  say "      stops at the Container App ('unable to pull image using Managed identity'). A platform team then runs"
  say "        az role assignment create --assignee-object-id \$(az identity show -g $RG -n $NAME-id --query principalId -o tsv) \\"
  say "          --assignee-principal-type ServicePrincipal --role 7f951dda-4ed3-4680-a7ca-43fe172d538d --scope $(pget registryId)"
  say "      plus the key role from the nextStep output, and you run this command again."
fi
say "deploying (about 20 minutes on a first deploy, most of it the Container Apps environment)…"
OUT=$(azs deployment group create -g "$RG" -n konect -f "$TEMPLATE" -p @"$PARAMS" \
  --query 'properties.outputs.{url:url.value, keyId:keyId.value, postgresServer:postgresServer.value, nextStep:nextStep.value}' -o json)
printf '%s\n' "$OUT"
say "Expect at most one container restart in the first ten minutes (the new Entra administrator takes a few minutes)."

# A container that cannot start (a wrong or revoked install token exits 78) does not fail the deployment: ask Konect.
URL=$(jq -r '.url // empty' <<<"$OUT" 2>/dev/null || true)
if [ -n "$URL" ] && command -v curl >/dev/null 2>&1; then
  say "waiting for Konect at $URL/v1/health (up to ${HEALTH_WAIT_SECONDS:=480} s)…"
  up=""
  for _ in $(seq 1 $((HEALTH_WAIT_SECONDS / 15))); do
    if h=$(curl -fsS --max-time 10 "$URL/v1/health" 2>/dev/null) && [ -n "$h" ]; then up=$h; break; fi
    sleep 15
  done
  if [ -n "$up" ]; then
    say "Konect $(jq -r '.version' <<<"$up") answers: $(jq -r '.status + (if .locked then " (" + .locked.code + ")" else "" end)' <<<"$up")"
  else
    say "Konect does not answer yet. The deployment succeeded, but the container may be failing to start: a wrong or"
    say "revoked install token, or an unknown version, stops it (exit 78) and Azure restarts it in a loop. Look at:"
    say "  az containerapp logs show -g $RG -n $NAME${SUB:+ --subscription \"$SUB\"} --type console --tail 20"
    exit 1
  fi
fi
