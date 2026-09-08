package router

import "testing"

// These three tests are the load-bearing proof for the post-recovery
// weight-ramp feature (health.go's ramping/rampStep/rampCredit fields and
// admitRampedTurn), closing the real gap
// evals/tests/fixtures/regression_corpus_routing_chaos.json's
// "chaos-recovery-instant-full-weight-no-ramp" case documented: before
// this feature, the instant a deployment's Nth consecutive successful
// probe flipped it healthy, Select treated it as 100% eligible for its
// full configured Weight, with no dampening at all.
//
// All three reuse TestSelectProportionalForWeightedDeployments's own
// rigor: real, exact counts over many Select calls, not a single
// spot-checked call or a statistical/tolerance-based approximation — the
// underlying mechanism (admitRampedTurn's Bresenham-style accumulator,
// composed with the existing deterministic smooth-WRR schedule) has no
// randomness anywhere, so every count below is exactly reproducible, not
// merely "expected on average."

// TestSelectGrantsReducedShareImmediatelyAfterRecovery proves claim 1: a
// deployment that just recovered gets noticeably less traffic share than
// its configured Weight would imply, immediately after recovery — not
// merely "eventually" or "on the next probe cycle."
func TestSelectGrantsReducedShareImmediatelyAfterRecovery(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
	}, HealthConfig{UnhealthyThreshold: 1, HealthyThreshold: 2, RecoveryRampSteps: 4, RecoveryRampInitialPercent: 20})

	r.ReportProbeResult("a", false)                    // ejects "a" (UnhealthyThreshold=1)
	r.ReportProbeResult("a", true)                     // 1 of 2 required successes
	healthy, changed := r.ReportProbeResult("a", true) // 2 of 2 -> recovers, ramp starts
	if !healthy || !changed {
		t.Fatalf("setup: ReportProbeResult on the 2nd recovery success = (healthy=%v, changed=%v), want (true, true)", healthy, changed)
	}

	const totalCalls = 600 // a multiple of this scenario's exact acceptance period (see below), so the count lands exactly, not on a rounded boundary
	counts := map[string]int{}
	for i := 0; i < totalCalls; i++ {
		name, ok := r.Select("gpt-4o")
		if !ok {
			t.Fatalf("call %d: Select returned ok=false, want true", i)
		}
		counts[name]++
	}

	// With "a" and "b" at equal (unset, i.e. default 1) configured
	// weight, "a"'s configured share would imply 300/600 (50%) — the
	// exact number TestSelectDegradesToRoundRobinForEqualWeights-style
	// math would produce with no ramp active at all. Freshly recovered
	// at RecoveryRampInitialPercent=20, "a" instead gets exactly
	// 100/600 (16.7%): admitRampedTurn's accumulator admits exactly 1
	// of every 5 offers "a" receives, and — because an ADMITTED turn
	// ends that Select call immediately while a REJECTED one falls
	// through and consumes "b"'s own next turn in the same call — the
	// two mechanisms compose into an exact 1-admission-per-6-outer-calls
	// steady state, not the naively-expected 1-in-5. Either way, this
	// is unambiguously and dramatically less than "a"'s configured 50%
	// share, which is the property this test exists to prove.
	if got := counts["a"]; got != 100 {
		t.Errorf(`counts["a"] = %d, want 100 (600 calls immediately after recovery, at RecoveryRampInitialPercent=20 — nowhere close to the 300 its configured equal weight would imply)`, got)
	}
	if got := counts["b"]; got != 500 {
		t.Errorf(`counts["b"] = %d, want 500`, got)
	}
}

