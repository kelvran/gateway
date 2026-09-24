package dataplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/streaming"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// realOpenAISSEStream is a minimal but genuine OpenAI streaming response:
// role delta, two content deltas, a finish_reason chunk, a usage-only
// chunk (stream_options.include_usage), and the [DONE] sentinel.
const realOpenAISSEStream = "" +
	`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"lo!"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}` + "\n\n" +
	"data: [DONE]\n\n"

type nopCloserReader struct{ io.Reader }

func (nopCloserReader) Close() error { return nil }

func newStreamingTestPipeline(t *testing.T, upstreamStream UpstreamStreamCaller, deployments []Deployment, adapters adapter.Registry) *Pipeline {
	t.Helper()
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if deployments == nil {
		deployments = []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapters,
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream should never be called by a streaming test")
			return nil, nil
		},
		UpstreamStream: upstreamStream,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

func newStreamingTestPipelineWithKeysAndBudget(t *testing.T, upstreamStream UpstreamStreamCaller, deployments []Deployment, adapters adapter.Registry, keys []identity.VirtualKey, tracker *budget.Tracker) *Pipeline {
	t.Helper()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if deployments == nil {
		deployments = []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         tracker,
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapters,
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream should never be called by a streaming test")
			return nil, nil
		},
		UpstreamStream: upstreamStream,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestHandleChatCompletionStreamModelNotAllowedCheckedFirst mirrors
// dataplane_test.go's buffered-path equivalent for the streaming entry
// point: allowed-models must be checked before rate-limit/budget, and
// before ever touching UpstreamStream.
func TestHandleChatCompletionStreamModelNotAllowedCheckedFirst(t *testing.T) {
	keys := []identity.VirtualKey{{
		ID:              "team-x",
		KeyHash:         testHashOf("team-x-secret"),
		RateLimitBurst:  0, // already exhausted
		RateLimitRefill: 0,
		BudgetUSD:       decimal.RequireFromString("0.01"),
		AllowedModels:   map[string]struct{}{"gpt-4o-mini": {}},
	}}
	tracker := budget.NewTracker()
	tracker.Record("team-x", decimal.RequireFromString("999"), 0)

	p := newStreamingTestPipelineWithKeysAndBudget(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		t.Fatal("UpstreamStream must never be called for a model-not-allowed request")
		return nil, nil
	}, nil, adapter.Registry{"openai": openai.New()}, keys, tracker)

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer team-x-secret", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec, "")
	if !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("err = %v, want ErrModelNotAllowed (must be checked before rate-limit/budget)", err)
	}
}

// TestHandleChatCompletionStreamBudgetExceededRejectsBeforeUpstream mirrors
// dataplane_test.go's buffered-path equivalent for the streaming entry
// point.
func TestHandleChatCompletionStreamBudgetExceededRejectsBeforeUpstream(t *testing.T) {
	keys := []identity.VirtualKey{
		{ID: "team-x", KeyHash: testHashOf("team-x-secret"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("0.01")},
	}
	tracker := budget.NewTracker()
	tracker.Record("team-x", decimal.RequireFromString("1.0"), 0)

	p := newStreamingTestPipelineWithKeysAndBudget(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		t.Fatal("UpstreamStream must never be called once budget is exceeded")
		return nil, nil
	}, nil, adapter.Registry{"openai": openai.New()}, keys, tracker)

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer team-x-secret", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec, "")
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
}

// TestHandleChatCompletionStreamCacheIsolatedAcrossVirtualKeys is the
// streaming-path mirror of dataplane_test.go's load-bearing
// TestHandleChatCompletionCacheIsolatedAcrossVirtualKeys: two different
// virtual keys streaming a byte-identical request must each get their own
// cache entry, proven through the real tee-to-accumulator streaming path,
// not just the buffered one.
func TestHandleChatCompletionStreamCacheIsolatedAcrossVirtualKeys(t *testing.T) {
	var upstreamCalls int
	keys := []identity.VirtualKey{
		{ID: "team-alpha", KeyHash: testHashOf("alpha-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		{ID: "team-beta", KeyHash: testHashOf("beta-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	p := newStreamingTestPipelineWithKeysAndBudget(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		upstreamCalls++
		return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
	}, nil, adapter.Registry{"openai": openai.New()}, keys, budget.NewTracker())

	req := adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "identical question"}},
	}

	rec1 := httptest.NewRecorder()
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer alpha-secret", "", "", req, rec1, ""); err != nil {
		t.Fatalf("team-alpha first call: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after team-alpha's first call = %d, want 1", upstreamCalls)
	}

	// team-beta's identical request must be a real cache MISS (a second
	// real UpstreamStream call), not served from team-alpha's cache entry.
	rec2 := httptest.NewRecorder()
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer beta-secret", "", "", req, rec2, ""); err != nil {
		t.Fatalf("team-beta first call: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after team-beta's first (should be a MISS) call = %d, want 2 — cross-tenant cache leakage", upstreamCalls)
	}

	// Each key's own SECOND identical request must now be a fake-streamed
	// cache hit — upstreamCalls stays at 2.
	rec3 := httptest.NewRecorder()
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer alpha-secret", "", "", req, rec3, ""); err != nil {
		t.Fatalf("team-alpha second call: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after team-alpha's second (should be a HIT) call = %d, want still 2", upstreamCalls)
	}
}

