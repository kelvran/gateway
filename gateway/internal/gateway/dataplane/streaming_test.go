package dataplane

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/gemini"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
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
	p, err := NewPipeline(Config{
		Verifier:   verifier,
		Limiter:    ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:     budget.NewTracker(),
		Cache:      inprocess.New(0),
		CacheL2:    inprocess.New(0),
		CacheL3:    inprocess.NewLexicalCache(0),
		Guardrails: guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:   adapters,
		Deployments: func() []Deployment {
			if deployments != nil {
				return deployments
			}
			return []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
		}(),
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
	p, err := NewPipeline(Config{
		Verifier:   verifier,
		Limiter:    ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:     tracker,
		Cache:      inprocess.New(0),
		CacheL2:    inprocess.New(0),
		CacheL3:    inprocess.NewLexicalCache(0),
		Guardrails: guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:   adapters,
		Deployments: func() []Deployment {
			if deployments != nil {
				return deployments
			}
			return []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
		}(),
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
	tracker.Record("team-x", decimal.RequireFromString("999"))

	p := newStreamingTestPipelineWithKeysAndBudget(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		t.Fatal("UpstreamStream must never be called for a model-not-allowed request")
		return nil, nil
	}, nil, adapter.Registry{"openai": openai.New()}, keys, tracker)

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer team-x-secret", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec)
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
	tracker.Record("team-x", decimal.RequireFromString("1.0"))

	p := newStreamingTestPipelineWithKeysAndBudget(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		t.Fatal("UpstreamStream must never be called once budget is exceeded")
		return nil, nil
	}, nil, adapter.Registry{"openai": openai.New()}, keys, tracker)

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer team-x-secret", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec)
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
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer alpha-secret", req, rec1); err != nil {
		t.Fatalf("team-alpha first call: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after team-alpha's first call = %d, want 1", upstreamCalls)
	}

	// team-beta's identical request must be a real cache MISS (a second
	// real UpstreamStream call), not served from team-alpha's cache entry.
	rec2 := httptest.NewRecorder()
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer beta-secret", req, rec2); err != nil {
		t.Fatalf("team-beta first call: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after team-beta's first (should be a MISS) call = %d, want 2 — cross-tenant cache leakage", upstreamCalls)
	}

	// Each key's own SECOND identical request must now be a fake-streamed
	// cache hit — upstreamCalls stays at 2.
	rec3 := httptest.NewRecorder()
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer alpha-secret", req, rec3); err != nil {
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
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, rec)
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
	err = p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, rec2)
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

func TestHandleChatCompletionStreamUnsupportedProviderReturnsTypedError(t *testing.T) {
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		t.Fatal("upstream stream should never be called for a provider that doesn't support streaming")
		return nil, nil
	}, []Deployment{{Name: "d1", Model: "gemini-pro", Provider: "gemini", UpstreamModel: "gemini-pro", BaseURL: "http://unused"}},
		adapter.Registry{"gemini": gemini.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "gemini-pro", Stream: true,
	}, rec)
	if !errors.Is(err, ErrStreamingNotSupported) {
		t.Fatalf("err = %v, want ErrStreamingNotSupported", err)
	}
}

func TestHandleChatCompletionStreamNotConfiguredOnCacheMiss(t *testing.T) {
	p := newStreamingTestPipeline(t, nil, nil, adapter.Registry{"openai": openai.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec)
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
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec)
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
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec)
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
