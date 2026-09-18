package router

import (
	"fmt"
	"testing"
)

// TestSelectStickyIsDeterministicForTheSameKey proves the core sticky
// guarantee at its simplest: the same key must land on the same
// deployment on every call, across many independent calls.
func TestSelectStickyIsDeterministicForTheSameKey(t *testing.T) {
	r := New([]Deployment{
		{Name: "stable", Model: "gpt-4o", Weight: 9},
		{Name: "canary", Model: "gpt-4o", Weight: 1, Sticky: true},
	}, HealthConfig{})

	first, ok := r.SelectSticky("gpt-4o", nil, "tenant-a")
	if !ok {
		t.Fatal("SelectSticky returned ok=false, want true")
	}
	for i := 0; i < 50; i++ {
		got, ok := r.SelectSticky("gpt-4o", nil, "tenant-a")
		if !ok || got != first {
			t.Fatalf("call %d: SelectSticky(tenant-a) = (%q, %v), want (%q, true) -- same key must always land on the same deployment", i, got, ok, first)
		}
	}
}

// TestSelectStickyDistributionApproximatesCanaryWeightAcrossManyKeys
// proves the hash split actually respects the configured weight ratio,
// statistically, over many distinct keys -- mirrors
// TestSelectProportionalForWeightedDeployments's own style, generous
// tolerance since this is a hash distribution, not the exact deterministic
// WRR count that test proves for plain Select.
func TestSelectStickyDistributionApproximatesCanaryWeightAcrossManyKeys(t *testing.T) {
	r := New([]Deployment{
		{Name: "stable", Model: "gpt-4o", Weight: 9},
		{Name: "canary", Model: "gpt-4o", Weight: 1, Sticky: true},
	}, HealthConfig{})

	const totalKeys = 10000
	canaryCount := 0
	for i := 0; i < totalKeys; i++ {
		got, ok := r.SelectSticky("gpt-4o", nil, fmt.Sprintf("tenant-%d", i))
		if !ok {
			t.Fatalf("key %d: SelectSticky returned ok=false", i)
		}
		if got == "canary" {
			canaryCount++
		}
	}

	// Want ~10% (1/(9+1)) on the canary side -- generous +/-3 percentage
	// point tolerance for hash-distribution noise at this sample size.
	gotPercent := float64(canaryCount) / float64(totalKeys) * 100
	if gotPercent < 7 || gotPercent > 13 {
		t.Errorf("canary share = %.2f%% (%d/%d), want approximately 10%%", gotPercent, canaryCount, totalKeys)
	}
}

// TestSelectStickyRampUpOnlyAddsKeysNeverBouncesExistingCanaryKeysBack is
// the core monotonicity guarantee this feature exists to provide: ramping
// the canary's own weight up via SetWeight must only ever ADD newly
// bucketed keys to the canary side -- every key already on the canary
// side at the lower weight must STILL be on the canary side at the
// higher weight.
func TestSelectStickyRampUpOnlyAddsKeysNeverBouncesExistingCanaryKeysBack(t *testing.T) {
	r := New([]Deployment{
		{Name: "stable", Model: "gpt-4o", Weight: 95},
		{Name: "canary", Model: "gpt-4o", Weight: 5, Sticky: true},
	}, HealthConfig{})

	const totalKeys = 2000
	canaryAtLowWeight := map[string]bool{}
	for i := 0; i < totalKeys; i++ {
		key := fmt.Sprintf("tenant-%d", i)
		got, ok := r.SelectSticky("gpt-4o", nil, key)
		if !ok {
			t.Fatalf("key %q: SelectSticky returned ok=false", key)
		}
		if got == "canary" {
			canaryAtLowWeight[key] = true
		}
	}
	if len(canaryAtLowWeight) == 0 {
		t.Fatal("no key landed on the canary side at 5% -- test setup is broken")
	}

	if err := r.SetWeight("gpt-4o", "canary", 50); err != nil {
		t.Fatalf("SetWeight: %v", err)
	}

	for key := range canaryAtLowWeight {
		got, ok := r.SelectSticky("gpt-4o", nil, key)
		if !ok || got != "canary" {
			t.Errorf("key %q was on the canary side at weight=5, but landed on %q after ramping canary to weight=50 -- monotonicity violated", key, got)
		}
	}
}

