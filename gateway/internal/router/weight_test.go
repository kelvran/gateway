package router

import (
	"sync"
	"testing"
	"time"
)

// TestSetWeightUpdatesSelectionDistributionForThatModelOnly proves the
// live-mutated weight actually changes Select's own output distribution
// for that model — not just that SetWeight returns nil.
func TestSetWeightUpdatesSelectionDistributionForThatModelOnly(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
	}, HealthConfig{})

	// Advance the cursor partway under the original 1:1 weights, to prove
	// SetWeight rebuilds the cursor rather than merely reweighting deps
	// in place (which would leave i/cw referring to a now-meaningless
	// position).
	if got, _ := r.Select("gpt-4o", nil); got != "a" {
		t.Fatalf("warm-up call 1 = %q, want %q", got, "a")
	}

	if err := r.SetWeight("gpt-4o", "a", 3); err != nil {
		t.Fatalf("SetWeight: %v", err)
	}

	want := []string{"a", "a", "a", "b", "a", "a", "a", "b"}
	for i, w := range want {
		got, ok := r.Select("gpt-4o", nil)
		if !ok {
			t.Fatalf("call %d: Select returned ok=false, want true", i)
		}
		if got != w {
			t.Fatalf("call %d: Select = %q, want %q (full wanted 3:1 sequence: %v)", i, got, w, want)
		}
	}
}

// TestSetWeightLeavesOtherModelsWRRCursorsUntouched proves SetWeight only
// ever replaces the ONE model's *modelState it targets — a different
// model's own in-progress WRR cursor must keep advancing exactly as if
// SetWeight had never been called.
func TestSetWeightLeavesOtherModelsWRRCursorsUntouched(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
		{Name: "x", Model: "gpt-4o-mini"},
		{Name: "y", Model: "gpt-4o-mini"},
	}, HealthConfig{})

	// Advance gpt-4o-mini's cursor partway through its own round-robin.
	if got, _ := r.Select("gpt-4o-mini", nil); got != "x" {
		t.Fatalf("gpt-4o-mini warm-up = %q, want %q", got, "x")
	}

	if err := r.SetWeight("gpt-4o", "a", 3); err != nil {
		t.Fatalf("SetWeight: %v", err)
	}

	// gpt-4o-mini's cursor must continue exactly where it left off (y,
	// then x, then y, ...) -- NOT reset back to x, which a buggy
	// implementation that rebuilt every model (or shared one global
	// cursor) would produce.
	want := []string{"y", "x", "y", "x"}
	for i, w := range want {
		got, ok := r.Select("gpt-4o-mini", nil)
		if !ok {
			t.Fatalf("call %d: Select(gpt-4o-mini) returned ok=false, want true", i)
		}
		if got != w {
			t.Fatalf("call %d: Select(gpt-4o-mini) = %q, want %q — SetWeight on gpt-4o must not touch this model's own cursor", i, got, w)
		}
	}
}

