# Kelvran gateway — multi-stage build producing a single static binary,
# matching gateway/ARCHITECTURE.md's stated distribution shape
# ("Single static binary, scratch/alpine Docker image").

# --- builder stage ---
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Cache module downloads separately from source changes. This pass has
# no third-party Go dependencies (go.sum doesn't exist yet), but this
# still keeps the Dockerfile correct once real deps are ever added.
COPY go.mod ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 for a truly static binary that runs on scratch.
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/gateway ./cmd/gateway

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

ENTRYPOINT ["/gateway"]
CMD ["-config", "/config.yaml"]
