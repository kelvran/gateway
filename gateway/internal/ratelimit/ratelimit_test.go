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

	if !b.Allow() || !b.Allow() {
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
	if !b.Allow() || !b.Allow() {
		t.Fatal("expected two Allow() calls to succeed at full capacity")
	}
	if b.Allow() {
		t.Fatal("Allow() succeeded a third time; refill must be capped at capacity")
	}
}
