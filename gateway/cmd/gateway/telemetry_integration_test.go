package main

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/kelvran/gateway/internal/telemetry"
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