func TestHandleChatCompletionStreamCacheMissRealStream(t *testing.T) {
	var upstreamCalls int
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		upstreamCalls++
		return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
	}, nil, adapter.Registry{"openai": openai.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, rec, "")
	if err != nil {
		t.Fatalf("HandleChatCompletionStream: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1", upstreamCalls)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"content":"Hel"`) || !strings.Contains(body, `"content":"lo!"`) {
		t.Errorf("body missing expected content deltas: %s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("body does not end with [DONE] sentinel: %s", body)
	}

	// Second, identical request must be served from cache — upstream call
	// count must stay at 1, and the fake-streamed body must still carry
	// the accumulated content and the correct finish_reason.
	rec2 := httptest.NewRecorder()
	err = p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, rec2, "")
	if err != nil {
		t.Fatalf("second (cache-hit) HandleChatCompletionStream: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after cache-hit call = %d, want still 1", upstreamCalls)
	}
	body2 := rec2.Body.String()
	if !strings.Contains(body2, `"content":"Hello!"`) {
		t.Errorf("fake-streamed cache-hit body missing full accumulated content: %s", body2)
	}
	if !strings.Contains(body2, `"finish_reason":"stop"`) {
		t.Errorf("fake-streamed cache-hit body missing finish_reason: %s", body2)
	}
}

// Uses "bedrock" — per docs/rfcs/2026-09-04-gemini-adapter.md, gemini is
// now a real streaming adapter and is deliberately no longer this test's
// example of a provider that doesn't support streaming.
// nonStreamingAdapter is a minimal adapter.Adapter stand-in for a provider
// that hasn't implemented streaming.StreamingAdapter. Every adapter
// actually registered in cmd/gateway/main.go now streams (openai,
// anthropic, gemini, openaicompat, and bedrock via its own binary-framed
// path) — there is no longer a real, production example of this scenario,
// so this test needs its own deliberately non-streaming fixture to keep
// exercising streamDeployment's generic ErrStreamingNotSupported branch.
type nonStreamingAdapter struct{}

func (nonStreamingAdapter) ToProvider(adapter.ChatRequest) (any, error) { return nil, nil }
func (nonStreamingAdapter) FromProvider(any) (adapter.ChatResponse, error) {
	return adapter.ChatResponse{}, nil
}
func (nonStreamingAdapter) Name() string { return "non-streaming" }

func TestHandleChatCompletionStreamUnsupportedProviderReturnsTypedError(t *testing.T) {
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		t.Fatal("upstream stream should never be called for a provider that doesn't support streaming")
		return nil, nil
	}, []Deployment{{Name: "d1", Model: "claude-legacy", Provider: "legacy-buffered-only", UpstreamModel: "claude-legacy", BaseURL: "http://unused"}},
		adapter.Registry{"legacy-buffered-only": nonStreamingAdapter{}})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{
		Model: "claude-legacy", Stream: true,
	}, rec, "")
	if !errors.Is(err, ErrStreamingNotSupported) {
		t.Fatalf("err = %v, want ErrStreamingNotSupported", err)
	}
}

func TestHandleChatCompletionStreamNotConfiguredOnCacheMiss(t *testing.T) {
	p := newStreamingTestPipeline(t, nil, nil, adapter.Registry{"openai": openai.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec, "")
	if !errors.Is(err, ErrStreamingNotConfigured) {
		t.Fatalf("err = %v, want ErrStreamingNotConfigured", err)
	}
}

func TestHandleChatCompletionStreamFallbackBeforeFirstByte(t *testing.T) {
	var calls []string
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		calls = append(calls, dep.Name)
		if dep.Name == "primary" {
			return nil, errors.New("simulated connection failure before any byte was read")
		}
		return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
	}, []Deployment{
		{Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "secondary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}, adapter.Registry{"openai": openai.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec, "")
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	if len(calls) != 2 || calls[0] != "primary" || calls[1] != "secondary" {
		t.Fatalf("calls = %v, want [primary secondary]", calls)
	}
	if !strings.Contains(rec.Body.String(), `"content":"Hel"`) {
		t.Errorf("body missing content from the fallback deployment's stream: %s", rec.Body.String())
	}
}

// TestHandleChatCompletionStreamMultiHopFallbackChainRoutesByClassAndHop
// is the streaming-path parity proof for
// docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md: the
// streaming path must walk the same error-classified, multi-hop chain
// the buffered path does, not stay on the old single-fallback rule
// forever. "hop-1" is configured but itself also fails before any byte
// reaches the client, so "hop-2" (the SECOND configured hop, not the
// old round-robin's own "next" pick) must be the one that ultimately
// serves the response.
func TestHandleChatCompletionStreamMultiHopFallbackChainRoutesByClassAndHop(t *testing.T) {
	primary := Deployment{
		Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused",
		FallbackChains: map[string][]string{
			FallbackClassContentPolicy: {"hop-1", "hop-2"},
		},
	}
	deployments := []Deployment{
		primary,
		{Name: "hop-1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "hop-2", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}

	var calls []string
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		calls = append(calls, dep.Name)
		switch dep.Name {
		case "primary":
			return nil, &UpstreamHTTPError{StatusCode: 400, Body: "content_policy_violation"}
		case "hop-1":
			return nil, &UpstreamHTTPError{StatusCode: 500, Body: "hop-1 unavailable"}
		default:
			return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
		}
	}, deployments, adapter.Registry{"openai": openai.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec, "")
	if err != nil {
		t.Fatalf("expected the chain to eventually succeed at hop-2, got error: %v", err)
	}
	want := []string{"primary", "hop-1", "hop-2"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i, name := range want {
		if calls[i] != name {
			t.Fatalf("calls = %v, want %v (in exact order)", calls, want)
		}
	}
	if !strings.Contains(rec.Body.String(), `"content":"Hel"`) {
		t.Errorf("body missing content from hop-2's stream: %s", rec.Body.String())
	}
}

// TestHandleChatCompletionStreamMultiHopChainStopsOnceChunkSent proves the
// "no further hop once a chunk reached the client" rule applies to EVERY
// hop of a multi-hop chain, not only the very first attempt: hop-1 (a
// configured, non-first target) sends one real chunk before dying
// mid-stream, so hop-2 — configured, and would otherwise be tried next —
// must never be called.
func TestHandleChatCompletionStreamMultiHopChainStopsOnceChunkSent(t *testing.T) {
	firstChunkEnd := strings.Index(realOpenAISSEStream, "\n\n") + len("\n\n")

	primary := Deployment{
		Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused",
		FallbackChains: map[string][]string{
			FallbackClassGeneric: {"hop-1", "hop-2"},
		},
	}
	deployments := []Deployment{
		primary,
		{Name: "hop-1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "hop-2", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}

	var calls []string
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		calls = append(calls, dep.Name)
		switch dep.Name {
		case "primary":
			return nil, errors.New("simulated connection failure before any byte was read")
		case "hop-1":
			return nopCloserReader{&failAfterNBytesReader{r: strings.NewReader(realOpenAISSEStream), n: firstChunkEnd + 5}}, nil
		default:
			t.Fatalf("hop-2 must never be called once hop-1 already sent a chunk to the client, but was called")
			return nil, nil
		}
	}, deployments, adapter.Registry{"openai": openai.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec, "")
	if err == nil {
		t.Fatal("expected an error from hop-1's mid-stream connection loss, got nil")
	}
	want := []string{"primary", "hop-1"}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("calls = %v, want %v — hop-2 must never run", calls, want)
	}
	if !strings.Contains(rec.Body.String(), `"role":"assistant"`) {
		t.Errorf("body should still contain the chunk hop-1 wrote before failing: %s", rec.Body.String())
	}
}

// failAfterNBytesReader wraps a Reader and returns a real I/O error after
// serving its first n bytes — simulating an upstream connection dying
// mid-stream, after some real content has already reached the client.
type failAfterNBytesReader struct {
	r  *strings.Reader
	n  int
	at int
}

func (f *failAfterNBytesReader) Read(p []byte) (int, error) {
	if f.at >= f.n {
		return 0, errors.New("simulated mid-stream connection loss")
	}
	if len(p) > f.n-f.at {
		p = p[:f.n-f.at]
	}
	n, err := f.r.Read(p)
	f.at += n
	return n, err
}

func TestHandleChatCompletionStreamNoFallbackAfterFirstByte(t *testing.T) {
	var calls []string
	// Fail partway through the FIRST real content chunk's own bytes, so at
	// least one full chunk is guaranteed to have already been decoded and
	// written to the client before the read error surfaces.
	firstChunkEnd := strings.Index(realOpenAISSEStream, "\n\n") + len("\n\n")

	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		calls = append(calls, dep.Name)
		return nopCloserReader{&failAfterNBytesReader{r: strings.NewReader(realOpenAISSEStream), n: firstChunkEnd + 5}}, nil
	}, []Deployment{
		{Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "secondary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}, adapter.Registry{"openai": openai.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec, "")
	if err == nil {
		t.Fatal("expected an error from the mid-stream connection loss, got nil")
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want exactly 1 (no fallback once a chunk reached the client)", calls)
	}
	if !strings.Contains(rec.Body.String(), `"role":"assistant"`) {
		t.Errorf("body should still contain the chunk written before the failure: %s", rec.Body.String())
	}
}

// TestHandleChatCompletionStreamDisconnectAfterRealContentStillBillsIt is
// the direct proof of the client-disconnect billing fix: real content
// delivered to the client before a mid-stream read error (a client
// disconnect is the common real trigger) must be billed at its real,
// estimated cost — never silently discarded to $0 — per
// docs/upgrade-research/request-lifecycle-reliability-2026-09-15.md.
// Unlike TestHandleChatCompletionStreamNoFallbackAfterFirstByte above
// (which deliberately fails BEFORE any real text content is decoded, so
// its own correct bill is $0 and proves nothing about this fix), this
// fixture lets three full content frames (60 real chars total) through
// first.
func TestHandleChatCompletionStreamDisconnectAfterRealContentStillBillsIt(t *testing.T) {
	contentStream := "" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"` + strings.Repeat("a", 20) + `"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"` + strings.Repeat("b", 20) + `"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"` + strings.Repeat("c", 20) + `"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":15,"total_tokens":20}}` + "\n\n" +
		"data: [DONE]\n\n"
	// Fail 5 bytes into the finish_reason frame -- all three 20-char
	// content frames (60 real chars total) are guaranteed fully delivered
	// first; the usage frame is never reached at all.
	thirdContentFrameEnd := strings.LastIndex(contentStream, `"content":"`+strings.Repeat("c", 20))
	thirdContentFrameEnd = strings.Index(contentStream[thirdContentFrameEnd:], "\n\n") + thirdContentFrameEnd + len("\n\n")

	keys := []identity.VirtualKey{{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	tracker := budget.NewTracker()
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := NewPipeline(Config{
		Verifier:    verifier,
		Limiter:     ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:      tracker,
		Cache:       inprocess.New(0),
		CacheL2:     inprocess.New(0),
		CacheL3:     inprocess.NewLexicalCache(0),
		Guardrails:  guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:    adapter.Registry{"openai": openai.New()},
		Router:      testRouter(deployments),
		Deployments: deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{
			"gpt-4o": {CompletionPerToken: decimal.NewFromFloat(0.01)},
		}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream should never be called by this test")
			return nil, nil
		},
		UpstreamStream: func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
			return nopCloserReader{&failAfterNBytesReader{r: strings.NewReader(contentStream), n: thirdContentFrameEnd + 5}}, nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	rec := httptest.NewRecorder()
	streamErr := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, rec, "")
	if streamErr == nil {
		t.Fatal("expected an error from the mid-stream connection loss, got nil")
	}

	// 60 real chars / streamRunawayCharsPerToken(4) = 15 estimated
	// completion tokens * $0.01/token = $0.15 -- real, non-zero, exactly
	// computable. Before this fix, the missing usage frame on this exact
	// error path meant real cost was left at $0, silently discarding 60
	// real, already-delivered, provider-billed characters' worth of
	// content.
	got := tracker.SpentUSD(context.Background(), "test-key", 0)
	want := decimal.NewFromFloat(0.15)
	if !got.Equal(want) {
		t.Errorf("SpentUSD after the disconnect = %s, want %s — real content delivered before a client disconnect must still be billed", got, want)
	}
}

// TestHandleChatCompletionStreamTruncatedResponseNeverCached is the
// streaming-path counterpart to TestHandleChatCompletionTruncatedResponseNeverCached
// (dataplane_test.go) -- live-verified 2026-09-13: unlike L1/L2's own
// cache.Key/NormalizedKey (which already fold in req.MaxTokens),
// L3-lite's MinHashSignature has no MaxTokens dimension at all, so a
// stream that finished with finish_reason:"length" must never populate
// the cache at any layer, mirroring the existing Block-tier-response
// guard a few lines above this one in guardrail_test.go.
func TestHandleChatCompletionStreamTruncatedResponseNeverCached(t *testing.T) {
	var upstreamCalls int
	truncatedStream := "" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}` + "\n\n" +
		"data: [DONE]\n\n"

	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		upstreamCalls++
		return nopCloserReader{strings.NewReader(truncatedStream)}, nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, adapter.Registry{"openai": openai.New()})

	req := adapter.ChatRequest{Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "give me a truncated answer"}}}

	rec1 := httptest.NewRecorder()
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, rec1, ""); err != nil {
		t.Fatalf("first HandleChatCompletionStream: %v", err)
	}

	rec2 := httptest.NewRecorder()
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, rec2, ""); err != nil {
		t.Fatalf("second HandleChatCompletionStream: %v", err)
	}

	if upstreamCalls != 2 {
		t.Errorf("upstreamCalls = %d, want 2 — a truncated (finish_reason:\"length\") streamed response must never populate the cache", upstreamCalls)
	}
}

// TestEstimateOrRealUsageUsesProviderUsageWhenAvailable proves the
// unestimated regression case: when the provider DID send a real usage
// frame, that value is returned verbatim, with estimated=false — this
// fix must never override or alter genuinely-reported usage.
func TestEstimateOrRealUsageUsesProviderUsageWhenAvailable(t *testing.T) {
	req := adapter.ChatRequest{Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	acc := newStreamAccumulator()
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Content: "ignored"}}}})
	real := adapter.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}

	usage, estimated := estimateOrRealUsage(req, acc, &real)
	if estimated {
		t.Error("estimated = true, want false — a real provider usage frame must never be flagged as estimated")
	}
	if usage != real {
		t.Errorf("usage = %+v, want the real usage verbatim (%+v), unaltered by the accumulator's own content", usage, real)
	}
}

// TestEstimateOrRealUsageEstimatesFromAccumulatorWhenUsageIsNil is the
// direct, isolated proof of this fix's core arithmetic: when the
// provider never sent a usage frame, completion tokens are estimated
// from the accumulator's real content length via
// streamRunawayCharsPerToken — never left at zero.
func TestEstimateOrRealUsageEstimatesFromAccumulatorWhenUsageIsNil(t *testing.T) {
	req := adapter.ChatRequest{Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	acc := newStreamAccumulator()
	// 40 chars of real content -> 40/streamRunawayCharsPerToken(4) = 10
	// estimated completion tokens.
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Content: strings.Repeat("x", 40)}}}})

	usage, estimated := estimateOrRealUsage(req, acc, nil)
	if !estimated {
		t.Error("estimated = false, want true — no provider usage frame arrived, so this must be flagged as an estimate")
	}
	if usage.CompletionTokens != 10 {
		t.Errorf("CompletionTokens = %d, want 10 (40 real chars / streamRunawayCharsPerToken)", usage.CompletionTokens)
	}
	if usage.CompletionTokens == 0 {
		t.Error("CompletionTokens = 0 — this is the exact bug this fix closes: real, already-delivered content must never be billed as zero")
	}
	if usage.TotalTokens != usage.PromptTokens+usage.CompletionTokens {
		t.Errorf("TotalTokens = %d, want PromptTokens(%d) + CompletionTokens(%d)", usage.TotalTokens, usage.PromptTokens, usage.CompletionTokens)
	}
}

// TestFinishStreamedResponseLogsWarningOnDuplicateIndexAfterFinish proves
// finishStreamedResponse's own new warning fires through the REAL
// pipeline when a provider sends a real content delta to an index whose
// finish_reason chunk already arrived -- closing the SILENT half of the
// gap streamAccumulator.add's own detection exists to fix (see that
// method's doc comment): the anomaly is now at least visible in logs,
// not just safely-but-quietly folded into the accumulated content.
func TestFinishStreamedResponseLogsWarningOnDuplicateIndexAfterFinish(t *testing.T) {
	const sseWithDuplicateAfterFinish = "" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hello!"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		// A real anomaly: more content for index 0, AFTER it already finished.
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":" again"},"finish_reason":null}]}` + "\n\n" +
		"data: [DONE]\n\n"

	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream should never be called by a streaming test")
			return nil, nil
		},
		UpstreamStream: func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
			return nopCloserReader{strings.NewReader(sseWithDuplicateAfterFinish)}, nil
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	rec := httptest.NewRecorder()
	err = p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, rec, "")
	if err != nil {
		t.Fatalf("HandleChatCompletionStream: %v", err)
	}

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "stream_duplicate_index_after_finish") {
		t.Errorf("expected a stream_duplicate_index_after_finish warning; got log output:\n%s", logOutput)
	}
}

