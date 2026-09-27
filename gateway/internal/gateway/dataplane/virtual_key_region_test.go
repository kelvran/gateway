package dataplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
)

// --- isRegionAllowed unit tests ---

func TestIsRegionAllowedNoConstraintAllowsEverything(t *testing.T) {
	vk := &identity.VirtualKey{}
	if !isRegionAllowed(vk, "eu-west-1") {
		t.Error("isRegionAllowed with no AllowedRegions constraint = false, want true")
	}
	if !isRegionAllowed(vk, "") {
		t.Error("isRegionAllowed with no constraint against an empty region = false, want true")
	}
}

func TestIsRegionAllowedMatchingRegion(t *testing.T) {
	vk := &identity.VirtualKey{AllowedRegions: map[string]struct{}{"eu-west-1": {}, "eu-central-1": {}}}
	if !isRegionAllowed(vk, "eu-west-1") {
		t.Error("isRegionAllowed(eu-west-1) = false, want true (in the allowed set)")
	}
	if isRegionAllowed(vk, "us-east-1") {
		t.Error("isRegionAllowed(us-east-1) = true, want false (not in the allowed set)")
	}
}

// TestIsRegionAllowedEmptyRegionFailsClosedAgainstARealConstraint proves
// the deliberate divergence from isModelAllowed's own shape: a deployment
// with no region at all (every non-Bedrock provider today) does NOT
// satisfy a real AllowedRegions constraint, per
// identity.VirtualKey.AllowedRegions' own doc comment.
func TestIsRegionAllowedEmptyRegionFailsClosedAgainstARealConstraint(t *testing.T) {
	vk := &identity.VirtualKey{AllowedRegions: map[string]struct{}{"eu-west-1": {}}}
	if isRegionAllowed(vk, "") {
		t.Error("isRegionAllowed(\"\") against a real constraint = true, want false (fail closed on an unknown region)")
	}
}

// --- attemptFallbackChain unit test, mirroring
// TestAttemptFallbackChainSkipsTargetLackingRequiredCapability's own
// pattern exactly ---

// TestAttemptFallbackChainSkipsOutOfRegionTarget is the load-bearing
// proof for the confirmed finding in
// docs/upgrade-research/data-residency-regional-routing-2026-09-15.md: a
// fallback target outside vk's own AllowedRegions is skipped exactly
// like a router-unhealthy, rate-limited, capacity-constrained, or
// incapable one -- never attempted, never charged against
// consecutiveFailures/realAttempts.
func TestAttemptFallbackChainSkipsOutOfRegionTarget(t *testing.T) {
	p := &Pipeline{deploymentsByName: map[string]Deployment{
		"b": {Name: "b", Region: "us-east-1"},
		"c": {Name: "c", Region: "eu-west-1"},
	}}
	vk := &identity.VirtualKey{AllowedRegions: map[string]struct{}{"eu-west-1": {}}}

	var regionChecked []string
	regionOK := func(d Deployment) bool {
		regionChecked = append(regionChecked, d.Name)
		return isRegionAllowed(vk, d.Region)
	}

	var calls []string
	call := func(d Deployment) (adapter.ChatResponse, error) {
		calls = append(calls, d.Name)
		return adapter.ChatResponse{Model: "served-by-" + d.Name}, nil
	}

	_, resp, err, attempted := p.attemptFallbackChain(context.Background(), []string{"b", "c"}, map[string]bool{"a": true}, call, func() bool { return false }, alwaysAllowRateLimit, alwaysAllowDeploymentCapacity, p.releaseDeploymentConcurrency, alwaysAllowCapability, regionOK)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !attempted {
		t.Fatal("attempted = false, want true")
	}
	if resp.Model != "served-by-c" {
		t.Errorf("resp.Model = %q, want served-by-c (b is out of region and must never be attempted)", resp.Model)
	}
	if len(calls) != 1 || calls[0] != "c" {
		t.Fatalf("calls = %v, want [c] only -- b must be skipped, never attempted", calls)
	}
	if len(regionChecked) != 2 || regionChecked[0] != "b" || regionChecked[1] != "c" {
		t.Fatalf("regionChecked = %v, want [b c] -- both candidates checked in order", regionChecked)
	}
}

