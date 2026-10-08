// Package telemetry wires the gateway's dataplane into real OpenTelemetry
// tracing, per docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md.
//
// This is the first package in gateway/ to depend on anything outside the
// standard library — go.opentelemetry.io/otel and its SDK/exporters — a
// deliberate, pre-approved exception to the stdlib-only habit established
// for streaming's SSE transport and the hand-rolled YAML config parser
// (see gateway/ARCHITECTURE.md's Tech Stack table, which named the OTel Go
// SDK specifically before any code in this repository existed).
//
// Like internal/budget, this package is a dependency-free leaf: it never
// imports internal/identity or internal/adapter, taking only primitive
// values (see ChatCompletionResult in result.go) — per
// gateway/ARCHITECTURE.md's dependency rules, leaves don't depend on each
// other.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Tracer is the package-level Tracer gateway's dataplane starts spans
// with. Obtained via otel.Tracer at package-init time, NOT injected
// through dataplane.Config — OTel's global TracerProvider is specifically
// designed so a Tracer obtained before Init runs still delegates
// correctly to whatever provider Init later installs (documented
// init-order independence). This is the one place in this codebase that
// deliberately doesn't follow the usual explicit-dependency-injection
// convention, because the SDK's no-op default requires zero setup for
// every test that doesn't care about tracing.
var Tracer = otel.Tracer("github.com/kelvran/gateway/gateway/internal/gateway/dataplane")

// InstanceID identifies this specific gateway process for the lifetime of
// the process, computed once at package-init time — the "instance ID"
// docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md needs to
// correlate cache-check events across gateway replicas, per
// docs/upgrade-research/cache-2026-09-06.md Finding 5. Confirmed by grep
// before adding this (no UUID, no hostname, no OTel service.instance.id
// resource attribute existed anywhere in this codebase) that nothing
// already served this purpose. Deliberately hostname+pid, not a random
// UUID: zero new runtime dependency (os.Hostname/os.Getpid are stdlib;
// github.com/google/uuid is only an indirect, test-tooling dependency
// pulled in transitively today, never used by any production code path —
// see the RFC's Alternatives considered), and Kelvran's real deployment
// model gives every replica its own hostname (one container/pod each) —
// pid guards only the narrower, non-containerized case of two gateway
// processes sharing one host.
var InstanceID = computeInstanceID()

func computeInstanceID() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

// meter mirrors Tracer's own package-init-time-obtained, re-delegating
// pattern for the separate OTel Metrics signal, per
// docs/rfcs/2026-09-05-gateway-ratelimit-fail-open-metric.md — the first
// use of OTel Metrics anywhere in this codebase (tracing-only until now).
// otel.Meter's own documented behavior is identical to otel.Tracer's:
// obtained before a real MeterProvider is registered, it still delegates
// correctly once Init installs one.
var meter = otel.Meter("github.com/kelvran/gateway/gateway/internal/gateway/dataplane")

// rateLimitFailOpenCounter counts every request allowed through despite a
// rate-limiter backend error (fail-open), per
// docs/rfcs/2026-09-03-distributed-rate-limiting.md's fail-open policy —
// previously only a per-request log line/span attribute, with no
// aggregate, alertable signal. Constructed once at package init: the
// instrument name is a fixed, compile-time-known constant, so a
// construction error here can only ever indicate a real programming
// mistake, not a runtime condition — panicking is the same posture Go's
// own OTel SDK examples use for this exact situation.
var rateLimitFailOpenCounter = mustInt64Counter(
	meter,
	"kelvran.ratelimit.fail_open",
	metric.WithDescription("Requests allowed through despite a rate-limiter backend error (fail-open)."),
	metric.WithUnit("{request}"),
)

// budgetFailOpenCounter counts every request allowed through despite a
// Redis-backed budget.Tracker backend error (fail-open) — the budget
// dimension's own equivalent of rateLimitFailOpenCounter above, needed
// once budget.Tracker gained a genuine Redis-mode (see
// internal/budget/redisbudget) that can itself fail, unlike the
// always-in-memory Tracker this counter's absence previously reflected.
var budgetFailOpenCounter = mustInt64Counter(
	meter,
	"kelvran.budget.fail_open",
	metric.WithDescription("Requests allowed through despite a budget-tracker backend error (fail-open)."),
	metric.WithUnit("{request}"),
)

// RecordBudgetFailOpen increments the fail-open counter for keyID. Callers
// (dataplane's budget-reservation call sites) call this at the exact same
// point they already log a budget_backend_unavailable warning — an
// additional, aggregate-friendly signal, not a replacement for that log
// line, mirroring RecordRateLimitFailOpen's identical convention.
func RecordBudgetFailOpen(ctx context.Context, keyID string) {
	budgetFailOpenCounter.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrKelvranVirtualKeyID, keyID)))
}