// streamingNearDuplicateCollisionMetricDelta reads
// "kelvran.streaming.near_duplicate_collision" for keyID out of rm — the
// same metricdata.Sum[int64]/dp.Attributes.Value extraction shape
// gatewayevents_test.go's own snapshotDataplaneTelemetry already uses for
// kelvran.ratelimit.fail_open, kept local to this file (rather than
// extending that shared helper) since this test file's own scope is
// deliberately limited to streaming.go's own surface.
func streamingNearDuplicateCollisionMetricDelta(t *testing.T, rm metricdata.ResourceMetrics, keyID string) int64 {
	t.Helper()
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "kelvran.streaming.near_duplicate_collision" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("kelvran.streaming.near_duplicate_collision data type = %T, want metricdata.Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				gotKeyID, hasAttr := dp.Attributes.Value(attribute.Key(telemetry.AttrKelvranVirtualKeyID))
				if hasAttr && gotKeyID.AsString() == keyID {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// TestStreamingInFlightCounterTracksConcurrentRequestsForSameKey is the
// load-bearing proof for streamingInFlightByL1Key itself (dataplane.go):
// n genuinely concurrent, byte-identical streaming requests (same l1Key)
// must all be observed as truly in flight AT THE SAME TIME (the shared
// counter reads exactly n while every one of them is still blocked in its
// own real upstream call), and the map entry must be fully released (n-1
// decrements plus a final delete-on-zero) once every one of them
// completes — mirroring TestHandleChatCompletionCoalescedFollowerDoes-
// NotRechargeBudget's own concurrent-request harness shape
// (cost_double_counting_test.go), the closest existing precedent for
// driving n genuinely concurrent identical requests through this
// pipeline. Also proves n-1 of the n requests recorded a genuine
// collision via the real kelvran.streaming.near_duplicate_collision
// counter (never n, since exactly one request must be first to observe
// an empty slot) — the same metric-delta verification style
// TestRateLimitFailOpenIncrementsMetricCounter (gatewayevents_test.go)
// already established for this codebase's other fail-open-style counter.
func TestStreamingInFlightCounterTracksConcurrentRequestsForSameKey(t *testing.T) {
	const n = 5
	const testKeyID = "streaming-inflight-counter-key"

	reader := dataplaneTelemetryMetricsReaderForTest()
	var before metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &before); err != nil {
		t.Fatalf("reader.Collect (before): %v", err)
	}
	deltaBefore := streamingNearDuplicateCollisionMetricDelta(t, before, testKeyID)

	var upstreamCallsStarted atomic.Int64
	release := make(chan struct{})
	allStarted := make(chan struct{})

	keys := []identity.VirtualKey{
		{ID: testKeyID, KeyHash: testHashOf(testKeyID), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	p := newStreamingTestPipelineWithKeysAndBudget(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		if upstreamCallsStarted.Add(1) == n {
			close(allStarted)
		}
		<-release
		return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
	}, nil, adapter.Registry{"openai": openai.New()}, keys, budget.NewTracker())

	req := adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}

	var ready sync.WaitGroup
	ready.Add(n)
	goCh := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-goCh
			rec := httptest.NewRecorder()
			errs[i] = p.HandleChatCompletionStream(context.Background(), "Bearer "+testKeyID, "", "", req, rec, "")
		}(i)
	}

	ready.Wait()
	close(goCh)

	select {
	case <-allStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for all n upstream calls to start")
	}

	// While all n requests are genuinely in flight — each blocked in its
	// own real, independently-started upstream call, never coalesced —
	// the shared in-flight counter for their one common l1Key must read
	// exactly n.
	p.streamingInFlightMu.Lock()
	numKeys := len(p.streamingInFlightByL1Key)
	var gotCount int
	for _, c := range p.streamingInFlightByL1Key {
		gotCount = c
	}
	p.streamingInFlightMu.Unlock()
	if numKeys != 1 {
		t.Fatalf("streamingInFlightByL1Key has %d distinct keys, want exactly 1 (all %d requests are byte-identical)", numKeys, n)
	}
	if gotCount != n {
		t.Fatalf("in-flight count = %d, want %d while all %d requests are concurrently blocked in their own upstream call", gotCount, n, n)
	}

	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if got := upstreamCallsStarted.Load(); got != n {
		t.Fatalf("upstreamCallsStarted = %d, want exactly %d -- every request must make its own real, unshared upstream call regardless of the collision counter (observation-only, never coalescing)", got, n)
	}

	// Every request has now completed and released its slot -- the map
	// entry must be deleted entirely (never left behind as a stale zero),
	// mirroring ConcurrencyLimiter.recordRunReleaseLocked's own identical
	// convention (concurrency.go).
	p.streamingInFlightMu.Lock()
	remaining := len(p.streamingInFlightByL1Key)
	p.streamingInFlightMu.Unlock()
	if remaining != 0 {
		t.Fatalf("streamingInFlightByL1Key has %d leftover entries after all requests completed, want 0", remaining)
	}

	var afterRM metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &afterRM); err != nil {
		t.Fatalf("reader.Collect (after): %v", err)
	}
	deltaAfter := streamingNearDuplicateCollisionMetricDelta(t, afterRM, testKeyID)
	if got := deltaAfter - deltaBefore; got != n-1 {
		t.Fatalf("kelvran.streaming.near_duplicate_collision[key_id=%s] delta = %d, want exactly %d (n-1) -- exactly one of %d byte-identical concurrent requests must be the first to observe an empty slot, every other one must observe a real collision", testKeyID, got, n-1, n)
	}
}

