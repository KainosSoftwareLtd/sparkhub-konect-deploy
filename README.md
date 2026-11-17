# sparkhub-konect-deploy

The public deployment kit for **Konect**, the SparkHub companion server.

**This repository holds no Konect code.** Konect itself is built and signed by Kainos and delivered through SparkHub:
a deployment presents an install token (`kit_…`) to SparkHub, receives a short-lived download link for a pinned
version, checks the release signature with keys compiled into the bootstrap, and runs the verified binary.

What lives here:

| Path | What |
|---|---|
| `cmd/konect-bootstrap/`, `internal/` | The bootstrap: a small Go program that resolves, downloads, verifies and `exec`s Konect |
| `Dockerfile` | The public bootstrap image (`ghcr.io/kainossoftwareltd/sparkhub-konect-bootstrap`) |
| `strict/Dockerfile` | Build-time path for environments that forbid downloads at start |
| `install/install.sh` | The laptop installer (macOS, Linux); SparkHub serves its own copy |
| `azure/`, `aws/`, `scripts/` | Deployment templates and thin wrappers |
| `compose/`, `k8s/` | Docker Compose and Kubernetes samples |
| `fixtures/` | Signed manifest test vectors (positive and negative) |

Licence: Apache-2.0 (see `LICENSE`).
