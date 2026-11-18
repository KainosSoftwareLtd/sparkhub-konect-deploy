# sparkhub-konect-deploy

The public deployment kit for **Konect**, the SparkHub companion server.

**This repository holds no Konect code.** Konect itself is built and signed by Kainos and delivered through SparkHub:
a deployment presents an install token (`kit_…`) to SparkHub, receives a short-lived download link for a pinned
version, checks the release signature with keys compiled into the bootstrap, and runs the verified binary.

| Path | What |
|---|---|
| `cmd/konect-bootstrap/`, `internal/` | The bootstrap: a small Go program (standard library only) that resolves, downloads, verifies and `exec`s Konect |
| `Dockerfile` | The public bootstrap image, `ghcr.io/kainossoftwareltd/sparkhub-konect-bootstrap` |
| `strict/Dockerfile` | Build-time path for environments that forbid downloads at start |
| `install/install.sh` | The laptop installer (macOS, Linux); SparkHub serves its own copy |
| `compose/`, `k8s/` | Docker Compose and Kubernetes samples |
| `azure/`, `aws/`, `scripts/` | Deployment templates and thin wrappers |
| `fixtures/` | Signed manifest test vectors (positive and negative), shared with SparkHub's verifier |
| `e2e/` | The end-to-end test: the real binary against a stub SparkHub |

Licence: Apache-2.0 (see `LICENSE`).

## Getting started

1. In SparkHub, open **Konect → Install**, create an install token and note the bootstrap image digest shown there.
2. Pick a way to run Konect:
   - **Docker on a VM**: `compose/compose.yml` (instructions at the top of the file).
   - **Kubernetes**: `k8s/` (`kubectl apply -k k8s`, after filling in `secret.example.yaml`).
   - **Laptop**: `curl -fsSL https://sparkhub.studio/konect/install.sh | sh -s -- --org <org> --version <X.Y.Z>`.
   - **No downloads at start**: `strict/Dockerfile` builds your own image (instructions in the file).
3. Open Konect's URL, click **Connect to SparkHub**; a Konect manager of your organization approves it.

Always pin the image by **digest** (`…sparkhub-konect-bootstrap@sha256:…`), never by tag. You can mirror it (ACR
artifact cache, ECR pull-through) and keep the same digest.

## The bootstrap

```
konect-bootstrap [konect args…]          resolve, fetch or reuse, verify, exec Konect (default: serve --host 0.0.0.0 --insecure-http)
konect-bootstrap health [args]           exec the cached Konect's health check; no network; exit 1 while nothing is cached
konect-bootstrap fetch --out <dir>       resolve, download, verify; write konect + manifest.json + manifest.sig.json
konect-bootstrap verify <dir>            verify a fetched directory offline (--binary, --version, --platform, --sparkhub)
konect-bootstrap version [--json]        its version and the release keys it trusts (with fingerprints)
konect-bootstrap -- <args>               pass args to Konect that start with one of the words above
```

| Variable | Default | Meaning |
|---|---|---|
| `KONECT_INSTALL_TOKEN` | required | The install token. Never logged. **Removed from the environment before Konect starts** |
| `KONECT_VERSION` | required | Exact `X.Y.Z` (`X.Y.Z-rc.N` on preprod only). No `latest`. Passed on to Konect |
| `KONECT_SPARKHUB` | `prod` | `prod` (`<org>.sparkhub.studio`) or `preprod` (`<org>.sparkhub.guru`); Konect reads the same variable |
| `KONECT_BOOTSTRAP_CACHE` | `/data/.konect-bootstrap` | Cache directory on the data volume |
| `HTTPS_PROXY`, `NO_PROXY` | | Honoured |
| `SSL_CERT_FILE`, `SSL_CERT_DIR` | the image's CA bundle | Behind a TLS-inspecting proxy, put the proxy CA here for the bootstrap **and** into the system store for Konect (`--use-system-ca`) |

What it does at start:

1. Looks at the cache `<cache>/<version>/`: verifies the signature and hashes the binary **through the file descriptor
   it will execute**. A mismatch deletes the entry (`cache_mismatch` in the log); it is never run.
2. Asks SparkHub (`POST https://<org>.<host>/api/konect/install/v1/resolve`) for the version, with the cached hash.
3. Downloads when needed: capped at the signed object size, gunzipped with a cap at the signed binary size, every hash
   checked against the **signed** manifest, then placed in the cache atomically.
4. Keeps the pinned version and the previous one (rollback without a download), deletes the rest.
5. Strips `KONECT_INSTALL_TOKEN` and every `KONECT_BOOTSTRAP_*` variable, adds `KONECT_BOOTSTRAP=<its version>` and
   `KONECT_ARTIFACT_SHA256`, and `execveat`s the verified file descriptor. Konect replaces the bootstrap as PID 1.
   If the cache mount is `noexec`, it copies the verified bytes into a sealed `memfd`, hashes the copy and runs that.

