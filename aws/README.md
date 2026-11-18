# Konect on AWS

`konect.yaml` is a CloudFormation template; `scripts/aws.sh` deploys it with `aws cloudformation deploy`. There is no
console button yet: the form would open on the same VPC, subnet, domain and certificate fields you look up anyway.

## What you need

- An **install token** (`kit_…`): SparkHub → Konect → **Install** → Create token. It only downloads Konect.
- The **Konect version** to run, for example `0.7.0`.
- A VPC with two public subnets in different Availability Zones (load balancer) and two subnets for the database
  (private recommended).
- A **domain name** and an **ACM certificate** for it in the same region. An ALB has no TLS host of its own.
  Optionally the Route 53 hosted zone, so the stack creates the DNS record.
- Rights to create the resources below, IAM roles included (`CAPABILITY_IAM`). If your organisation requires a
  permissions boundary on new roles, pass it with `--permissions-boundary`.
- About 15 minutes, most of it RDS.

## What it creates

| Resource | Notes |
|---|---|
| ECS cluster, task definition, service | Fargate ARM64, one task, read-only root file system, deployment circuit breaker with rollback |
| Application Load Balancer | HTTPS 443 (`ELBSecurityPolicy-TLS13-1-2-2021-06`), HTTP 80 redirects; health check `GET /v1/health` |
| RDS PostgreSQL 16 | `db.t4g.micro`, encrypted, private, 7-day backups, deletion protection (switched on by `aws.sh` once the create succeeds), **RDS-managed master password** |
| KMS key + alias `alias/<stack>-local-half` | Wraps Konect's local key half. **Retained** when the stack is deleted (not when the create itself fails) |
| Secrets Manager secret | The install token (bootstrap only) |
| Two IAM roles | Execution role (pull, logs, the token secret); task role (the KMS key with Konect's encryption context, the database secret) |
| Security groups, log group (30 days), optional Route 53 record, optional ECS service-linked role | |

Konect reads the database password from the RDS-managed secret itself and re-reads it after a rotation, so the weekly
rotation never breaks it and the password is never in the task's environment.

## Deploy

```bash
export KONECT_INSTALL_TOKEN=kit_…
scripts/aws.sh --region eu-west-2 --version 0.7.0 \
  --domain konect.example.com --certificate-arn arn:aws:acm:eu-west-2:<account>:certificate/<id> \
  --vpc vpc-… --alb-subnets subnet-a,subnet-b --db-subnets subnet-c,subnet-d \
  --image ghcr.io/kainossoftwareltd/sparkhub-konect-bootstrap@sha256:<digest> \
  [--hosted-zone Z…] [--permissions-boundary arn:aws:iam::<account>:policy/…] [--create-ecs-service-linked-role]
```

Without `--apply` the script creates the change set, prints it and leaves it unexecuted (a new stack then sits in
`REVIEW_IN_PROGRESS`; the script prints how to drop it). Add `--apply` to deploy. `scripts/aws.sh --help` lists every
flag. The token comes from `KONECT_INSTALL_TOKEN` or a hidden prompt, never from the command line.

Without `--hosted-zone`, point the domain at the `AlbDnsName` output. Then open the URL and click **Connect to
SparkHub**; a Konect manager approves it in SparkHub.

`--create-ecs-service-linked-role` is for an account that never ran ECS. `--no-public-ip` puts the task without a public
address; it then needs a NAT gateway to reach SparkHub, `ghcr.io`, `pkg-containers.githubusercontent.com`, S3, KMS and
Secrets Manager. A strict image (`strict/Dockerfile`) goes with `--strict --image <your image>`; no token is stored.

The install token can be read by anyone with `secretsmanager:GetSecretValue` on the secret the stack creates. That is
by design: it only downloads Konect.

### A failed create

If the first task cannot start (a wrong install token, an unknown version, no route to SparkHub), the deployment circuit
breaker stops after three failed tasks and CloudFormation rolls the whole stack back (about 15 minutes in all). The
database is created without deletion protection for exactly this reason (`aws.sh` turns it on after a successful
create), and the new KMS key is deleted with the rollback. What stays: the database's final snapshot (empty; delete it)
and, with `--create-ecs-service-linked-role`, the account's ECS service-linked role. The task's log in the stack's log
group says why it stopped. Fix the cause and run the same command again.

## Update and roll back

```bash
scripts/aws.sh --region eu-west-2 --version 0.7.1 --apply
```

Every parameter not given keeps its current value, the token included. CloudFormation registers a new task definition
and the service rolls: the new task starts before the old one stops. A version that cannot start is rolled back
automatically after three failed tasks (deployment circuit breaker). Rolling back by hand works to any version the
database still accepts. In the console: update the stack, use the current template, change `KonectVersion`.

## Backup and restore

Backups are RDS automated backups (7 days) and a final snapshot when the stack or the instance is deleted. The KMS key is
retained, so a snapshot stays readable. **A restore needs the same key and the same database.**

1. Create a new stack from the snapshot and the old key:
   ```bash
   scripts/aws.sh … --stack-name konect-restored --restore-snapshot <snapshot id> --existing-kms-key <old KmsKeyArn> --apply
   ```
   In the same account that is all: the key policy's `KeyAdministration` statement (the account root) lets IAM
   policies grant use of the key, and the new stack's task-role policy grants `kms:Encrypt`/`kms:Decrypt` on
   `ExistingKmsKeyArn` with the same encryption context (checked 2026-11-18: the restored stack started straight into
   `sparkhub_not_connected`).
2. Only if the old key's policy does not delegate to IAM (you removed the root statement, or the key is in another
   account): its `KonectLocalHalf` statement names the old task role, and a stack cannot change a key it does not own.
   Name the new task role (the new stack's `TaskRoleArn` output) there:
   ```bash
   aws kms get-key-policy --key-id <old key> --policy-name default --query Policy --output text > policy.json
   # edit: aws:PrincipalArn in the KonectLocalHalf statement = the new TaskRoleArn
   aws kms put-key-policy --key-id <old key> --policy-name default --policy file://policy.json
   ```
   Konect stays locked (`kms_unavailable`) until then and heals by itself.

## Delete

`aws cloudformation delete-stack` **fails while the database has deletion protection** (intended). Turn it off first
(`aws rds modify-db-instance --db-instance-identifier <DbInstanceId> --no-deletion-protection --apply-immediately`), then
delete the stack. CloudFormation keeps a final RDS snapshot and the KMS key. Clean up by hand when you no longer need
them: delete the snapshot, then schedule the key's deletion (`aws kms schedule-key-deletion`).
