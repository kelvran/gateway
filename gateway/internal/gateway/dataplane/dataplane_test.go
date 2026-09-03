package dataplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// discardLogger silences log output during tests.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// testHashOf is the test-only equivalent of what an operator does once
// with `openssl rand -hex 32 | sha256sum` per
// docs/rfcs/2026-09-02-virtual-keys-budgets.md.
func testHashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// defaultTestVirtualKeys is a single virtual key (bearer secret "test-key",
// ID "test-key") with an effectively-unlimited rate limit and no budget
// cap — the common case for tests that don't care about limiting.
func defaultTestVirtualKeys() []identity.VirtualKey {
	return []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
}

// keyConfigsFromVirtualKeys converts the test-only identity.VirtualKey
// shape into the []ratelimit.KeyConfig shape Config.Limiter needs.
// Production code never does this conversion this way — cmd/gateway's
// buildPipeline builds ratelimit.KeyConfig directly from
// controlplane.VirtualKeyConfig, never by way of identity.VirtualKey.
func keyConfigsFromVirtualKeys(keys []identity.VirtualKey) []ratelimit.KeyConfig {
	configs := make([]ratelimit.KeyConfig, 0, len(keys))
	for _, k := range keys {
		configs = append(configs, ratelimit.KeyConfig{
			ID:              k.ID,
			Capacity:        k.RateLimitBurst,
			RefillPerSecond: k.RateLimitRefill,
		})
	}
	return configs
}

func newTestPipelineWithKeysAndBudget(t *testing.T, upstream UpstreamCaller, deployments []Deployment, keys []identity.VirtualKey, tracker *budget.Tracker) *Pipeline {
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
		Adapters: adapter.Registry{
			"openai": openai.New(),
		},
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

func newTestPipelineWithKeys(t *testing.T, upstream UpstreamCaller, deployments []Deployment, keys []identity.VirtualKey) *Pipeline {
	t.Helper()
	return newTestPipelineWithKeysAndBudget(t, upstream, deployments, keys, budget.NewTracker())
}

func newTestPipeline(t *testing.T, upstream UpstreamCaller, deployments []Deployment) *Pipeline {
	t.Helper()
	return newTestPipelineWithKeys(t, upstream, deployments, defaultTestVirtualKeys())
}

func fakeOpenAIResponse(model string) *openai.Response {
	return &openai.Response{
		ID:    "chatcmpl-fake",
		Model: model,
		Choices: []openai.Choice{
			{Index: 0, Message: openai.Message{Role: "assistant", Content: "hello"}, FinishReason: "stop"},
		},
		Usage: openai.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
	}
}

func TestHandleChatCompletionRejectsMissingAuth(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		t.Fatal("upstream should never be called when auth fails")
		return nil, nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	_, err := p.HandleChatCompletion(context.Background(), "", adapter.ChatRequest{Model: "gpt-4o"})
	if err == nil {
		t.Fatal("expected an error for missing Authorization header")
	}
}

func TestHandleChatCompletionRejectsRateLimited(t *testing.T) {
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 0, RateLimitRefill: 0}, // always empty
	}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		t.Fatal("upstream should never be called when rate-limited")
		return nil, nil
	}, []Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}, keys)

	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestHandleChatCompletionMissThenCacheHit(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	req := adapter.ChatRequest{
		Model:    "gpt-4o",
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}

	resp1, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if resp1.Model != "gpt-4o" {
		t.Errorf("resp1.Model = %q, want %q", resp1.Model, "gpt-4o")
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after first call = %d, want 1", upstreamCalls)
	}

	resp2, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after second (cache-hit) call = %d, want still 1", upstreamCalls)
	}
	if resp2.Choices[0].Message.Content != resp1.Choices[0].Message.Content {
		t.Errorf("cached response content mismatch: %q vs %q", resp2.Choices[0].Message.Content, resp1.Choices[0].Message.Content)
	}
}

func TestHandleChatCompletionFallsBackOnUpstreamError(t *testing.T) {
	var calls []string
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		calls = append(calls, dep.Name)
		if dep.Name == "primary" {
			return nil, errors.New("simulated upstream failure")
		}
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{
		{Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "secondary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	})

	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("upstream calls = %v, want exactly 2 (primary then secondary)", calls)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("resp.Choices[0].FinishReason = %q, want %q", resp.Choices[0].FinishReason, "stop")
	}
}

func TestHandleChatCompletionNoDeploymentForModel(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		t.Fatal("upstream should never be called for an unconfigured model")
		return nil, nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "unknown-model"})
	if err == nil {
		t.Fatal("expected an error for an unconfigured model")
	}
}

