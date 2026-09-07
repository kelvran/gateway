package ratelimit

import "testing"

// TestConcurrencyLimiterRejectsNPlusOneWhileNInFlight is the pure,
// non-HTTP proof of the mechanism: acquire exactly MaxInFlight slots,
// assert the next Acquire for the same key fails while they're all still
// held, then release one and assert the next Acquire now succeeds.
func TestConcurrencyLimiterRejectsNPlusOneWhileNInFlight(t *testing.T) {
	l := NewConcurrencyLimiter([]ConcurrencyConfig{{ID: "key-a", MaxInFlight: 3}})

	for i := 0; i < 3; i++ {
		if !l.Acquire("key-a") {
			t.Fatalf("Acquire #%d = false, want true (still under the cap of 3)", i+1)
		}
	}
	if got := l.InFlight("key-a"); got != 3 {
		t.Fatalf("InFlight = %d, want 3", got)
	}

	if l.Acquire("key-a") {
		t.Fatal("4th Acquire = true, want false -- key-a is already at its MaxInFlight cap of 3")
	}
	if got := l.InFlight("key-a"); got != 3 {
		t.Fatalf("InFlight after a rejected Acquire = %d, want still 3 (a rejected Acquire must not reserve a slot)", got)
	}

	l.Release("key-a")
	if got := l.InFlight("key-a"); got != 2 {
		t.Fatalf("InFlight after one Release = %d, want 2", got)
	}
	if !l.Acquire("key-a") {
		t.Fatal("Acquire after a Release = false, want true -- a freed slot must become available again")
	}
}

// TestConcurrencyLimiterUnconfiguredKeyIsUnlimited proves the
// "unconfigured means unlimited" convention: a keyID never registered
// with a positive MaxInFlight always succeeds, no matter how many times
// Acquire is called.
func TestConcurrencyLimiterUnconfiguredKeyIsUnlimited(t *testing.T) {
	l := NewConcurrencyLimiter(nil)
	for i := 0; i < 50; i++ {
		if !l.Acquire("no-limit-key") {
			t.Fatalf("Acquire #%d = false, want true -- an unconfigured key must never be rejected", i+1)
		}
	}
}

// TestConcurrencyLimiterZeroOrNegativeMaxInFlightMeansUnlimited proves
// ConcurrencyConfig's own documented "<= 0 means unlimited" rule at
// construction time.
func TestConcurrencyLimiterZeroOrNegativeMaxInFlightMeansUnlimited(t *testing.T) {
	l := NewConcurrencyLimiter([]ConcurrencyConfig{
		{ID: "zero-key", MaxInFlight: 0},
		{ID: "negative-key", MaxInFlight: -5},
	})
	for _, key := range []string{"zero-key", "negative-key"} {
		for i := 0; i < 10; i++ {
			if !l.Acquire(key) {
				t.Fatalf("key %q: Acquire #%d = false, want true (MaxInFlight <= 0 means unlimited)", key, i+1)
			}
		}
	}
}

// TestConcurrencyLimiterPerKeyIsolation proves one key's cap is never
// affected by a different key's in-flight count -- the same per-tenant
// isolation discipline this codebase's other per-key controls already
// enforce.
func TestConcurrencyLimiterPerKeyIsolation(t *testing.T) {
	l := NewConcurrencyLimiter([]ConcurrencyConfig{
		{ID: "key-a", MaxInFlight: 1},
		{ID: "key-b", MaxInFlight: 1},
	})

	if !l.Acquire("key-a") {
		t.Fatal("key-a's first Acquire = false, want true")
	}
	if l.Acquire("key-a") {
		t.Fatal("key-a's second Acquire = true, want false -- already at cap 1")
	}
	if !l.Acquire("key-b") {
		t.Fatal("key-b's Acquire = false, want true -- key-a being at cap must never affect key-b")
	}
}

// TestConcurrencyLimiterReleaseWithoutAcquireNeverGoesNegative proves
// Release is a safe no-op for a keyID with no outstanding slots --
// defends against a Release without a matching successful Acquire.
func TestConcurrencyLimiterReleaseWithoutAcquireNeverGoesNegative(t *testing.T) {
	l := NewConcurrencyLimiter([]ConcurrencyConfig{{ID: "key-a", MaxInFlight: 2}})
	l.Release("key-a")
	l.Release("key-a")
	if got := l.InFlight("key-a"); got != 0 {
		t.Fatalf("InFlight = %d, want 0 (Release without a matching Acquire must never go negative)", got)
	}
	// Confirms the cap still enforces normally afterward -- an
	// undefended negative-count bug would let more than MaxInFlight
	// through here.
	firstOK := l.Acquire("key-a")
	secondOK := l.Acquire("key-a")
	if !firstOK || !secondOK {
		t.Fatalf("expected the first two Acquire calls to succeed, got %v and %v", firstOK, secondOK)
	}
	if l.Acquire("key-a") {
		t.Fatal("3rd Acquire = true, want false -- still capped at 2")
	}
}

// TestConcurrencyLimiterRegisterLiveUpdateNeverDropsInFlightCount mirrors
// KeyLimiter.Register's own live-update precedent: an admin update
// changing a key's limit while requests are genuinely in flight for it
// must not lose track of them.
func TestConcurrencyLimiterRegisterLiveUpdateNeverDropsInFlightCount(t *testing.T) {
	l := NewConcurrencyLimiter([]ConcurrencyConfig{{ID: "key-a", MaxInFlight: 5}})
	l.Acquire("key-a")
	l.Acquire("key-a")
	if got := l.InFlight("key-a"); got != 2 {
		t.Fatalf("InFlight = %d, want 2", got)
	}

	l.Register(ConcurrencyConfig{ID: "key-a", MaxInFlight: 3})
	if got := l.InFlight("key-a"); got != 2 {
		t.Fatalf("InFlight after Register = %d, want still 2 -- a live limit update must never reset in-flight bookkeeping", got)
	}
	if !l.Acquire("key-a") {
		t.Fatal("3rd Acquire after raising the cap to 3 = false, want true")
	}
	if l.Acquire("key-a") {
		t.Fatal("4th Acquire = true, want false -- the new cap of 3 must be enforced")
	}

	// Registering MaxInFlight <= 0 must remove the limit entirely,
	// mirroring KeyLimiter.Register's identical "an update must not leave
	// a stale entry behind" rule for its own TPM/per-model buckets.
	l.Register(ConcurrencyConfig{ID: "key-a", MaxInFlight: 0})
	if !l.Acquire("key-a") {
		t.Fatal("Acquire after clearing the limit = false, want true -- MaxInFlight <= 0 must mean unlimited again")
	}
}