// --- End-to-end dataplane tests: first-pick reroute + fallback, via the
// real HandleChatCompletion path ---

func regionConstraintChatRequest() adapter.ChatRequest {
	return adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
}

// newRegionConstraintPipeline builds a two-deployment Pipeline for the
// same canonical model, in two different regions, with a fake Upstream
// closure recording which deployment actually served the call --
// mirroring bedrock_structured_output_test.go's own
// newBedrockStructuredOutputPipelineTwoDeployments harness pattern.
func newRegionConstraintPipeline(t *testing.T, vk identity.VirtualKey, authCred string, deployments []Deployment, upstream UpstreamCaller) *Pipeline {
	t.Helper()
	verifier, err := identity.NewVerifier([]identity.VirtualKey{vk})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys([]identity.VirtualKey{vk})),
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
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestHandleChatCompletionFirstPickRerouteRespectsRegionConstraint proves
// rerouteToCapableDeploymentIfNeeded's region half end to end: when
// req.Model's own deployment pool contains an in-region deployment, the
// FIRST attempt must land on it -- even if WRR's own cursor picked the
// out-of-region one -- never a hard error, never a silent region-boundary
// crossing.
func TestHandleChatCompletionFirstPickRerouteRespectsRegionConstraint(t *testing.T) {
	authCred := "region-constrained-cred"
	vk := identity.VirtualKey{
		ID: "region-key", KeyHash: testHashOf(authCred),
		AllowedRegions: map[string]struct{}{"eu-west-1": {}},
		RateLimitBurst: 100, RateLimitRefill: 100,
	}
	deployments := []Deployment{
		{Name: "us", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", Region: "us-east-1"},
		{Name: "eu", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", Region: "eu-west-1"},
	}
	var served string
	p := newRegionConstraintPipeline(t, vk, authCred, deployments, func(ctx context.Context, dep Deployment, req any) (any, error) {
		served = dep.Name
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	})

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "", regionConstraintChatRequest(), ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if served != "eu" {
		t.Errorf("served by %q, want eu -- the first pick must be rerouted to the only in-region deployment regardless of WRR's own cursor", served)
	}
}

// TestHandleChatCompletionRegionConstraintWithNoEligibleDeploymentStillSucceeds
// proves the negative, mirroring
// TestHandleChatCompletionSilentlyOmitsStructuredOutputEnforcementForUnsupportedBedrockModelOnFirstAttempt's
// own "never a hard error" contract: when NO deployment in the pool
// satisfies vk's own AllowedRegions, the first pick is used unchanged --
// this is a best-effort reroute, not an enforcement mechanism that can
// ever reject a request outright.
func TestHandleChatCompletionRegionConstraintWithNoEligibleDeploymentStillSucceeds(t *testing.T) {
	authCred := "region-constrained-no-match-cred"
	vk := identity.VirtualKey{
		ID: "region-key-no-match", KeyHash: testHashOf(authCred),
		AllowedRegions: map[string]struct{}{"ap-southeast-1": {}},
		RateLimitBurst: 100, RateLimitRefill: 100,
	}
	deployments := []Deployment{
		{Name: "us", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", Region: "us-east-1"},
	}
	p := newRegionConstraintPipeline(t, vk, authCred, deployments, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	})

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "", regionConstraintChatRequest(), ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v, want a normal successful response -- a region constraint with no eligible deployment is a best-effort no-op, never a hard error", err)
	}
}

// TestHandleChatCompletionUnconstrainedKeyUnaffectedByRegions is
// regression coverage: a virtual key with no AllowedRegions set at all
// must behave exactly as before this feature existed, regardless of
// which deployment WRR happens to pick.
func TestHandleChatCompletionUnconstrainedKeyUnaffectedByRegions(t *testing.T) {
	authCred := "region-unconstrained-cred"
	vk := identity.VirtualKey{ID: "unconstrained-key", KeyHash: testHashOf(authCred), RateLimitBurst: 100, RateLimitRefill: 100}
	deployments := []Deployment{
		{Name: "us", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", Region: "us-east-1"},
		{Name: "eu", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", Region: "eu-west-1"},
	}
	p := newRegionConstraintPipeline(t, vk, authCred, deployments, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	})

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "", regionConstraintChatRequest(), ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v, want success -- an unconstrained key must be unaffected", err)
	}
}

// TestHandleChatCompletionFallbackRespectsRegionConstraint proves the
// attemptFallbackChain half end to end: when the first-picked deployment
// fails and a fallback_chains target exists but sits outside vk's own
// AllowedRegions, the fallback must skip it -- exactly the confirmed live
// bug from docs/upgrade-research/data-residency-regional-routing-2026-09-15.md,
// now proven fixed through the real pipeline, not just the fallback.go
// unit test above.
func TestHandleChatCompletionFallbackRespectsRegionConstraint(t *testing.T) {
	authCred := "region-fallback-cred"
	vk := identity.VirtualKey{
		ID: "region-fallback-key", KeyHash: testHashOf(authCred),
		AllowedRegions: map[string]struct{}{"eu-west-1": {}},
		RateLimitBurst: 100, RateLimitRefill: 100,
	}
	deployments := []Deployment{
		{
			Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", Region: "eu-west-1",
			FallbackChains: map[string][]string{FallbackClassGeneric: {"us-alt", "eu-alt"}},
		},
		{Name: "us-alt", Model: "gpt-4.1", Provider: "openai", UpstreamModel: "gpt-4.1", BaseURL: "http://unused", Region: "us-east-1"},
		{Name: "eu-alt", Model: "gpt-4.1", Provider: "openai", UpstreamModel: "gpt-4.1", BaseURL: "http://unused", Region: "eu-west-1"},
	}
	var served string
	p := newRegionConstraintPipeline(t, vk, authCred, deployments, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if dep.Name == "primary" {
			return nil, errors.New("primary deployment failing, forcing a fallback")
		}
		served = dep.Name
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	})

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "", regionConstraintChatRequest(), ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if served != "eu-alt" {
		t.Errorf("served by %q, want eu-alt -- the fallback chain must skip the out-of-region us-alt target entirely", served)
	}
}

