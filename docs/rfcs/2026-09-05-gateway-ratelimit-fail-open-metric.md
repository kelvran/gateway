# RFC: Live metric at the rate-limiter fail-open path

## Status

Accepted, implemented 2026-09-05.

## Context

A fresh backlog audit ranked "a live metric at the rate-limiter fail-open path" as a Small-effort item: `dataplane.checkRateLimit`'s fail-open branch (`docs/rfcs/2026-09-03-distributed-rate-limiting.md`'s "Fail-open, not fail-closed" policy) already logs `ratelimit_backend_unavailable` and surfaces `RateLimitFailOpen` on `GatewayDecisionEvent` per-request, but there was no aggregate, alertable signal — an operator could not answer "how many times has this happened in the last hour" without scanning logs or traces one request at a time.

Implementing it surfaced a materially bigger gap than advertised: this codebase has used OTel **Tracing** exclusively since inception (`internal/telemetry.Tracer`, `sdktrace`) — zero `MeterProvider`/`go.opentelemetry.io/otel/metric` usage anywhere. "Add a counter next to the log line" actually required standing up a full, separate OTel **Metrics** pipeline first. Given a choice between a lightweight in-process counter exposed via the admin API and building the real OTel Metrics pipeline, the real pipeline was chosen — consistent with every other telemetry signal in this codebase being real OTel, not a bespoke parallel mechanism.

## Design

### A second signal type, mirroring the first exactly

`internal/telemetry.Init` already switches on `cfg.Exporter` (`"stdout"` / `"otlp"` / `"none"`) to build a `sdktrace.SpanExporter` and a `TracerProvider`. This change adds the identical switch for a `sdkmetric.Exporter` and a `MeterProvider`, sharing the same `Resource` and the same exporter-kind decision — deliberately not a different shape for the second signal:

- `"stdout"` → `stdoutmetric.New()` (alongside the existing `stdouttrace.New()`)
- `"otlp"` → `otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpoint(cfg.OTLPEndpoint))` (alongside `otlptracehttp.New`)
- `"none"` → both providers skipped, same early return as before

`otel.SetMeterProvider(meterProvider)` is called right after `otel.SetTracerProvider(tracerProvider)`.

### `meter`/`rateLimitFailOpenCounter`: same package-init-time pattern as `Tracer`

`var meter = otel.Meter(...)` sits next to the existing `var Tracer = otel.Tracer(...)`, obtained before `Init` ever runs. `otel.Meter`'s own documented behavior (confirmed via `go doc`) is identical to `otel.Tracer`'s: an instrument obtained before a real provider is registered still delegates correctly once `Init` installs one — this codebase already relies on that exact guarantee for `Tracer`.

`rateLimitFailOpenCounter` (`kelvran.ratelimit.fail_open`, unit `{request}`) is constructed once at package init via a `mustInt64Counter` helper that panics on a construction error — the instrument name is a fixed, compile-time-known constant, so a construction failure here can only mean a real programming mistake, never a runtime condition, matching this codebase's existing posture for that class of error.

`RecordRateLimitFailOpen(ctx, keyID)` increments it with a `kelvran.virtual_key.id` attribute, and is called from `dataplane.checkRateLimit`'s existing fail-open branch, immediately after the pre-existing `Warn` log call — additive, not a replacement for that log line or for `GatewayDecisionEvent.RateLimitFailOpen`.

### New dependencies

`go.opentelemetry.io/otel/metric` and `go.opentelemetry.io/otel/sdk/metric` were already transitively resolved at v1.46.0 (matching every other OTel package in this module). `go.opentelemetry.io/otel/exporters/stdout/stdoutmetric` and `go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp` were fetched via `go get`, both resolving cleanly at v1.46.0. All four promoted to direct dependencies in `go.mod` via `go mod tidy` (they're imported directly by `internal/telemetry`, not merely transitive).

### Shutdown behavior: metrics flush failures are non-fatal, unlike traces

Discovered while verifying: with the metrics pipeline wired in, the pre-existing `TestInitOTLPExporterConstructsWithoutDialing` (which proves `Init` doesn't need a live collector reachable at construction *or* shutdown time — the trace exporter is non-blocking at both) started failing with a real `dial tcp [::1]:4318: connect: connection refused` from `meterProvider.Shutdown`.

Root cause: `sdkmetric.PeriodicReader.Shutdown` unconditionally attempts one last collect+export, even with zero data points ever recorded, whereas the trace pipeline's batch span processor has nothing queued to flush when no spans were ever recorded — a genuine, external asymmetry between the two OTel Go SDK signal types, not a bug in this codebase's wiring.

Fix: `Init`'s returned shutdown function now treats a `meterProvider.Shutdown` error as non-fatal — logged via `slog.WarnContext` (the same "leaf package uses `slog` directly rather than taking an injected logger" convention already established in `internal/budget`) but not propagated to the caller. `tracerProvider.Shutdown`'s error is still returned as before. This mirrors this codebase's own fail-open philosophy — the exact one `rateLimitFailOpenCounter` itself instruments: losing the last few seconds of counter data on shutdown because a collector is transiently unreachable is an accepted, industry-standard tradeoff for an aggregate metrics signal, unlike a span, which represents a discrete, non-repeatable event this codebase treats as more precious.

## Alternatives considered

**A lightweight in-process counter exposed via the admin API, skipping OTel Metrics entirely** — rejected by explicit choice: every other signal in this codebase (traces, structured logs) is real OTel or a real structured format: a bespoke admin-API-only counter would be a second, inconsistent telemetry mechanism for no reason other than avoiding standing up the Metrics SDK once.

**Skipping this item for now** — rejected; the underlying gap (no aggregate fail-open signal) was real and the audit ranked it as worth doing.

**Silently swallowing the metrics shutdown error instead of logging it** — rejected as inconsistent with this project's "never silently swallow errors" rule; `slog.WarnContext` keeps it observable without making it fatal.

## Verification

`go build ./... && go vet ./...`, `golangci-lint run ./...` (0 issues), `go run github.com/fe3dback/go-arch-lint@v1.18.0 check` (OK), `go mod tidy` (minimal diff — 4 packages promoted indirect→direct, one new transitive hash). `go test ./... -race`: green except the pre-existing, already-documented rootless-Docker `TestIntegrationTwoGatewayInstancesShareOneRedisRateLimit` failure.

New `TestRateLimitFailOpenIncrementsMetricCounter` (`internal/gateway/dataplane/gatewayevents_test.go`) installs a real `sdkmetric.ManualReader`-backed `MeterProvider` as the global provider, drives a real fail-open request through `HandleChatCompletion` via the existing `failingRedisBackend` fixture, then `Collect`s and asserts a `kelvran.ratelimit.fail_open` data point of value `1` carrying the expected `kelvran.virtual_key.id` attribute. Sanity-checked by temporarily removing the `RecordRateLimitFailOpen` call: the new test failed with "counter did not record a value of 1," then restored.

`TestInitOTLPExporterConstructsWithoutDialing` re-verified directly: before the shutdown fix, it failed with a real dial error; after, it passes and the log output shows the expected non-fatal `telemetry_metrics_shutdown_flush_failed` warning.