// TestSelectStickyFallsThroughToPlainSelectHealthyWhenStickyPickIsUnhealthy
// proves a sticky pick can never bypass this package's own existing
// health/ramp safety: once the sticky-selected deployment is marked
// unhealthy, SelectSticky must fall through to a real, healthy
// alternative, never return the unhealthy pick and never return
// ("", false) just because the sticky path failed.
func TestSelectStickyFallsThroughToPlainSelectHealthyWhenStickyPickIsUnhealthy(t *testing.T) {
	r := New([]Deployment{
		{Name: "stable", Model: "gpt-4o", Weight: 1},
		{Name: "canary", Model: "gpt-4o", Weight: 1, Sticky: true},
	}, HealthConfig{UnhealthyThreshold: 1, HealthyThreshold: 1})

	// Find a key that lands on canary at this even 1:1 split.
	var stickyKey string
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("tenant-%d", i)
		if got, ok := r.SelectSticky("gpt-4o", nil, key); ok && got == "canary" {
			stickyKey = key
			break
		}
	}
	if stickyKey == "" {
		t.Fatal("no key landed on canary in 200 tries -- test setup is broken")
	}

	r.ReportProbeResult("canary", false) // 1 failure trips UnhealthyThreshold:1.

	got, ok := r.SelectSticky("gpt-4o", nil, stickyKey)
	if !ok {
		t.Fatal("SelectSticky returned ok=false, want a real fallback to the healthy stable deployment")
	}
	if got != "stable" {
		t.Errorf("SelectSticky returned %q for a key whose sticky pick (canary) is unhealthy, want the healthy fallback %q", got, "stable")
	}
}

// TestSelectStickyNeverReturnsAnExcludedName proves exclude is honored
// even when the sticky pick would otherwise have matched it.
func TestSelectStickyNeverReturnsAnExcludedName(t *testing.T) {
	r := New([]Deployment{
		{Name: "stable", Model: "gpt-4o", Weight: 1},
		{Name: "canary", Model: "gpt-4o", Weight: 1, Sticky: true},
	}, HealthConfig{})

	var stickyKey string
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("tenant-%d", i)
		if got, ok := r.SelectSticky("gpt-4o", nil, key); ok && got == "canary" {
			stickyKey = key
			break
		}
	}
	if stickyKey == "" {
		t.Fatal("no key landed on canary in 200 tries -- test setup is broken")
	}

	got, ok := r.SelectSticky("gpt-4o", map[string]bool{"canary": true}, stickyKey)
	if !ok {
		t.Fatal("SelectSticky returned ok=false, want the non-excluded stable deployment")
	}
	if got == "canary" {
		t.Errorf("SelectSticky returned the explicitly excluded name %q", got)
	}
}

// TestSelectUnaffectedByStickyGroupsWhenStickyKeyEmpty proves an empty
// stickyKey is a guaranteed no-op: SelectSticky must degrade to plain
// selectHealthy's own byte-identical behavior, exactly like Select.
func TestSelectUnaffectedByStickyGroupsWhenStickyKeyEmpty(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
		{Name: "c", Model: "gpt-4o"},
	}, HealthConfig{})

	want := []string{"a", "b", "c", "a", "b", "c"}
	for i, w := range want {
		got, ok := r.SelectSticky("gpt-4o", nil, "")
		if !ok || got != w {
			t.Fatalf("call %d: SelectSticky(stickyKey=\"\") = (%q, %v), want (%q, true) -- must match plain round-robin exactly", i, got, ok, w)
		}
	}
}