// guardrailFailOpenCounter counts every request allowed through despite a
// guardrail detector error — the guardrail dimension's own equivalent of
// rateLimitFailOpenCounter/budgetFailOpenCounter above. guardrail.Engine
// is category-tiered fail-open/fail-closed (see internal/guardrail's
// Policy.ErrorActions): a detector in a Warn-tier category that errors
// (or panics) leaves the request flowing with that detector's coverage
// silently missing. Until 2026-10-07 the only trace of that was the
// engine's own guardrail_detector_error log line, which carries neither
// a trace id nor a virtual-key id. Attributed with kelvran.virtual_key.id
// and kelvran.guardrail.stage ("precall"/"postcall"/"embeddings") because
// a pre-call fail-open (an unscreened PROMPT reached a provider) is a
// materially different signal from a post-call one (an unscreened
// RESPONSE reached a client) — per
// docs/upgrade-research/kelvran-deep-research-round3-2026-10-07.md.
var guardrailFailOpenCounter = mustInt64Counter(
	meter,
	"kelvran.guardrail.fail_open",
	metric.WithDescription("Requests allowed through despite a guardrail detector error (fail-open)."),
	metric.WithUnit("{request}"),
)

// RecordGuardrailFailOpen increments the guardrail fail-open counter for
// keyID at the given stage (one of the GuardrailStage* constants). Callers
// (dataplane's five guardrail Check call sites) call this at the exact
// same point they log a guardrail_fail_open warning — an additional,
// aggregate-friendly signal, not a replacement for that log line,
// mirroring RecordRateLimitFailOpen's identical convention. Recorded at
// most once per request per stage, never once per detector or per
// embeddings input.
func RecordGuardrailFailOpen(ctx context.Context, keyID, stage string) {
	guardrailFailOpenCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrKelvranVirtualKeyID, keyID),
		attribute.String(AttrKelvranGuardrailStage, stage),
	))
}

func mustInt64Counter(m metric.Meter, name string, opts ...metric.Int64CounterOption) metric.Int64Counter {
	counter, err := m.Int64Counter(name, opts...)
	if err != nil {
		panic(fmt.Errorf("telemetry: constructing %q counter: %w", name, err))
	}
	return counter
}

func mustFloat64Histogram(m metric.Meter, name string, opts ...metric.Float64HistogramOption) metric.Float64Histogram {
	histogram, err := m.Float64Histogram(name, opts...)
	if err != nil {
		panic(fmt.Errorf("telemetry: constructing %q histogram: %w", name, err))
	}
	return histogram
}

func mustFloat64Counter(m metric.Meter, name string, opts ...metric.Float64CounterOption) metric.Float64Counter {
	counter, err := m.Float64Counter(name, opts...)
	if err != nil {
		panic(fmt.Errorf("telemetry: constructing %q counter: %w", name, err))
	}
	return counter
}

// RecordRateLimitFailOpen increments the rate-limit fail-open counter for
// keyID. Callers record it at the exact point they already log their
// fail-open Warn line -- checkRateLimit (ratelimit_backend_unavailable and
// ratelimit_tpm_backend_unavailable), HandleEmbeddings
// (embeddings_ratelimit_backend_unavailable) and the mid-stream TPM top-up
// (ratelimit_tpm_backend_unavailable, once per stream) -- never as a
// replacement for that log line, which carries the backend error text. The
// fallback-hop admission check (ratelimit_backend_unavailable_fallback_hop)
// logs without counting: when the primary check also failed open the request
// is already counted, and the unit is {request}; when the primary check
// passed and Redis failed before the hop, the admission goes uncounted -- a
// known gap, docs/operations/FAILURE-MODES.md section 9.
func RecordRateLimitFailOpen(ctx context.Context, keyID string) {
	rateLimitFailOpenCounter.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrKelvranVirtualKeyID, keyID)))
}

// streamCostEstimatedCounter is per
// docs/upgrade-research/request-lifecycle-reliability-2026-09-15.md — an
// aggregate, alertable signal for how often a streamed response's cost
// was billed from an estimate rather than the provider's own reported
// usage, mirroring rateLimitFailOpenCounter's own construction pattern.
var streamCostEstimatedCounter = mustInt64Counter(
	meter,
	"kelvran.streaming.cost_estimated",
	metric.WithDescription("Streamed responses billed against an estimated, not provider-reported, token count."),
	metric.WithUnit("{response}"),
)

// RecordStreamCostEstimated increments the cost-estimated counter for
// keyID. The caller (dataplane.finalize) calls this exactly when
// ChatCompletionResult.CostEstimated is true — see that field's own doc
// comment for what triggers it.
func RecordStreamCostEstimated(ctx context.Context, keyID string) {
	streamCostEstimatedCounter.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrKelvranVirtualKeyID, keyID)))
}

// streamingNearDuplicateCollisionCounter is per
// docs/upgrade-research/gateway-performance-optimization-2026-09-24.md
// Finding 1's own recommendation: before building streaming-path request
// coalescing (a genuinely more involved mechanism than runMissPath's
// buffered-path singleflight.Group — a broadcast/fan-out to every
// concurrent waiter, not a single shared return value), first measure
// whether genuinely-identical concurrent streaming requests for the same
// l1Key actually happen often enough in real traffic to justify building
// it. This is a traffic-SHAPE question, not a traffic-volume one —
// answerable with this cheap, log-only instrumentation alone, without
// committing to the real build first. Mirrors rateLimitFailOpenCounter's
// own construction pattern.
var streamingNearDuplicateCollisionCounter = mustInt64Counter(
	meter,
	"kelvran.streaming.near_duplicate_collision",
	metric.WithDescription("Streaming requests that found another streaming request for the identical (exact-match) l1Key already in flight -- observation only, never coalesced or blocked."),
	metric.WithUnit("{collision}"),
)

