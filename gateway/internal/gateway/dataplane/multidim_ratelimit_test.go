package dataplane

// Load-bearing proof for docs/rfcs/2026-09-07-gateway-multi-dimensional-
// rate-limits.md: a virtual key's per-model rate-limit override is
// actually consulted by the real HandleChatCompletion pipeline, exhausts
// independently of the key's own default bucket, and a request for an
// unconfigured model is unaffected — mirroring
// tpm_ratelimit_test.go's own conventions for a Pipeline built with an
// explicit, hand-configured ratelimit.KeyLimiter.

import (
	"context"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// perModelTestPipeline builds a Pipeline with an explicit, PerModel-
// configured ratelimit.KeyLimiter and two deployments so a request can be
// routed to either "gpt-4o" or "claude-opus-4" — the same pattern
// tpmTestPipeline uses for TPM, since the shared newTestPipeline* helpers
// build their Limiter from identity.VirtualKey, which carries no
// per-model fields.
func perModelTestPipeline(t *testing.T, upstream UpstreamCaller, perModel map[string]ratelimit.ModelRateLimit) *Pipeline {
	t.Helper()
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "d2", Model: "claude-opus-4", Provider: "openai", UpstreamModel: "claude-opus-4", BaseURL: "http://unused"},
	}
	limiter := ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{
		{ID: "test-key", Capacity: 100, RefillPerSecond: 100, PerModel: perModel},
	})
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        limiter,
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream:       upstream,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestHandleChatCompletionPerModelRateLimitRejectsOnlyThatModel proves a
// virtual key's per-model override exhausts independently of its default
// bucket: once gpt-4o's own 1-request override is spent, a THIRD gpt-4o
// request is rejected, but claude-opus-4 (sharing the key's own 100-
// capacity default bucket) is unaffected — different requests each turn
// use distinct message content so no cache layer masks a real upstream
// call.
func TestHandleChatCompletionPerModelRateLimitRejectsOnlyThatModel(t *testing.T) {
	var upstreamCalls int
	p := perModelTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, map[string]ratelimit.ModelRateLimit{"gpt-4o": {Capacity: 1, RefillPerSecond: 0}})

	gptReq := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "first gpt-4o call"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", gptReq); err != nil {
		t.Fatalf("first gpt-4o request: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after first gpt-4o request = %d, want 1", upstreamCalls)
	}

	secondGptReq := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "second gpt-4o call, gpt-4o's own override bucket is now exhausted"}}}
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", secondGptReq)
	if err == nil {
		t.Fatal("second gpt-4o request succeeded, want ErrRateLimited — gpt-4o's own 1-capacity override should be exhausted")
	}

	claudeReq := adapter.ChatRequest{Model: "claude-opus-4", Messages: []adapter.Message{{Role: "user", Content: "a claude-opus-4 call, unrelated to gpt-4o's own exhausted override"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", claudeReq); err != nil {
		t.Fatalf("claude-opus-4 request failed: %v — a model with no PerModel override must be unaffected by gpt-4o's own exhausted one, sharing the key's own 100-capacity default bucket", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after the claude-opus-4 request = %d, want 2 (the rejected gpt-4o request must never reach upstream)", upstreamCalls)
	}
}

// TestHandleChatCompletionWithoutPerModelBehavesUnchanged proves the
// backward-compatibility guarantee end-to-end through the real pipeline:
// a virtual key with no PerModel entries configured routes every model
// through the identical shared default bucket, exactly as before this
// feature existed.
func TestHandleChatCompletionWithoutPerModelBehavesUnchanged(t *testing.T) {
	var upstreamCalls int
	p := perModelTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, nil)

	gptReq := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "a gpt-4o call with no per-model override configured"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", gptReq); err != nil {
		t.Fatalf("gpt-4o request: %v", err)
	}
	claudeReq := adapter.ChatRequest{Model: "claude-opus-4", Messages: []adapter.Message{{Role: "user", Content: "a claude-opus-4 call with no per-model override configured"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", claudeReq); err != nil {
		t.Fatalf("claude-opus-4 request: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2 — both requests should succeed against the key's own shared 100-capacity default bucket", upstreamCalls)
	}
}
