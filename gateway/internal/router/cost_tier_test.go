package router

import "testing"

// TestSelectIgnoresCostTierWhenAnyDeploymentInGroupIsUntiered is the
// critical backward-compat proof, per Deployment.CostTier's own doc
// comment: tier filtering is a strict opt-in — a model group with even
// ONE untiered deployment (CostTier <= 0, the default, and every
// deployment configured before this feature existed) must be
// byte-for-byte unaffected, degrading to the exact same plain WRR
// sequence TestSelectDegradesToRoundRobinForEqualWeights already proves.
func TestSelectIgnoresCostTierWhenAnyDeploymentInGroupIsUntiered(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o", CostTier: 1},
		{Name: "b", Model: "gpt-4o"}, // untiered — disables filtering for the WHOLE group
		{Name: "c", Model: "gpt-4o", CostTier: 2},
	}, HealthConfig{})

	want := []string{"a", "b", "c", "a", "b", "c"}
	for i, w := range want {
		got, ok := r.Select("gpt-4o")
		if !ok || got != w {
			t.Fatalf("call %d: Select = (%q, %v), want (%q, true) — a mixed tiered/untiered group must behave exactly like plain WRR", i, got, ok, w)
		}
	}
}

// TestSelectPrefersLowestHealthyCostTier proves the real feature: once
// every deployment in a group is tiered, Select prefers the lowest tier
// with a currently-healthy deployment, never offering a more expensive
// tier while a cheaper one is available.
func TestSelectPrefersLowestHealthyCostTier(t *testing.T) {
	r := New([]Deployment{
		{Name: "cheap", Model: "gpt-4o", CostTier: 1},
		{Name: "expensive", Model: "gpt-4o", CostTier: 2},
	}, HealthConfig{})

	for i := 0; i < 10; i++ {
		got, ok := r.Select("gpt-4o")
		if !ok || got != "cheap" {
			t.Fatalf("call %d: Select = (%q, %v), want (%q, true) — the cheaper tier is healthy, must always be preferred", i, got, ok, "cheap")
		}
	}
}

// TestSelectFallsThroughToNextTierWhenCheaperTierFullyUnhealthy proves
// the fallthrough half: once every deployment in the cheapest tier is
// unhealthy, Select falls through to the next-cheapest tier that still
// has a healthy deployment — never fails open prematurely while a real,
// merely-more-expensive alternative exists.
func TestSelectFallsThroughToNextTierWhenCheaperTierFullyUnhealthy(t *testing.T) {
	r := New([]Deployment{
		{Name: "cheap", Model: "gpt-4o", CostTier: 1},
		{Name: "expensive", Model: "gpt-4o", CostTier: 2},
	}, HealthConfig{UnhealthyThreshold: 1})

	r.ReportProbeResult("cheap", false) // trips UnhealthyThreshold=1 immediately

	for i := 0; i < 10; i++ {
		got, ok := r.Select("gpt-4o")
		if !ok || got != "expensive" {
			t.Fatalf("call %d: Select = (%q, %v), want (%q, true) — the cheap tier is fully unhealthy, must fall through to the next tier", i, got, ok, "expensive")
		}
	}
}

// TestSelectProportionalWeightingHoldsWithinAPreferredCostTier proves
// tier filtering composes with, rather than overrides, weighted
// selection: among the multiple deployments sharing the preferred
// (cheapest healthy) tier, TestSelectProportionalForWeightedDeployments'
// own exact-count guarantee still holds.
func TestSelectProportionalWeightingHoldsWithinAPreferredCostTier(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o", Weight: 2, CostTier: 1},
		{Name: "b", Model: "gpt-4o", Weight: 1, CostTier: 1},
		{Name: "expensive", Model: "gpt-4o", Weight: 1, CostTier: 2},
	}, HealthConfig{})

	counts := map[string]int{}
	const totalCalls = 300 // a multiple of tier 1's own total weight (3)
	for i := 0; i < totalCalls; i++ {
		got, ok := r.Select("gpt-4o")
		if !ok {
			t.Fatalf("call %d: Select returned ok=false", i)
		}
		counts[got]++
	}

	if counts["expensive"] != 0 {
		t.Errorf("counts[expensive] = %d, want 0 — tier 1 is fully healthy, tier 2 must never be offered", counts["expensive"])
	}
	if want := totalCalls * 2 / 3; counts["a"] != want {
		t.Errorf("counts[a] = %d, want %d (weight 2 of tier 1's total weight 3)", counts["a"], want)
	}
	if want := totalCalls * 1 / 3; counts["b"] != want {
		t.Errorf("counts[b] = %d, want %d (weight 1 of tier 1's total weight 3)", counts["b"], want)
	}
}

// TestSelectPrefersARealAdmittedFallbackOverAnUnhealthyLastExaminedCandidate
// is the load-bearing safety proof for selectHealthy's tier-filtering
// fallback path. A tier-mismatched candidate must still be admitTurn-
// checked (never skipped via a bare tier-mismatch continue BEFORE
// admission is checked) -- otherwise the loop's exhaustion fallback can
// return the literal last-examined candidate without ever having been
// health-checked this call, even when a genuinely admitTurn-passing
// candidate was examined earlier in the exact same cycle.
//
// Scenario, matching real WRR cursor order for 3 equal-weight
// deployments (cheap, expensiveHealthy, expensiveDown, visited in that
// exact order within one Select call, per
// TestSelectDegradesToRoundRobinForEqualWeights's own proof):
//   - "cheap" (tier 1, the preferred tier since it's genuinely healthy)
//     is mid-ramp immediately after recovering -- its one offer this
//     cycle is admitTurn-rejected (rampCredit starts at 0, +20% < 100%).
//   - "expensiveHealthy" (tier 2, wrong tier) is genuinely healthy --
//     admitTurn passes, but the tier mismatch means it can't be
//     returned immediately.
//   - "expensiveDown" (tier 2, wrong tier) is genuinely UNHEALTHY --
//     admitTurn correctly rejects it.
//
// The only safe return value here is "expensiveHealthy" -- the one
// candidate this cycle that actually passed a real health check.
func TestSelectPrefersARealAdmittedFallbackOverAnUnhealthyLastExaminedCandidate(t *testing.T) {
	r := New([]Deployment{
		{Name: "cheap", Model: "gpt-4o", CostTier: 1},
		{Name: "expensiveHealthy", Model: "gpt-4o", CostTier: 2},
		{Name: "expensiveDown", Model: "gpt-4o", CostTier: 2},
	}, HealthConfig{UnhealthyThreshold: 1, HealthyThreshold: 1, RecoveryRampSteps: 4, RecoveryRampInitialPercent: 20})

	r.ReportProbeResult("cheap", false) // ejects (UnhealthyThreshold=1)
	r.ReportProbeResult("cheap", true)  // 1 of 1 required success -> recovers, ramp starts at 20%

	r.ReportProbeResult("expensiveDown", false) // genuinely unhealthy (UnhealthyThreshold=1)

	got, ok := r.Select("gpt-4o")
	if !ok {
		t.Fatalf("Select returned ok=false, want true")
	}
	if got != "expensiveHealthy" {
		t.Fatalf("Select = %q, want %q — a real, admitTurn-passing candidate examined this same cycle must be preferred over the raw last-examined (unhealthy) candidate", got, "expensiveHealthy")
	}
}