// RecordStreamingNearDuplicateCollision increments the near-duplicate-
// collision counter for keyID. The caller
// (dataplane.HandleChatCompletionStream) calls this at the exact same
// point it already logs a streaming_near_duplicate_collision warning —
// an additional, aggregate-friendly signal, not a replacement for that
// log line, mirroring RecordRateLimitFailOpen's own identical convention.
// Only ever called when a genuine collision was detected (see
// streamingInFlightAcquire's own doc comment, dataplane.go/streaming.go)
// — never unconditionally on every streaming request, matching this
// file's existing "only emit when meaningful" convention.
func RecordStreamingNearDuplicateCollision(ctx context.Context, keyID string) {
	streamingNearDuplicateCollisionCounter.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrKelvranVirtualKeyID, keyID)))
}

// RecordFallbackHop emits a "fallback_hop" span event for a single FAILED
// fallback-chain hop attempt, per AttrKelvranFallbackHopErrorClass's own
// doc comment (result.go). Uses trace.SpanFromContext(ctx) rather than an
// explicit trace.Span parameter (unlike RecordChatCompletionResult):
// dataplane.attemptFallbackChain is called from two real call sites
// (runMissPath, streamDeploymentWithFallback) several stack frames below
// HandleChatCompletion/HandleChatCompletionStream's own span, and ctx
// already carries that exact span end to end (telemetry.Tracer.Start
// reassigns ctx, not just a bare span) — threading an explicit span
// parameter through attemptFallbackChain's own ~13 call sites (2 real, 11
// pre-existing unit tests) for a single event emission would be needless
// churn for no behavioral gain. A no-op span (every pre-existing unit
// test calling attemptFallbackChain directly against
// context.Background()) silently discards AddEvent, per the OTel API's
// own documented contract — safe, not a bug, and requires zero test
// updates.
//
// An event on the request's already-open chat span, not a new child
// span: each hop is a lightweight, in-process routing decision plus one
// upstream call, exactly OTel's own "events vs. spans" guidance for a
// sub-step that doesn't need its own timing/status/parent-child
// relationship.
//
// Only ever called for a hop that failed — see attemptFallbackChain's own
// call site. A hop that SUCCEEDS is the chain's terminal result, already
// fully captured by the request's own final span attributes at
// finalize() (kelvran.deployment.name, gen_ai.response.*, etc. via
// RecordChatCompletionResult) — recording it a second time here would be
// redundant, not additive.
func RecordFallbackHop(ctx context.Context, deploymentName, errorClass string, duration time.Duration) {
	trace.SpanFromContext(ctx).AddEvent("fallback_hop", trace.WithAttributes(
		attribute.String(AttrKelvranDeploymentName, deploymentName),
		attribute.String(AttrKelvranFallbackHopErrorClass, errorClass),
		attribute.Float64(AttrKelvranFallbackHopDurationMs, float64(duration.Milliseconds())),
	))
}

// operationDurationHistogram is the OTel GenAI semantic-conventions
// instrument named in docs/upgrade-research/gateway-2026-09-06.md
// Finding 3: gen_ai.client.operation.duration (Histogram, unit "s") —
// additive to docs/rfcs/2026-09-05-gateway-ratelimit-fail-open-metric.md,
// which stood up this codebase's only prior instrument and explicitly
// named this as the *first* metric, not the only one. The instrument
// name is the spec's own canonical string, not a kelvran.*-namespaced
// equivalent — deliberately, so a GenAI-aware dashboard (e.g. Envoy AI
// Gateway's published Grafana dashboard, the concrete precedent this
// Finding cites) can query it without any Kelvran-specific translation.
// A live, provisioned Grafana dashboard
// (docs/operations/grafana/dashboards/kelvran-overview.json) and live
// Prometheus SLO rules (docs/operations/grafana/prometheus/
// kelvran-slo-rules.yml) query this exact instrument by name — never
// rename or remove it without updating both.
var operationDurationHistogram = mustFloat64Histogram(
	meter,
	"gen_ai.client.operation.duration",
	metric.WithDescription("Duration of a GenAI client operation, from the gateway's own request boundary."),
	metric.WithUnit("s"),
)

// tokenBucketBoundaries are the semantic-conventions spec's own
// recommended ExplicitBucketBoundaries for the two per-operation token
// histograms below — shared because both instruments cover the same
// {token} value range.
var tokenBucketBoundaries = metric.WithExplicitBucketBoundaries(
	1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864,
)