// TestHandleChatCompletionModelNotAllowedCheckedBeforeRateLimitAndBudget
// proves the check ordering docs/rfcs/2026-09-02-virtual-keys-budgets.md
// specifies: allowed-models is checked first. The virtual key here also
// has its rate limit AND budget already exhausted — if the allowed-models
// check ran anywhere other than first, this request would fail with
// ErrRateLimited or ErrBudgetExceeded instead, silently hiding which rule
// actually applies.
func TestHandleChatCompletionModelNotAllowedCheckedBeforeRateLimitAndBudget(t *testing.T) {
	keys := []identity.VirtualKey{{
		ID:              "team-x",
		KeyHash:         testHashOf("team-x-secret"),
		RateLimitBurst:  0, // already exhausted
		RateLimitRefill: 0,
		BudgetUSD:       decimal.RequireFromString("0.01"),
		AllowedModels:   map[string]struct{}{"gpt-4o-mini": {}},
	}}
	tracker := budget.NewTracker()
	tracker.Record("team-x", decimal.RequireFromString("999")) // already over budget too

	p := newTestPipelineWithKeysAndBudget(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		t.Fatal("upstream must never be called for a model-not-allowed request")
		return nil, nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, keys, tracker)

	_, err := p.HandleChatCompletion(context.Background(), "Bearer team-x-secret", adapter.ChatRequest{Model: "gpt-4o"})
	if !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("err = %v, want ErrModelNotAllowed (must be checked before rate-limit/budget)", err)
	}
}

// TestHandleChatCompletionPerKeyRateLimitsAreIndependent proves exhausting
// one virtual key's rate limit never affects another key's — the whole
// point of moving from one global bucket to a per-key map of buckets.
func TestHandleChatCompletionPerKeyRateLimitsAreIndependent(t *testing.T) {
	keys := []identity.VirtualKey{
		{ID: "exhausted", KeyHash: testHashOf("exhausted-secret"), RateLimitBurst: 0, RateLimitRefill: 0},
		{ID: "fresh", KeyHash: testHashOf("fresh-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, keys)

	_, err := p.HandleChatCompletion(context.Background(), "Bearer exhausted-secret", adapter.ChatRequest{Model: "gpt-4o"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("exhausted key: err = %v, want ErrRateLimited", err)
	}

	_, err = p.HandleChatCompletion(context.Background(), "Bearer fresh-secret", adapter.ChatRequest{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("fresh key was blocked by the exhausted key's rate limit: %v", err)
	}
}

// TestHandleChatCompletionBudgetExceededRejectsBeforeUpstream proves a
// key that has already spent its cap is rejected with ErrBudgetExceeded
// and never reaches the upstream call.
func TestHandleChatCompletionBudgetExceededRejectsBeforeUpstream(t *testing.T) {
	keys := []identity.VirtualKey{
		{ID: "team-x", KeyHash: testHashOf("team-x-secret"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("0.01")},
	}
	tracker := budget.NewTracker()
	tracker.Record("team-x", decimal.RequireFromString("1.0")) // already well over the 0.01 cap

	p := newTestPipelineWithKeysAndBudget(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		t.Fatal("upstream must never be called once budget is exceeded")
		return nil, nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, keys, tracker)

	_, err := p.HandleChatCompletion(context.Background(), "Bearer team-x-secret", adapter.ChatRequest{Model: "gpt-4o"})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
}

// TestHandleChatCompletionCacheIsolatedAcrossVirtualKeys is the
// load-bearing test for this whole feature: two different virtual keys
// sending a byte-identical request must each get their own cache entry —
// the second key's first request must be a MISS (reaching upstream), not
// a HIT off the first key's cached response. This proves cache.Key's new
// tenant dimension (unit-tested in isolation in internal/cache/key_test.go)
// is actually wired through end-to-end from HandleChatCompletion.
func TestHandleChatCompletionCacheIsolatedAcrossVirtualKeys(t *testing.T) {
	var upstreamCalls int
	keys := []identity.VirtualKey{
		{ID: "team-alpha", KeyHash: testHashOf("alpha-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		{ID: "team-beta", KeyHash: testHashOf("beta-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, keys)

	req := adapter.ChatRequest{
		Model:    "gpt-4o",
		Messages: []adapter.Message{{Role: "user", Content: "identical question"}},
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer alpha-secret", req); err != nil {
		t.Fatalf("team-alpha first call: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after team-alpha's first call = %d, want 1", upstreamCalls)
	}

	// team-beta's identical request must be a cache MISS against
	// team-alpha's entry — the load-bearing assertion.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer beta-secret", req); err != nil {
		t.Fatalf("team-beta first call: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after team-beta's first (should be a MISS) call = %d, want 2 — cross-tenant cache leakage", upstreamCalls)
	}

	// Each key's own SECOND identical request must now be a cache hit.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer alpha-secret", req); err != nil {
		t.Fatalf("team-alpha second call: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after team-alpha's second (should be a HIT) call = %d, want still 2", upstreamCalls)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer beta-secret", req); err != nil {
		t.Fatalf("team-beta second call: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after team-beta's second (should be a HIT) call = %d, want still 2", upstreamCalls)
	}
}

// TestPipelineCloseOnPlainBudgetTrackerIsANoOp proves Close works cleanly
// on the common (no persistence configured) case every other test in
// this file already exercises — the real restart-survival proof, with a
// persistent store configured, is a cmd/gateway integration test per
// docs/plans/2026-09-03-budget-persistence.md's Task 4.
func TestPipelineCloseOnPlainBudgetTrackerIsANoOp(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	if err := p.Close(); err != nil {
		t.Errorf("Close() on a Pipeline with no persistent budget store = %v, want nil", err)
	}
}

// compile-time sanity: cache.Cache is used, not redefined, inside this
// test file's imports.
var _ cache.Cache = (*inprocess.Cache)(nil)
