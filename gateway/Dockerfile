# Kelvran gateway — multi-stage build producing a single static binary,
# matching gateway/ARCHITECTURE.md's stated distribution shape
# ("Single static binary, scratch/alpine Docker image").

# --- builder stage ---
# Pinned by digest (independently re-verified via `docker buildx
# imagetools inspect golang:1.27.1-alpine` -- matches exactly, not
# blindly copied from a remediation suggestion) -- closes a real open
# OpenSSF Scorecard Pinned-Dependencies finding (PinnedDependenciesID:
# "containerImage not pinned by hash"). Resolves to golang:1.27.1-alpine
# as of this pin -- a trailing "# 1.27.1-alpine" comment on the FROM
# line itself is NOT valid Dockerfile syntax (confirmed by a real local
# build failure, "FROM requires either one or three arguments", not
# assumed), so the human-readable tag is named here instead.
# The builder stage always runs on the BUILD platform and cross-compiles for
# the TARGET platform (TARGETOS/TARGETARCH are set by BuildKit per platform
# of a multi-platform build; empty under a legacy single-platform build,
# where Go then defaults to the host). Only the tiny FROM scratch stage
# below is per-platform, and it only COPYs files, so a linux/arm64 image
# needs neither an emulated compile nor QEMU at all.
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS builder

WORKDIR /src

# Cache module downloads separately from source changes. This pass has
# no third-party Go dependencies (go.sum doesn't exist yet), but this
# still keeps the Dockerfile correct once real deps are ever added.
COPY go.mod ./
RUN go mod download

COPY . .

# Build identity for `gateway -version` and the build_info log line. The
# defaults keep a plain `docker build` honest ("dev"); CI passes the real
# values as build-args (see .github/workflows/ci.yml's publish-image job).
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# CGO_ENABLED=0 for a truly static binary that runs on scratch; -trimpath and
# -s -w for reproducible paths and a smaller binary.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /out/gateway ./cmd/gateway

# --- final stage ---
FROM scratch

# Copy CA certificates so outbound HTTPS calls to upstream providers
# (OpenAI, Anthropic, ...) can verify TLS certificates.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=builder /out/gateway /gateway

# The gateway's own config file is not baked into the image — mount a
# real config.yaml (never config.example.yaml with real secrets in it,
# since it never has any) at runtime, e.g.:
#   docker run -v $(pwd)/config.yaml:/config.yaml ... /gateway -config /config.yaml
EXPOSE 8080

# Run as a real non-root numeric UID -- FROM scratch has no /etc/passwd
# for name resolution, so USER takes the raw UID directly (the kernel
# only ever sees the number). 65532 matches distroless's own
# established "nonroot" convention rather than an arbitrary value.
# ListenAddr's default port (8080) is well above 1024, so no root
# privilege is needed to bind it either way.
USER 65532:65532

ENTRYPOINT ["/gateway"]
CMD ["-config", "/config.yaml"]
