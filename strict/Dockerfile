# syntax=docker/dockerfile:1.10
# check=skip=SecretsUsedInArgOrEnv
#
# Strict path (design #1702 §6): build your own Konect image at BUILD time, so nothing is downloaded when it starts
# and the image you scan is the image you run. The release signature and hashes are checked by the bootstrap during
# the build; the install token is a BuildKit secret and never lands in a layer; the image holds no SparkHub credential.
#
#   KONECT_INSTALL_TOKEN=… docker buildx build --platform linux/amd64,linux/arm64 \
#     --build-arg KONECT_VERSION=0.7.0 --secret id=konect_token,env=KONECT_INSTALL_TOKEN \
#     --build-arg BOOTSTRAP=ghcr.io/kainossoftwareltd/sparkhub-konect-bootstrap@sha256:<digest from SparkHub → Konect → Install> \
#     -t <your-registry>/konect:0.7.0 https://github.com/KainosSoftwareLtd/sparkhub-konect-deploy.git#<tag>:strict
#
# Preprod and pre-releases: add --build-arg KONECT_SPARKHUB=preprod. Behind a TLS-inspecting proxy: the predefined
# --build-arg HTTPS_PROXY=… works as usual, and the proxy's CA goes in as a second secret (a PEM bundle with the public
# roots and the proxy CA): --secret id=proxy_ca,src=<bundle.pem> --build-arg SSL_CERT_FILE=/run/secrets/proxy_ca.
# Re-verify the image later with
# `konect-bootstrap verify /usr/share/konect --binary /usr/local/bin/konect --version 0.7.0` (copy them out first).

# The bootstrap image, pinned by digest. Pass the digest SparkHub shows; the default is the release this file was
# published with.
ARG BOOTSTRAP=ghcr.io/kainossoftwareltd/sparkhub-konect-bootstrap@sha256:bbd47e455e4463aa013052bf47409bee9e99ec418c5314607565328dcf9b4903
# gcr.io/distroless/cc-debian12:nonroot (multi-arch index)
ARG RUNTIME_IMAGE=gcr.io/distroless/cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f

FROM ${BOOTSTRAP} AS fetch
ARG KONECT_VERSION
ARG KONECT_SPARKHUB=prod
# Empty = the image's CA bundle; /run/secrets/proxy_ca with the proxy_ca secret (above)
ARG SSL_CERT_FILE=
# exec form: no shell in the image; the ARGs reach the program as environment variables
RUN --mount=type=secret,id=konect_token,env=KONECT_INSTALL_TOKEN,required=true \
    --mount=type=secret,id=proxy_ca,target=/run/secrets/proxy_ca,required=false,mode=0444 \
    ["/usr/local/bin/konect-bootstrap", "fetch", "--out", "/tmp/konect-out", "--client", "build"]

FROM ${RUNTIME_IMAGE}
COPY --from=fetch --chmod=0755 /tmp/konect-out/konect /usr/local/bin/konect
COPY --from=fetch /tmp/konect-out/manifest.json /tmp/konect-out/manifest.sig.json /usr/share/konect/
COPY --from=fetch --chown=65532:65532 --chmod=0700 /data-template /data
USER 65532:65532
WORKDIR /data
VOLUME ["/data"]
ENV HOME=/data \
    SQLITE_TMPDIR=/data/konect \
    KONECT_LOCAL_KEY_FILE=/run/secrets/konect-local.key \
    KONECT_CONFIG=builtin:sqlite
EXPOSE 41800
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/konect", "health", "--url", "http://127.0.0.1:41800", "--allow-locked"]
ENTRYPOINT ["/usr/local/bin/konect"]
CMD ["serve", "--host", "0.0.0.0", "--insecure-http"]