// TestSetWeightOnCanaryDeploymentPreservesExistingCanaryAssignments is
// the same monotonicity property as
// TestSelectStickyRampUpOnlyAddsKeysNeverBouncesExistingCanaryKeysBack,
// re-proven specifically through a real SetWeight call (not just
// stickyPick's own pure-function math) at a more extreme ramp (5% to
// 90%), to guard against a regression that only shows up at a wider
// weight swing.
func TestSetWeightOnCanaryDeploymentPreservesExistingCanaryAssignments(t *testing.T) {
	r := New([]Deployment{
		{Name: "stable", Model: "claude-haiku-4-5", Weight: 95},
		{Name: "canary", Model: "claude-haiku-4-5", Weight: 5, Sticky: true},
	}, HealthConfig{})

	const totalKeys = 2000
	canaryKeys := map[string]bool{}
	for i := 0; i < totalKeys; i++ {
		key := fmt.Sprintf("k-%d", i)
		if got, ok := r.SelectSticky("claude-haiku-4-5", nil, key); ok && got == "canary" {
			canaryKeys[key] = true
		}
	}
	if len(canaryKeys) == 0 {
		t.Fatal("no key landed on canary at 5% -- test setup is broken")
	}

	if err := r.SetWeight("claude-haiku-4-5", "canary", 90); err != nil {
		t.Fatalf("SetWeight: %v", err)
	}

	for key := range canaryKeys {
		if got, ok := r.SelectSticky("claude-haiku-4-5", nil, key); !ok || got != "canary" {
			t.Errorf("key %q was on canary at weight=5, landed on %q after SetWeight to weight=90", key, got)
		}
	}
}

// TestSelectStickyNeverDoubleChargesRampCreditOnATierRejectedCandidate
// is the regression proof for a real bug an audit found:
// SelectSticky's own direct admitTurn(name) call already consumes a
// ramping candidate's own "turn" for this logical selection, but the
// subsequent fallthrough to selectHealthy did not exclude name — so
// selectHealthy's own scan (which calls admitTurn on EVERY offered
// candidate, including one whose tier doesn't match, per its own doc
// comment) could re-offer and re-admission-check the SAME candidate,
// incrementing its rampCredit accumulator a second time for one real
// selection decision. This undermines RecoveryRampSteps/
// RecoveryRampInitialPercent's entire documented purpose — letting a
// just-recovered deployment ramp to full traffic admission faster than
// configured.
//
// Constructed so the double-charge is deterministic, not probabilistic:
// two equal-weight deployments (sumW=2, so selectHealthy's bounded scan
// offers each exactly once per call), BOTH freshly ramping with a
// RecoveryRampInitialPercent low enough (1) that a single admitTurn
// call on either reliably fails admission (0+1 < rampCreditFull=100) —
// forcing selectHealthy's scan to examine both stable (tier-preferred,
// but ramp-rejected) and canary (the sticky pick, already directly
// admitTurn-checked once by SelectSticky itself) within the same call.
func TestSelectStickyNeverDoubleChargesRampCreditOnATierRejectedCandidate(t *testing.T) {
	r := New([]Deployment{
		{Name: "stable", Model: "gpt-4o", Weight: 1, CostTier: 1},
		{Name: "canary", Model: "gpt-4o", Weight: 1, Sticky: true, CostTier: 2},
	}, HealthConfig{UnhealthyThreshold: 1, HealthyThreshold: 1, RecoveryRampSteps: 4, RecoveryRampInitialPercent: 1})

	// Put BOTH deployments into a freshly-ramping state (rampStep=0,
	// rampCredit=0): one failure trips UnhealthyThreshold:1, one
	// success re-includes and starts the ramp, per ReportProbeResult's
	// own doc comment.
	for _, name := range []string{"stable", "canary"} {
		r.ReportProbeResult(name, false)
		r.ReportProbeResult(name, true)
	}

	// Find a key that sticks to canary at this even 1:1 split.
	var stickyKey string
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("tenant-%d", i)
		if name, ok := r.stickyPick(r.models["gpt-4o"], key); ok && name == "canary" {
			stickyKey = key
			break
		}
	}
	if stickyKey == "" {
		t.Fatal("no key stuck to canary in 200 tries -- test setup is broken")
	}

	if _, ok := r.SelectSticky("gpt-4o", nil, stickyKey); !ok {
		t.Fatal("SelectSticky returned ok=false")
	}

	// The load-bearing assertion: canary's rampCredit must reflect
	// EXACTLY one admitTurn call (0 + RecoveryRampInitialPercent(1) = 1)
	// for this one SelectSticky call, never two (which would mean
	// selectHealthy's own scan re-admission-checked it after
	// SelectSticky's own direct call already had).
	r.healthMu.Lock()
	gotRampCredit := r.health["canary"].rampCredit
	r.healthMu.Unlock()
	if gotRampCredit != 1 {
		t.Errorf("canary.rampCredit = %d after one SelectSticky call, want exactly 1 (RecoveryRampInitialPercent) -- selectHealthy's own scan double-charged it", gotRampCredit)
	}
}