// TestSelectRampShareIncreasesToFullWeightOverRecoveryWindow proves
// claims 2 and 3 together: "a"'s share strictly increases at every
// subsequent successful probe during the ramp window, and reaches
// exactly its full configured share (as if it had never been unhealthy
// at all) the moment RecoveryRampSteps additional successes complete the
// ramp — never a fraction below full weight forever, and never an
// overshoot past it.
func TestSelectRampShareIncreasesToFullWeightOverRecoveryWindow(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
	}, HealthConfig{UnhealthyThreshold: 1, HealthyThreshold: 2, RecoveryRampSteps: 4, RecoveryRampInitialPercent: 20})

	r.ReportProbeResult("a", false)
	r.ReportProbeResult("a", true)
	r.ReportProbeResult("a", true) // recovers, ramp starts at rampStep=0 (20%)

	const perStage = 600
	countA := func() int {
		n := 0
		for i := 0; i < perStage; i++ {
			if name, ok := r.Select("gpt-4o"); ok && name == "a" {
				n++
			}
		}
		return n
	}

	// One exact count per ramp stage (rampStep 0..3, each at
	// 20/40/60/80 percent), then one more successful probe to complete
	// the ramp (rampStep reaches RecoveryRampSteps=4) and a final count
	// proving full parity with "a"'s configured share.
	wantByStage := []int{100, 171, 225, 267}
	for stage, want := range wantByStage {
		got := countA()
		if got != want {
			t.Fatalf("ramp stage %d (percent=%d): counts[\"a\"] over %d calls = %d, want %d", stage, defaultRecoveryRampInitialPercentPlusStage(stage), perStage, got, want)
		}
		if stage > 0 && got <= wantByStage[stage-1] {
			t.Fatalf("ramp stage %d: counts[\"a\"] = %d did not increase over the previous stage's %d", stage, got, wantByStage[stage-1])
		}
		r.ReportProbeResult("a", true) // advance to the next ramp stage (or complete it, on the last iteration)
	}

	// The success just reported above was the 4th additional one
	// (RecoveryRampSteps=4) — the ramp is now complete. "a" must be back
	// at its full configured share: exactly half of every call, byte
	// for byte the same math as two never-ramped equal-weight
	// deployments (see TestSelectDegradesToRoundRobinForEqualWeights).
	got := countA()
	if got != perStage/2 {
		t.Fatalf("after the ramp completes: counts[\"a\"] over %d calls = %d, want %d (its full configured 50%% share)", perStage, got, perStage/2)
	}
}

// defaultRecoveryRampInitialPercentPlusStage is a tiny formatting helper
// purely for this test file's own failure messages — mirrors
// health.go's admitRampedTurn percent formula so a failing assertion's
// message names the exact percent under test, not just an opaque stage
// index.
func defaultRecoveryRampInitialPercentPlusStage(stage int) int {
	return defaultRecoveryRampInitialPercent + (rampCreditFull-defaultRecoveryRampInitialPercent)*stage/defaultRecoveryRampSteps
}

// TestSelectFullyRampedDeploymentMatchesUnmodifiedProportionalWeightMath
// is the load-bearing backward-compatibility proof, mirroring
// TestSelectProportionalForWeightedDeployments's own exact-count rigor
// exactly (same weight ratio, same call count, same expected numbers): a
// deployment that has fully completed its recovery ramp must be
// completely indistinguishable, in Select's actual weight-selection math,
// from a deployment that was never unhealthy at all — no residual
// slow-down, no residual state, nothing.
func TestSelectFullyRampedDeploymentMatchesUnmodifiedProportionalWeightMath(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o", Weight: 2},
		{Name: "b", Model: "gpt-4o", Weight: 1},
		{Name: "c", Model: "gpt-4o", Weight: 1},
	}, HealthConfig{UnhealthyThreshold: 1, HealthyThreshold: 2, RecoveryRampSteps: 4, RecoveryRampInitialPercent: 20})

	r.ReportProbeResult("a", false) // eject
	r.ReportProbeResult("a", true)
	r.ReportProbeResult("a", true) // recover, ramp starts
	for i := 0; i < 4; i++ {
		r.ReportProbeResult("a", true) // RecoveryRampSteps=4 additional successes -> ramp completes
	}
	if healthy := r.IsHealthy("a"); !healthy {
		t.Fatal("setup: \"a\" should be healthy after recovering and completing its ramp")
	}

	counts := map[string]int{}
	const totalCalls = 400 // identical call count to TestSelectProportionalForWeightedDeployments
	for i := 0; i < totalCalls; i++ {
		name, ok := r.Select("gpt-4o")
		if !ok {
			t.Fatalf("call %d: Select returned ok=false, want true", i)
		}
		counts[name]++
	}

	// These are the EXACT SAME numbers TestSelectProportionalForWeightedDeployments
	// asserts for an identical 2:1:1 weight configuration with no health
	// probing involved at all — proving the fully-ramped "a" now behaves
	// byte-for-byte identically to a deployment that was never touched by
	// this feature.
	want := map[string]int{"a": 200, "b": 100, "c": 100}
	for name, wantCount := range want {
		if counts[name] != wantCount {
			t.Errorf("counts[%q] = %d, want %d (weight ratio 2:1:1 over %d calls, post-ramp — must match the never-ramped case exactly)", name, counts[name], wantCount, totalCalls)
		}
	}
}