// inputTokensCounter, outputTokensCounter, cacheReadTokensCounter,
// cacheWriteTokensCounter, inputTokensOperationHistogram, and
// outputTokensOperationHistogram are the current OTel GenAI
// semantic-conventions token-metrics instruments
// (open-telemetry/semantic-conventions-genai, docs/gen-ai/
// gen-ai-token-metrics.md), replacing this package's own prior
// gen_ai.client.token.usage Histogram + gen_ai.token.type enum
// attribute — removed outright (clean cutover, not a dual-emit
// transition) after a live end-to-end research pass confirmed
// gen_ai.client.token.usage was itself removed upstream
// (open-telemetry/semantic-conventions-genai PR #374, merged
// 2026-09-22) and that zero Kelvran dashboard/PromQL rule anywhere
// referenced it by name, unlike operationDurationHistogram above.
//
// The 5th counter in the same spec family,
// gen_ai.client.inference.usage.reasoning.output_tokens
// (reasoningTokensCounter below), was deliberately held back until
// reasoning-token tracking became a real feature: an instrument that
// only ever recorded a fabricated 0 would have read as "confirmed zero
// reasoning tokens" to a dashboard, not "never measured". It exists
// since 2026-10-07, fed by adapter.Usage.ReasoningTokens (Anthropic
// output_tokens_details.thinking_tokens, Bedrock's copy of it via
// additionalModelResponseFields, OpenAI/openaicompat
// completion_tokens_details.reasoning_tokens), and is still only
// recorded when > 0 — a provider that does not report the breakdown
// never produces a series.
//
// The *_tokens Counters carry a gen_ai.token.modality attribute per
// the spec's own requirement; the two *OperationHistogram instruments
// deliberately do not — the spec explains percentiles across
// modalities don't add up meaningfully, so modality is omitted there.
var (
	// reasoningTokensCounter: "The number of output tokens used for
	// reasoning (e.g. chain-of-thought, extended thinking)", "a subset of
	// gen_ai.client.inference.usage.output_tokens" — verified 2026-10-07
	// against model/gen-ai/token-metrics.yaml (requirement_level:
	// recommended, instrument counter, unit {token}).
	reasoningTokensCounter = mustInt64Counter(
		meter,
		"gen_ai.client.inference.usage.reasoning.output_tokens",
		metric.WithDescription("The number of output tokens used for reasoning (e.g. chain-of-thought, extended thinking); a subset of output tokens."),
		metric.WithUnit("{token}"),
	)
	inputTokensCounter = mustInt64Counter(
		meter,
		"gen_ai.client.inference.usage.input_tokens",
		metric.WithDescription("The number of input (prompt) tokens used, including cached tokens."),
		metric.WithUnit("{token}"),
	)
	outputTokensCounter = mustInt64Counter(
		meter,
		"gen_ai.client.inference.usage.output_tokens",
		metric.WithDescription("The number of output (completion) tokens used, including reasoning tokens."),
		metric.WithUnit("{token}"),
	)
	cacheReadTokensCounter = mustInt64Counter(
		meter,
		"gen_ai.client.inference.usage.cache_read.input_tokens",
		metric.WithDescription("The number of input tokens served from a provider-managed cache."),
		metric.WithUnit("{token}"),
	)
	cacheWriteTokensCounter = mustInt64Counter(
		meter,
		"gen_ai.client.inference.usage.cache_write.input_tokens",
		metric.WithDescription("The number of input tokens written to a provider-managed cache."),
		metric.WithUnit("{token}"),
	)
	inputTokensOperationHistogram = mustFloat64Histogram(
		meter,
		"gen_ai.client.inference.operation.input_tokens",
		metric.WithDescription("The number of input (prompt) tokens used per inference operation."),
		metric.WithUnit("{token}"),
		tokenBucketBoundaries,
	)
	outputTokensOperationHistogram = mustFloat64Histogram(
		meter,
		"gen_ai.client.inference.operation.output_tokens",
		metric.WithDescription("The number of output (completion) tokens used per inference operation."),
		metric.WithUnit("{token}"),
		tokenBucketBoundaries,
	)
)

// AttrGenAITokenModality is the semantic-conventions spec's
// gen_ai.token.modality attribute — required on every Counter declared
// above. Only "text"/"unknown" are ever produced by this codebase today
// (see genAITokenModalityFor's own doc comment in dataplane.go):
// Bedrock's Converse API never breaks usage down by modality, so a
// request carrying any multimodal adapter.ContentPart genuinely cannot
// be attributed a real text-vs-image split, and the spec's own guidance
// is to report "unknown" rather than guess. "image"/"audio" are real,
// well-known spec values this codebase has no code path to produce yet.
const AttrGenAITokenModality = "gen_ai.token.modality"

const (
	GenAITokenModalityText    = "text"
	GenAITokenModalityUnknown = "unknown"
)