// TestStreamingNearDuplicateCollisionNeverFiresForALoneRequest proves the
// negative case streamingInFlightAcquire's own doc comment depends on: a
// single streaming request with no concurrent twin for the same l1Key
// must never be misreported as a collision -- neither the structured log
// line nor the real kelvran.streaming.near_duplicate_collision counter.
func TestStreamingNearDuplicateCollisionNeverFiresForALoneRequest(t *testing.T) {
	const testKeyID = "streaming-inflight-lone-request-key"

	reader := dataplaneTelemetryMetricsReaderForTest()
	var before metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &before); err != nil {
		t.Fatalf("reader.Collect (before): %v", err)
	}
	deltaBefore := streamingNearDuplicateCollisionMetricDelta(t, before, testKeyID)

	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	keys := []identity.VirtualKey{
		{ID: testKeyID, KeyHash: testHashOf(testKeyID), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream should never be called by a streaming test")
			return nil, nil
		},
		UpstreamStream: func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
			return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	rec := httptest.NewRecorder()
	err = p.HandleChatCompletionStream(context.Background(), "Bearer "+testKeyID, "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "a genuinely solo request, no concurrent twin"}},
	}, rec, "")
	if err != nil {
		t.Fatalf("HandleChatCompletionStream: %v", err)
	}

	if strings.Contains(logBuf.String(), "streaming_near_duplicate_collision") {
		t.Fatalf("a lone streaming request with no concurrent twin must never log a collision; got:\n%s", logBuf.String())
	}

	p.streamingInFlightMu.Lock()
	remaining := len(p.streamingInFlightByL1Key)
	p.streamingInFlightMu.Unlock()
	if remaining != 0 {
		t.Fatalf("streamingInFlightByL1Key has %d leftover entries after the lone request completed, want 0", remaining)
	}

	var afterRM metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &afterRM); err != nil {
		t.Fatalf("reader.Collect (after): %v", err)
	}
	deltaAfter := streamingNearDuplicateCollisionMetricDelta(t, afterRM, testKeyID)
	if got := deltaAfter - deltaBefore; got != 0 {
		t.Fatalf("kelvran.streaming.near_duplicate_collision[key_id=%s] delta = %d, want 0 -- a lone request with no concurrent twin must never be recorded as a collision", testKeyID, got)
	}
}

