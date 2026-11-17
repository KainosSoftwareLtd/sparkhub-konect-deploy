# syntax=docker/dockerfile:1.10
#
# The Konect bootstrap image (design #1702 §5.3): ghcr.io/kainossoftwareltd/sparkhub-konect-bootstrap.
# It holds no Konect code. At start the bootstrap asks SparkHub for the pinned KONECT_VERSION, verifies the
# release signature and hashes, caches the binary under /data/.konect-bootstrap and execs it: Konect runs as PID 1.
#
#   docker run -d -p 127.0.0.1:41800:41800 -v konect-data:/data --read-only \
#     -v "$PWD/konect-local.key:/run/secrets/konect-local.key:ro" \
#     -e KONECT_INSTALL_TOKEN -e KONECT_VERSION=0.7.0 \
#     ghcr.io/kainossoftwareltd/sparkhub-konect-bootstrap@sha256:<digest from SparkHub → Konect → Install>
#
# Non-root (65532), no shell, no package manager; works with a read-only root (everything written is under /data).
# The binary is built with the same pinned toolchain and flags as the release assets, so the image's
# /usr/local/bin/konect-bootstrap is byte-identical to the release's konect-bootstrap-linux-<arch> (CI checks it).

# golang:1.27.4-bookworm (multi-arch index)
ARG GO_IMAGE=golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea
# gcr.io/distroless/cc-debian12:nonroot (multi-arch index): glibc + libstdc++, what a bun-compiled Konect needs
ARG RUNTIME_IMAGE=gcr.io/distroless/cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.0.0-dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOFLAGS=-mod=readonly GOTOOLCHAIN=local \
    go build -trimpath -buildvcs=false -ldflags "-s -w -buildid= -X main.version=${VERSION}" \
      -o /out/konect-bootstrap ./cmd/konect-bootstrap \
 && mkdir -p /data-template/konect && chmod 0700 /data-template /data-template/konect

# The binary alone, for CI's reproducibility check (docker buildx build --target binary --output type=local,…).
FROM scratch AS binary
COPY --from=build /out/konect-bootstrap /konect-bootstrap

FROM ${RUNTIME_IMAGE}
ARG VERSION=0.0.0-dev
LABEL org.opencontainers.image.title="SparkHub Konect bootstrap" \
      org.opencontainers.image.description="Gets a signed Konect release from SparkHub, verifies it and runs it. Holds no Konect code." \
      org.opencontainers.image.vendor="Kainos Software Ltd" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/KainosSoftwareLtd/sparkhub-konect-deploy" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build --chmod=0755 /out/konect-bootstrap /usr/local/bin/konect-bootstrap
# /data: seeds a Docker named volume (Konect's dataDir is /data/konect, the bootstrap's cache /data/.konect-bootstrap).
COPY --from=build --chown=65532:65532 --chmod=0700 /data-template /data
# /data-template: for the strict Dockerfile (strict/Dockerfile), which copies it into its own image.
COPY --from=build --chown=65532:65532 --chmod=0700 /data-template /data-template
USER 65532:65532
WORKDIR /data
VOLUME ["/data"]
ENV HOME=/data \
    SQLITE_TMPDIR=/data/konect \
    KONECT_LOCAL_KEY_FILE=/run/secrets/konect-local.key \
    KONECT_CONFIG=builtin:sqlite
EXPOSE 41800
# Docker only (compose, a VM): Container Apps and ECS ignore HEALTHCHECK and probe /v1/health over HTTP.
# start-period 120 s: the first start downloads about 35 MB.
HEALTHCHECK --interval=30s --timeout=5s --start-period=120s --retries=3 CMD ["/usr/local/bin/konect-bootstrap", "health"]
ENTRYPOINT ["/usr/local/bin/konect-bootstrap"]
CMD ["serve", "--host", "0.0.0.0", "--insecure-http"]