// RecordChatCompletionMetrics records both GenAI histograms from r — the
// exact same ChatCompletionResult RecordChatCompletionResult already
// populates at dataplane.finalize's existing call site, per this
// Finding's own "reuse the existing per-request data capture point,
// don't add a new one" framing. Not merged into RecordChatCompletionResult
// itself: that function takes a trace.Span and has no context.Context,
// which metric.Float64Histogram.Record requires.
//
// gen_ai.client.operation.duration is recorded unconditionally — every
// call to finalize represents one finished (or failed) operation,
// success or rejection, matching this codebase's own "ALWAYS runs, even
// on error/cancel" framing for finalize itself. error.type is attached
// only when r.ErrorType is non-empty (r.Err != nil at the call site),
// per the spec's own "conditionally required on failure" framing for
// that attribute — never a fabricated empty string on success.
//
// The 5 token-usage Counters and 2 per-operation Histograms below are
// recorded only when r.Billable — a cache hit (any layer) or a
// coalesced singleflight follower replays token counts from a real
// upstream call this specific request itself never made, per
// docs/rfcs/2026-09-05-gateway-cost-double-counting.md's billable gate
// (already used identically by budget.Record and limiter.RecordTokens).
// Unlike AttrKelvranCostUSD/InputTokens on the span — a single
// per-request attribute, safe to report as "what this would have cost"
// even on a cache hit — a Counter/Histogram accumulates across many
// requests; replaying the same cached token count on every subsequent
// hit would inflate an aggregate token-throughput query by however many
// times that entry was served, not just report it once.
func RecordChatCompletionMetrics(ctx context.Context, r ChatCompletionResult) {
	var attrs []attribute.KeyValue
	attrs = append(attrs, attribute.String(AttrGenAIOperationName, "chat"))
	if r.Provider != "" {
		attrs = append(attrs, attribute.String(AttrGenAIProviderName, genAIProviderName(r.Provider)))
	}
	if r.RequestModel != "" {
		attrs = append(attrs, attribute.String(AttrGenAIRequestModel, r.RequestModel))
	}
	if r.ResponseModel != "" {
		attrs = append(attrs, attribute.String(AttrGenAIResponseModel, r.ResponseModel))
	}
	if r.ErrorType != "" {
		attrs = append(attrs, attribute.String(AttrErrorType, r.ErrorType))
	}

	operationDurationHistogram.Record(ctx, r.Duration.Seconds(), metric.WithAttributes(attrs...))

	if !r.Billable {
		return
	}
	// modalityAttrs gets its own copy of attrs, rather than sharing attrs's
	// backing array via append — a real, if benign here (each Record call
	// fully consumes its slice before the next append runs), footgun not
	// worth relying on. The two *OperationHistogram Records below reuse
	// bare attrs directly (no modality attribute, per the spec's own
	// per-instrument attribute set).
	modalityAttrs := append(append([]attribute.KeyValue{}, attrs...), attribute.String(AttrGenAITokenModality, r.TokenModality))
	if r.InputTokens > 0 {
		inputTokensCounter.Add(ctx, int64(r.InputTokens), metric.WithAttributes(modalityAttrs...))
		inputTokensOperationHistogram.Record(ctx, float64(r.InputTokens), metric.WithAttributes(attrs...))
	}
	if r.OutputTokens > 0 {
		outputTokensCounter.Add(ctx, int64(r.OutputTokens), metric.WithAttributes(modalityAttrs...))
		outputTokensOperationHistogram.Record(ctx, float64(r.OutputTokens), metric.WithAttributes(attrs...))
	}
	if r.CacheReadTokens > 0 {
		cacheReadTokensCounter.Add(ctx, int64(r.CacheReadTokens), metric.WithAttributes(modalityAttrs...))
	}
	if r.CacheCreationTokens > 0 {
		cacheWriteTokensCounter.Add(ctx, int64(r.CacheCreationTokens), metric.WithAttributes(modalityAttrs...))
	}
	if r.ReasoningTokens > 0 {
		reasoningTokensCounter.Add(ctx, int64(r.ReasoningTokens), metric.WithAttributes(modalityAttrs...))
	}
}

// cacheL3GateOutcomeCounter records the pass/reject outcome of each of
// Cache L3-lite's existing gate checks, per
// docs/upgrade-research/cache-2026-09-06.md Finding 1: GroundedCache's own
// per-gate ablation methodology found that of its four gates, one did most
// of the safety work while the others were near-zero-cost backup — nobody
// at Kelvran currently knows whether that's also true of L3-lite's own
// three gates. One dimensioned instrument (gate × outcome), not three
// separate counters, so a single query can compare reject rates across
// gates directly — exactly the comparison an ablation needs.
var cacheL3GateOutcomeCounter = mustInt64Counter(
	meter,
	"kelvran.cache.l3.gate_outcome",
	metric.WithDescription("Cache L3-lite gate check outcomes (pass/reject), per gate."),
	metric.WithUnit("{check}"),
)

// Cache L3-lite gate names, per checkLexicalCache's own three existing
// checks (dataplane.go) — the exact three Finding 1 named for ablation.
const (
	CacheL3GateVolatileBypass     = "volatile_bypass"
	CacheL3GateEntityMismatch     = "entity_mismatch"
	CacheL3GateFreshnessRiskModel = "freshness_risk_model"
	// CacheL3GateNegationMismatch is per DECISIONS.md's [2026-09-12]
	// entry — a narrow, additive gate closing a syntactic-negation-
	// particle-insertion failure mode distinct from the antonym-flip
	// case the [2026-09-08] entry already investigated and rejected
	// fixing here. Not one of Finding 1's original three named gates
	// (per checkLexicalCache's own doc comment on that distinction).
	CacheL3GateNegationMismatch = "negation_mismatch"
)

// RecordCacheL3GateOutcome increments the counter for gate with outcome
// "reject" or "pass". The caller (dataplane.checkLexicalCache) calls this
// at each of its own three existing gate-decision points — no new gate
// logic, only a counter alongside logic that already exists.
//
// Also attributed with InstanceID, per
// docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md: L3-lite has no
// exact-key concept to correlate the way L1/L2 do (its own match is a
// fuzzy near-duplicate search, never a single deterministic key — see
// that RFC's Alternatives considered for why L3 doesn't get its own
// exact-key correlation event), so this is L3's own, narrower share of
// "touch the checkLexicalCache call site": once this already-shipped
// ablation counter (Finding 1) sees traffic from more than one InstanceID
// value, that alone is a real, low-effort signal that multi-instance
// deployment has begun. InstanceID is a small, fixed-cardinality value
// (one per running process) — safe as a metric attribute, unlike a raw
// cache key (see logCacheCrossInstanceCheck's own doc comment on that
// distinction).
func RecordCacheL3GateOutcome(ctx context.Context, gate string, rejected bool) {
	outcome := "pass"
	if rejected {
		outcome = "reject"
	}
	cacheL3GateOutcomeCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrKelvranCacheL3Gate, gate),
		attribute.String(AttrKelvranCacheL3Outcome, outcome),
		attribute.String(AttrKelvranInstanceID, InstanceID),
	))
}

