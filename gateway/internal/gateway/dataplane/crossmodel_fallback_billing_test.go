package dataplane

// Load-bearing proofs for the fixes to
// evals/tests/fixtures/regression_corpus_cost_abuse.json's
// costabuse-crossmodel-fallback-billed-at-requested-not-served-model-price
// and costabuse-permodel-ratelimit-bypassed-via-crossmodel-fallback-chain
// cases: a fallback_chains hop that switches to a genuinely different,
// more expensive model must be BILLED at that model's own real price
// (never the client's originally-requested, cheaper model's price), and
// a virtual key's PerModel RPM cap on that expensive model must still be
// enforced even when it's reached only via the cheap model's own
// fallback hop.

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// crossModelFallbackPriceTable is the same two-entry price table
// (gpt-4o-mini cheap, gpt-4o expensive) the corpus case's own verified
// repro used, reused byte-for-byte here rather than re-derived, so this
// test and that case's own recorded numbers can be cross-checked directly
// against each other.
func crossModelFallbackPriceTable() costaccounting.PriceTable {
	return costaccounting.PriceTable{
		"gpt-4o-mini": {
			PromptPerToken:     decimal.NewFromFloat(0.0000001),
			CompletionPerToken: decimal.NewFromFloat(0.0000004),
		},
		"gpt-4o": {
			PromptPerToken:     decimal.NewFromFloat(0.0000025),
			CompletionPerToken: decimal.NewFromFloat(0.00001),
		},
	}
}

// crossModelFallbackDeployments is a 2-deployment pipeline wired exactly
// like the corpus case's own repro: "primary-cheap" (model gpt-4o-mini,
// always fails in these tests) falls over via FallbackChains[generic] to
// "fallback-expensive" (model gpt-4o).
func crossModelFallbackDeployments() []Deployment {
	return []Deployment{
		{
			Name: "primary-cheap", Model: "gpt-4o-mini", Provider: "openai", UpstreamModel: "gpt-4o-mini", BaseURL: "http://unused",
			FallbackChains: map[string][]string{FallbackClassGeneric: {"fallback-expensive"}},
		},
		{Name: "fallback-expensive", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
}

// newCrossModelFallbackPipeline builds a Pipeline with
// crossModelFallbackDeployments, the real crossModelFallbackPriceTable,
// and perModel as test-key's PerModel rate-limit overrides — the shared
// setup for all three tests below.
func newCrossModelFallbackPipeline(t *testing.T, upstream UpstreamCaller, tracker *budget.Tracker, perModel map[string]ratelimit.ModelRateLimit) *Pipeline {
	t.Helper()
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := crossModelFallbackDeployments()
	limiter := ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{
		{ID: "test-key", Capacity: 100, RefillPerSecond: 100, PerModel: perModel},
	})
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        limiter,
		Budget:         tracker,
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(crossModelFallbackPriceTable()),
		Upstream:       upstream,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestHandleChatCompletionBillsCrossModelFallbackAtServedModelPrice is the
// load-bearing proof for
// costabuse-crossmodel-fallback-billed-at-requested-not-served-model-price:
// a request for the cheap "gpt-4o-mini" that falls over via
// fallback_chains to the expensive "gpt-4o" deployment must be billed at
// gpt-4o's own real price for the usage actually incurred — never at
// gpt-4o-mini's price, even though gpt-4o-mini is what the client asked
// for. fakeOpenAIResponse's usage (5 prompt + 3 completion tokens) is
// fixed, so the two possible dollar amounts are unambiguous:
// gpt-4o-mini's price would record $0.0000017; gpt-4o's real price
// records $0.0000425 — a ~25x difference that makes silently mispricing
// this immediately obvious in the assertion below, not a rounding-error
// scale difference that could be missed.
func TestHandleChatCompletionBillsCrossModelFallbackAtServedModelPrice(t *testing.T) {
	tracker := budget.NewTracker()
	var calls []string
	p := newCrossModelFallbackPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		calls = append(calls, dep.Name)
		if dep.Name == "primary-cheap" {
			return nil, &UpstreamHTTPError{StatusCode: 500, Body: "primary-cheap unavailable"}
		}
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, tracker, nil)

	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o-mini"})
	if err != nil {
		t.Fatalf("expected the fallback to succeed, got error: %v", err)
	}
	if resp.Model != "gpt-4o-mini" {
		t.Errorf("resp.Model = %q, want %q (echoed client-facing canonical model — unchanged by this fix)", resp.Model, "gpt-4o-mini")
	}
	if len(calls) != 2 || calls[0] != "primary-cheap" || calls[1] != "fallback-expensive" {
		t.Fatalf("calls = %v, want [primary-cheap fallback-expensive]", calls)
	}

	spent := tracker.SpentUSD("test-key", 0)
	wantServedPrice := decimal.NewFromFloat(0.0000425)    // 5*0.0000025 + 3*0.00001, gpt-4o's real price
	wantRequestedPrice := decimal.NewFromFloat(0.0000017) // 5*0.0000001 + 3*0.0000004, gpt-4o-mini's price — the pre-fix bug
	if spent.Equal(wantRequestedPrice) {
		t.Fatalf("spent = %s, billed at the REQUESTED model's (gpt-4o-mini) price — want billed at the model that GENUINELY served the response (gpt-4o), %s", spent, wantServedPrice)
	}
	if !spent.Equal(wantServedPrice) {
		t.Errorf("spent = %s, want %s (5 prompt + 3 completion tokens at gpt-4o's own real price)", spent, wantServedPrice)
	}
}

// TestHandleChatCompletionResponseModelReflectsRealServingModelOnFallback
// proves the sibling observability fix: the OTel span's
// gen_ai.response.model attribute must genuinely reflect the model that
// served the response (gpt-4o) on a cross-model fallback, not stay a
// silent duplicate of the request model (gpt-4o-mini) the way
// resp.Model's own client-facing echo (unchanged by this fix, per the
// assertion above) always does.
func TestHandleChatCompletionResponseModelReflectsRealServingModelOnFallback(t *testing.T) {
	before := len(spanRecorder.Ended())

	p := newCrossModelFallbackPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if dep.Name == "primary-cheap" {
			return nil, &UpstreamHTTPError{StatusCode: 500, Body: "primary-cheap unavailable"}
		}
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, budget.NewTracker(), nil)

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o-mini"}); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	spans := spansSince(before)
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "chat gpt-4o-mini" {
		t.Errorf("span name = %q, want %q (proves the REQUESTED model was gpt-4o-mini)", span.Name(), "chat gpt-4o-mini")
	}
	v, ok := spanAttr(t, span.Attributes(), telemetry.AttrGenAIResponseModel)
	if !ok {
		t.Fatal("gen_ai.response.model attribute not set at all")
	}
	if v.AsString() == "gpt-4o-mini" {
		t.Fatalf("%s = %q, a silent duplicate of the requested model — want %q, the model that GENUINELY served the response after the fallback hop", telemetry.AttrGenAIResponseModel, v.AsString(), "gpt-4o")
	}
	if v.AsString() != "gpt-4o" {
		t.Errorf("%s = %q, want %q", telemetry.AttrGenAIResponseModel, v.AsString(), "gpt-4o")
	}
}

