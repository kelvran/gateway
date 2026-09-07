package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentReserveTPMBoundsAdmissionAgainstCapacityOneBucket is the
// load-bearing proof for
// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md:
// reproduces the exact scenario
// evals/tests/fixtures/regression_corpus_cost_abuse.json's
// costabuse-ratelimit-tpm-hasbalance-debit-toctou-race case documented
// as a 100%-reproducible bug (20 of 20 concurrent requests passed the
// TPM pre-check against a capacity-1 bucket) and confirms
// ReserveTPM/ReconcileTPM now bounds it correctly.
//
// Uses the identical two-phase-barrier technique the original finding
// used: every one of the 20 goroutines completes its OWN ReserveTPM call
// (the check) before ANY goroutine is allowed to proceed to
// ReconcileTPM (the debit) — honestly modeling the real production gap,
// which is the full upstream LLM completion latency.
func TestConcurrentReserveTPMBoundsAdmissionAgainstCapacityOneBucket(t *testing.T) {
	b := NewTokenBucket(1, 0) // capacity 1, no refill
	const goroutines = 20

	var reserveDone sync.WaitGroup
	reserveDone.Add(goroutines)
	var proceedToReconcile sync.WaitGroup
	proceedToReconcile.Add(1)

	var passedCount atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()

			allowed, reservedTokens := b.ReserveTPM()
			reserveDone.Done()

			// The real upstream-call gap: nothing reconciles until every
			// goroutine's own check has already happened.
			proceedToReconcile.Wait()

			if allowed {
				passedCount.Add(1)
				realTokens := 1.0
				b.ReconcileTPM(reservedTokens, &realTokens)
			}
		}()
	}

	reserveDone.Wait()
	proceedToReconcile.Done()
	wg.Wait()

	if got := passedCount.Load(); got != 1 {
		t.Fatalf("passedCount = %d, want exactly 1 -- at most as many concurrent requests as the configured TPM bucket capacity (1) supports should pass the pre-check before any Debit-equivalent call lands", got)
	}
}

// TestConcurrentReserveTPMReproducibleAcrossManyRuns re-runs the above
// scenario 5 times, matching the original finding's own "5 consecutive
// runs" reproducibility bar.
func TestConcurrentReserveTPMReproducibleAcrossManyRuns(t *testing.T) {
	for run := 0; run < 5; run++ {
		b := NewTokenBucket(1, 0)
		const goroutines = 20

		var reserveDone sync.WaitGroup
		reserveDone.Add(goroutines)
		var proceedToReconcile sync.WaitGroup
		proceedToReconcile.Add(1)

		var passedCount atomic.Int64
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				allowed, reservedTokens := b.ReserveTPM()
				reserveDone.Done()
				proceedToReconcile.Wait()
				if allowed {
					passedCount.Add(1)
					realTokens := 1.0
					b.ReconcileTPM(reservedTokens, &realTokens)
				}
			}()
		}
		reserveDone.Wait()
		proceedToReconcile.Done()
		wg.Wait()

		if got := passedCount.Load(); got != 1 {
			t.Fatalf("run %d: passedCount = %d, want exactly 1", run, got)
		}
	}
}

// TestConcurrentHasBalanceDebitStillRacesIfCalledDirectly is a permanent,
// live pin proving WHY ReserveTPM/ReconcileTPM exist and must be used by
// real request-handling code instead of HasBalance/Debit directly:
// HasBalance/Debit themselves are deliberately left unmodified by
// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md (every
// existing HasBalance/Debit test keeps passing unmodified), which means
// they are STILL not concurrency-safe across the check-then-act gap when
// used directly — exactly the bug
// evals/tests/fixtures/regression_corpus_cost_abuse.json's
// costabuse-ratelimit-tpm-hasbalance-debit-toctou-race case documents.
// This test uses the identical barrier technique as the ReserveTPM proof
// above, against the SAME scenario, and asserts the ORIGINAL buggy
// outcome (20 of 20 pass) still reproduces via HasBalance/Debit —
// serving as this codebase's sanity-check-by-breaking evidence:
// reverting dataplane.go's checkRateLimit/finalize back to
// AllowTPM/RecordTokens would reintroduce exactly this race, which is
// why they must not be.
func TestConcurrentHasBalanceDebitStillRacesIfCalledDirectly(t *testing.T) {
	b := NewTokenBucket(1, 0) // capacity 1, no refill
	const goroutines = 20

	var checkDone sync.WaitGroup
	checkDone.Add(goroutines)
	var proceedToDebit sync.WaitGroup
	proceedToDebit.Add(1)

	var passedCount atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			passed := b.HasBalance()
			checkDone.Done()
			proceedToDebit.Wait()
			if passed {
				passedCount.Add(1)
				b.Debit(1)
			}
		}()
	}
	checkDone.Wait()
	proceedToDebit.Done()
	wg.Wait()

	if got := passedCount.Load(); got != goroutines {
		t.Fatalf("passedCount = %d, want %d -- HasBalance/Debit used directly (bypassing ReserveTPM/ReconcileTPM) must still reproduce the original TOCTOU race; if this now fails, either HasBalance/Debit's own locking changed (it must not — every other HasBalance/Debit test in this package depends on it staying as-is) or this test's own barrier technique is no longer exercising the race", got, goroutines)
	}
}
