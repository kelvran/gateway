package budget

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/shopspring/decimal"
)

// TestConcurrentReserveReconcileBoundsAdmissionAgainstNearExhaustedCap is
// the load-bearing proof for
// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md: reproduces
// the exact scenario evals/tests/fixtures/regression_corpus_cost_abuse.json's
// costabuse-budget-allow-record-toctou-race case documented as a
// 100%-reproducible bug (20 of 20 concurrent requests allowed against a
// $1 cap, $20 real spend) and confirms Reserve/Reconcile now bounds it
// correctly.
//
// Uses the identical two-phase-barrier technique the original finding
// used: every one of the 20 goroutines completes its OWN Reserve call
// (the check) before ANY goroutine is allowed to proceed to Reconcile
// (the debit) — honestly modeling the real production gap, which is the
// full upstream LLM completion latency, not an artificially tight
// coincidence.
func TestConcurrentReserveReconcileBoundsAdmissionAgainstNearExhaustedCap(t *testing.T) {
	tr := NewTracker()
	const goroutines = 20
	capUSD := d("1")
	cost := d("1") // exactly enough remaining headroom for ONE request of this cost

	var reserveDone sync.WaitGroup // released once EVERY goroutine's Reserve call has returned
	reserveDone.Add(goroutines)
	var proceedToReconcile sync.WaitGroup // released once every Reserve call is done
	proceedToReconcile.Add(1)

	var allowedCount atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()

			allowed, reserved, reservedUSD := tr.Reserve("team-race", capUSD, 0)
			reserveDone.Done()

			// The real upstream-call gap: nothing reconciles until every
			// goroutine's own check has already happened.
			proceedToReconcile.Wait()

			if allowed {
				allowedCount.Add(1)
			}
			if reserved {
				tr.Reconcile("team-race", reservedUSD, &cost, 0)
			}
		}()
	}

	reserveDone.Wait()
	proceedToReconcile.Done()
	wg.Wait()

	if got := allowedCount.Load(); got != 1 {
		t.Fatalf("allowedCount = %d, want exactly 1 -- at most 1 of %d concurrent requests should be allowed when only one request's worth of budget headroom remains", got, goroutines)
	}
	finalSpent := tr.SpentUSD("team-race", 0)
	if !finalSpent.Equal(cost) {
		t.Fatalf("finalSpent = %s, want exactly %s -- recorded spend should never exceed the configured cap", finalSpent, cost)
	}
}

// TestConcurrentReserveReconcileReproducibleAcrossManyRuns re-runs the
// above scenario 5 times, matching the original finding's own "5
// consecutive runs" reproducibility bar -- proving the fix is
// deterministic, not merely lucky once.
func TestConcurrentReserveReconcileReproducibleAcrossManyRuns(t *testing.T) {
	for run := 0; run < 5; run++ {
		tr := NewTracker()
		const goroutines = 20
		capUSD := d("1")
		cost := d("1")

		var reserveDone sync.WaitGroup
		reserveDone.Add(goroutines)
		var proceedToReconcile sync.WaitGroup
		proceedToReconcile.Add(1)

		var allowedCount atomic.Int64
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				allowed, reserved, reservedUSD := tr.Reserve("team-race", capUSD, 0)
				reserveDone.Done()
				proceedToReconcile.Wait()
				if allowed {
					allowedCount.Add(1)
				}
				if reserved {
					tr.Reconcile("team-race", reservedUSD, &cost, 0)
				}
			}()
		}
		reserveDone.Wait()
		proceedToReconcile.Done()
		wg.Wait()

		if got := allowedCount.Load(); got != 1 {
			t.Fatalf("run %d: allowedCount = %d, want exactly 1", run, got)
		}
		if spent := tr.SpentUSD("team-race", 0); !spent.Equal(cost) {
			t.Fatalf("run %d: finalSpent = %s, want exactly %s", run, spent, cost)
		}
	}
}

// TestConcurrentAllowRecordStillRacesIfCalledDirectly is a permanent,
// live pin proving WHY Reserve/Reconcile exists and must be used by real
// request-handling code instead of Allow/Record directly: Allow/Record
// themselves are deliberately left unmodified by
// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md (every
// existing Allow/Record test keeps passing unmodified), which means they
// are STILL not concurrency-safe across the check-then-act gap when used
// directly -- exactly the bug
// evals/tests/fixtures/regression_corpus_cost_abuse.json's
// costabuse-budget-allow-record-toctou-race case documents. This test
// uses the identical barrier technique as the Reserve/Reconcile proof
// above, against the SAME scenario, and asserts the ORIGINAL buggy
// outcome (20 of 20 allowed) still reproduces via Allow/Record -- serving
// as this codebase's sanity-check-by-breaking evidence: reverting
// dataplane.go's/streaming.go's call sites back to Allow/Record would
// reintroduce exactly this race, which is why they must not be.
func TestConcurrentAllowRecordStillRacesIfCalledDirectly(t *testing.T) {
	tr := NewTracker()
	const goroutines = 20
	capUSD := d("1")
	cost := d("1")

	var checkDone sync.WaitGroup
	checkDone.Add(goroutines)
	var proceedToRecord sync.WaitGroup
	proceedToRecord.Add(1)

	var allowedCount atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			allowed := tr.Allow("team-race-unsafe", capUSD, 0)
			checkDone.Done()
			proceedToRecord.Wait()
			if allowed {
				allowedCount.Add(1)
				tr.Record("team-race-unsafe", cost, 0)
			}
		}()
	}
	checkDone.Wait()
	proceedToRecord.Done()
	wg.Wait()

	if got := allowedCount.Load(); got != goroutines {
		t.Fatalf("allowedCount = %d, want %d -- Allow/Record used directly (bypassing Reserve/Reconcile) must still reproduce the original TOCTOU race; if this now fails, either Allow/Record's own locking changed (it must not — every other Allow/Record test in this package depends on it staying as-is) or this test's own barrier technique is no longer exercising the race", got, goroutines)
	}
	finalSpent := tr.SpentUSD("team-race-unsafe", 0)
	want := decimal.NewFromInt(goroutines).Mul(cost)
	if !finalSpent.Equal(want) {
		t.Fatalf("finalSpent = %s, want exactly %s (%d x %s) -- Allow/Record's known-unsafe overspend shape", finalSpent, want, goroutines, cost)
	}
}
