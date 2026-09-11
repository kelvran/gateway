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