// TestStreamingInFlightAcquireReleaseThenReacquireNeverFalselyCollides is
// the direct, unit-level proof that streamingInFlightRelease's delete-on-
// zero cleanup (streaming.go) actually leaves the map key-reusable rather
// than a stale nonzero count behind: acquire, release, then acquire the
// SAME l1Key again on the SAME Pipeline instance must report collided ==
// false both times, and the map must be empty after each release. Neither
// TestStreamingInFlightCounterTracksConcurrentRequestsForSameKey (n
// genuinely concurrent requests, never sequential reuse) nor
// TestStreamingNearDuplicateCollisionNeverFiresForALoneRequest (a single
// acquire/release pair, never re-acquired) exercises this exact sequence.
// Note (verified by actually running a mutation, not assumed): given how
// this code is shaped, any bug that leaves a stale nonzero count behind
// also leaves streamingInFlightByL1Key's len() != 0, which both existing
// tests' own "leftover entries ... want 0" assertions already catch --
// e.g. mutating streamingInFlightRelease to decrement a local copy
// instead of the map entry fails BOTH existing tests too, not just this
// one. This test's real value is therefore directness, not closing a
// live detection gap: it asserts the documented "acquire after a full
// release never collides" contract in its own terms (the collided return
// value on a real reacquire) rather than only inferring it from a map-
// length side effect, so it stays a correct, legible regression guard
// even if streamingInFlightByL1Key's internal representation ever
// changes in a way that decouples "map length" from "would a reacquire
// falsely collide." Deliberately bypasses HandleChatCompletionStream/
// NewPipeline entirely (a bare Pipeline literal is safe here because
// these two methods touch only streamingInFlightMu/
// streamingInFlightByL1Key, per NewPipeline's own doc comment on this
// field) so the test is fast, deterministic, and isolates exactly the
// two functions under test.
func TestStreamingInFlightAcquireReleaseThenReacquireNeverFalselyCollides(t *testing.T) {
	p := &Pipeline{streamingInFlightByL1Key: map[string]int{}}
	const l1Key = "reused-l1-key"

	if collided := p.streamingInFlightAcquire(l1Key); collided {
		t.Fatal("first acquire of a fresh key reported a collision, want false")
	}
	p.streamingInFlightRelease(l1Key)
	if remaining := len(p.streamingInFlightByL1Key); remaining != 0 {
		t.Fatalf("streamingInFlightByL1Key has %d entries after the first release, want 0 (delete-on-zero)", remaining)
	}

	if collided := p.streamingInFlightAcquire(l1Key); collided {
		t.Fatal("re-acquiring the same l1Key after a full release+delete falsely reported a collision -- a stale nonzero count survived release")
	}
	p.streamingInFlightRelease(l1Key)
	if remaining := len(p.streamingInFlightByL1Key); remaining != 0 {
		t.Fatalf("streamingInFlightByL1Key has %d entries after the second release, want 0 (delete-on-zero)", remaining)
	}
}