// cacheSavingsCounter is the notional USD cost of every cache-hit
// request, summed by layer: what the request WOULD have cost had it not
// been served from cache. Per docs/rfcs/2026-09-10-gateway-cache-savings-
// metric.md: dataplane.finalize already computes this exact figure on
// every cache hit (gated only on err == nil, never on billable — see
// that function's own doc comment naming "a cache-savings dashboard" as
// the intended use), and already emits kelvran.cache.hit/kelvran.cache.layer
// on the same span — this instrument is the one missing piece, a real,
// already-aggregatable exported counter an operator's own Prometheus/
// Grafana can sum/graph by layer directly, rather than a raw-span query
// they'd have to write themselves. Dimensioned by the SAME
// AttrKelvranCacheLayer key the span attribute already uses, so one query
// groups both signals identically.
var cacheSavingsCounter = mustFloat64Counter(
	meter,
	"kelvran.cache.savings_usd",
	metric.WithDescription("Notional USD cost of cache-hit requests -- what this request would have cost had it not been served from cache -- summed by cache layer."),
	metric.WithUnit("{USD}"),
)

// RecordCacheSavings increments cacheSavingsCounter by savingsUSD,
// dimensioned by layer ("L1"/"L2"/"L3"). Callers must only call this on a
// genuine cache hit (layer non-empty) — mirrors
// RecordChatCompletionMetrics's own "only record when meaningful" gating
// convention, avoiding a misleading zero-value data point on every miss.
func RecordCacheSavings(ctx context.Context, layer string, savingsUSD float64) {
	cacheSavingsCounter.Add(ctx, savingsUSD, metric.WithAttributes(attribute.String(AttrKelvranCacheLayer, layer)))
}

// cacheLookupCounter records every cache lookup outcome (hit/miss), per
// docs/upgrade-research/cache-cost-observability-2026-09-11.md Finding 1:
// kelvran.cache.hit was previously only a per-request span attribute,
// with no queryable aggregate an operator's own Prometheus/Grafana could
// sum a hit-rate from directly (every vendor surveyed by that research
// queries a hit/miss ratio from raw counters at read time -- none ships
// a pre-computed hit-rate% metric).
var cacheLookupCounter = mustInt64Counter(
	meter,
	"kelvran.cache.lookup",
	metric.WithDescription("Cache lookup outcomes (hit/miss), by layer for a hit."),
	metric.WithUnit("{lookup}"),
)

// RecordCacheLookup increments cacheLookupCounter for every finalized
// request, hit or miss -- unconditional, mirroring AttrKelvranCacheHit's
// own "false is a real value, not unknown" span-attribute convention
// (result.go). layer is only attributed when hit is true; a miss never
// carries a fabricated empty-string layer, matching this package's
// existing "skip the attribute rather than write a placeholder"
// convention (e.g. RecordCacheSavings, CacheSimilarity).
func RecordCacheLookup(ctx context.Context, layer string, hit bool) {
	outcome := "miss"
	if hit {
		outcome = "hit"
	}
	attrs := []attribute.KeyValue{
		attribute.String(AttrKelvranCacheLookupOutcome, outcome),
		attribute.String(AttrKelvranInstanceID, InstanceID),
	}
	if hit {
		attrs = append(attrs, attribute.String(AttrKelvranCacheLayer, layer))
	}
	cacheLookupCounter.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// llmSpendCounter is the real USD cost of every genuine, unshared
// upstream call this specific request itself paid for -- the
// counterpart cost-observability research needs to compute a
// savings-as-percent-of-spend dashboard ratio: kelvran.cache.savings_usd
// already existed, but the research's own example panel referenced a
// spend counter that did not (cost was only a per-request span attribute
// string, kelvran.cost.usd, never an aggregatable counter).
var llmSpendCounter = mustFloat64Counter(
	meter,
	"kelvran.llm.spend_usd",
	metric.WithDescription("Real USD cost of billable upstream LLM calls."),
	metric.WithUnit("{USD}"),
)

// RecordLLMSpend increments llmSpendCounter by spendUSD. Callers must
// only call this when the request was genuinely billable (see
// ChatCompletionResult.Billable's own doc comment) -- mirrors
// RecordChatCompletionMetrics's own gen_ai.client.token.usage gating, so
// a cache hit or coalesced singleflight follower never replays another
// call's already-recorded spend a second time.
func RecordLLMSpend(ctx context.Context, spendUSD float64) {
	llmSpendCounter.Add(ctx, spendUSD)
}

// budgetThresholdCrossedCounter counts each NEW crossing of
// dataplane.checkBudgetAlertLadder's fixed 50/75/90/100%-of-cap ladder,
// per docs/upgrade-research/cost-intelligence-finops-2026-09-14.md:
// checkBudgetWarnThreshold's existing single, per-tenant-configurable
// warn percent is a log line only, re-logging on every request past
// threshold by design — this is a separate, aggregate-queryable "how
// close to its cap is this key" signal, mirroring
// rateLimitFailOpenCounter's own "make a discrete event alertable in
// aggregate, not just visible in logs" precedent.
var budgetThresholdCrossedCounter = mustInt64Counter(
	meter,
	"kelvran.budget.threshold_crossed",
	metric.WithDescription("Count of newly-crossed budget-cap threshold-ladder buckets (50/75/90/100%), once per key per rolling-window epoch per bucket."),
	metric.WithUnit("{crossing}"),
)

// RecordBudgetThresholdCrossed increments the counter for keyID at
// percentBucket (one of budget.BudgetAlertBuckets). Callers must only call
// this on a genuinely NEW crossing — dataplane.checkBudgetAlertLadder's
// own dedup, via budget.Tracker.CheckAndMarkBudgetAlertBucket, already
// guarantees this — never on every request past an already-alerted
// threshold, unlike checkBudgetWarnThreshold's own log line.
func RecordBudgetThresholdCrossed(ctx context.Context, keyID string, percentBucket float64) {
	budgetThresholdCrossedCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrKelvranVirtualKeyID, keyID),
		attribute.Float64(AttrKelvranBudgetPercentBucket, percentBucket),
	))
}

