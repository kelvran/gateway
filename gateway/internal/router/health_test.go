package router

import "testing"

// TestSelectExcludesOnlyAfterNConsecutiveFailures is the load-bearing
// N-of-M proof from docs/rfcs/2026-09-07-gateway-active-health-probing.md:
// a deployment must stay eligible through N-1 consecutive failures, and
// be excluded only once the Nth consecutive failure is reported — never
// after a single failure, unlike LiteLLM's own aggressive default.
func TestSelectExcludesOnlyAfterNConsecutiveFailures(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
	}, HealthConfig{UnhealthyThreshold: 3, HealthyThreshold: 2})

	for i := 1; i <= 2; i++ {
		healthy, changed := r.ReportProbeResult("a", false)
		if !healthy {
			t.Fatalf("after %d consecutive failure(s): healthy = false, want true (threshold is 3)", i)
		}
		if changed {
			t.Fatalf("after %d consecutive failure(s): changed = true, want false", i)
		}
	}
	// "a" must still be selectable after only 2 of 3 required failures.
	sawA := false
	for i := 0; i < 4; i++ {
		if name, _ := r.Select("gpt-4o"); name == "a" {
			sawA = true
		}
	}
	if !sawA {
		t.Fatal("\"a\" was never selected after only 2 consecutive failures (below the 3-failure threshold) — it should still be eligible")
	}

	healthy, changed := r.ReportProbeResult("a", false)
	if healthy {
		t.Fatal("after the 3rd consecutive failure: healthy = true, want false")
	}
	if !changed {
		t.Fatal("after the 3rd consecutive failure: changed = false, want true")
	}

	for i := 0; i < 10; i++ {
		if name, ok := r.Select("gpt-4o"); !ok || name != "b" {
			t.Fatalf("call %d after \"a\" tripped its 3rd consecutive failure: Select = (%q, %v), want (%q, true)", i, name, ok, "b")
		}
	}
}

// TestSelectReincludesOnlyAfterMConsecutiveSuccesses is the mirror-image
// proof: an excluded deployment must stay excluded through M-1
// consecutive successes, and only become eligible again once the Mth
// consecutive success is reported.
func TestSelectReincludesOnlyAfterMConsecutiveSuccesses(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
	}, HealthConfig{UnhealthyThreshold: 3, HealthyThreshold: 2})

	for i := 0; i < 3; i++ {
		r.ReportProbeResult("a", false)
	}
	if healthy := r.IsHealthy("a"); healthy {
		t.Fatal("setup: \"a\" should be unhealthy after 3 consecutive failures")
	}

	// healthy reflects the OVERALL verdict, not this single call's own
	// success — after only 1 of 2 required successes, "a" must still
	// read as unhealthy.
	healthy, changed := r.ReportProbeResult("a", true)
	if healthy {
		t.Fatal("after only 1 of 2 required consecutive successes: healthy = true, want false")
	}
	if changed {
		t.Fatal("after only 1 of 2 required consecutive successes: changed = true, want false")
	}
	for i := 0; i < 10; i++ {
		if name, ok := r.Select("gpt-4o"); !ok || name != "b" {
			t.Fatalf("call %d after only 1 of 2 required successes: Select = (%q, %v), want (%q, true) — \"a\" must stay excluded", i, name, ok, "b")
		}
	}

	healthy, changed = r.ReportProbeResult("a", true)
	if !healthy {
		t.Fatal("after the 2nd consecutive success: healthy = false, want true")
	}
	if !changed {
		t.Fatal("after the 2nd consecutive success: changed = false, want true")
	}

	sawA := false
	for i := 0; i < 10; i++ {
		if name, _ := r.Select("gpt-4o"); name == "a" {
			sawA = true
		}
	}
	if !sawA {
		t.Fatal("\"a\" was never selected after its 2nd consecutive success re-included it")
	}
}

// TestReportProbeResultNonConsecutiveFailuresNeverExclude proves "N
// consecutive" genuinely means consecutive: a failure/success/failure/
// success... pattern that never accumulates N failures IN A ROW must
// never exclude the deployment, no matter how many total failures were
// reported.
func TestReportProbeResultNonConsecutiveFailuresNeverExclude(t *testing.T) {
	r := New([]Deployment{{Name: "a", Model: "gpt-4o"}}, HealthConfig{UnhealthyThreshold: 3, HealthyThreshold: 2})

	for i := 0; i < 20; i++ {
		success := i%2 == 0 // alternates: never 2 failures in a row, let alone 3
		if healthy, changed := r.ReportProbeResult("a", success); !healthy || changed {
			t.Fatalf("iteration %d (success=%v): healthy=%v changed=%v, want healthy=true changed=false", i, success, healthy, changed)
		}
	}
}

// TestSelectFailsOpenWhenEveryDeploymentUnhealthy proves the fail-open
// contract explicitly: when every deployment for a model has tripped the
// unhealthy threshold, Select still returns one of them (ok=true) rather
// than ("", false) — a known-bad deployment is a better answer than "no
// deployment configured for this model at all."
func TestSelectFailsOpenWhenEveryDeploymentUnhealthy(t *testing.T) {
	r := New([]Deployment{{Name: "solo", Model: "gpt-4o"}}, HealthConfig{UnhealthyThreshold: 3, HealthyThreshold: 2})
	for i := 0; i < 3; i++ {
		r.ReportProbeResult("solo", false)
	}
	if r.IsHealthy("solo") {
		t.Fatal("setup: \"solo\" should be unhealthy after 3 consecutive failures")
	}

	name, ok := r.Select("gpt-4o")
	if !ok {
		t.Fatal("Select with the only configured deployment unhealthy returned ok=false, want true (fail open)")
	}
	if name != "solo" {
		t.Fatalf("Select = %q, want %q", name, "solo")
	}
}

// TestSelectSkipsUnhealthyDespiteHeavilySkewedWeight is the correctness
// proof for selectHealthy's sumW-bounded loop (see wrr.go/health.go's own
// doc comments): a low-weight healthy deployment must still be found
// even when a much-higher-weight sibling is unhealthy — a naive bound of
// just len(deployments) attempts would starve it, since the smooth-WRR
// schedule can return the high-weight deployment many times in a row
// before ever reaching a low-weight one.
func TestSelectSkipsUnhealthyDespiteHeavilySkewedWeight(t *testing.T) {
	r := New([]Deployment{
		{Name: "heavy", Model: "gpt-4o", Weight: 50},
		{Name: "light", Model: "gpt-4o", Weight: 1},
	}, HealthConfig{UnhealthyThreshold: 3, HealthyThreshold: 2})

	for i := 0; i < 3; i++ {
		r.ReportProbeResult("heavy", false)
	}
	if r.IsHealthy("heavy") {
		t.Fatal("setup: \"heavy\" should be unhealthy after 3 consecutive failures")
	}

	for i := 0; i < 20; i++ {
		if name, ok := r.Select("gpt-4o"); !ok || name != "light" {
			t.Fatalf("call %d: Select = (%q, %v), want (%q, true) — \"heavy\" is unhealthy, \"light\" must always be chosen instead", i, name, ok, "light")
		}
	}
}