// TestSetWeightIsSafeUnderConcurrentSelect hammers Select and SetWeight
// concurrently on the same model — this test's only real job is to fail
// under `go test -race` if modelsMu is missing or misapplied anywhere.
// The exact sequence Select returns during concurrent SetWeight calls is
// deliberately not asserted (it's not deterministic); only that every
// call still succeeds and returns a real member of the group.
func TestSetWeightIsSafeUnderConcurrentSelect(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
	}, HealthConfig{})

	valid := map[string]bool{"a": true, "b": true}
	stop := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, ok := r.Select("gpt-4o", nil)
				if !ok || !valid[got] {
					t.Errorf("Select returned (%q, %v), want a valid member name with ok=true", got, ok)
					return
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for w := 1; w <= 5; w++ {
			if err := r.SetWeight("gpt-4o", "a", w); err != nil {
				t.Errorf("SetWeight(%d): %v", w, err)
				return
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestSetWeightReturnsErrorForUnknownModel proves SetWeight never
// silently no-ops for a model with no configured deployment group.
func TestSetWeightReturnsErrorForUnknownModel(t *testing.T) {
	r := New([]Deployment{{Name: "a", Model: "gpt-4o"}}, HealthConfig{})
	if err := r.SetWeight("claude-opus", "a", 3); err == nil {
		t.Fatal("SetWeight for an unconfigured model returned nil error, want an error")
	}
}

// TestSetWeightReturnsErrorForUnknownDeploymentInModel proves SetWeight
// never adds a deployment to a group — a name that isn't already a
// member of model's group is rejected, changing nothing.
func TestSetWeightReturnsErrorForUnknownDeploymentInModel(t *testing.T) {
	r := New([]Deployment{{Name: "a", Model: "gpt-4o"}}, HealthConfig{})
	if err := r.SetWeight("gpt-4o", "does-not-exist", 3); err == nil {
		t.Fatal("SetWeight for an unknown deployment name returned nil error, want an error")
	}

	// Confirm nothing changed: Select still only ever returns "a".
	if got, ok := r.Select("gpt-4o", nil); !ok || got != "a" {
		t.Fatalf("Select after a failed SetWeight = (%q, %v), want (%q, true) — a failed SetWeight must not corrupt state", got, ok, "a")
	}
}

// TestSetWeightNormalizesZeroOrNegativeWeightToOne is the regression
// proof for a live 2026-09-16 adversarial-audit finding: SetWeight's own
// doc comment documents that weight <= 0 normalizes to 1 (the same
// "unset" convention newModelState itself already applies), and
// POST /admin/deployments/{name}/weight explicitly accepts weight=0 as
// meaningful — but nothing had ever exercised that branch through
// SetWeight itself (only through New's initial-weight path). Proves the
// resulting Select distribution actually reflects weight-1, not a
// silently-dropped or zero-traffic deployment.
func TestSetWeightNormalizesZeroOrNegativeWeightToOne(t *testing.T) {
	for _, weight := range []int{0, -5} {
		t.Run("", func(t *testing.T) {
			r := New([]Deployment{
				{Name: "a", Model: "gpt-4o", Weight: 3},
				{Name: "b", Model: "gpt-4o", Weight: 3},
			}, HealthConfig{})

			if err := r.SetWeight("gpt-4o", "a", weight); err != nil {
				t.Fatalf("SetWeight(%d): %v", weight, err)
			}

			// a is now weight-1 (normalized), b stays weight-3 -- a 1:3
			// ratio, degenerate case of the same wrr.c algorithm already
			// proven elsewhere in this package. Sequence hand-traced
			// against wrr.go's own next() implementation, not guessed:
			// deps=[a(1),b(3)], gcd=1, maxW=3, sumW=4.
			want := []string{"b", "b", "a", "b"}
			for i, w := range want {
				got, ok := r.Select("gpt-4o", nil)
				if !ok {
					t.Fatalf("call %d: Select returned ok=false, want true", i)
				}
				if got != w {
					t.Fatalf("call %d: Select = %q, want %q (full wanted 3:1 sequence for weight=%d: %v) — weight<=0 must normalize to 1, never mean zero traffic or get silently dropped", i, got, w, weight, want)
				}
			}
		})
	}
}

// TestSetWeightIsSafeUnderConcurrentReportProbeResult is the regression
// proof for a live 2026-09-16 adversarial-audit finding: no test combined
// SetWeight (modelsMu) with ReportProbeResult (healthMu) concurrently on
// the SAME deployment/model — the two existing concurrency tests each
// only paired one of {SetWeight, ReportProbeResult} with Select, never
// with each other. Run under `go test -race`.
func TestSetWeightIsSafeUnderConcurrentReportProbeResult(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
	}, HealthConfig{UnhealthyThreshold: 2, HealthyThreshold: 2, RecoveryRampSteps: 3, RecoveryRampInitialPercent: 25})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.ReportProbeResult("a", i%2 == 0)
		}(i)
	}
	for w := 1; w <= 50; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			_ = r.SetWeight("gpt-4o", "a", w)
		}(w)
	}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Select("gpt-4o", nil)
		}()
	}
	wg.Wait()
}