// persistenceFailedCounter is per
// docs/upgrade-research/admin-operator-experience-2026-09-14.md: the
// Warn-log-only call sites (six when this was written, eight today:
// budget_persist_failed x5, identity_persist_failed x2, across
// internal/budget and gateway/dataplane, plus -- since 2026-10-08 -- the
// Redis-mode Reconcile's budget_redis_backend_unavailable op=reconcile,
// whose lost write locks the key at its cap rather than merely losing
// restart durability; each one records this counter first) had no
// paired metric at all — a durable-store write
// failure was invisible to any dashboard/alert that doesn't tail logs.
// One shared counter, not one per store kind — see
// AttrKelvranPersistenceStoreKind's own doc comment.
var persistenceFailedCounter = mustInt64Counter(
	meter,
	"kelvran.persistence.failed",
	metric.WithDescription("Durable-store writes that failed (budget or identity), by store kind."),
	metric.WithUnit("{write}"),
)

// RecordPersistenceFailed increments the persistence-failure counter for
// storeKind ("budget" or "identity") and keyID. Callers pair this
// counter-then-log at each existing Warn-log call site (the more recent
// of this codebase's two established orderings), never as a replacement
// for that log line -- the log carries the actual error detail this
// counter deliberately does not (a raw error string is unbounded-
// cardinality and not a meaningful metric attribute).
func RecordPersistenceFailed(ctx context.Context, storeKind, keyID string) {
	persistenceFailedCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrKelvranPersistenceStoreKind, storeKind),
		attribute.String(AttrKelvranVirtualKeyID, keyID),
	))
}

// configPropagationSubscribeStoppedCounter is a real gap an end-to-end
// audit found: cmd/gateway's config-propagation subscriber goroutine had
// a Warn-log-only call site (configpropagation_subscribe_stopped) for
// its own Subscribe loop returning a non-context-cancellation error, with
// no paired metric at all -- exactly the class RecordPersistenceFailed
// above already closed for the two older persistence call sites. In
// practice this should be rare: go-redis v9's own PubSub.Channel()
// backing goroutine retries forever on any Receive error except an
// explicit Close()-set sentinel, and a background ~3s health-check ping
// silently reconnects on failure, so a real network partition should
// never reach this call site at all (see
// gateway/internal/configpropagation's own TestSubscribeSurvivesA-
// RealRedisPartitionAndDeliversEventsAfterRecovery, which proves this
// directly against a real, paused Redis container) -- but "rare" is
// exactly the case a dashboard/alert needs a queryable signal for, not
// just a log line an operator has to already be tailing.
var configPropagationSubscribeStoppedCounter = mustInt64Counter(
	meter,
	"kelvran.configpropagation.subscribe_stopped",
	metric.WithDescription("Config-propagation Subscribe loops that returned a non-context-cancellation error, by instance."),
	metric.WithUnit("{event}"),
)

// RecordConfigPropagationSubscribeStopped increments the subscribe-
// stopped counter for this instance. The caller (cmd/gateway's
// subscriber goroutine) calls this at the exact same point it already
// logs configpropagation_subscribe_stopped -- an additional, aggregate-
// friendly signal, not a replacement for that log line, mirroring
// RecordRateLimitFailOpen's own identical convention.
func RecordConfigPropagationSubscribeStopped(ctx context.Context) {
	configPropagationSubscribeStoppedCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrKelvranInstanceID, InstanceID),
	))
}

// configPropagationPublishFailedCounter is the publish-side twin of
// configPropagationSubscribeStoppedCounter, added 2026-10-08 while
// writing docs/operations/FAILURE-MODES.md. The dataplane's three publish
// sites (virtual-key upsert, virtual-key delete, deployment weight) logged
// configpropagation_publish_failed and nothing else, so a Redis outage
// that left every OTHER replica stale -- the local mutation applied and
// the admin caller got its success -- was invisible to any dashboard or
// alert. Attributed by instance and by the closed set of event types,
// never by key or deployment name; the paired log line carries those.
var configPropagationPublishFailedCounter = mustInt64Counter(
	meter,
	"kelvran.configpropagation.publish_failed",
	metric.WithDescription("Config-propagation mutation events this instance applied locally but failed to publish to the other replicas, by event type."),
	metric.WithUnit("{event}"),
)

