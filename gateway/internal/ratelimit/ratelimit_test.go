package ratelimit

import (
	"testing"
	"time"
)

// fakeClock is an injectable clock that only advances when Advance is
// called, letting tests exercise token-bucket refill deterministically
// and instantly rather than sleeping on wall-clock time.
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func TestBurstCapacityConsumableImmediately(t *testing.T) {
	b := NewTokenBucket(3, 1) // burst of 3, refill 1/sec

	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatalf("Allow() #%d = false, want true (within burst capacity)", i+1)
		}
	}
}

func TestRejectedUntilRefill(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	b := NewTokenBucketWithClock(2, 1, clock.now) // burst of 2, refill 1/sec

	// Two distinct calls, each consuming a separate token — written as
	// two named results (rather than the equivalent
	// "!b.Allow() || !b.Allow()") so staticcheck's SA4000
	// identical-expression check doesn't mistake this for a copy-paste
	// bug: both calls are intentional and each has its own side effect.
	firstAllowed, secondAllowed := b.Allow(), b.Allow()
	if !firstAllowed || !secondAllowed {
		t.Fatal("expected first two Allow() calls to succeed within burst capacity")
	}

	// Burst exhausted, no time elapsed yet.
	if b.Allow() {
		t.Fatal("Allow() succeeded with no tokens and no elapsed time")
	}

	// Not enough time elapsed for even one token to refill.
	clock.Advance(500 * time.Millisecond)
	if b.Allow() {
		t.Fatal("Allow() succeeded before a full token had refilled")
	}

	// Enough time for exactly one token.
	clock.Advance(600 * time.Millisecond) // total 1.1s elapsed since exhaustion
	if !b.Allow() {
		t.Fatal("Allow() failed after enough time elapsed for a token to refill")
	}

	// That token is now spent; immediately rejected again.
	if b.Allow() {
		t.Fatal("Allow() succeeded immediately after consuming the just-refilled token")
	}
}

func TestRefillNeverExceedsCapacity(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	b := NewTokenBucketWithClock(2, 100, clock.now) // fast refill, low capacity

	// Let a huge amount of (fake) time pass.
	clock.Advance(time.Hour)

	// Capacity caps tokens at 2, not unbounded — only two Allow() calls
	// should succeed before this refill cycle rejects a third.
	// See TestRejectedUntilRefill's comment above: two intentional,
	// separately-consuming calls, named to avoid staticcheck's SA4000
	// identical-expression false positive.
	firstAllowed, secondAllowed := b.Allow(), b.Allow()
	if !firstAllowed || !secondAllowed {
		t.Fatal("expected two Allow() calls to succeed at full capacity")
	}
	if b.Allow() {
		t.Fatal("Allow() succeeded a third time; refill must be capped at capacity")
	}
}

// TestDebitCanOverdraftBelowZero proves the deliberate design choice in
// docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md: real token usage is
// only known after a request completes, so Debit must be able to push
// the balance negative — there's no way to have "checked before
// spending" for a cost that wasn't known at check time.
func TestDebitCanOverdraftBelowZero(t *testing.T) {
	b := NewTokenBucket(10, 1)
	b.Debit(15)
	if b.HasBalance() {
		t.Fatal("HasBalance() = true after debiting more than the full balance, want false")
	}
}

// TestHasBalanceNeverConsumes proves HasBalance is read-only, unlike
// Allow — calling it repeatedly must never itself drain the bucket.
func TestHasBalanceNeverConsumes(t *testing.T) {
	b := NewTokenBucket(5, 0) // no refill, so any drain would be visible
	for i := 0; i < 10; i++ {
		if !b.HasBalance() {
			t.Fatalf("HasBalance() call #%d = false, want true (never consumes)", i+1)
		}
	}
}

// TestDebitRecoversViaOrdinaryRefill proves an overdrawn bucket recovers
// exactly like a normal one — refill is unconditional, not gated on the
// balance having stayed non-negative.
func TestDebitRecoversViaOrdinaryRefill(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	b := NewTokenBucketWithClock(10, 5, clock.now) // refill 5/sec
	b.Debit(12)                                    // balance now -2
	if b.HasBalance() {
		t.Fatal("HasBalance() = true immediately after overdrafting, want false")
	}
	clock.Advance(time.Second) // +5 tokens -> balance now 3
	if !b.HasBalance() {
		t.Fatal("HasBalance() = false after enough refill to recover from the overdraft, want true")
	}
}

