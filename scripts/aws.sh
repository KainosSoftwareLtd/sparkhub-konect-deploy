#!/usr/bin/env bash
# Konect on AWS: a thin wrapper around `aws cloudformation deploy` with aws/konect.yaml. Dry run unless --apply: the
# change set is created, printed and left unexecuted.
#
#   KONECT_INSTALL_TOKEN=kit_… scripts/aws.sh --region eu-west-2 --version 0.7.0 --domain konect.example.com \
#     --certificate-arn arn:aws:acm:… --vpc vpc-… --alb-subnets subnet-a,subnet-b --db-subnets subnet-c,subnet-d [--apply]
#
# An update (new version, rollback) needs only --stack-name/--region and the flags that change: every other parameter,
# the install token included, keeps its current value. Needs aws (signed in) with rights to create the stack's
# resources, IAM roles included (CAPABILITY_IAM).

# shellcheck source=scripts/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
TEMPLATE="$ROOT/aws/konect.yaml"

usage() {
  cat >&2 <<EOF
Usage: scripts/aws.sh --region <r> --version <X.Y.Z> [options]
  First deploy needs: --domain <host> --certificate-arn <acm arn> --vpc <vpc-id> --alb-subnets <a,b> --db-subnets <c,d>
  --stack-name <n>               (default konect)
  --profile <p>                  AWS CLI profile
  --task-subnets <a,b>           task subnets (default: the ALB subnets, with a public IP)
  --no-public-ip                 AssignPublicIp=DISABLED (needs a NAT gateway)
  --hosted-zone <id>             create the Route 53 alias record for --domain
  --permissions-boundary <arn>   set on both IAM roles
  --create-ecs-service-linked-role   for an account that never ran ECS
  --sparkhub prod|preprod        which SparkHub (default \$KONECT_SPARKHUB or prod)
  --image <ref@sha256:…>         the container image (the bootstrap digest of this release, or your strict image)
  --strict                       ImageKind strict (no install token anywhere)
  --db-class <class> --cpu <units> --memory <MiB>
  --restore-snapshot <id> --existing-kms-key <arn>    restore (aws/README.md)
  --apply                        execute (default: create and print the change set only)
  --non-interactive              never prompt
EOF
  exit 2
}

STACK=konect REGION="" PROFILE=""
[ -z "${KONECT_SPARKHUB:-}" ] || pset SparkHub "$KONECT_SPARKHUB"
while [ $# -gt 0 ]; do
  case $1 in
    --region) REGION=$2; shift ;;
    --profile) PROFILE=$2; shift ;;
    --stack-name) STACK=$2; shift ;;
    --version) check_version "$2"; pset KonectVersion "$2"; shift ;;
    --domain) pset DomainName "$2"; shift ;;
    --certificate-arn) pset CertificateArn "$2"; shift ;;
    --vpc) pset VpcId "$2"; shift ;;
    --alb-subnets) pset AlbSubnetIds "$2"; shift ;;
    --task-subnets) pset TaskSubnetIds "$2"; shift ;;
    --db-subnets) pset DbSubnetIds "$2"; shift ;;
    --no-public-ip) pset AssignPublicIp DISABLED ;;
    --hosted-zone) pset HostedZoneId "$2"; shift ;;
    --permissions-boundary) pset PermissionsBoundaryArn "$2"; shift ;;
    --create-ecs-service-linked-role) pset CreateEcsServiceLinkedRole true ;;
    --sparkhub) pset SparkHub "$2"; shift ;;
    --image) pset Image "$2"; shift ;;
    --strict) pset ImageKind strict ;;
    --db-class) pset DbInstanceClass "$2"; shift ;;
    --cpu) pset TaskCpu "$2"; shift ;;
    --memory) pset TaskMemory "$2"; shift ;;
    --restore-snapshot) pset DbSnapshotIdentifier "$2"; shift ;;
    --existing-kms-key) pset ExistingKmsKeyArn "$2"; shift ;;
    --apply) APPLY=1 ;;
    --non-interactive) export NON_INTERACTIVE=1 ;;
    -h|--help) usage ;;
    *) die "unknown option $1 (--help)" ;;
  esac
  shift
done
[ -n "$REGION" ] || usage
need aws
awsr() { if [ -n "$PROFILE" ]; then aws --region "$REGION" --profile "$PROFILE" "$@"; else aws --region "$REGION" "$@"; fi; }

STATUS=$(awsr cloudformation describe-stacks --stack-name "$STACK" --query 'Stacks[0].StackStatus' --output text 2>/dev/null || true)
if [ -z "$STATUS" ] || [ "$STATUS" = REVIEW_IN_PROGRESS ]; then
  # A first deploy: the required parameters must all be given.
  for k in KonectVersion DomainName CertificateArn VpcId AlbSubnetIds DbSubnetIds Image; do
    [ -n "$(pget "$k")" ] || die "a first deploy needs $k (--help)"
  done
  NEW=1
else
  say "stack $STACK exists ($STATUS): parameters not given keep their current values"
  NEW=0
fi

# Install token: bootstrap only. On an update it is kept unless KONECT_INSTALL_TOKEN is exported.
if [ "$(pget ImageKind || echo bootstrap)" = bootstrap ] && { [ "$NEW" = 1 ] || [ -n "${KONECT_INSTALL_TOKEN:-}" ]; }; then
  secret KONECT_INSTALL_TOKEN "Install token (kit_…, from SparkHub → Konect → Install)"
  check_token
  pset InstallToken "$KONECT_INSTALL_TOKEN"
fi

private_tmp
PARAMS="$TMPD/parameters.json"
(
  umask 077
  sep='['
  i=0
  while [ "$i" -lt "${#PK[@]}" ]; do printf '%s%s' "$sep" "$(json_str "${PK[$i]}=${PV[$i]}")"; sep=','; i=$((i + 1)); done
  printf ']\n'
) >"$PARAMS"

hr
say "Konect on AWS: stack $STACK in $REGION${PROFILE:+ (profile $PROFILE)}, $([ "$APPLY" = 1 ] && echo APPLY || echo 'DRY RUN (change set only; pass --apply)')"
i=0
while [ "$i" -lt "${#PK[@]}" ]; do
  if [ "${PK[$i]}" = InstallToken ]; then say "  InstallToken = (hidden)"; else say "  ${PK[$i]} = ${PV[$i]}"; fi
  i=$((i + 1))
done
hr
args=(cloudformation deploy --stack-name "$STACK" --template-file "$TEMPLATE" --capabilities CAPABILITY_IAM
      --parameter-overrides "file://$PARAMS" --tags app=konect --no-fail-on-empty-changeset)
if [ "$APPLY" = 0 ]; then
  awsr "${args[@]}" --no-execute-changeset >/dev/null
  CS=$(awsr cloudformation list-change-sets --stack-name "$STACK" --query 'sort_by(Summaries, &CreationTime)[-1].ChangeSetId' --output text)
  awsr cloudformation describe-change-set --change-set-name "$CS" \
    --query 'Changes[].ResourceChange.{Action:Action,Resource:LogicalResourceId,Type:ResourceType,Replace:Replacement}' --output table
  say "change set $CS left unexecuted.$([ "$NEW" = 1 ] && printf ' The stack is in REVIEW_IN_PROGRESS; drop it with: aws cloudformation delete-stack --stack-name %s' "$STACK")"
  exit 0
fi
say "deploying (about 15 minutes on a first deploy, most of it RDS)…"
awsr "${args[@]}"
awsr cloudformation describe-stacks --stack-name "$STACK" --query 'Stacks[0].Outputs[].[OutputKey,OutputValue]' --output text
