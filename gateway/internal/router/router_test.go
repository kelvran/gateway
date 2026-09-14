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
	}, HealthConfig{})

	want := []string{"a", "b", "c", "a", "b", "c", "a", "b", "c"}
	for i, w := range want {
		got, ok := r.Select("gpt-4o", nil)
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
	}, HealthConfig{})

	want := []string{"a", "b", "a", "b", "a", "b"}
	for i, w := range want {
		got, ok := r.Select("gpt-4o", nil)
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
	}, HealthConfig{})

	counts := map[string]int{}
	const totalCalls = 400 // a multiple of the total weight (4), so counts land exactly on the weight ratio
	for i := 0; i < totalCalls; i++ {
		got, ok := r.Select("gpt-4o", nil)
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
	r := New([]Deployment{{Name: "a", Model: "gpt-4o"}}, HealthConfig{})
	if _, ok := r.Select("claude-opus-4", nil); ok {
		t.Fatal("Select for an unconfigured model returned ok=true, want false")
	}
}

// TestSelectSingleDeploymentAlwaysReturnsIt covers the common
// one-deployment-per-model case: nothing to route around, every call
// returns the same, only candidate.
func TestSelectSingleDeploymentAlwaysReturnsIt(t *testing.T) {
	r := New([]Deployment{{Name: "solo", Model: "gpt-4o"}}, HealthConfig{})
	for i := 0; i < 5; i++ {
		got, ok := r.Select("gpt-4o", nil)
		if !ok || got != "solo" {
			t.Fatalf("call %d: Select = (%q, %v), want (%q, true)", i, got, ok, "solo")
		}
	}
}

// TestSelectExcludeAvoidsSameModelReselectAfterConcurrentInterleave
// reproduces, deterministically, the real bug a live 20-request
// concurrent burst against the pilot found: with two equal-weight
// deployments, Select's own WRR cursor (modelState.next, wrr.go) is one
// shared sequence across every caller for a model. Two back-to-back
// calls from the SAME logical request always alternate (proven above)
// — but if another concurrent request's own Select call lands in
// between, the parity flips, and a caller's own second pick (its
// same-model fallback re-pick, after its first pick's deployment call
// already failed) can land right back on the exact name it just
// failed on. Passing that name via exclude is what dataplane.go's and
// streaming.go's own fallback branches now do.
func TestSelectExcludeAvoidsSameModelReselectAfterConcurrentInterleave(t *testing.T) {
	r := New([]Deployment{
		{Name: "broken", Model: "claude-haiku-4-5"},
		{Name: "healthy", Model: "claude-haiku-4-5"},
	}, HealthConfig{})

	first, ok := r.Select("claude-haiku-4-5", nil)
	if !ok || first != "broken" {
		t.Fatalf("first pick = (%q, %v), want (%q, true)", first, ok, "broken")
	}

	// Simulates a DIFFERENT concurrent request's own Select call landing
	// between this request's first pick and its fallback re-pick.
	if interleaved, ok := r.Select("claude-haiku-4-5", nil); !ok || interleaved != "healthy" {
		t.Fatalf("interleaved call = (%q, %v), want (%q, true)", interleaved, ok, "healthy")
	}

	// Without exclusion, this next call would land back on "broken" —
	// the exact bug (proven by the two calls above: the cursor is now
	// back at the same parity as before the first call). Excluding the
	// already-failed name is what must change the outcome.
	second, ok := r.Select("claude-haiku-4-5", map[string]bool{"broken": true})
	if !ok {
		t.Fatal("fallback re-pick with exclude={broken} returned ok=false, want true (a real, non-excluded candidate exists)")
	}
	if second != "healthy" {
		t.Fatalf("fallback re-pick with exclude={broken} = %q, want %q -- excluding the just-failed deployment must never be silently ignored", second, "healthy")
	}
}

// TestSelectExcludeAllCandidatesReturnsNotFound proves exclude can
// correctly report "nothing left to try" rather than ever falling back
// to a name the caller explicitly excluded.
func TestSelectExcludeAllCandidatesReturnsNotFound(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
	}, HealthConfig{})

	if _, ok := r.Select("gpt-4o", map[string]bool{"a": true, "b": true}); ok {
		t.Fatal("Select with every real candidate excluded returned ok=true, want false")
	}
}
