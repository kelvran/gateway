package router

import "testing"

// Read-only getters added for GET /admin/deployments (RFC-3 decision 5,
// slice (c)): the live per-deployment weight, the latency de-weighting
// percentage, and the configured sticky flag, read without disturbing the
// selection state.

func TestWeightReportsConfiguredThenLiveWeight(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "m", Weight: 3},
		{Name: "b", Model: "m"}, // zero: the router treats it as 1
		{Name: "c", Model: "other", Weight: 2},
	}, HealthConfig{})
	if w, ok := r.Weight("a"); !ok || w != 3 {
		t.Errorf("Weight(a) = (%d, %v), want (3, true)", w, ok)
	}
	if w, ok := r.Weight("b"); !ok || w != 1 {
		t.Errorf("Weight(b) = (%d, %v), want the resolved default (1, true)", w, ok)
	}
	if err := r.SetWeight("m", "a", 7); err != nil {
		t.Fatalf("SetWeight: %v", err)
	}
	if w, _ := r.Weight("a"); w != 7 {
		t.Errorf("Weight(a) after SetWeight = %d, want 7", w)
	}
	if w, _ := r.Weight("c"); w != 2 {
		t.Errorf("Weight(c) in another model group = %d, want 2", w)
	}
	if _, ok := r.Weight("nope"); ok {
		t.Error("Weight of an unknown deployment must report ok=false")
	}
}

func TestLatencyFactorPercentReportsZeroUntilSetThenTheClampedValue(t *testing.T) {
	r := New([]Deployment{{Name: "a", Model: "m"}}, HealthConfig{})
	if got := r.LatencyFactorPercent("a"); got != 0 {
		t.Errorf("LatencyFactorPercent before any signal = %d, want 0 (no de-weighting)", got)
	}
	r.SetLatencyFactor("a", 60)
	if got := r.LatencyFactorPercent("a"); got != 60 {
		t.Errorf("LatencyFactorPercent after SetLatencyFactor(60) = %d, want 60", got)
	}
	r.SetLatencyFactor("a", 500)
	if got := r.LatencyFactorPercent("a"); got != 100 {
		t.Errorf("LatencyFactorPercent after SetLatencyFactor(500) = %d, want the clamp 100", got)
	}
	r.SetLatencyFactor("a", 1)
	if got := r.LatencyFactorPercent("a"); got != latencyFactorFloorPercent {
		t.Errorf("LatencyFactorPercent after SetLatencyFactor(1) = %d, want the floor %d", got, latencyFactorFloorPercent)
	}
	if got := r.LatencyFactorPercent("unknown"); got != 0 {
		t.Errorf("LatencyFactorPercent of an unknown deployment = %d, want 0", got)
	}
}

func TestIsStickyReportsTheConfiguredFlag(t *testing.T) {
	r := New([]Deployment{{Name: "a", Model: "m", Sticky: true}, {Name: "b", Model: "m"}}, HealthConfig{})
	if !r.IsSticky("a") || r.IsSticky("b") || r.IsSticky("unknown") {
		t.Errorf("IsSticky: a=%v b=%v unknown=%v, want true/false/false", r.IsSticky("a"), r.IsSticky("b"), r.IsSticky("unknown"))
	}
}
