package telemetry

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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

// TestOTLPExporterRecordingIsFireAndForgetAgainstAnUnresponsiveCollector
// is the empirical proof this package's suite was missing: recording a
// span/metric against a real OTLP exporter must never block the request
// path on the collector actually responding. Points Init at a real TCP
// listener that accepts every connection but never writes a response
// (never even reads the request, so the client-side HTTP round trip
// never completes), then drives many real Tracer.Start/span.End and
// RecordChatCompletionMetrics calls under a short deadline -- proving
// the fire-and-forget property empirically, not by inspecting the
// WithBatcher/PeriodicReader config choice alone.
func TestOTLPExporterRecordingIsFireAndForgetAgainstAnUnresponsiveCollector(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed at test end
			}
			// Deliberately never read from or write to conn -- a
			// collector that accepted the connection but is otherwise
			// completely unresponsive. Tracked and closed only at test
			// end, via t.Cleanup above.
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()

	shutdown, err := Init(context.Background(), Config{Exporter: "otlp", OTLPEndpoint: ln.Addr().String()})
	if err != nil {
		t.Fatalf("Init(otlp): %v", err)
	}
	// A bounded shutdown context, not context.Background(): the OTLP
	// HTTP exporter defaults to TLS, and this test's plain-TCP listener
	// never completes a TLS handshake, so a flush-on-shutdown attempt
	// against it would otherwise hang indefinitely -- this is test
	// cleanup hygiene, not the property under test (which is proven by
	// the done-channel select below, well before shutdown ever runs).
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_ = shutdown(shutdownCtx)
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx := context.Background()
		for i := 0; i < 50; i++ {
			_, span := Tracer.Start(ctx, "fire-and-forget-test-span")
			RecordChatCompletionMetrics(ctx, ChatCompletionResult{
				Provider:      "openai",
				RequestModel:  "fire-and-forget-test-model",
				ResponseModel: "fire-and-forget-test-model",
				Billable:      true,
				InputTokens:   1,
				OutputTokens:  1,
				Duration:      time.Millisecond,
			})
			span.End()
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("50 span/metric recording calls against an unresponsive OTLP collector did not complete within 2s — recording is blocking on the collector, not fire-and-forget")
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