| SparkHub answers | With a verified cache | Without |
|---|---|---|
| 200 | run (the manifest must be byte-identical to the cached one, else exit 78 `manifest_changed`) | download, verify, run |
| 200 + `bootstrap_deprecated` | run, warn on every start | download, run, warn |
| 401 / 403 / 404 / 400 / 426 | exit 78 | exit 78 |
| 410 withdrawn | run, with a loud warning | exit 78 |
| 429 | run (offline start) | wait `Retry-After` if it fits the budget, else exit 1 naming the time it clears |
| 5xx, timeout, DNS, TLS | run (offline start) | retry at 5, 10, 20 s, then every 60 s, 15 minutes in total, then exit 1 |

Exit codes: **78** = a refusal or a configuration error (fix it; a restart fails the same way); **1** = could not get
Konect in time (the platform restarts it); **143** = SIGTERM during a download.

Egress: `<org>.sparkhub.studio` (or `.sparkhub.guru`), the release bucket (`sparkhub-kainos-global.s3.us-east-2.amazonaws.com`
for prod), and for the image pull `ghcr.io` and `pkg-containers.githubusercontent.com`.

## Releases and tags

This repository has two kinds of tag, both created only by the repository admins (the `release-tags` ruleset):

| Tag | What it is | What runs |
|---|---|---|
| `v<semver>` | a **bootstrap release** | `release.yml`: binaries, image, attestations, GitHub Release |
| `templates-v<semver>` | a **templates release**: the deployment templates (`azure/`, `aws/`, `strict/`, `scripts/`) at a commit whose defaults pin a published bootstrap image by digest | nothing; the tag is only a fixed ref |

A bootstrap tag can never pin its own image: the digest exists only after that tag's build. So the templates are
versioned separately, and the Deploy to Azure button, the CLI snippets and SparkHub's Install page all point at the
latest `templates-v*` tag, never at a `v*` tag.

**The rule:** after each bootstrap release `vX.Y.Z`, open a PR that pins its image index digest (from the release
notes) in `azure/main.bicep` (then `scripts/build-azure.sh`), `aws/konect.yaml` and `strict/Dockerfile`, merge it,
and tag the merge commit on `main` as the next `templates-v*` (patch bump for a digest-only change). Then move the
button in [`azure/README.md`](azure/README.md) and SparkHub's templates version (Platform Config
`KONECT_TEMPLATES_VERSION`) to the new tag.

| Templates tag | Bootstrap image it pins |
|---|---|
| `templates-v1.0.1` | `1.0.0` (`sha256:5a9bd5fb82f0e10f5f9613c1e938c6b7bc5f61b775880285d540b3cba5b0d76a`) |

The `v1.0.0` tag's templates still pin `1.0.0-rc.1`; use `templates-v1.0.1` or later.

## Verifying what you run

Every release of this repository publishes the four binaries with `SHA256SUMS`, an SPDX SBOM per binary, GitHub
build-provenance and SBOM attestations, and the multi-arch image signed with cosign (keyless) and attested
(provenance + SBOM pushed to the registry). The release notes list the commands:

```
gh attestation verify konect-bootstrap-linux-x64 --repo KainosSoftwareLtd/sparkhub-konect-deploy
gh attestation verify oci://ghcr.io/kainossoftwareltd/sparkhub-konect-bootstrap@sha256:<digest> --repo KainosSoftwareLtd/sparkhub-konect-deploy
cosign verify ghcr.io/kainossoftwareltd/sparkhub-konect-bootstrap@sha256:<digest> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/KainosSoftwareLtd/sparkhub-konect-deploy/.github/workflows/release.yml@refs/tags/v'
```

The build is reproducible: `scripts/build.sh dist <version>` with the Go version named in the release notes gives
the same `SHA256SUMS`, and the image's `/usr/local/bin/konect-bootstrap` is byte-identical to the release asset for
its architecture (CI checks both).

## `install.sh` hand-off with SparkHub

SparkHub serves `install/install.sh` at `https://sparkhub.studio/konect/install.sh` (preprod: `sparkhub.guru`). It
replaces only the block between `# >>> sparkhub-filled` and `# <<< sparkhub-filled` with values from its current
`bootstrap` catalog row:

| Variable | Value |
|---|---|
| `SPARKHUB_ENV` | `prod` or `preprod` (the serving environment) |
| `BOOTSTRAP_VERSION` | the catalog's bootstrap version |
| `BOOTSTRAP_URL_<PLATFORM>` | the asset URL on this repository's GitHub Release (`LINUX_X64`, `LINUX_ARM64`, `MACOS_ARM64`, `MACOS_X64`) |
| `BOOTSTRAP_SHA256_<PLATFORM>` | that asset's SHA-256 from the release's `SHA256SUMS` (as imported) |

Each value is a plain shell string in double quotes; SparkHub must only write `[0-9A-Za-z._:/-]` characters there.
The repository copy has the block empty and refuses to run.

## Development

Go is pinned in the workflows (`GO_VERSION`). `go test ./...` runs the unit tests; `go test -tags e2e ./e2e/` builds the
bootstrap with the `e2e` tag (it then trusts a key from its environment and may call a plain-HTTP stub on the loopback
interface; a release build has neither) and runs it against a stub SparkHub.