// TestHandleChatCompletionLegacyFallbackRespectsRegionConstraint is
// TestHandleChatCompletionFallbackRespectsRegionConstraint's sibling for
// the OLD-style, no-FallbackChains-configured branch -- a real,
// pre-existing gap (not introduced by this fix; unrelated to the
// commits that added AllowedRegions itself) a 2026-09-27 holistic
// review found: unlike the first pick (rerouteToCapableDeploymentIfNeeded)
// and the explicit fallback_chains hop (attemptFallbackChain's own
// regionOK closure, proven above), the legacy fallback branch
// (runMissPath's "else if fallbackDep, hasFallback :=
// p.nextEligibleDeployment(...)" branch) previously called plain
// nextDeployment with no region filter at all, so a data-residency-
// restricted key could silently land on an out-of-region deployment on
// an ordinary transient upstream error -- no attacker action needed.
func TestHandleChatCompletionLegacyFallbackRespectsRegionConstraint(t *testing.T) {
	authCred := "region-legacy-fallback-cred"
	vk := identity.VirtualKey{
		ID: "region-legacy-fallback-key", KeyHash: testHashOf(authCred),
		AllowedRegions: map[string]struct{}{"eu-west-1": {}},
		RateLimitBurst: 100, RateLimitRefill: 100,
	}
	// Deliberately NO FallbackChains on "primary" -- its own upstream
	// failure must take the legacy branch, not attemptFallbackChain.
	deployments := []Deployment{
		{Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", Region: "eu-west-1"},
		{Name: "us-alt", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", Region: "us-east-1"},
		{Name: "eu-alt", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", Region: "eu-west-1"},
	}
	var served string
	p := newRegionConstraintPipeline(t, vk, authCred, deployments, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if dep.Name == "primary" {
			return nil, errors.New("primary deployment failing, forcing the legacy fallback branch")
		}
		served = dep.Name
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	})

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "", regionConstraintChatRequest(), ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if served != "eu-alt" {
		t.Errorf("served by %q, want eu-alt -- the legacy fallback branch must skip the out-of-region us-alt target entirely", served)
	}
}

// TestHandleChatCompletionLegacyFallbackSkipsEntirelyWhenNoEligibleTargetExists
// proves the degenerate case's own safety direction: when EVERY
// alternate deployment is out-of-region, the legacy fallback must not
// happen at all (never silently violate the constraint by falling
// through to an ineligible target anyway) -- the original upstream
// error must propagate instead. This is deliberately stricter than
// rerouteToCapableDeploymentIfNeeded's own first-pick contract (which
// falls through to the ineligible original when nothing better exists,
// a documented v1 scope limit) -- a fallback HOP, unlike the first pick,
// has a real alternative (don't fall back at all) that doesn't require
// either rejecting every request outright or silently proceeding.
func TestHandleChatCompletionLegacyFallbackSkipsEntirelyWhenNoEligibleTargetExists(t *testing.T) {
	authCred := "region-legacy-fallback-no-eligible-cred"
	vk := identity.VirtualKey{
		ID: "region-legacy-fallback-no-eligible-key", KeyHash: testHashOf(authCred),
		AllowedRegions: map[string]struct{}{"eu-west-1": {}},
		RateLimitBurst: 100, RateLimitRefill: 100,
	}
	deployments := []Deployment{
		{Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", Region: "eu-west-1"},
		{Name: "us-alt", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", Region: "us-east-1"},
	}
	var usAltCalled bool
	p := newRegionConstraintPipeline(t, vk, authCred, deployments, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if dep.Name == "primary" {
			return nil, errors.New("primary deployment failing")
		}
		usAltCalled = true
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	})

	_, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "", regionConstraintChatRequest(), "")
	if err == nil {
		t.Fatal("HandleChatCompletion: nil error, want the original upstream error to propagate -- the only fallback candidate is out-of-region and must be skipped, not silently served")
	}
	if usAltCalled {
		t.Error("the out-of-region fallback target was called -- it must be skipped outright, never selected")
	}
}

// TestHandleEmbeddingsRerouteRespectsRegionConstraint is
// TestHandleChatCompletionFirstPickRerouteRespectsRegionConstraint's
// embeddings-path sibling -- a second, distinct real gap the same
// 2026-09-27 review found: HandleEmbeddings' own sole deployment pick
// had NO region check at all, unlike HandleChatCompletion's first pick.
// Unlike the chat-completion legacy-fallback bug, this one needs no
// transient upstream error to trigger -- every embeddings request from
// a region-restricted key hit it unconditionally.
func TestHandleEmbeddingsRerouteRespectsRegionConstraint(t *testing.T) {
	authCred := "region-embeddings-cred"
	keys := []identity.VirtualKey{{
		ID: "region-embeddings-key", KeyHash: testHashOf(authCred),
		AllowedRegions: map[string]struct{}{"eu-west-1": {}},
		RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("1000"),
	}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{
		{Name: "us-emb", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding", Region: "us-east-1"},
		{Name: "eu-emb", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding", Region: "eu-west-1"},
	}
	var served string
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
			t.Fatal("chat Upstream should never be called by an embeddings test")
			return nil, nil
		},
		EmbeddingUpstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			served = dep.Name
			return &openai.EmbeddingResponseWire{
				Model: "text-embedding-3-small",
				Data:  []openai.EmbeddingDataWire{{Index: 0, Embedding: []float64{0.1, 0.2, 0.3}}},
				Usage: openai.EmbeddingUsageWire{PromptTokens: 2, TotalTokens: 2},
			}, nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	if _, err := p.HandleEmbeddings(context.Background(), "Bearer "+authCred, "", adapter.EmbeddingRequest{
		Model: "text-embedding-3-small", Input: []string{"hello"},
	}); err != nil {
		t.Fatalf("HandleEmbeddings: %v", err)
	}
	if served != "eu-emb" {
		t.Errorf("served by %q, want eu-emb -- the router's own first pick landed on the out-of-region us-emb, and it must be rerouted", served)
	}
}
