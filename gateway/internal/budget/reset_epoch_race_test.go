package budget

import (
	"testing"
	"time"
)

// TestReconcileDoesNotUndercountAcrossAConcurrentlyTriggeredReset
// reproduces a real, previously unaddressed race: a rolling-window reset
// triggered by a DIFFERENT concurrent caller (any
// Allow/SpentUSD/Reserve/Record/Reconcile call against the same key)
// between one request's Reserve call and its own later Reconcile call.
// Before the epoch fix, Reconcile had no way to detect this and
// unconditionally subtracted its own (now-stale) reservedUSD from the
// ALREADY-RESET ledger, driving spend negative by exactly the leaked
// reservation amount -- a real money-leak (the tenant silently gains
// free headroom equal to the reservation size in the new window).
func TestReconcileDoesNotUndercountAcrossAConcurrentlyTriggeredReset(t *testing.T) {
	tr := NewTracker()
	clock := newFakeClock(time.Now())
	tr.now = clock.now
	const window = time.Hour
	capUSD := d("100")

	// Establish billing history so the reservation amount isn't "full
	// headroom" (which would make the race harder to see clearly).
	allowed, reserved, reservedUSD, epoch := tr.Reserve("k", capUSD, window)
	if !allowed || !reserved {
		t.Fatalf("first reserve: allowed=%v reserved=%v", allowed, reserved)
	}
	cost1 := d("10")
	tr.Reconcile("k", reservedUSD, epoch, &cost1, window)

	// R1's own reservation, still within the same window.
	allowed, reserved, r1Reserved, r1Epoch := tr.Reserve("k", capUSD, window)
	if !allowed || !reserved {
		t.Fatalf("second reserve: allowed=%v reserved=%v", allowed, reserved)
	}

	// A DIFFERENT concurrent caller crosses the reset boundary while R1's
	// own upstream call is still in flight.
	clock.advance(window + time.Second)
	tr.SpentUSD("k", window) // triggers the reset on R1's behalf

	// R1 now finishes and reconciles its (window-crossed) reservation.
	cost2 := d("12")
	tr.Reconcile("k", r1Reserved, r1Epoch, &cost2, window)

	got := tr.SpentUSD("k", window)
	if !got.Equal(cost2) {
		t.Errorf("spent after R1's reconcile across a concurrent reset = %s, want %s -- R1's real cost must land fully in the new window, not be undercounted by its own stale, already-reset reservation (%s)", got, cost2, r1Reserved)
	}
}

// TestIncreaseReservationDoesNotUndercountAcrossAConcurrentlyTriggeredReset
// is IncreaseReservation's own counterpart to the Reconcile proof above —
// a long-running stream's mid-stream top-up call can straddle the exact
// same concurrently-triggered reset. Without the epoch check, the top-up
// computes its delta against a currentReservedUSD that no longer exists
// in the (already-reset) ledger, applying only the delta rather than
// reserving the full newReservedUSD fresh — silently undercounting the
// key's real outstanding claim by exactly the stale reservation amount.
func TestIncreaseReservationDoesNotUndercountAcrossAConcurrentlyTriggeredReset(t *testing.T) {
	tr := NewTracker()
	clock := newFakeClock(time.Now())
	tr.now = clock.now
	const window = time.Hour
	capUSD := d("100")

	// Establish billing history so the SECOND reservation is small
	// (historical-average-sized, $10) rather than "full headroom" —
	// otherwise a $80 top-up would never exceed currentReservedUSD and
	// the no-op fast path would trigger regardless of epoch.
	allowed, reserved, reservedUSD, epoch := tr.Reserve("k", capUSD, window)
	if !allowed || !reserved {
		t.Fatalf("first reserve: allowed=%v reserved=%v", allowed, reserved)
	}
	cost1 := d("10")
	tr.Reconcile("k", reservedUSD, epoch, &cost1, window)

	allowed, reserved, r1Reserved, r1Epoch := tr.Reserve("k", capUSD, window)
	if !allowed || !reserved || !r1Reserved.Equal(d("10")) {
		t.Fatalf("second reserve: allowed=%v reserved=%v r1Reserved=%v, want (true, true, 10)", allowed, reserved, r1Reserved)
	}

	// A DIFFERENT concurrent caller crosses the reset boundary while R1's
	// own stream is still in flight.
	clock.advance(window + time.Second)
	tr.SpentUSD("k", window) // triggers the reset on R1's behalf

	// R1's stream grew past its original $10 reservation and tries to
	// top up to $80, still carrying its now-stale r1Epoch.
	allowedTopup, applied, newEpoch := tr.IncreaseReservation("k", capUSD, r1Reserved, d("80"), r1Epoch, window)
	if !allowedTopup {
		t.Fatalf("IncreaseReservation across a concurrent reset = allowed=false, want true (the fresh window has full $100 headroom)")
	}

	got := tr.SpentUSD("k", window)
	if !got.Equal(d("80")) {
		t.Errorf("spent after a stale-epoch top-up = %s, want 80 — the top-up must reserve newReservedUSD fresh against the new window (0 + 80), never compute a delta (80-10=70) against the old, already-reset reservation", got)
	}

	finalCost := d("70")
	tr.Reconcile("k", applied, newEpoch, &finalCost, window)
	if got := tr.SpentUSD("k", window); !got.Equal(finalCost) {
		t.Errorf("spent after Reconcile = %s, want %s", got, finalCost)
	}
}
