package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
)

func TestInitStdoutExporterShutsDownCleanly(t *testing.T) {
	shutdown, err := Init(context.Background(), Config{Exporter: "stdout"})
	if err != nil {
		t.Fatalf("Init(stdout): %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown(): %v", err)
	}
}

func TestInitDefaultsEmptyExporterToStdout(t *testing.T) {
	shutdown, err := Init(context.Background(), Config{})
	if err != nil {
		t.Fatalf("Init(\"\"): %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown(): %v", err)
	}
}

func TestInitOTLPExporterConstructsWithoutDialing(t *testing.T) {
	// otlptracehttp.New is non-blocking (it doesn't dial the endpoint at
	// construction time), so this doesn't need a real OTLP collector
	// running to prove Init wires it up without error.
	shutdown, err := Init(context.Background(), Config{Exporter: "otlp", OTLPEndpoint: "localhost:4318"})
	if err != nil {
		t.Fatalf("Init(otlp): %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown(): %v", err)
	}
}

func TestInitNoneExporterIsANoOp(t *testing.T) {
	shutdown, err := Init(context.Background(), Config{Exporter: "none"})
	if err != nil {
		t.Fatalf("Init(none): %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown(): %v", err)
	}
}

func TestInitRejectsUnknownExporter(t *testing.T) {
	_, err := Init(context.Background(), Config{Exporter: "carrier-pigeon"})
	if err == nil {
		t.Fatal("Init with an unknown exporter returned nil error, want an error")
	}
}

func TestExtractContextRoundTripsAgentRunID(t *testing.T) {
	// Set the composite propagator explicitly (rather than depending on a
	// prior Init call in this test file's execution order) so this test
	// is self-contained regardless of test run order.
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})

	member, err := baggage.NewMember("agent_run_id", "run-abc123")
	if err != nil {
		t.Fatalf("baggage.NewMember: %v", err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatalf("baggage.New: %v", err)
	}
	injectCtx := baggage.ContextWithBaggage(context.Background(), bag)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	prop.Inject(injectCtx, propagation.HeaderCarrier(req.Header))

	if req.Header.Get("baggage") == "" {
		t.Fatal("Inject did not set a baggage header — test setup is broken")
	}

	extracted := prop.Extract(context.Background(), propagation.HeaderCarrier(req.Header))
	got := AgentRunIDFromContext(extracted)
	if got != "run-abc123" {
		t.Errorf("AgentRunIDFromContext after round trip = %q, want %q", got, "run-abc123")
	}
}

func TestAgentRunIDFromContextEmptyWhenAbsent(t *testing.T) {
	if got := AgentRunIDFromContext(context.Background()); got != "" {
		t.Errorf("AgentRunIDFromContext on a bare context = %q, want empty", got)
	}
}
