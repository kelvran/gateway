package dataplane

import (
	"context"
	"errors"
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

// tpmTestPipeline builds a Pipeline with an explicit, TPM-configured
// ratelimit.KeyLimiter — the shared newTestPipeline* helpers all build
// their Limiter from identity.VirtualKey via keyConfigsFromVirtualKeys,
// which carries no TPM fields (identity.VirtualKey deliberately has
// none, per docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md — unlike
// RateLimitBurst/RateLimitRefill, which exist there but are never read
// by any production code either).
func tpmTestPipeline(t *testing.T, upstream UpstreamCaller, tpmCapacity, tpmRefillPerSecond float64) *Pipeline {
	t.Helper()
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	limiter := ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{
		{ID: "test-key", Capacity: 100, RefillPerSecond: 100, TPMCapacity: tpmCapacity, TPMRefillPerSecond: tpmRefillPerSecond},
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

// TestHandleChatCompletionRejectsOnceTPMBucketExhausted is the
// load-bearing proof for docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md:
// once real usage (fakeOpenAIResponse's 8 total tokens per call) has
// debited the TPM bucket below zero, the NEXT request must be rejected
// — purely from past usage, since no request's own future cost is known
// at decision time.
func TestHandleChatCompletionRejectsOnceTPMBucketExhausted(t *testing.T) {
	var upstreamCalls int
	p := tpmTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, 10, 0) // capacity 10, no refill; each real call costs 8 tokens

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}

	// Call 1: balance starts at 10 (>0) -> allowed; debits 8 -> balance 2.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after call 1 = %d, want 1", upstreamCalls)
	}

	// Call 2: balance is 2 (>0) -> allowed; debits 8 -> balance -6 (overdraft).
	req2 := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "a different request"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req2); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after call 2 = %d, want 2", upstreamCalls)
	}

	// Call 3: balance is -6 (<=0) -> rejected, no third upstream call.
	req3 := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "yet another distinct request"}}}
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req3)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("call 3 err = %v, want ErrRateLimited", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after call 3 = %d, want still 2 (call 3 must never reach upstream)", upstreamCalls)
	}
}

// TestHandleChatCompletionCacheHitDoesNotDebitTPMBucket proves the same
// billable gate cost-double-counting introduced
// (docs/rfcs/2026-09-05-gateway-cost-double-counting.md) also protects
// the TPM bucket: a repeat cache hit incurred no real, incremental token
// usage, so it must never debit the bucket again for tokens already
// counted once.
func TestHandleChatCompletionCacheHitDoesNotDebitTPMBucket(t *testing.T) {
	var upstreamCalls int
	p := tpmTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, 10, 0) // capacity 10; each real call costs 8 tokens — if a cache hit
	// double-debited, the bucket would go negative before the second real
	// (different) request below ever ran, rejecting it.

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}

	// Real miss: balance 10 -> debit 8 -> balance 2.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("first (real-miss) call: %v", err)
	}
	// Repeat the IDENTICAL request: a real L1 cache hit. Must not debit.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("second (cache-hit) call: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after the cache-hit call = %d, want still 1", upstreamCalls)
	}

	// A second, genuinely different request: if the cache hit above left
	// the balance at 2 (correct), this is allowed and reaches upstream.
	// If the cache hit incorrectly re-debited (balance -6), this is
	// rejected instead.
	req2 := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "a genuinely different request"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req2); err != nil {
		t.Fatalf("third (different, real-miss) call: %v — a cache hit must never re-debit the TPM bucket", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after the third call = %d, want 2", upstreamCalls)
	}
}

// TestHandleChatCompletionUnaffectedByTPMWhenNotConfigured proves the
// negative case at the dataplane level: a key with no TPM capacity
// configured must never be throttled by the TPM dimension, regardless of
// how much real usage accumulates.
func TestHandleChatCompletionUnaffectedByTPMWhenNotConfigured(t *testing.T) {
	var upstreamCalls int
	p := tpmTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, 0, 0) // TPM not configured

	for i := 0; i < 5; i++ {
		req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "distinct content " + string(rune('a'+i))}}}
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if upstreamCalls != 5 {
		t.Fatalf("upstreamCalls = %d, want 5 (TPM not configured, must never throttle)", upstreamCalls)
	}
}