// RecordConfigPropagationPublishFailed increments the publish-failed
// counter for eventType (one of configpropagation's Type* constants).
// Callers record counter-then-log at the existing
// configpropagation_publish_failed Warn sites, never instead of them --
// the log line carries the key id or deployment name and the error text.
func RecordConfigPropagationPublishFailed(ctx context.Context, eventType string) {
	configPropagationPublishFailedCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrKelvranInstanceID, InstanceID),
		attribute.String(AttrKelvranConfigPropagationEventType, eventType),
	))
}

// Config selects how spans are exported.
type Config struct {
	// Exporter is "stdout", "otlp", or "none". "" defaults to "stdout" —
	// applied here, not in controlplane, since this is an operational
	// default, not a config-shape concern.
	Exporter string
	// OTLPEndpoint is read only when Exporter == "otlp".
	OTLPEndpoint string
}

// Init sets the global TracerProvider AND MeterProvider (per cfg, sharing
// the same exporter-kind switch and Resource — per
// docs/rfcs/2026-09-05-gateway-ratelimit-fail-open-metric.md, the first
// metrics pipeline in this codebase, deliberately mirroring the
// tracing pipeline's own 3-exporter design rather than inventing a
// different shape for a second signal type) and the global
// TextMapPropagator (a composite of W3C TraceContext + Baggage,
// unconditionally — extraction should work regardless of exporter
// choice, since agent_run_id needs to be readable even when tracing
// itself is off). Returns a shutdown func the caller should flush on
// graceful exit, which shuts down both providers.
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	exporterKind := cfg.Exporter
	if exporterKind == "" {
		exporterKind = "stdout"
	}

	if exporterKind == "none" {
		return func(context.Context) error { return nil }, nil
	}

	res, err := resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", "kelvran-gateway"),
		// service.instance.id is the standard OTel semantic-conventions
		// resource attribute for exactly this purpose — set here so every
		// exported span/metric automatically carries it, per
		// docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md.
		attribute.String("service.instance.id", InstanceID),
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry: building resource: %w", err)
	}

	var traceExporter sdktrace.SpanExporter
	var metricExporter sdkmetric.Exporter
	switch exporterKind {
	case "stdout":
		traceExporter, err = stdouttrace.New()
		if err != nil {
			return nil, fmt.Errorf("telemetry: constructing stdout trace exporter: %w", err)
		}
		metricExporter, err = stdoutmetric.New()
		if err != nil {
			return nil, fmt.Errorf("telemetry: constructing stdout metric exporter: %w", err)
		}
	case "otlp":
		traceExporter, err = otlptracehttp.New(ctx, otlptracehttp.WithEndpoint(cfg.OTLPEndpoint))
		if err != nil {
			return nil, fmt.Errorf("telemetry: constructing OTLP trace exporter: %w", err)
		}
		metricExporter, err = otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpoint(cfg.OTLPEndpoint))
		if err != nil {
			return nil, fmt.Errorf("telemetry: constructing OTLP metric exporter: %w", err)
		}
	default:
		return nil, fmt.Errorf("telemetry: unknown exporter %q (want \"stdout\", \"otlp\", or \"none\")", cfg.Exporter)
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tracerProvider)

	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(meterProvider)

	return func(shutdownCtx context.Context) error {
		// A failed final metrics flush is treated as non-fatal — losing the
		// last few seconds of counter data on shutdown is an accepted,
		// industry-standard tradeoff for an aggregate signal (unlike a
		// span, which represents a discrete, non-repeatable event this
		// codebase treats as more precious), and mirrors the same
		// fail-open posture already used at dataplane.checkRateLimit — the
		// exact path rateLimitFailOpenCounter instruments. Unlike traces
		// (whose BatchSpanProcessor has nothing queued to flush when no
		// spans were ever recorded), the metrics SDK's PeriodicReader
		// unconditionally attempts one last collect+export on Shutdown, so
		// this can legitimately fail whenever the configured OTLP
		// endpoint isn't reachable — never fatal to the caller's own
		// shutdown sequence.
		if err := meterProvider.Shutdown(shutdownCtx); err != nil {
			slog.WarnContext(shutdownCtx, "telemetry_metrics_shutdown_flush_failed", "error", err)
		}
		return tracerProvider.Shutdown(shutdownCtx)
	}, nil
}

// ExtractContext returns a copy of ctx carrying any W3C trace context and
// Baggage present in r's headers, via the global TextMapPropagator Init
// installed. Callers pass the returned context into the dataplane
// pipeline so a caller's own trace (if any) becomes the parent of
// Kelvran's span, and agent_run_id (carried as a Baggage member) becomes
// readable via AgentRunIDFromContext.
func ExtractContext(ctx context.Context, r *http.Request) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(r.Header))
}

// AgentRunIDFromContext extracts the "agent_run_id" Baggage member from
// ctx, per docs/operations/TELEMETRY.md's design. Returns "" if absent —
// never fabricated.
func AgentRunIDFromContext(ctx context.Context) string {
	return baggage.FromContext(ctx).Member("agent_run_id").Value()
}
