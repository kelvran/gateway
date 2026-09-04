package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// spanRecorder is installed exactly once for this test binary, in
// TestMain — never per-test. See
// gateway/internal/gateway/dataplane/telemetry_wiring_test.go's own
// TestMain for the full explanation: go.opentelemetry.io/otel's global
// TracerProvider delegate only meaningfully redirects a given Tracer
// handle ONCE, verified empirically, so installing the real provider
// before any test's first span (not per-test) is the pattern that works.
// No test in this package ever calls run() (which would call
// telemetry.Init itself), so there is no second SetTracerProvider call to
// conflict with this one.
var spanRecorder *tracetest.SpanRecorder

func TestMain(m *testing.M) {
	spanRecorder = tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	otel.SetTracerProvider(tp)
	// telemetry.Init would normally set this too, but no test in this
	// package calls run() (the only caller of Init) — without this, the
	// global propagator stays the SDK's no-op default and
	// telemetry.ExtractContext silently extracts nothing, regardless of
	// what headers a request actually carries. This is exactly what a
	// real production process gets for free from Init at startup; this
	// test binary has to set it up itself since it never calls Init.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	os.Exit(m.Run())
}

// TestIntegrationAgentRunIDPropagatesFromBaggageHeaderToSpan proves the
// FULL real path per docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md:
// a real HTTP client sends a standard W3C "baggage" header carrying
// agent_run_id through the real gateway HTTP server, and the resulting
// span (recorded via the shared TracerProvider above) carries it as the
// kelvran.agent_run_id attribute — not just at the dataplane-unit level
// (already proven in internal/gateway/dataplane's own tests), but through
// chatCompletionsHandler's real telemetry.ExtractContext call too.
func TestIntegrationAgentRunIDPropagatesFromBaggageHeaderToSpan(t *testing.T) {
	before := len(spanRecorder.Ended())

	upstream, _ := newMockUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_L")

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"trace me"}]}`
	httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer test-gateway-key")
	httpReq.Header.Set("baggage", "agent_run_id=run-integration-001")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	ended := spanRecorder.Ended()
	spans := ended[before:]
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}

	var found bool
	for _, kv := range spans[0].Attributes() {
		if string(kv.Key) == telemetry.AttrKelvranAgentRunID {
			found = true
			if kv.Value.AsString() != "run-integration-001" {
				t.Errorf("%s = %q, want %q", telemetry.AttrKelvranAgentRunID, kv.Value.AsString(), "run-integration-001")
			}
		}
	}
	if !found {
		t.Errorf("%s not set on the span at all — real baggage-header propagation is broken", telemetry.AttrKelvranAgentRunID)
	}
}

// TestIntegrationOtelHTTPMiddlewareNestsGenAISpanAsChild proves
// wrapHTTPServerSpan's real effect end to end: a real HTTP request
// through a server built exactly the way run() builds one (mux wrapped
// via wrapHTTPServerSpan, unlike newIntegrationServer's bare mux) produces
// TWO spans sharing one trace — the outer, generic otelhttp SERVER span
// and the existing per-request GenAI span nested inside it as a real
// child (proven via TraceID equality and the inner span's ParentSpanID
// matching the outer span's own SpanID, not just "2 spans exist").
func TestIntegrationOtelHTTPMiddlewareNestsGenAISpanAsChild(t *testing.T) {
	before := len(spanRecorder.Ended())

	upstream, _ := newMockUpstream(t)
	gw := newIntegrationServerWithOtelHTTP(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_OTELHTTP")

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"trace me through otelhttp too"}]}`
	httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer test-gateway-key")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	ended := spanRecorder.Ended()
	spans := ended[before:]
	if len(spans) != 2 {
		t.Fatalf("len(spans) = %d, want 2 (the otelhttp server span plus the GenAI span nested inside it)", len(spans))
	}

	// otelhttp ends its own span only after the handler returns, so it
	// ends AFTER the inner GenAI span — spans[0] is the inner one,
	// spans[1] is the outer otelhttp one.
	inner, outer := spans[0], spans[1]

	if outer.SpanKind() != trace.SpanKindServer {
		t.Errorf("outer span kind = %v, want SpanKindServer", outer.SpanKind())
	}
	if inner.SpanContext().TraceID() != outer.SpanContext().TraceID() {
		t.Error("inner and outer spans do not share a TraceID — otelhttp's context is not reaching chatCompletionsHandler")
	}
	if inner.Parent().SpanID() != outer.SpanContext().SpanID() {
		t.Error("inner span's parent is not the outer otelhttp span — nesting is broken, not just coincidentally sharing a trace")
	}
}

// newIntegrationServerWithOtelHTTP mirrors newIntegrationServer's exact
// pipeline/config construction, but wraps the mux with wrapHTTPServerSpan
// — exactly what run() does — since newIntegrationServer deliberately
// does not, to keep every other integration test's span-count assertions
// (e.g. the "want 1" check above) unaffected by this addition.
func newIntegrationServerWithOtelHTTP(t *testing.T, upstreamURL, gatewayKey, upstreamKeyEnvVar string) *httptest.Server {
	t.Helper()
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")

	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash(gatewayKey), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "gpt4o-primary",
				Model:         "gpt-4o",
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       upstreamURL,
				APIKeyEnv:     upstreamKeyEnvVar,
			},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"gpt-4o": {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler(pipeline))

	srv := httptest.NewServer(wrapHTTPServerSpan(mux))
	t.Cleanup(srv.Close)
	return srv
}