// perModelTPMTestPipeline mirrors tpmTestPipeline, but with two
// deployments (so HandleChatCompletion can route two DIFFERENT client-
// requested models through the SAME virtual key) and a PerModel TPM
// override on "gpt-4o" only — "gpt-4o-mini" has no override, so it always
// falls through to the key's own default TPM bucket, per the Phase 4
// PerModel-for-TPM direct-path extension (docs/upgrade-research/gateway-
// per-deployment-concurrency-2026-09-09.md's own follow-on).
func perModelTPMTestPipeline(t *testing.T, upstream UpstreamCaller, defaultTPMCapacity, gpt4oTPMCapacity float64) *Pipeline {
	t.Helper()
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{
		{Name: "d1", Model: "gpt-4o-mini", Provider: "openai", UpstreamModel: "gpt-4o-mini", BaseURL: "http://unused"},
		{Name: "d2", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	limiter := ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{
		{
			ID: "test-key", Capacity: 100, RefillPerSecond: 100,
			TPMCapacity: defaultTPMCapacity, TPMRefillPerSecond: 0,
			PerModel: map[string]ratelimit.ModelRateLimit{
				"gpt-4o": {Capacity: 100, RefillPerSecond: 100, TPMCapacity: gpt4oTPMCapacity, TPMRefillPerSecond: 0},
			},
		},
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

// TestHandleChatCompletionPerModelTPMExhaustsIndependentlyOfDefaultBucket
// is the load-bearing full-pipeline proof for the Phase 4 PerModel-for-
// TPM direct-path extension: gpt-4o's own small TPM override bucket
// exhausts after real usage while gpt-4o-mini (no override, same virtual
// key) keeps succeeding against the key's own much larger default TPM
// bucket, completely unaffected.
func TestHandleChatCompletionPerModelTPMExhaustsIndependentlyOfDefaultBucket(t *testing.T) {
	var upstreamCalls int
	p := perModelTPMTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, 1000, 10) // default bucket: capacity 1000; gpt-4o's own override: capacity 10. Each real call costs 8 tokens (fakeOpenAIResponse's fixed usage).

	// Two real gpt-4o calls exhaust its own 10-token override bucket: the
	// first reserves its FULL fresh balance (10, no billing history yet)
	// then reconciles down to a real 8-token cost (balance 2); the second
	// reserves the now-established historical-average estimate (8, from
	// billedTokens/billedCount) against a balance of only 2, going
	// negative, then reconciles to the same real 8-token cost.
	gpt4oReq1 := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "gpt-4o request one"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", gpt4oReq1); err != nil {
		t.Fatalf("gpt-4o call 1: %v", err)
	}
	gpt4oReq2 := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "gpt-4o request two"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", gpt4oReq2); err != nil {
		t.Fatalf("gpt-4o call 2: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after 2 gpt-4o calls = %d, want 2", upstreamCalls)
	}

	// A third gpt-4o call must now be rejected — its own override bucket
	// is exhausted (negative balance after call 2).
	gpt4oReq3 := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "gpt-4o request three"}}}
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", gpt4oReq3)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("gpt-4o call 3 err = %v, want ErrRateLimited — its own override bucket should be exhausted", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after gpt-4o call 3 = %d, want still 2 (rejected before reaching upstream)", upstreamCalls)
	}

	// A DIFFERENT model on the SAME virtual key must be completely
	// unaffected — it has no PerModel TPM override, so it falls through
	// to the key's own default 1000-token bucket, untouched by gpt-4o's
	// own exhaustion.
	miniReq := adapter.ChatRequest{Model: "gpt-4o-mini", Messages: []adapter.Message{{Role: "user", Content: "gpt-4o-mini request"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", miniReq); err != nil {
		t.Fatalf("gpt-4o-mini call: %v — must succeed against the key's own default TPM bucket, unaffected by gpt-4o's own exhausted override", err)
	}
	if upstreamCalls != 3 {
		t.Fatalf("upstreamCalls after the gpt-4o-mini call = %d, want 3", upstreamCalls)
	}
}
