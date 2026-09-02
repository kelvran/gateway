- **Status**: accepted
- **Date**: 2026-09-02
- **Author(s)**: project founder + Claude Code

## Summary

Add real OpenTelemetry tracing to the gateway's dataplane request pipeline: one span per chat-completion request (buffered or streaming), carrying GenAI semantic-convention attributes plus Kelvran-custom attributes (virtual key, cache hit, cost, and — the headline feature — `agent_run_id`, propagated in from the caller via standard W3C Baggage). This fulfills `gateway/ARCHITECTURE.md`'s Tech Stack commitment ("OTel Go SDK, GenAI semantic-convention attributes from day one") and `PRD.md`'s v1 scope line ("OTel span emission with `agent_run_id` propagation from day one") — both written before any code existed, neither delivered until now.

## Motivation

`PRD.md`'s Problem Statement opens with the gap this closes: *"Every gateway surveyed... understands a single request/response pair as the unit of cost, rate limiting, and observability. None of them understand a multi-step agent run — so 'why did this agent session cost $4' has no answer beyond a raw total."* Kelvran now has per-key budget tracking (`docs/rfcs/2026-09-02-virtual-keys-budgets.md`), which answers "which key" — but not "which agent run." Right now the gateway has zero observability beyond structured JSON log lines (`logRequest` in `internal/gateway/dataplane`), which are gateway-local and not trace-correlated across a multi-step agent run at all.

This RFC is scoped narrowly to the **gateway-emits-spans** half of the documented vision. It deliberately does **not** build the `api/otel/` or `api/gatewayevents/` versioned cross-language protobuf contract (`DESIGN.md`'s "Shared-Contract Concept") — `evals` has no consumer for gateway trace data yet, and per this project's own `api/README.md`, that contract gets written "when Gateway's Phase 0 MVP needs its first OTel span shape" for a *real* cross-language need, not speculatively. Building a custom protobuf schema with no consumer would risk getting it wrong and duplicates OTLP, which already has a real wire format for exactly this. If/when `evals` needs to ingest gateway traces, that's its own RFC.

## Detailed Design

### New dependency: the OTel Go SDK

`gateway/go.mod` currently has **zero** external dependencies — this RFC adds the first ones: `go.opentelemetry.io/otel`, `go.opentelemetry.io/otel/sdk`, `go.opentelemetry.io/otel/exporters/stdout/stdouttrace`, and `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp`. This is a deliberate, pre-approved exception to this project's stdlib-only habit (established for streaming's SSE transport and the hand-rolled YAML parser) — `gateway/ARCHITECTURE.md`'s Tech Stack table already named the OTel Go SDK specifically, before any code existed, and there is no meaningful stdlib alternative to a real tracing SDK.

**GenAI semantic conventions are still "development" stability** in the upstream spec (`open-telemetry/semantic-conventions`), not yet 1.0/stable. This RFC hardcodes the attribute *string keys* it needs as untyped Go constants in `internal/telemetry`, rather than depending on `go.opentelemetry.io/otel/semconv`'s incubating GenAI module — see Alternatives Considered for why. Attribute names used (verified against the spec's `gen-ai` model at implementation time):

```
gen_ai.operation.name           = "chat"
gen_ai.provider.name            (e.g. "openai", "anthropic")
gen_ai.request.model
gen_ai.request.stream           (set only when true)
gen_ai.response.model
gen_ai.response.id
gen_ai.response.finish_reasons  (array)
gen_ai.usage.input_tokens
gen_ai.usage.output_tokens
```
Span name, per the spec's own naming rule: `"{gen_ai.operation.name} {gen_ai.request.model}"` (e.g. `"chat gpt-4o"`).

