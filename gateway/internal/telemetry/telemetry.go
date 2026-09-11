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

// RecordRateLimitFailOpen increments the fail-open counter for keyID. The
// caller (dataplane.checkRateLimit) calls this at the exact same point it
// already logs a rate_limit_backend_unavailable warning — this is an
// additional, aggregate-friendly signal, not a replacement for that log
// line.
func RecordRateLimitFailOpen(ctx context.Context, keyID string) {
	rateLimitFailOpenCounter.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrKelvranVirtualKeyID, keyID)))
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

// tokenUsageHistogram and operationDurationHistogram are the OTel GenAI
// semantic-conventions instruments named in
// docs/upgrade-research/gateway-2026-09-06.md Finding 3:
// gen_ai.client.token.usage (Histogram, unit "{token}") and
// gen_ai.client.operation.duration (Histogram, unit "s") — additive to
// docs/rfcs/2026-09-05-gateway-ratelimit-fail-open-metric.md, which stood
// up this codebase's only prior instrument and explicitly named this as
// the *first* metric, not the only one. Both instrument names are the
// spec's own canonical strings, not a kelvran.*-namespaced equivalent —
// deliberately, so a GenAI-aware dashboard (e.g. Envoy AI Gateway's
// published Grafana dashboard, the concrete precedent this Finding
// cites) can query them without any Kelvran-specific translation. See
// gen_ai.client.token.usage's own "Development," not "Stable," spec
// stability badge, called out as a documented risk in
// docs/rfcs/2026-09-07-gateway-genai-metrics.md, not a reason to wait —
// this codebase already has precedent (gen_ai.* trace attributes, per
// docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md) for building
// against development-stability OTel GenAI conventions.
var tokenUsageHistogram = mustFloat64Histogram(
	meter,
	"gen_ai.client.token.usage",
	metric.WithDescription("Number of input and output tokens used by this GenAI client operation."),
	metric.WithUnit("{token}"),
)

var operationDurationHistogram = mustFloat64Histogram(
	meter,
	"gen_ai.client.operation.duration",
	metric.WithDescription("Duration of a GenAI client operation, from the gateway's own request boundary."),
	metric.WithUnit("s"),
)

// GenAI token-type attribute values, per the semantic-conventions spec's
// gen_ai.token.type enum — the two used by RecordChatCompletionMetrics.
const (
	GenAITokenTypeInput  = "input"
	GenAITokenTypeOutput = "output"
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
// gen_ai.client.token.usage is recorded only when r.Billable — a cache
// hit (any layer) or a coalesced singleflight follower replays token
// counts from a real upstream call this specific request itself never
// made, per docs/rfcs/2026-09-05-gateway-cost-double-counting.md's
// billable gate (already used identically by budget.Record and
// limiter.RecordTokens). Unlike AttrKelvranCostUSD/InputTokens on the
// span — a single per-request attribute, safe to report as "what this
// would have cost" even on a cache hit — a histogram accumulates across
// many requests; replaying the same cached token count on every
// subsequent hit would inflate an aggregate token-throughput query by
// however many times that entry was served, not just report it once.
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
	// Each token-type data point gets its own copy of attrs, rather than
	// two successive append(attrs, ...) calls sharing attrs's backing
	// array — a real, if benign here (each Record call fully consumes its
	// slice before the next append runs), footgun not worth relying on.
	if r.InputTokens > 0 {
		inputAttrs := append(append([]attribute.KeyValue{}, attrs...), attribute.String(AttrGenAITokenType, GenAITokenTypeInput))
		tokenUsageHistogram.Record(ctx, float64(r.InputTokens), metric.WithAttributes(inputAttrs...))
	}
	if r.OutputTokens > 0 {
		outputAttrs := append(append([]attribute.KeyValue{}, attrs...), attribute.String(AttrGenAITokenType, GenAITokenTypeOutput))
		tokenUsageHistogram.Record(ctx, float64(r.OutputTokens), metric.WithAttributes(outputAttrs...))
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
