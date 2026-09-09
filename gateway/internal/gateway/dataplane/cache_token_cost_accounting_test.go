package dataplane

// Full-pipeline proof for docs/rfcs/2026-09-10-gateway-cache-token-cost-
// accounting.md: a genuine provider-cache-hit response (Kelvran's own
// L1/L2/L3 cache still misses -- this is the PROVIDER's cache, e.g.
// Anthropic's cache_control, serving part of the prompt) must have its
// real cache-read/cache-creation tokens folded into cost accounting AND
// the TPM rate-limit debit -- both previously undercounted to the point
// of dropping the cache slice entirely. This is the exact scenario
// docs/upgrade-research/gateway-next-upgrade-round2-2026-09-09.md's
// Finding 1 named as having zero test coverage before this pass.

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// fakeAnthropicResponseWithCacheTokens mirrors fakeAnthropicResponse (in
// cache_control_auto_populate_test.go) but carries real cache-read/
// cache-creation usage, the shape Anthropic actually sends once
// CacheControl auto-populate (default-ON) causes a provider-side cache
// write or read.
func fakeAnthropicResponseWithCacheTokens(model string) *anthropic.Response {
	return &anthropic.Response{
		ID:         "msg_cached",
		Model:      model,
		Role:       "assistant",
		Content:    []anthropic.ContentBlock{{Type: "text", Text: "hi"}},
		StopReason: "end_turn",
		Usage: anthropic.Usage{
			InputTokens:              5,
			OutputTokens:             1,
			CacheCreationInputTokens: 248,
			CacheReadInputTokens:     1800,
		},
	}
}

// TestCallDeploymentCacheTokensFoldIntoCostAndTPM is the load-bearing
// end-to-end proof for the whole Phase 2 fix. Two independent, previously
// undercounted-to-zero signals are checked against the SAME real
// provider-cache-bearing response:
//
//  1. Cost accounting (via budget.Tracker.SpentUSD, a real priced
//     dollar figure): must reflect all 2054 real tokens
//     (5+1800+248 prompt + 1 completion), not just the 6 tokens Kelvran
//     would have seen before this fix (input_tokens+output_tokens only).
//  2. TPM rate-limiting: a bucket sized to survive a 6-token debit but
//     not a 2054-token debit must reject the SECOND request under the
//     fix (this call really did consume ~2054 tokens) but would have
//     allowed it under the pre-fix bug (which only ever saw 6).
func TestCallDeploymentCacheTokensFoldIntoCostAndTPM(t *testing.T) {
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "claude-opus-4", Provider: "anthropic", UpstreamModel: "claude-opus-4", BaseURL: "http://unused"}}

	// TPM bucket sized to survive exactly one 6-token (buggy) debit twice
	// (12 <= 20) but not one 2054-token (correct) debit even once.
	limiter := ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{
		{ID: "test-key", Capacity: 100, RefillPerSecond: 100, TPMCapacity: 20, TPMRefillPerSecond: 0},
	})

	promptRate := decimal.RequireFromString("0.000015")
	completionRate := decimal.RequireFromString("0.000075")
	priceTable := costaccounting.PriceTable{
		"claude-opus-4": {PromptPerToken: promptRate, CompletionPerToken: completionRate},
	}

	budgetTracker := budget.NewTracker()
	p, err := NewPipeline(Config{
		Verifier:   verifier,
		Limiter:    limiter,
		Budget:     budgetTracker,
		Cache:      inprocess.New(0),
		CacheL2:    inprocess.New(0),
		CacheL3:    inprocess.NewLexicalCache(0),
		Guardrails: guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters: adapter.Registry{
			"anthropic": anthropic.New(),
		},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(priceTable),
		Upstream: func(ctx context.Context, dep Deployment, providerReq any) (any, error) {
			return fakeAnthropicResponseWithCacheTokens(dep.UpstreamModel), nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	req := adapter.ChatRequest{Model: "claude-opus-4", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("call 1: %v", err)
	}

	// (1) Cost accounting: fresh(5)*promptRate + cacheRead(1800, unset
	// rate falls back to promptRate) + cacheCreation(248, same fallback)
	// + completion(1)*completionRate.
	wantSpent := decimal.NewFromInt(5 + 1800 + 248).Mul(promptRate).Add(decimal.NewFromInt(1).Mul(completionRate))
	gotSpent := budgetTracker.SpentUSD("test-key", 0)
	if !gotSpent.Equal(wantSpent) {
		t.Errorf("budget.SpentUSD after one cache-bearing call = %v, want %v (cache-inclusive PromptTokens priced at the fallback prompt rate)", gotSpent, wantSpent)
	}

	// (2) TPM: the bucket started at 20, real usage this call was
	// 5+1800+248+1 = 2054 tokens (far more than the bucket's own
	// capacity) -- ReconcileTPM must debit the bucket below zero, so the
	// VERY NEXT request is rejected. Under the pre-fix bug (only
	// input_tokens+output_tokens = 6 tokens ever counted), the bucket
	// would still have 14 tokens left and this second call would be
	// wrongly ALLOWED.
	req2 := adapter.ChatRequest{Model: "claude-opus-4", Messages: []adapter.Message{{Role: "user", Content: "a different request"}}}
	_, err = p.HandleChatCompletion(context.Background(), "Bearer test-key", req2)
	if err == nil {
		t.Fatal("call 2 succeeded, want a TPM rejection -- the cache-inclusive token count from call 1 (2054) should have exhausted a 20-token bucket; a pre-fix accounting bug (6 tokens) would have wrongly allowed this")
	}
}