Kelvran-custom attributes, under a `kelvran.*` namespace (matching `docs/operations/TELEMETRY.md`'s existing framing — "Kelvran-custom attributes on top of `gen_ai.*`"):

```
kelvran.virtual_key.id      — empty if auth never resolved (a failed-auth span still exists)
kelvran.agent_run_id        — from Baggage, per below. Never fabricated: absent if the caller never sent one.
kelvran.cache.hit           — bool
kelvran.cost.usd
kelvran.deployment.name     — which deployment actually served the response (or was last attempted, on failure)
```

**Never added, by design:** prompt or completion content. `docs/operations/TELEMETRY.md`'s Privacy & Redaction section already commits to content being opt-in, not default-on — this RFC doesn't build the opt-in mechanism at all, so there is no way for content to leak into a span in this pass, not even behind a flag someone could enable.

### `agent_run_id` propagation: W3C Baggage, not a custom header

`docs/operations/TELEMETRY.md` already specifies the mechanism: *"`agent_run_id` via OTel Baggage."* This RFC implements exactly that, and also wires the standard W3C `traceparent` propagator alongside it in one composite propagator — so a calling agent framework that's already OTel-instrumented gets Kelvran's span correctly nested as a child of its own trace, and `agent_run_id` (an application-level correlation ID orthogonal to trace/span IDs, not a substitute for them) rides along as a Baggage member:

```
baggage: agent_run_id=run-abc123
traceparent: 00-4bf92f...-00f067...-01     (optional, standard W3C trace context)
```

`internal/telemetry.ExtractContext(ctx, r *http.Request) context.Context` does both extractions in one call via `otel.GetTextMapPropagator().Extract(...)`. `internal/telemetry.AgentRunIDFromContext(ctx) string` reads the `agent_run_id` Baggage member back out (empty string if absent).

### `internal/telemetry` (new package)

```go
type Config struct {
    Exporter     string // "stdout" | "otlp" | "none" — "" defaults to "stdout"
    OTLPEndpoint string // used only when Exporter == "otlp"
}

// Init sets the global TracerProvider and TextMapPropagator per cfg.
// Returns a shutdown func the caller flushes on graceful exit.
func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error)

func ExtractContext(ctx context.Context, r *http.Request) context.Context
func AgentRunIDFromContext(ctx context.Context) string

// Tracer is package-level (obtained via otel.Tracer(...) at init time) —
// NOT injected through dataplane.Config. OTel's global TracerProvider is
// specifically designed so a tracer grabbed before Init runs still
// delegates correctly to whatever provider Init later installs
// (documented init-order independence) — this is the one place in this
// codebase where the usual explicit-dependency-injection convention is
// deliberately not followed, because the no-op default requires zero
// setup for every test that doesn't care about tracing.
var Tracer = otel.Tracer("github.com/kelvran/gateway/internal/gateway/dataplane")

// ChatCompletionResult carries only primitive values (string/int/float/
// bool/error), never identity.VirtualKey or adapter.ChatResponse directly
// — telemetry stays a dependency-free leaf, exactly like internal/budget
// already does for the same reason (gateway/ARCHITECTURE.md's dependency
// rules: leaves don't depend on each other).
type ChatCompletionResult struct {
    VirtualKeyID, Provider, DeploymentName    string
    ResponseModel, ResponseID                 string
    FinishReasons                             []string
    InputTokens, OutputTokens                 int
    CacheHit                                  bool
    CostUSD                                   float64
    Err                                       error
}

// RecordChatCompletionResult sets every attribute only knowable once a
// request has finished (or failed) and records Err on span, if non-nil.
// Shared by both HandleChatCompletion and HandleChatCompletionStream so
// there is exactly one implementation of "what a finished span looks
// like," not two that can drift.
func RecordChatCompletionResult(span trace.Span, r ChatCompletionResult)
```

### Dataplane wiring

Both `HandleChatCompletion` and `HandleChatCompletionStream` start a span immediately (span name computable from `req.Model` before auth even runs, so an auth failure still produces a real, named span — `kelvran.virtual_key.id` just stays empty on that path, exactly like `logRequest`'s existing "always finalize" pattern). The same deferred closure that already calls `logRequest` is extended to also call `telemetry.RecordChatCompletionResult` and `span.End()` — one finalize step, not two independently-evolving ones. `HandleChatCompletion`'s existing fallback-tracking `dep` variable is updated on the fallback path too (a one-line addition — today it's read but never reassigned there), so `kelvran.deployment.name`/`gen_ai.provider.name` reflect whichever deployment actually served the response, not just the first one attempted.

### Config

```yaml
telemetry:
  exporter: "stdout"              # "stdout" | "otlp" | "none" — omit for "stdout"
  otlp_endpoint: "localhost:4318"  # only read when exporter: "otlp"
```

Matches `docs/operations/TELEMETRY.md`'s already-stated defaults exactly: *"For local development, a console exporter... is the intended default"* and *"OTLP is the baseline... for any OTLP-compatible collector/backend."* `telemetry.Init` applies the `""` → `"stdout"` default itself (not `controlplane`), since it's an operational default, not a config-shape concern — mirroring how `cmd/gateway` (not `controlplane`) already owns the rate-limit defaults from the virtual-keys RFC.

### Where `telemetry.Init` is called

From `cmd/gateway`'s `run()`, before `buildPipeline` — a process-startup concern, not a pipeline-construction one. This is a deliberate choice: `buildPipeline`'s signature and behavior stay unchanged, so every existing integration-test helper that calls `buildPipeline` directly (bypassing `run()`) keeps working unmodified, with the SDK's documented no-op default tracer, unless a specific test opts into a real in-memory span recorder via `otel.SetTracerProvider` itself.

## Drawbacks

- First external Go dependency this module has ever had. Accepted: `gateway/ARCHITECTURE.md` pre-committed to it, and there's no real stdlib alternative to a tracing SDK.
- GenAI semantic conventions are pre-1.0 and will likely rename/reshape attributes before stabilizing — this RFC's hardcoded attribute-key constants will need a follow-up pass when that happens. Documented as a known, dated tradeoff, not a surprise for future maintainers.
- No graceful shutdown (SIGTERM handling) exists anywhere in `cmd/gateway` yet — `Init`'s returned `shutdown` func is wired but only actually exercised if `http.ListenAndServe` returns due to an error, not on a real process signal. The SDK's batch span processor still exports periodically regardless, so this mostly costs the last few seconds of spans on a hard kill — an accepted, pre-existing limitation of this binary's process lifecycle, not something this RFC is scoped to fix.
- One span per request, not a full HTTP-server-level span nested around it (no `otelhttp` middleware) — a reasonable future addition, not built here; see Unresolved Questions.

## Alternatives Considered

1. **Depend on `go.opentelemetry.io/otel/semconv`'s incubating GenAI package for attribute-key constants** — rejected for v1: that package is explicitly unstable and its Go API surface (package path, constant names) changes between SDK versions as the spec itself changes. Hardcoded string constants for the exact attributes this RFC needs are simpler and avoid stacking a second moving target on top of the spec's own churn.
2. **A custom `agent_run_id` HTTP header instead of W3C Baggage** — rejected: `docs/operations/TELEMETRY.md` already specified Baggage before this RFC was written, and Baggage is the actual standards-compliant mechanism for exactly this kind of cross-cutting application data, with the added benefit of composing with `traceparent` extraction for free via the same composite propagator.
3. **Build `api/otel/`'s versioned protobuf contract now, since this is "when Gateway's Phase 0 MVP needs its first OTel span shape"** — considered and rejected for this exact pass: `evals` still has no real consumer, and OTLP already IS a real, standard wire format for exporting spans to any backend. The protobuf contract's actual job (per `DESIGN.md`) is the `gatewayevents` cost/usage schema specifically, which needs a concrete evals-side consumer to design against correctly — building it speculatively here would violate this project's own YAGNI convention.
4. **Full `otelhttp` middleware for generic HTTP-server spans** — rejected as premature scope: this RFC is about GenAI-specific request tracing, not general HTTP instrumentation; a wrapping HTTP-level span is a legitimate, independent future addition.

## Unresolved Questions

- Whether `gen_ai.provider.name`'s well-known enum values (`"openai"`, `"anthropic"`, etc.) should be validated/normalized against the spec's known list, or passed through verbatim from each adapter's `Name()` — this RFC passes them through verbatim; they already match the spec's known values for the two real adapters, so there's nothing to normalize yet, but a future non-standard provider name wouldn't be caught.
- Sampling: this RFc ships with the SDK's default `ParentBased(AlwaysSample())` — every request is traced. Revisit if/when trace volume becomes a real cost concern; no evidence of that yet.
- Full process-level graceful shutdown (SIGTERM → context cancellation → flush spans → exit) is a real gap for `cmd/gateway` as a whole, not just for tracing — flagged here since this RFC is what makes the gap concretely costly (lost spans, not just an abrupt exit), but fixing it is bigger than this RFC's scope.
