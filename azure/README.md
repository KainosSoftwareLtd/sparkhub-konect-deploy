# Konect on Azure

[![Deploy to Azure](https://aka.ms/deploytoazurebutton)](https://portal.azure.com/#create/Microsoft.Template/uri/https%3A%2F%2Fraw.githubusercontent.com%2FKainosSoftwareLtd%2Fsparkhub-konect-deploy%2Fv1.1.0%2Fazure%2Fazuredeploy.json)

The button deploys `azuredeploy.json` from the release tag in its URL, so it never changes under you. The same
template runs from the command line with `scripts/azure.sh`. `main.bicep` is the source; `azuredeploy.json` is built
from it (`scripts/build-azure.sh`) and CI fails when the two differ.

```
https://portal.azure.com/#create/Microsoft.Template/uri/<URL-encoded https://raw.githubusercontent.com/KainosSoftwareLtd/sparkhub-konect-deploy/v<release>/azure/azuredeploy.json>
```

The URL carries no parameters: the token is typed into the form, never put in a URL.

## What you need

- An **install token** (`kit_…`): SparkHub → Konect → **Install** → Create token. It only downloads Konect.
- The **Konect version** to run, for example `0.7.0`.
- Rights on the resource group: **Contributor**, plus **Owner**, **User Access Administrator** or **Role Based Access
  Control Administrator** for the one role assignment the template makes. Without those, set
  `createRoleAssignments = false` (below).
- About 20 minutes. Most of it is Azure creating the Container Apps environment.

## What it creates

| Resource | Notes |
|---|---|
| Container App `<name>` | One replica. Runs the bootstrap image, which downloads Konect, checks its signature and runs it. Probes `GET /v1/health` |
| Container Apps environment `<name>-env` | Consumption |
| Log Analytics `<name>-logs` | 30 days |
| Managed identity `<name>-id` | Signs in to PostgreSQL and wraps Konect's local key half |
| Key Vault `kv<suffix>` + key `konect-local-half` | RSA 3072, wrap/unwrap only. No secret is stored in the vault |
| PostgreSQL Flexible Server `<name>-pg-<suffix>` | Version 16, **Entra sign-in only (no password)**, the identity is its Entra administrator, 7-day backups |

The identity gets one Azure role: **Key Vault Crypto Service Encryption User** on the key. Its PostgreSQL role is the
identity's name.

## Parameters

| Parameter | Default | Notes |
|---|---|---|
| `name` | `konect` | 3–13 lowercase letters, digits, hyphens. Keep it for every redeploy |
| `location` | the resource group's | |
| `konectVersion` | required | `X.Y.Z` (`X.Y.Z-rc.N` on preprod). No `latest` |
| `imageKind` | `bootstrap` | `strict`: your image built with `strict/Dockerfile`; no token is stored anywhere |
| `image` | the bootstrap image of this release, by digest | `strict`: your image |
| `installToken` | | Required with `bootstrap`. Kept only as a Container Apps secret. Enter it again on every redeploy |
| `sparkhub` | `prod` | `preprod` for testing against SparkHub preprod |
| `customDomain` | empty | Empty: the generated `https://<name>.<environment domain>`. Bind your domain to the Container App afterwards |
| `createRoleAssignments` | `true` | `false` for deployers without role-assignment rights |
| `postgresSku` / `postgresTier` / `postgresStorageGb` | `Standard_B1ms` / `Burstable` / `32` | |
| `cpu` / `memory` | `0.5` / `1Gi` | |
| `purgeProtection` | `true` | `false` only for test deployments you will delete |
| `existingKeyId` | empty | Restore only (below) |
| `postgresServerName` | empty | Restore only (below) |

Invalid input stops the deployment in validation, before anything is created, with an error such as
`The language expression property 'installToken is required with imageKind bootstrap …' doesn't exist`.

## After the deploy

The outputs give `url` and `nextStep`. Open the URL and click **Connect to SparkHub**; a Konect manager approves it in
SparkHub.

- **First start:** the new Entra administrator takes a few minutes to work. Expect at most one container restart in the
  first ten minutes. Konect then starts locked until it is connected.
- **`createRoleAssignments = false`:** Konect starts and stays locked with `kms_unavailable` until a platform team runs
  the command in `nextStep`, then heals by itself:
  ```bash
  az role assignment create --assignee-object-id <identityPrincipalId> --assignee-principal-type ServicePrincipal \
    --role e147488a-f6f5-4113-8e2d-b22465e65bf6 --scope <keyId>
  ```

## Command line

```bash
KONECT_INSTALL_TOKEN=kit_… scripts/azure.sh --subscription <id|name> --resource-group konect-rg \
  --location westeurope --version 0.7.0            # what-if
KONECT_INSTALL_TOKEN=kit_… scripts/azure.sh … --apply   # deploy
```

`scripts/azure.sh --help` lists the flags. The token comes from `KONECT_INSTALL_TOKEN` or a hidden prompt, never from
the command line. Needs `az` and `jq`.

## Update and roll back

Redeploy with another `konectVersion`, everything else the same. Only the Container App changes and a new revision rolls.

- Portal: the deployment → **Redeploy**, change `konectVersion`, enter the token again.
- CLI: `scripts/azure.sh --resource-group konect-rg --version 0.7.1 --apply`. It keeps the previous deployment's settings
  and refuses a different `--name`.
- Rolling back works to any version the database still accepts (Konect refuses to start on a store migrated past it).
  The previous version is still in the container's cache.
- Do not change the image in the portal's revision editor: it rejects `@sha256:` image references (an open Azure bug).
  Redeploy the template instead.

## Backup and restore

Backups are PostgreSQL's automated backups (7 days, point-in-time) and the key in the vault (soft delete, purge
protection). There is no key file to keep. **A restore needs the same key and the same database.**

- **In place**, after data loss: restore the server to a point in time (a new server), then redeploy onto it:
  ```bash
  KONECT_INSTALL_TOKEN=kit_… scripts/azure.sh --resource-group konect-rg --version 0.7.0 \
    --restore-from <name>-pg-<suffix> --restore-time 2026-11-17T08:00:00Z --apply
  ```
  This runs `az postgres flexible-server restore` and deploys with `postgresServerName` set to the new server. The key
  is unchanged.
- **Into a new resource group:** pass the old server's resource ID to `--restore-from` and the old deployment's `keyId`
  output to `--existing-key-id` (template parameters `postgresServerName` and `existingKeyId`). The new identity is
  granted on the old key and made an Entra administrator of the restored server.

## Delete

Delete the resource group. The PostgreSQL server and its data are gone. The vault is soft-deleted (90 days with purge
protection, 7 without). Redeploying into the same group with the same `name` then meets the deleted vault: recover it
(`az keyvault recover --name kv<suffix>`) or use another `name`.

## Network

Konect needs outbound HTTPS to SparkHub, `ghcr.io` and `pkg-containers.githubusercontent.com` (the image), and the
release bucket SparkHub links to. PostgreSQL allows Azure services (firewall `0.0.0.0`) and the vault has a public
endpoint; private networking is a later option.
