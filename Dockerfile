# syntax=docker/dockerfile:1

# PZAdmin builds to a single static binary with no third-party dependencies, so
# the runtime image can be scratch: no shell, no package manager, no libc, and
# nothing for an attacker to pivot into if the web interface is ever breached.
#
# This one file builds both the local image (docker-compose.dev.yml) and
# the published one (GitHub Actions to ghcr.io/thewarboys2/pzadmin). The only
# difference between them is the VERSION build argument.
#
# Pin the builder hard. GO_IMAGE is an argument so you can pin by digest without
# editing this file:
#
#   docker pull golang:1.24-bookworm
#   docker image inspect golang:1.24-bookworm -f '{{index .RepoDigests 0}}'
#   docker build --build-arg GO_IMAGE=golang@sha256:... .
ARG GO_IMAGE=golang:1.24-bookworm

# The builder always runs on the machine doing the build and cross-compiles
# for the target. Go needs no emulator for that, so an arm64 image is built as
# fast as an amd64 one, and the tests run natively once for every platform.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
WORKDIR /src

# Copy the module definition first so the layer caches independently of source
# changes. There are no external modules to download, which is the point.
COPY go.mod ./
COPY . .

# vet and test before anything is built: an image is only produced from code
# that passes. Nothing above this line depends on the target platform, so a
# multi-platform build runs this step once.
RUN --mount=type=cache,target=/root/.cache/go-build \
    go vet ./... \
 && CGO_ENABLED=0 go test ./...

# VERSION is what the binary reports in its log, the UI and the event log.
# Builds from a Git tag pass it in (1.0.0); anything else is a development
# build and says so.
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags="-s -w -X github.com/TheWarBoys2/pzadmin/internal/server.Version=${VERSION}" \
      -o /out/pzadmin .

# The data directory is owned by the user PZAdmin runs as, so the named volume
# compose mounts over it is initialised with that owner on first creation.
ARG PZADMIN_UID=1000
ARG PZADMIN_GID=1000
RUN mkdir -p /out/data && chown ${PZADMIN_UID}:${PZADMIN_GID} /out/data

FROM scratch

# TLS roots so outbound webhook notifications can reach an https endpoint.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ARG PZADMIN_UID=1000
ARG PZADMIN_GID=1000
COPY --from=build --chown=${PZADMIN_UID}:${PZADMIN_GID} /out/data /data
COPY --from=build /out/pzadmin /pzadmin

# Run unprivileged, as the user that owns the server folders.
USER ${PZADMIN_UID}:${PZADMIN_GID}

ENV PZADMIN_DATA=/data \
    PZADMIN_ADDR=:27815 \
    TZ=UTC

EXPOSE 27815

# There is no shell in this image, so the binary healthchecks itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/pzadmin", "healthcheck"]

ENTRYPOINT ["/pzadmin"]
