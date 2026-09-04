package router

import "testing"

// TestSelectDegradesToRoundRobinForEqualWeights is the load-bearing proof
// from docs/rfcs/2026-09-04-weighted-routing.md: with every deployment at
// the default (unset, i.e. zero) weight, Select must return the exact
// same sequence dataplane.Pipeline's old atomic-counter round-robin did —
// byte-identical, not merely "expected to be similar."
func TestSelectDegradesToRoundRobinForEqualWeights(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
		{Name: "c", Model: "gpt-4o"},
	})

	want := []string{"a", "b", "c", "a", "b", "c", "a", "b", "c"}
	for i, w := range want {
		got, ok := r.Select("gpt-4o")
		if !ok {
			t.Fatalf("call %d: Select returned ok=false, want true", i)
		}
		if got != w {
			t.Fatalf("call %d: Select = %q, want %q (full wanted sequence: %v)", i, got, w, want)
		}
	}
}

// TestSelectDegradesToRoundRobinForExplicitEqualWeights proves the same
// guarantee holds for any explicitly-equal weight, not just the
// zero/unset default — the RFC's proof is general, not tied to weight 1.
func TestSelectDegradesToRoundRobinForExplicitEqualWeights(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o", Weight: 5},
		{Name: "b", Model: "gpt-4o", Weight: 5},
	})

	want := []string{"a", "b", "a", "b", "a", "b"}
	for i, w := range want {
		got, ok := r.Select("gpt-4o")
		if !ok || got != w {
			t.Fatalf("call %d: Select = (%q, %v), want (%q, true)", i, got, ok, w)
		}
	}
}

// TestSelectProportionalForWeightedDeployments proves weighting actually
// changes each deployment's share of selections, in exact proportion to
// its configured weight over a full multiple of the total weight — the
// algorithm is deterministic, so this is an exact count, not a
// statistical approximation.
func TestSelectProportionalForWeightedDeployments(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o", Weight: 2},
		{Name: "b", Model: "gpt-4o", Weight: 1},
		{Name: "c", Model: "gpt-4o", Weight: 1},
	})

	counts := map[string]int{}
	const totalCalls = 400 // a multiple of the total weight (4), so counts land exactly on the weight ratio
	for i := 0; i < totalCalls; i++ {
		got, ok := r.Select("gpt-4o")
		if !ok {
			t.Fatalf("call %d: Select returned ok=false, want true", i)
		}
		counts[got]++
	}

	want := map[string]int{"a": 200, "b": 100, "c": 100}
	for name, wantCount := range want {
		if counts[name] != wantCount {
			t.Errorf("counts[%q] = %d, want %d (weight ratio 2:1:1 over %d calls)", name, counts[name], wantCount, totalCalls)
		}
	}
}

// TestSelectUnconfiguredModelReturnsFalse mirrors
// dataplane.Pipeline.nextDeployment's existing "not found" contract.
func TestSelectUnconfiguredModelReturnsFalse(t *testing.T) {
	r := New([]Deployment{{Name: "a", Model: "gpt-4o"}})
	if _, ok := r.Select("claude-opus-4"); ok {
		t.Fatal("Select for an unconfigured model returned ok=true, want false")
	}
}

// TestSelectSingleDeploymentAlwaysReturnsIt covers the common
// one-deployment-per-model case: nothing to route around, every call
// returns the same, only candidate.
func TestSelectSingleDeploymentAlwaysReturnsIt(t *testing.T) {
	r := New([]Deployment{{Name: "solo", Model: "gpt-4o"}})
	for i := 0; i < 5; i++ {
		got, ok := r.Select("gpt-4o")
		if !ok || got != "solo" {
			t.Fatalf("call %d: Select = (%q, %v), want (%q, true)", i, got, ok, "solo")
		}
	}
}
