# syntax=docker/dockerfile:1
#
# sard — the Sardonyx CLI as a single static binary on FROM scratch.
#
# Build (buildx; COPY --link requires BuildKit ≥ 0.11):
#   docker buildx build \
#     --platform linux/amd64,linux/arm64 \
#     --build-arg VERSION=1.0.0 \
#     --build-arg REVISION=$(git rev-parse HEAD) \
#     --build-arg CREATED="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
#     -t sard:1.0.0 --push .
#
# Single-arch quick build:
#   docker build -t sard:latest .
#
# Runtime notes:
#   - The image is one hardlinked ELF file (COPY --link) — no OS, no shell,
#     no git binary. The local-directory source works (mount a volume with
#     Markdown files). The git-repo source shells out to `git` at runtime
#     and therefore does not work in this image; build it on top of a base
#     that provides git if you need cloning.
#   - Provenance/SBOM annotations (org.opencontainers.artifact.*) are set
#     with buildx --provenance/--sbom, not in this file.

# ── Build stage ─────────────────────────────────────────────────────────────
ARG GO_VERSION=1.27
FROM golang:${GO_VERSION} AS build

ARG VERSION=dev
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Static, stripped, version-stamped (README "Building" recipe), target-arch aware.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/sard ./cmd/sard

# ── Final image ─────────────────────────────────────────────────────────────
FROM scratch

ARG VERSION=dev
ARG CREATED
ARG REVISION

# OCI image labels (org.opencontainers.image.*). There are no
# org.opencontainers.image.base.* entries: the base is scratch, which has
# no upstream image name or digest to record.
LABEL org.opencontainers.image.title="sard" \
      org.opencontainers.image.description="Ingests Markdown files from a git repository URL or a local directory into Onyx via the Ingestion API." \
      org.opencontainers.image.vendor="jnohlgard" \
      org.opencontainers.image.url="https://github.com/jnohlgard/sardonyx" \
      org.opencontainers.image.source="https://github.com/jnohlgard/sardonyx" \
      org.opencontainers.image.licenses="AGPL-3.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.revision="${REVISION}"

# --link hardlinks the binary into the final layer instead of rewriting it,
# so the image carries the exact bytes the build stage produced.
COPY --link --chmod=0755 --from=build /out/sard /usr/local/bin/sard

ENV PATH=/usr/local/bin
WORKDIR /

ENTRYPOINT ["/usr/local/bin/sard"]