// TestStreamingNearDuplicateCollisionSequentialKeyReuseNeverFalselyFires
// is the live, full-pipeline complement to
// TestStreamingInFlightAcquireReleaseThenReacquireNeverFalselyCollides:
// two genuinely SEQUENTIAL (never overlapping) real streaming requests
// for the byte-identical request -- and therefore the same l1Key -- on
// the same long-running Pipeline instance must both be observed as lone
// requests, exactly like
// TestStreamingNearDuplicateCollisionNeverFiresForALoneRequest's single
// call, but proven across a real release-then-reacquire cycle rather
// than a single acquire/release pair. This is the "plausible collision
// scenario" TestStreamingNearDuplicateCollisionNeverFiresForALoneRequest
// itself never exercises (per that test's own single-call shape): a
// second, wholly independent request reusing the first's now-released
// l1Key is exactly the ordinary traffic pattern (the same tenant asking
// the same model the same prompt twice, sequentially) that a stale-count
// bug would misreport as a collision in production.
func TestStreamingNearDuplicateCollisionSequentialKeyReuseNeverFalselyFires(t *testing.T) {
	const testKeyID = "streaming-inflight-sequential-reuse-key"

	reader := dataplaneTelemetryMetricsReaderForTest()
	var before metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &before); err != nil {
		t.Fatalf("reader.Collect (before): %v", err)
	}
	deltaBefore := streamingNearDuplicateCollisionMetricDelta(t, before, testKeyID)

	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	keys := []identity.VirtualKey{
		{ID: testKeyID, KeyHash: testHashOf(testKeyID), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream should never be called by a streaming test")
			return nil, nil
		},
		UpstreamStream: func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
			return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	req := adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "same request, asked twice, never concurrently"}},
	}

	for attempt := 1; attempt <= 2; attempt++ {
		rec := httptest.NewRecorder()
		if err := p.HandleChatCompletionStream(context.Background(), "Bearer "+testKeyID, "", "", req, rec, ""); err != nil {
			t.Fatalf("HandleChatCompletionStream (attempt %d): %v", attempt, err)
		}

		p.streamingInFlightMu.Lock()
		remaining := len(p.streamingInFlightByL1Key)
		p.streamingInFlightMu.Unlock()
		if remaining != 0 {
			t.Fatalf("streamingInFlightByL1Key has %d leftover entries after attempt %d completed, want 0", remaining, attempt)
		}
	}

	if strings.Contains(logBuf.String(), "streaming_near_duplicate_collision") {
		t.Fatalf("two sequential, non-overlapping requests reusing the same l1Key must never log a collision; got:\n%s", logBuf.String())
	}

	var afterRM metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &afterRM); err != nil {
		t.Fatalf("reader.Collect (after): %v", err)
	}
	deltaAfter := streamingNearDuplicateCollisionMetricDelta(t, afterRM, testKeyID)
	if got := deltaAfter - deltaBefore; got != 0 {
		t.Fatalf("kelvran.streaming.near_duplicate_collision[key_id=%s] delta = %d, want 0 -- sequential key reuse after a full release must never be recorded as a collision", testKeyID, got)
	}
}
