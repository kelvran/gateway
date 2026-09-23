package router

import "testing"

// These tests are the load-bearing proof for SetLatencyFactor/
// admitLatencyThinnedTurn — closing the self-hosted-inference-server-
// integration-depth-2026-09-22.md research finding that Select uses
// only static weight, health, and ramp state, with zero load/latency
// signal from self-hosted backends. Mirrors recovery_ramp_test.go's own
// rigor exactly: real, exact counts over many Select calls (the
// underlying Bresenham-style accumulator has no randomness), not a
// statistical/tolerance-based approximation.

// TestSelectReducesShareForLatencyDeweightedDeployment proves the
// headline property: a deployment with a latency factor set well below
// 100 receives dramatically less traffic than its equal configured
// Weight would otherwise imply — never a hard exclusion (it still gets
// SOME turns), but a clear, measurable reduction.
func TestSelectReducesShareForLatencyDeweightedDeployment(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
	}, HealthConfig{})
	r.ReportProbeResult("a", true)
	r.ReportProbeResult("b", true)
	r.SetLatencyFactor("a", 33) // "a" is roughly 3x slower than its peer.

	const totalCalls = 600
	counts := map[string]int{}
	for i := 0; i < totalCalls; i++ {
		name, ok := r.Select("gpt-4o", nil)
		if !ok {
			t.Fatalf("call %d: Select returned ok=false, want true", i)
		}
		counts[name]++
	}

	// With "a" and "b" at equal configured weight, "a"'s share with no
	// de-weighting at all would be 300/600 (50%). At a 33% latency
	// factor, admitLatencyThinnedTurn's accumulator composes with
	// selectHealthy's own "a rejected turn falls through to b in the
	// same outer call" behavior (the identical composition effect
	// recovery_ramp_test.go's own tests document) into an exact
	// 149/600 (~25%) steady state, not the naively-expected 1-in-3
	// (200/600).
	if got := counts["a"]; got != 149 {
		t.Errorf(`counts["a"] = %d, want 149 (600 calls at a 33%% latency factor — dramatically less than "a"'s configured 50%% share)`, got)
	}
	if got := counts["b"]; got != 451 {
		t.Errorf(`counts["b"] = %d, want 451`, got)
	}
}

// TestSelectNeverFullyStarvesALatencyDeweightedDeployment proves the
// soft-de-weighting contract: even at the most extreme input
// (percent=1, clamped up to latencyFactorFloorPercent), the deployment
// still receives a real, nonzero, meaningfully-sized share -- this is
// deliberately never a hard exclusion, unlike an unhealthy deployment.
func TestSelectNeverFullyStarvesALatencyDeweightedDeployment(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
	}, HealthConfig{})
	r.ReportProbeResult("a", true)
	r.ReportProbeResult("b", true)
	r.SetLatencyFactor("a", 1) // below the floor -- must clamp up, not go to zero.

	const totalCalls = 600
	counts := map[string]int{}
	for i := 0; i < totalCalls; i++ {
		name, ok := r.Select("gpt-4o", nil)
		if !ok {
			t.Fatalf("call %d: Select returned ok=false, want true", i)
		}
		counts[name]++
	}

	if got := counts["a"]; got != 54 {
		t.Errorf(`counts["a"] = %d, want 54 (clamped to latencyFactorFloorPercent, never zero)`, got)
	}
}

// TestSetLatencyFactorClampsAboveFloorAndAtCeiling proves the exact
// clamping contract at both ends, independent of Select's own
// composition effects.
func TestSetLatencyFactorClampsAboveFloorAndAtCeiling(t *testing.T) {
	r := New([]Deployment{{Name: "a", Model: "gpt-4o"}}, HealthConfig{})
	r.ReportProbeResult("a", true)

	r.SetLatencyFactor("a", -5)
	if got := r.health["a"].latencyFactorPercent; got != 0 {
		t.Errorf("SetLatencyFactor(-5): latencyFactorPercent = %d, want 0 (unset)", got)
	}

	r.SetLatencyFactor("a", 5)
	if got := r.health["a"].latencyFactorPercent; got != latencyFactorFloorPercent {
		t.Errorf("SetLatencyFactor(5): latencyFactorPercent = %d, want %d (floor)", got, latencyFactorFloorPercent)
	}

	r.SetLatencyFactor("a", 500)
	if got := r.health["a"].latencyFactorPercent; got != 100 {
		t.Errorf("SetLatencyFactor(500): latencyFactorPercent = %d, want 100 (ceiling)", got)
	}
}

// TestSetLatencyFactorZeroOrBelowClearsToUnset proves the "0 means
// unset, always admit" contract at the admission-gate level, not just
// the stored-value level.
func TestSetLatencyFactorZeroOrBelowClearsToUnset(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
	}, HealthConfig{})
	r.ReportProbeResult("a", true)
	r.ReportProbeResult("b", true)
	r.SetLatencyFactor("a", 20)
	r.SetLatencyFactor("a", 0) // clear it back to unset.

	const totalCalls = 600
	counts := map[string]int{}
	for i := 0; i < totalCalls; i++ {
		name, ok := r.Select("gpt-4o", nil)
		if !ok {
			t.Fatalf("call %d: Select returned ok=false, want true", i)
		}
		counts[name]++
	}
	if got := counts["a"]; got != 300 {
		t.Errorf(`counts["a"] = %d, want 300 (equal 50%% share, unaffected once cleared back to unset)`, got)
	}
}

// TestSelectUnaffectedByLatencyFactorWhenNeverSet proves the
// zero-value/pre-feature-parity guarantee: a deployment SetLatencyFactor
// was never called for behaves byte-for-byte like before this feature
// existed.
func TestSelectUnaffectedByLatencyFactorWhenNeverSet(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
	}, HealthConfig{})
	r.ReportProbeResult("a", true)
	r.ReportProbeResult("b", true)

	const totalCalls = 600
	counts := map[string]int{}
	for i := 0; i < totalCalls; i++ {
		name, ok := r.Select("gpt-4o", nil)
		if !ok {
			t.Fatalf("call %d: Select returned ok=false, want true", i)
		}
		counts[name]++
	}
	if got := counts["a"]; got != 300 {
		t.Errorf(`counts["a"] = %d, want 300 (equal 50%% share, unaffected)`, got)
	}
}
