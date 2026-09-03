package dataplane

// Cache L3-lite's own load-bearing full-pipeline proofs, per
// docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md: a lexical
// near-duplicate is a real hit, an entity-mismatched near-duplicate is a
// real miss, a volatile query never hits L3 at all, and an L3 backend
// error fails closed to a real upstream call rather than crashing or
// silently serving.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// TestHandleChatCompletionLexicalNearDuplicateHitsL3 proves a request whose
// only difference from a prior one is internal whitespace — untouched by
// L2's narrow 3-operation allowlist, so both L1 and L2 genuinely miss — is
// nonetheless a real L3 hit. Shingles' strings.Fields-based word splitting
// collapses whitespace runs, so the two texts produce byte-identical
// MinHash signatures (a deterministic Jaccard estimate of 1.0, not a
// probabilistic near-miss); neither message carries any entity/number/date,
// so the hard gate's fingerprint check trivially passes too.
func TestHandleChatCompletionLexicalNearDuplicateHitsL3(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	first := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "Explain how binary search works in a sorted array"}}}
	second := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "Explain how binary search   works in a sorted array"}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", first); err != nil {
		t.Fatalf("first HandleChatCompletion: %v", err)
	}
	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", second)
	if err != nil {
		t.Fatalf("second HandleChatCompletion: %v", err)
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("second response model = %q, want %q", resp.Model, "gpt-4o")
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1 (the second, lexically-near-duplicate request should be an L3 cache hit, not a real upstream call)", upstreamCalls)
	}
}

// TestHandleChatCompletionEntityMismatchIsNotAnL3Hit proves the hard gate's
// whole reason for existing actually holds end-to-end: a query about $92
// must never be served from an entry cached for a query about $93, even
// though the two requests are otherwise near-identical.
func TestHandleChatCompletionEntityMismatchIsNotAnL3Hit(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	first := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "What is 15% of $92"}}}
	second := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "What is 15% of $93"}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", first); err != nil {
		t.Fatalf("first HandleChatCompletion: %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", second); err != nil {
		t.Fatalf("second HandleChatCompletion: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2 — a different dollar amount must never be served from a cached entry for a different amount, per the entity/number hard gate", upstreamCalls)
	}
}

// TestHandleChatCompletionVolatileQueryNeverHitsL3 proves the volatility
// bypass takes priority over an otherwise-valid L3 hit: the second request
// here is a byte-for-byte lexical near-duplicate of the first (same
// whitespace-collapse guarantee as the near-duplicate-hit test above) with
// an identical entity fingerprint ("Paris"), yet must never be served from
// L3 because it matches the volatility keyword list.
func TestHandleChatCompletionVolatileQueryNeverHitsL3(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	first := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "What is the weather in Paris right now"}}}
	second := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "What is the weather in Paris   right now"}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", first); err != nil {
		t.Fatalf("first HandleChatCompletion: %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", second); err != nil {
		t.Fatalf("second HandleChatCompletion: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2 — a volatile (weather) query must bypass L3 entirely, even though the second request is otherwise a lexical near-duplicate with an identical fingerprint", upstreamCalls)
	}
}

// failingLexicalCache is a cache.LexicalCache whose Search always errors —
// simulating a real L3 backend outage (e.g. a future networked
// implementation), never exercised by inprocess.LexicalCache itself, which
// cannot fail.
type failingLexicalCache struct{}

func (failingLexicalCache) Search(_ context.Context, _ string, _ []uint64, _ int) ([]cache.LexicalCandidate, error) {
	return nil, errors.New("simulated L3 backend failure")
}

func (failingLexicalCache) Put(_ context.Context, _ string, _ []uint64, _ []byte, _ map[string]struct{}, _ string, _ time.Duration) error {
	return nil
}

// TestHandleChatCompletionLexicalCacheSearchErrorFailsClosedToUpstream
// proves checkLexicalCache's fail-closed discipline end-to-end: an L3
// backend that errors on every Search call must never crash or silently
// serve an unchecked response — the request must still succeed via a real
// upstream call, exactly as if L3 had simply reported a miss.
func TestHandleChatCompletionLexicalCacheSearchErrorFailsClosedToUpstream(t *testing.T) {
	var upstreamCalls int
	p := newTestPipelineWithCacheL3(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, failingLexicalCache{})

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hello there"}}}
	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req)
	if err != nil {
		t.Fatalf("HandleChatCompletion with a failing L3 backend: %v", err)
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("response model = %q, want %q", resp.Model, "gpt-4o")
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1 — an L3 search error must fail closed to a real upstream call, never crash or silently serve", upstreamCalls)
	}
}

// newTestPipelineWithCacheL3 is this file's own local helper, needed
// because every other test pipeline helper hardcodes a real
// inprocess.NewLexicalCache(0) — this is the one test that specifically
// needs to inject a misbehaving L3 implementation instead.
func newTestPipelineWithCacheL3(t *testing.T, upstream UpstreamCaller, l3 cache.LexicalCache) *Pipeline {
	t.Helper()
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	p, err := NewPipeline(Config{
		Verifier: verifier,
		Limiter:  ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:   budget.NewTracker(),
		Cache:    inprocess.New(0),
		CacheL2:  inprocess.New(0),
		CacheL3:  l3,
		Adapters: adapter.Registry{
			"openai": openai.New(),
		},
		Deployments:    []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}},
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream:       upstream,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}
