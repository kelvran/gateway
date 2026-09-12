package budget

// Direct unit tests for IncreaseReservation — the mid-stream reservation
// top-up half of the streaming concurrent-sibling reservation gap fix,
// per docs/upgrade-research/gateway-streaming-concurrent-sibling-
// reservation-gap-2026-09-09.md.

import "testing"

func TestIncreaseReservationAppliesWhenDeltaFitsUnderCap(t *testing.T) {
	tr := NewTracker()
	allowed, reserved, initial, initialEpoch := tr.Reserve("team-alpha", d("10"), 0)
	if !allowed || !reserved || !initial.Equal(d("10")) {
		t.Fatalf("setup Reserve = (%v, %v, %v), want (true, true, 10) — cold start reserves full headroom", allowed, reserved, initial)
	}

	// Top up beyond the original reservation, but still within the same
	// $10 cap (nothing else has been recorded against this key).
	allowed2, applied, _ := tr.IncreaseReservation("team-alpha", d("10"), initial, d("10"), initialEpoch, 0)
	if !allowed2 || !applied.Equal(d("10")) {
		t.Fatalf("IncreaseReservation(10 -> 10, no real change) = (%v, %v), want (true, 10)", allowed2, applied)
	}

	// A genuinely larger key with real headroom: reserve 2, top up to 5.
	tr2 := NewTracker()
	_, _, r2, r2Epoch := tr2.Reserve("team-beta", d("100"), 0) // cold start: reserves the full 100
	// Simulate a smaller ORIGINAL reservation (as a historical-average
	// case would produce) by reconciling down to a small real cost first.
	realCost := d("2")
	tr2.Reconcile("team-beta", r2, r2Epoch, &realCost, 0)
	// Now billedCount > 0, so the NEXT Reserve sizes off the historical
	// average (2/1 = 2), not the full 98 remaining headroom.
	allowed3, reserved3, small, smallEpoch := tr2.Reserve("team-beta", d("100"), 0)
	if !allowed3 || !reserved3 || !small.Equal(d("2")) {
		t.Fatalf("second Reserve = (%v, %v, %v), want (true, true, 2) — historical average from the single $2 reconciled call", allowed3, reserved3, small)
	}

	allowed4, applied4, _ := tr2.IncreaseReservation("team-beta", d("100"), small, d("50"), smallEpoch, 0)
	if !allowed4 || !applied4.Equal(d("50")) {
		t.Fatalf("IncreaseReservation(2 -> 50) = (%v, %v), want (true, 50) — 48 more delta fits comfortably under the remaining headroom", allowed4, applied4)
	}
	// Spent so far: 2 (real, reconciled) + 50 (topped-up reservation) = 52.
	if spent := tr2.SpentUSD("team-beta", 0); !spent.Equal(d("52")) {
		t.Errorf("SpentUSD(team-beta) after top-up = %s, want 52", spent)
	}
}

// TestIncreaseReservationRejectsWhenDeltaExceedsCap is the load-bearing
// proof this fix exists for: a top-up that would push the key's total
// spend past its own cap must be rejected outright, leaving the existing,
// smaller reservation completely untouched — never a partial top-up.
func TestIncreaseReservationRejectsWhenDeltaExceedsCap(t *testing.T) {
	tr := NewTracker()
	// A key with only $1 of headroom, already holding a small $0.10
	// reservation (as if sized off a cheap historical average).
	tr.Record("team-alpha", d("0.90"), 0)
	current := d("0.10")

	allowed, applied, _ := tr.IncreaseReservation("team-alpha", d("1"), current, d("0.50"), 0, 0)
	if allowed {
		t.Fatal("IncreaseReservation(0.10 -> 0.50) against only $0.10 of real headroom = true, want false")
	}
	if !applied.Equal(current) {
		t.Errorf("appliedUSD on rejection = %s, want the ORIGINAL current reservation (0.10), unchanged", applied)
	}
	if spent := tr.SpentUSD("team-alpha", 0); !spent.Equal(d("0.90")) {
		t.Errorf("SpentUSD after a REJECTED top-up = %s, want 0.90 — spent must be completely untouched by a rejected delta", spent)
	}
}

// TestIncreaseReservationNoOpWhenNotActuallyIncreasing proves the cheap-
// no-lock-needed fast path: a call where newReservedUSD is not strictly
// greater than currentReservedUSD changes nothing and always succeeds —
// callers rely on this to skip the expensive path on every chunk of a
// stream that hasn't grown past its own reservation yet.
func TestIncreaseReservationNoOpWhenNotActuallyIncreasing(t *testing.T) {
	tr := NewTracker()
	tr.Record("team-alpha", d("0.99"), 0) // leaves only $0.01 of headroom under a $1 cap
	current := d("0.50")

	for _, newAmount := range []string{"0.50", "0.30", "0"} {
		allowed, applied, _ := tr.IncreaseReservation("team-alpha", d("1"), current, d(newAmount), 0, 0)
		if !allowed || !applied.Equal(current) {
			t.Errorf("IncreaseReservation(0.50 -> %s) = (%v, %v), want (true, 0.50) — not an increase, must be a no-op even with near-zero real headroom left", newAmount, allowed, applied)
		}
	}
	if spent := tr.SpentUSD("team-alpha", 0); !spent.Equal(d("0.99")) {
		t.Errorf("SpentUSD after only no-op calls = %s, want 0.99 — unchanged", spent)
	}
}

// TestIncreaseReservationUnlimitedKeyAlwaysAllowed mirrors Reserve's own
// "capUSD <= 0 means unlimited" convention.
func TestIncreaseReservationUnlimitedKeyAlwaysAllowed(t *testing.T) {
	tr := NewTracker()
	tr.Record("team-alpha", d("1000000"), 0)
	// capUSD<=0 mirrors Reserve's own unlimited convention: nothing is
	// ever tracked as a reservation for an unlimited key (Reserve itself
	// returns reservedUSD=decimal.Zero for one), so IncreaseReservation
	// correctly returns the UNCHANGED current value here, not
	// newReservedUSD — there is nothing to reconcile against later.
	allowed, applied, _ := tr.IncreaseReservation("team-alpha", d("0"), d("0"), d("999999"), 0, 0)
	if !allowed || !applied.Equal(d("0")) {
		t.Errorf("IncreaseReservation with capUSD<=0 = (%v, %v), want (true, 0) — unlimited, unchanged", allowed, applied)
	}
}