// TestHandleChatCompletionFallbackHopSkipsRateLimitedTargetButChainStillSucceeds
// is the load-bearing proof for
// costabuse-permodel-ratelimit-bypassed-via-crossmodel-fallback-chain: a
// fallback_chains hop to a deployment whose PerModel RPM cap is already
// exhausted for this exact virtual key must be skipped — never reached,
// confirmed via the mock upstream's own call log — while the chain still
// walks on to a THIRD, unaffected target and the request ultimately
// succeeds. Proves both halves in one test: the skip itself, and that the
// skip doesn't break an otherwise-healthy multi-hop chain.
func TestHandleChatCompletionFallbackHopSkipsRateLimitedTargetButChainStillSucceeds(t *testing.T) {
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{
		{
			Name: "primary-cheap", Model: "gpt-4o-mini", Provider: "openai", UpstreamModel: "gpt-4o-mini", BaseURL: "http://unused",
			FallbackChains: map[string][]string{FallbackClassGeneric: {"gpt-4o-ratelimited", "claude-fallback"}},
		},
		{Name: "gpt-4o-ratelimited", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "claude-fallback", Model: "claude-opus-4", Provider: "openai", UpstreamModel: "claude-opus-4", BaseURL: "http://unused"},
	}
	limiter := ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{
		{
			ID: "test-key", Capacity: 100, RefillPerSecond: 100,
			PerModel: map[string]ratelimit.ModelRateLimit{"gpt-4o": {Capacity: 1, RefillPerSecond: 0}},
		},
	})

	var calls []string
	p, err := NewPipeline(Config{
		Verifier: verifier,
		Limiter:  limiter,
		Budget:   budget.NewTracker(),
		Cache:    inprocess.New(0), CacheL2: inprocess.New(0), CacheL3: inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			calls = append(calls, dep.Name)
			if dep.Name == "gpt-4o-ratelimited" {
				t.Fatal("gpt-4o-ratelimited must never be called — its PerModel RPM cap is already exhausted for this virtual key")
			}
			if dep.Name == "primary-cheap" {
				return nil, &UpstreamHTTPError{StatusCode: 500, Body: "primary-cheap unavailable"}
			}
			return fakeOpenAIResponse(dep.UpstreamModel), nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	// Exhaust gpt-4o's own 1-capacity PerModel override directly — the
	// exact same "confirmed independently" methodology the corpus case's
	// own repro used, never a real HandleChatCompletion call against
	// gpt-4o (which would itself count as a real, billed attempt).
	allowed, err := p.limiter.AllowForModel(context.Background(), "test-key", "gpt-4o")
	if err != nil || !allowed {
		t.Fatalf("setup: first direct AllowForModel(gpt-4o) = (%v, %v), want (true, nil)", allowed, err)
	}
	if allowed, err := p.limiter.AllowForModel(context.Background(), "test-key", "gpt-4o"); err != nil || allowed {
		t.Fatalf("setup: second direct AllowForModel(gpt-4o) = (%v, %v), want (false, nil) — exhaustion must be real before the real test request runs", allowed, err)
	}

	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o-mini"})
	if err != nil {
		t.Fatalf("expected the chain to still succeed at claude-fallback, got error: %v", err)
	}
	if resp.Model != "gpt-4o-mini" {
		t.Errorf("resp.Model = %q, want %q", resp.Model, "gpt-4o-mini")
	}
	if len(calls) != 2 || calls[0] != "primary-cheap" || calls[1] != "claude-fallback" {
		t.Fatalf("calls = %v, want [primary-cheap claude-fallback] — gpt-4o-ratelimited skipped entirely, chain continues to the next target", calls)
	}
}