// TestIncreaseReservationAppliesWhenDeltaFitsBalance is the direct unit
// proof for IncreaseReservation — the mid-stream reservation top-up half
// of the streaming concurrent-sibling reservation gap fix, per
// docs/upgrade-research/gateway-streaming-concurrent-sibling-
// reservation-gap-2026-09-09.md.
func TestIncreaseReservationAppliesWhenDeltaFitsBalance(t *testing.T) {
	b := NewTokenBucket(100, 0)
	allowed, reserved := b.ReserveTPM() // fresh bucket, no history: reserves the full 100
	if !allowed || reserved != 100 {
		t.Fatalf("setup ReserveTPM() = (%v, %v), want (true, 100)", allowed, reserved)
	}
	// b.tokens is now 0. Topping up FROM 100 (the current reservation) TO
	// 100 (no real increase) must be a no-op regardless of balance.
	allowed2, applied2 := b.IncreaseReservation(100, 100)
	if !allowed2 || applied2 != 100 {
		t.Fatalf("IncreaseReservation(100 -> 100, no change) = (%v, %v), want (true, 100)", allowed2, applied2)
	}

	// A bucket with real remaining balance: reserve 2, top up to 5.
	b2 := NewTokenBucket(100, 0)
	b2.ReserveTPM() // reserves the full 100, tokens now 0
	realTokens := 2.0
	b2.ReconcileTPM(100, &realTokens) // tokens back to 100 - 2 = 98, billedCount=1
	_, small := b2.ReserveTPM()       // historical average 2/1=2; tokens now 98-2=96
	if small != 2 {
		t.Fatalf("second ReserveTPM() reservation = %v, want 2 (historical average)", small)
	}
	allowed3, applied3 := b2.IncreaseReservation(small, 50)
	if !allowed3 || applied3 != 50 {
		t.Fatalf("IncreaseReservation(2 -> 50) = (%v, %v), want (true, 50) — 48 more delta fits comfortably in the 96-token balance", allowed3, applied3)
	}
}

// TestIncreaseReservationRejectsWhenDeltaExceedsBalance is the
// load-bearing proof this fix exists for: a top-up that would push past
// the bucket's real remaining balance must be rejected outright, leaving
// the bucket's balance completely untouched — never a partial top-up.
func TestIncreaseReservationRejectsWhenDeltaExceedsBalance(t *testing.T) {
	b := NewTokenBucket(10, 0)
	b.Debit(9) // balance now 1 — as if a small reservation already reduced it
	current := 1.0

	allowed, applied := b.IncreaseReservation(current, 5)
	if allowed {
		t.Fatal("IncreaseReservation(1 -> 5) against a balance of only 1 = true, want false")
	}
	if applied != current {
		t.Errorf("appliedTokens on rejection = %v, want the ORIGINAL current reservation (1), unchanged", applied)
	}
	if b.tokens != 1 {
		t.Errorf("b.tokens after a REJECTED top-up = %v, want 1 — balance must be completely untouched", b.tokens)
	}
}

// TestIncreaseReservationNoOpWhenNotActuallyIncreasing mirrors
// budget.Tracker's identical proof: a call where newReservedTokens is
// not strictly greater than currentReservedTokens changes nothing and
// always succeeds, even with near-zero real balance left — callers rely
// on this cheap fast path to skip the lock-acquiring path on every chunk
// of a stream that hasn't grown past its own reservation yet.
func TestIncreaseReservationNoOpWhenNotActuallyIncreasing(t *testing.T) {
	b := NewTokenBucket(10, 0)
	b.Debit(9.99) // balance now 0.01
	current := 5.0

	for _, newAmount := range []float64{5, 3, 0} {
		allowed, applied := b.IncreaseReservation(current, newAmount)
		if !allowed || applied != current {
			t.Errorf("IncreaseReservation(5 -> %v) = (%v, %v), want (true, 5) — not an increase, must be a no-op even with near-zero real balance left", newAmount, allowed, applied)
		}
	}
	if diff := b.tokens - 0.01; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("b.tokens after only no-op calls = %v, want ~0.01 — unchanged", b.tokens)
	}
}
