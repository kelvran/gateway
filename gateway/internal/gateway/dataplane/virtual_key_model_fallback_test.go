package dataplane

// Regression proof for a real, confirmed gap a whole-session
// completeness sweep found on 2026-09-27: unlike AllowedRegions (fixed
// the same day, commit 1560d8cb), AllowedModels was checked exactly
// once per request, against the client's ORIGINALLY-REQUESTED model
// (req.Model) -- never re-checked against an explicit fallback_chains
// target's own canonical Model, even though attemptFallbackChain
// already composes an analogous per-target rateLimitOK check keyed to
// "the candidate's own Model" specifically to close an earlier,
// identically-shaped cross-model bypass (the PerModel-RPM one, see
// crossmodel_fallback_billing_test.go). A virtual key restricted to
// AllowedModels={gpt-4o-mini} could be served by a fallback_chains
// target whose own Model is gpt-4o -- not in that allowlist at all --
// with zero enforcement. The legacy (no fallback_chains configured)
// branch and HandleEmbeddings were independently confirmed NOT to have
// this gap (both are structurally scoped to req.Model's own pool via
// nextEligibleDeployment's first argument) -- this file covers only
// the one real gap, the explicit fallback_chains hop.

import (
	"context"
	"errors"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

// TestAttemptFallbackChainSkipsDisallowedModelTarget is the unit-level
// proof, mirroring virtual_key_region_test.go's own
// TestAttemptFallbackChainSkipsOutOfRegionTarget exactly (same fixture
// shape, same assertion structure) for AllowedModels instead of
// AllowedRegions.
func TestAttemptFallbackChainSkipsDisallowedModelTarget(t *testing.T) {
	p := &Pipeline{deploymentsByName: map[string]Deployment{
		"b": {Name: "b", Model: "gpt-4o"},
		"c": {Name: "c", Model: "gpt-4o-mini"},
	}}
	vk := &identity.VirtualKey{AllowedModels: map[string]struct{}{"gpt-4o-mini": {}}}

	var modelChecked []string
	modelOK := func(d Deployment) bool {
		modelChecked = append(modelChecked, d.Name)
		return isModelAllowed(vk, d.Model)
	}

	var calls []string
	call := func(d Deployment) (adapter.ChatResponse, error) {
		calls = append(calls, d.Name)
		return adapter.ChatResponse{Model: "served-by-" + d.Name}, nil
	}

	_, resp, err, attempted := p.attemptFallbackChain(context.Background(), []string{"b", "c"}, map[string]bool{"a": true}, call, func() bool { return false }, alwaysAllowRateLimit, alwaysAllowDeploymentCapacity, p.releaseDeploymentConcurrency, alwaysAllowCapability, alwaysAllowRegion, modelOK)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !attempted {
		t.Fatal("attempted = false, want true")
	}
	if resp.Model != "served-by-c" {
		t.Errorf("resp.Model = %q, want served-by-c (b's model gpt-4o is not in AllowedModels and must never be attempted)", resp.Model)
	}
	if len(calls) != 1 || calls[0] != "c" {
		t.Fatalf("calls = %v, want [c] only -- b must be skipped, never attempted", calls)
	}
	if len(modelChecked) != 2 || modelChecked[0] != "b" || modelChecked[1] != "c" {
		t.Fatalf("modelChecked = %v, want [b c] -- both candidates checked in order", modelChecked)
	}
}

// TestHandleChatCompletionFallbackRespectsAllowedModelsConstraint is the
// end-to-end proof, reusing crossmodel_fallback_billing_test.go's own
// real 2-deployment cross-model fixture (crossModelFallbackDeployments)
// via newRegionConstraintPipeline's own general-purpose harness (its
// name predates this file -- it builds an ordinary Pipeline from any
// virtual key + deployment set, nothing region-specific about its
// mechanics) -- proof that this exact, already-exercised configuration
// shape (a cheap model falling back to a real, different, more
// expensive model) is genuinely blocked end to end for a virtual key
// whose AllowedModels doesn't include the fallback target, rather than
// silently served.
func TestHandleChatCompletionFallbackRespectsAllowedModelsConstraint(t *testing.T) {
	authCred := "model-restricted-fallback-cred"
	vk := identity.VirtualKey{
		ID: "model-restricted-fallback-key", KeyHash: testHashOf(authCred),
		AllowedModels:  map[string]struct{}{"gpt-4o-mini": {}},
		RateLimitBurst: 100, RateLimitRefill: 100,
	}
	p := newRegionConstraintPipeline(t, vk, authCred, crossModelFallbackDeployments(), func(ctx context.Context, dep Deployment, req any) (any, error) {
		if dep.Name == "primary-cheap" {
			return nil, errors.New("primary-cheap unavailable, forcing a fallback")
		}
		t.Fatal("fallback-expensive (model gpt-4o) was called -- it is NOT in AllowedModels and must never be attempted")
		return nil, nil
	})

	_, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "", adapter.ChatRequest{Model: "gpt-4o-mini", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}, "")
	if err == nil {
		t.Fatal("HandleChatCompletion: nil error, want the original upstream error to propagate -- the only fallback candidate's model isn't allowed and must be skipped, not silently served")
	}
}
