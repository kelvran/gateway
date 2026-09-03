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
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
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

// Config selects how spans are exported.
type Config struct {
	// Exporter is "stdout", "otlp", or "none". "" defaults to "stdout" —
	// applied here, not in controlplane, since this is an operational
	// default, not a config-shape concern.
	Exporter string
	// OTLPEndpoint is read only when Exporter == "otlp".
	OTLPEndpoint string
}

// Init sets the global TracerProvider (per cfg) and the global
// TextMapPropagator (a composite of W3C TraceContext + Baggage,
// unconditionally — extraction should work regardless of exporter
// choice, since agent_run_id needs to be readable even when tracing
// itself is off). Returns a shutdown func the caller should flush on
// graceful exit.
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
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry: building resource: %w", err)
	}

	var exporter sdktrace.SpanExporter
	switch exporterKind {
	case "stdout":
		exporter, err = stdouttrace.New()
		if err != nil {
			return nil, fmt.Errorf("telemetry: constructing stdout exporter: %w", err)
		}
	case "otlp":
		exporter, err = otlptracehttp.New(ctx, otlptracehttp.WithEndpoint(cfg.OTLPEndpoint))
		if err != nil {
			return nil, fmt.Errorf("telemetry: constructing OTLP exporter: %w", err)
		}
	default:
		return nil, fmt.Errorf("telemetry: unknown exporter %q (want \"stdout\", \"otlp\", or \"none\")", cfg.Exporter)
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tracerProvider)

	return tracerProvider.Shutdown, nil
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
