package ratelimit

import (
	"testing"
	"time"
)

// TestEqualJitterBackoffFloorAndCeiling pins frac to its two extremes to
// assert the exact deterministic floor/ceiling bounds the formula
// guarantees at several attempt counts — this is what proves the
// FORMULA is right, independent of any real elapsed-time measurement.
func TestEqualJitterBackoffFloorAndCeiling(t *testing.T) {
	const base = 100 * time.Millisecond
	const maxDelay = 1 * time.Second

	tests := []struct {
		attempt   int
		wantFloor time.Duration
		wantCeil  time.Duration
	}{
		// attempt 1: exp = base*2^0 = 100ms, half = 50ms.
		{attempt: 1, wantFloor: 50 * time.Millisecond, wantCeil: 100 * time.Millisecond},
		// attempt 2: exp = base*2^1 = 200ms, half = 100ms.
		{attempt: 2, wantFloor: 100 * time.Millisecond, wantCeil: 200 * time.Millisecond},
		// attempt 4: exp = base*2^3 = 800ms, half = 400ms.
		{attempt: 4, wantFloor: 400 * time.Millisecond, wantCeil: 800 * time.Millisecond},
		// attempt 10: exp would be 100ms*2^9 = 51.2s, far past cap (1s) —
		// must clamp to cap, half = 500ms.
		{attempt: 10, wantFloor: 500 * time.Millisecond, wantCeil: 1 * time.Second},
		// attempt 0 (and negative) must behave exactly like attempt 1 --
		// there is no "zeroth" backoff.
		{attempt: 0, wantFloor: 50 * time.Millisecond, wantCeil: 100 * time.Millisecond},
	}

	for _, tt := range tests {
		floor := EqualJitterBackoff(tt.attempt, base, maxDelay, 0.0)
		if floor != tt.wantFloor {
			t.Errorf("EqualJitterBackoff(%d, frac=0.0) = %v, want exactly the floor %v", tt.attempt, floor, tt.wantFloor)
		}
		ceiling := EqualJitterBackoff(tt.attempt, base, maxDelay, 0.999999999)
		if ceiling < tt.wantFloor || ceiling > tt.wantCeil {
			t.Errorf("EqualJitterBackoff(%d, frac~1.0) = %v, want within [%v, %v]", tt.attempt, ceiling, tt.wantFloor, tt.wantCeil)
		}
		if ceiling <= floor {
			t.Errorf("EqualJitterBackoff(%d): frac~1.0 result %v must exceed frac=0.0 result %v", tt.attempt, ceiling, floor)
		}
	}
}

// TestRetryBackoffRecordGrowsWithConsecutiveStreakOverRealElapsedTime
// proves the backoff genuinely, measurably spaces out real wall-clock
// time across repeated rejections for the same key -- not merely that
// Record returns an increasing number in isolation. Uses the real
// (non-injected) math/rand source, and actually sleeps for each returned
// duration, then asserts the observed real elapsed time for later calls
// is never less than that streak position's theoretical floor.
func TestRetryBackoffRecordGrowsWithConsecutiveStreakOverRealElapsedTime(t *testing.T) {
	b := NewRetryBackoff()
	const keyID = "real-time-test-key"

	// Streak 1's floor: retryBackoffBase/2 = 250ms. Streak 2's floor:
	// retryBackoffBase (500ms). Sleeping for the ACTUAL returned delay
	// each time and measuring real time.Now() deltas is what makes this
	// a genuine real-elapsed-time proof, not a mocked-clock one.
	for streak := 1; streak <= 2; streak++ {
		wantFloor := EqualJitterBackoff(streak, retryBackoffBase, retryBackoffCap, 0.0)

		start := time.Now()
		delay := b.Record(keyID)
		time.Sleep(delay)
		elapsed := time.Since(start)

		if delay < wantFloor {
			t.Fatalf("streak %d: Record returned %v, want >= floor %v", streak, delay, wantFloor)
		}
		// A generous tolerance for scheduler slack (real production code,
		// no injected clock) -- this only needs to prove the sleep
		// genuinely happened for approximately the returned duration, not
		// pin exact scheduler timing.
		if elapsed < delay-5*time.Millisecond {
			t.Fatalf("streak %d: real elapsed time %v is less than the returned delay %v -- time.Sleep did not genuinely block for it", streak, elapsed, delay)
		}
	}
}

// TestRetryBackoffResetClearsStreak proves a success genuinely resets
// escalation for that key: after Reset, the next Record starts back at
// streak 1's floor, not a continuation of the prior streak.
func TestRetryBackoffResetClearsStreak(t *testing.T) {
	b := NewRetryBackoffWithRand(func() float64 { return 0.0 }) // pin jitter to the floor for a deterministic assertion
	const keyID = "reset-test-key"

	streak1 := b.Record(keyID)
	streak2 := b.Record(keyID)
	if streak2 <= streak1 {
		t.Fatalf("streak2 = %v, want > streak1 = %v -- consecutive rejections must escalate", streak2, streak1)
	}

	b.Reset(keyID)
	afterReset := b.Record(keyID)
	if afterReset != streak1 {
		t.Errorf("first Record after Reset = %v, want exactly streak1's original value %v -- Reset must clear the streak back to zero", afterReset, streak1)
	}
}

// TestRetryBackoffPerKeyIsolation proves one virtual key's rejection
// streak never affects a different key's -- the same per-tenant
// isolation discipline this codebase's other per-key controls (KeyLimiter,
// budget.Tracker) already enforce elsewhere.
func TestRetryBackoffPerKeyIsolation(t *testing.T) {
	b := NewRetryBackoffWithRand(func() float64 { return 0.0 })

	b.Record("key-a")
	b.Record("key-a")
	b.Record("key-a") // key-a is now at streak 3

	firstForKeyB := EqualJitterBackoff(1, retryBackoffBase, retryBackoffCap, 0.0)
	got := b.Record("key-b")
	if got != firstForKeyB {
		t.Errorf("key-b's first Record = %v, want %v (streak-1's floor) — key-a's rejections must never leak into key-b's streak", got, firstForKeyB)
	}
}
