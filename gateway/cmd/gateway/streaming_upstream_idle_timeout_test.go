// This file is package main for the same reason integration_test.go is
// (see that file's own doc comment): it drives the real
// chatCompletionsHandler, the way only an in-package test can without
// exporting it.
//
// These two tests prove the real fix for the gap the routing-chaos
// regression corpus found and named (evals/tests/fixtures/
// regression_corpus_routing_chaos.json's "chaos-streaming-no-upstream-
// timeout-gap" case, and the matching docs/agents/LOGS.md entry): before
// dataplane.NewHTTPUpstreamStreamCaller grew an idle-timeout parameter,
// a stalled streaming upstream — whether it never responded at all, or
// went silent mid-stream after already flushing real chunks to the
// client — hung the gateway's own call to it indefinitely, bounded only
// by the ORIGINAL inbound client giving up and canceling its own request
// context. Both tests here build the pipeline directly (not via
// buildPipeline, which always wires production's 60s idle window) so a
// short one can be injected — the whole point of these tests is to
// observe a bounded failure without waiting out a real 60 seconds.
package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/router"
)

// testStreamIdleTimeout is deliberately far shorter than production's
// real streamIdleTimeout (60s, main.go) — these tests only need to prove
// the bound is real and finite, not exercise the production value
// itself.
const testStreamIdleTimeout = 150 * time.Millisecond

