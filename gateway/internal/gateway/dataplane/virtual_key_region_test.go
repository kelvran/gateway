package dataplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

	_, resp, err, attempted := p.attemptFallbackChain(context.Background(), []string{"b", "c"}, map[string]bool{"a": true}, call, func() bool { return false }, alwaysAllowRateLimit, alwaysAllowDeploymentCapacity, alwaysAllowCapability, regionOK)
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

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, regionConstraintChatRequest()); err != nil {
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

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, regionConstraintChatRequest()); err != nil {
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

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, regionConstraintChatRequest()); err != nil {
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

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, regionConstraintChatRequest()); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if served != "eu-alt" {
		t.Errorf("served by %q, want eu-alt -- the fallback chain must skip the out-of-region us-alt target entirely", served)
	}
}