// newStreamingIdleTimeoutTestServer builds a real gateway HTTP server —
// the exact same dataplane.Pipeline + chatCompletionsHandler wiring
// buildPipeline produces — but constructs the Pipeline directly so
// UpstreamStream can be given testStreamIdleTimeout instead of
// production's 60s. Deliberately mirrors newIntegrationServer's shape
// (same real components: identity, in-memory rate limiter, in-process
// caches, real guardrail engine, real openai adapter, real
// weighted-round-robin router, real cost accounting) with one field
// swapped.
func newStreamingIdleTimeoutTestServer(t *testing.T, upstreamURL string) *httptest.Server {
	t.Helper()

	dep := dataplane.Deployment{
		Name:          "gpt4o-primary",
		Model:         "gpt-4o",
		Provider:      "openai",
		UpstreamModel: "gpt-4o",
		BaseURL:       upstreamURL,
	}
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testKeyHash("test-gateway-key"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	keyConfigs := []ratelimit.KeyConfig{{ID: "test-key", Capacity: 100, RefillPerSecond: 100}}
	depRouter := router.New([]router.Deployment{{Name: dep.Name, Model: dep.Model}}, router.HealthConfig{})

	pipeline, err := dataplane.NewPipeline(dataplane.Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigs),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         depRouter,
		Deployments:    []dataplane.Deployment{dep},
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(_ context.Context, _ dataplane.Deployment, _ any) (any, error) {
			t.Fatal("non-streaming Upstream should never be called by a streaming idle-timeout test")
			return nil, nil
		},
		UpstreamStream: dataplane.NewHTTPUpstreamStreamCaller(&http.Client{}, testStreamIdleTimeout),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler(pipeline))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestIntegrationStreamingUpstreamNeverRespondsIsBoundedByIdleTimeout
// drives a real end-to-end streaming request against a mock upstream
// that never writes a single byte — not even response headers — and
// blocks until its own request context is canceled. Before this fix,
// dataplane.NewHTTPUpstreamStreamCaller's client.Do call for this
// scenario would have blocked forever (no client.Timeout, no context
// deadline anywhere in streaming.go); this proves it now returns a real,
// classifiable error — and the whole HTTP round trip completes — well
// within testStreamIdleTimeout's own small multiple, not the test's
// outer safety-valve deadline.
func TestIntegrationStreamingUpstreamNeverRespondsIsBoundedByIdleTimeout(t *testing.T) {
	// Closed by t.Cleanup below, NOT by r.Context().Done() — net/http's
	// server only starts the background read that detects an early
	// client disconnect once a request's body has been fully drained (see
	// server.go's requestBodyRemains/registerOnHitEOF), and this handler
	// deliberately never reads r.Body (it never sends the first byte back
	// FOR ANY reason, including having looked at the request). Relying on
	// r.Context().Done() here would make httptest.Server.Close hang
	// waiting for a connection this handler would otherwise never let go
	// of — an artifact of the mock, unrelated to the real fix under test.
	unblock := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never write anything at all — block until test cleanup, exactly
		// modeling a stalled-before-first-byte upstream that this test's
		// own client-side assertions must not have to wait for.
		<-unblock
	}))
	// t.Cleanup runs LIFO — registering Close FIRST and the unblock
	// second means unblock fires FIRST (letting the handler goroutine
	// return), and only THEN does upstream.Close wait for it, rather than
	// the two racing against each other.
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { close(unblock) })

	gw := newStreamingIdleTimeoutTestServer(t, upstream.URL)

	reqBody := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"will the upstream ever answer"}]}`
	httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer test-gateway-key")
	httpReq.Header.Set("Content-Type", "application/json")

	// A generous outer safety valve — NOT the assertion itself. If this
	// fix regressed back to hanging indefinitely, this bounds the test
	// run itself to a finite time instead of hanging `go test` forever.
	client := &http.Client{Timeout: 15 * time.Second}

	start := time.Now()
	resp, err := client.Do(httpReq)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Do: %v (elapsed %s)", err, elapsed)
	}
	defer func() { _ = resp.Body.Close() }()

	// The real assertion: bounded by a small multiple of
	// testStreamIdleTimeout, not by the 15s safety valve above — proving
	// the gateway's OWN idle timeout fired, not the test's last resort.
	if elapsed > 5*time.Second {
		t.Errorf("request took %s, want well under 5s (testStreamIdleTimeout=%s) — looks like it hung rather than being bounded by the idle timeout", elapsed, testStreamIdleTimeout)
	}

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d (a generic upstream error before any chunk reached the client); body: %s", resp.StatusCode, http.StatusBadGateway, body)
	}
}

// TestIntegrationStreamingUpstreamStallsMidStreamIsBoundedByIdleTimeout
// drives a real end-to-end streaming request against a mock upstream
// that DOES respond and DOES flush real SSE frames — then goes silent
// forever mid-stream. Before this fix, the gateway would have already
// committed a 200 response and started forwarding chunks to the client,
// then hung forever waiting for more (again, no bound anywhere in
// streaming.go); this proves the client-facing connection now still
// terminates within bounded time once the gateway's own idle timeout
// fires on the stalled upstream Read, rather than staying open forever.
func TestIntegrationStreamingUpstreamStallsMidStreamIsBoundedByIdleTimeout(t *testing.T) {
	// See the sibling "never responds" test's identical comment: blocking
	// on r.Context().Done() here would make httptest.Server.Close hang,
	// since net/http's server never starts detecting an early client
	// disconnect on a connection whose request body this handler never
	// drains. An explicit, test-cleanup-closed channel sidesteps that
	// mock-only artifact entirely.
	unblock := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "ResponseWriter does not support flushing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-stall-test\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
		flusher.Flush()
		// Real chunk delivered — now go silent forever, exactly modeling
		// a mid-stream stall.
		<-unblock
	}))
	// LIFO order — see the sibling test's identical comment for why
	// unblock must close BEFORE upstream.Close waits on it.
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { close(unblock) })

	gw := newStreamingIdleTimeoutTestServer(t, upstream.URL)

	reqBody := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"will the stream keep going forever"}]}`
	httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer test-gateway-key")
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second} // outer safety valve, not the assertion

	resp, err := client.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d (the first real chunk already committed this); body: %s", resp.StatusCode, http.StatusOK, body)
	}

	start := time.Now()
	// readErr is deliberately not asserted either way: once the gateway's
	// own idle timeout fires on the stalled upstream Read, it terminates
	// the client-facing chunked response cleanly (writeErrorResponse's
	// diagnostic text plus a normal chunked-encoding end, per
	// handleStreamingChatCompletion's own doc comment on this exact
	// "failure after the first chunk" case) — a nil error here is the
	// EXPECTED, correct outcome, not a sign the bug is still present. The
	// bug this test guards against is a body Read that never returns at
	// all; whether it returns with or without an error once it does is
	// not what's under test.
	body, _ := io.ReadAll(resp.Body)
	elapsed := time.Since(start)

	// The real assertion: reading the body — which is blocked on the
	// stalled upstream until the gateway's own idle timeout aborts that
	// call — completes within a small multiple of testStreamIdleTimeout
	// rather than hanging until the 15s safety valve (or forever, pre-fix).
	if elapsed > 5*time.Second {
		t.Errorf("reading the response body took %s, want well under 5s (testStreamIdleTimeout=%s) — looks like it hung rather than being bounded by the idle timeout", elapsed, testStreamIdleTimeout)
	}
	if !strings.Contains(string(body), "partial") {
		t.Errorf("response body missing the one real chunk the mock upstream sent before stalling: %s", body)
	}
}
