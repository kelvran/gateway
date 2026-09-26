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

// TestPlainAcquireReleaseStillBehaveIdenticallyAfterAgentRunAddition is
// the explicit regression guard for the AcquireWithRun/ReleaseWithRun
// addition: reuses the exact same assertions
// TestConcurrencyLimiterRejectsNPlusOneWhileNInFlight already used before
// this change, proving the plain Acquire(keyID)/Release(keyID) wrappers
// this change introduces are non-breaking, not just additive.
func TestPlainAcquireReleaseStillBehaveIdenticallyAfterAgentRunAddition(t *testing.T) {
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

// TestAcquireWithRunTracksPerAgentRunCountsForCappedKey proves
// AcquireWithRun/ReleaseWithRun's new runCounts bookkeeping is accurate
// for a key that DOES have a configured MaxInFlight cap, and proves the
// pre-existing cap-enforcement logic still works unchanged underneath it
// -- runCounts is observation-only and never influences the admission
// decision.
func TestAcquireWithRunTracksPerAgentRunCountsForCappedKey(t *testing.T) {
	l := NewConcurrencyLimiter([]ConcurrencyConfig{{ID: "key-a", MaxInFlight: 5}})

	if !l.AcquireWithRun("key-a", "run-1") {
		t.Fatal("AcquireWithRun(key-a, run-1) #1 = false, want true")
	}
	if !l.AcquireWithRun("key-a", "run-1") {
		t.Fatal("AcquireWithRun(key-a, run-1) #2 = false, want true")
	}
	if !l.AcquireWithRun("key-a", "run-2") {
		t.Fatal("AcquireWithRun(key-a, run-2) = false, want true")
	}

	total, byRun := l.InFlightByAgentRun("key-a")
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if byRun["run-1"] != 2 {
		t.Errorf("byRun[run-1] = %d, want 2", byRun["run-1"])
	}
	if byRun["run-2"] != 1 {
		t.Errorf("byRun[run-2] = %d, want 1", byRun["run-2"])
	}

	// The overall cap (5) is still enforced across all agent runs
	// combined, exactly like before this change.
	l.AcquireWithRun("key-a", "run-1")
	l.AcquireWithRun("key-a", "run-2")
	if l.AcquireWithRun("key-a", "run-3") {
		t.Fatal("6th AcquireWithRun = true, want false -- key-a is already at its MaxInFlight cap of 5, combined across all agent runs")
	}

	l.ReleaseWithRun("key-a", "run-1")
	total, byRun = l.InFlightByAgentRun("key-a")
	if total != 4 {
		t.Fatalf("total after one release = %d, want 4", total)
	}
	if byRun["run-1"] != 2 {
		t.Errorf("byRun[run-1] after one release = %d, want 2", byRun["run-1"])
	}
}

// TestAcquireWithRunTracksPerAgentRunCountsForUncappedKeyEvenThoughInFlightNeverDoes
// is the critical case named in this feature's own spec: today's plain
// Acquire/Release never touch `inFlight` at all for a keyID with no
// configured MaxInFlight cap (Acquire's own early-return branch) --
// confirmed directly below via InFlight itself still reporting 0. Gating
// runCounts behind the same "only when limited" condition would make
// InFlightByAgentRun silently report zero/nothing for exactly the keys
// an operator most needs this signal for. This test proves that blind
// spot was NOT inherited: InFlightByAgentRun's own total/byAgentRun stay
// accurate for an uncapped key.
func TestAcquireWithRunTracksPerAgentRunCountsForUncappedKeyEvenThoughInFlightNeverDoes(t *testing.T) {
	l := NewConcurrencyLimiter(nil) // "no-limit-key" has no configured cap at all

	for i := 0; i < 5; i++ {
		if !l.AcquireWithRun("no-limit-key", "run-1") {
			t.Fatalf("AcquireWithRun #%d = false, want true -- an unconfigured key must never be rejected", i+1)
		}
	}
	if !l.AcquireWithRun("no-limit-key", "run-2") {
		t.Fatal("AcquireWithRun(no-limit-key, run-2) = false, want true")
	}

	if got := l.InFlight("no-limit-key"); got != 0 {
		t.Fatalf("InFlight(no-limit-key) = %d, want 0 -- confirms the pre-existing blind spot this test exists to route around, not a new bug introduced here", got)
	}

	total, byRun := l.InFlightByAgentRun("no-limit-key")
	if total != 6 {
		t.Fatalf("total = %d, want 6 -- InFlightByAgentRun must be accurate for an uncapped key even though InFlight itself is not", total)
	}
	if byRun["run-1"] != 5 {
		t.Errorf("byRun[run-1] = %d, want 5", byRun["run-1"])
	}
	if byRun["run-2"] != 1 {
		t.Errorf("byRun[run-2] = %d, want 1", byRun["run-2"])
	}

	l.ReleaseWithRun("no-limit-key", "run-1")
	total, byRun = l.InFlightByAgentRun("no-limit-key")
	if total != 5 {
		t.Fatalf("total after one release = %d, want 5", total)
	}
	if byRun["run-1"] != 4 {
		t.Errorf("byRun[run-1] after one release = %d, want 4", byRun["run-1"])
	}
}

// TestAcquireWithRunRejectionNeverRecordsANewAgentRun proves a rejected
// AcquireWithRun call for a BRAND-NEW agent_run_id (one that has never
// held a slot before) leaves runCounts/InFlightByAgentRun completely
// untouched for that run -- not just zero-valued, but absent as a map
// key entirely, proving no increment (transient or otherwise) ever
// happened. AcquireWithRun's own rejection branch (concurrency.go) returns
// false before ever calling recordRunAcquireLocked, entirely inside one
// Lock/defer Unlock critical section -- this is the regression guard for
// that structural guarantee.
func TestAcquireWithRunRejectionNeverRecordsANewAgentRun(t *testing.T) {
	l := NewConcurrencyLimiter([]ConcurrencyConfig{{ID: "key-a", MaxInFlight: 3}})

	for i, run := range []string{"run-1", "run-2", "run-3"} {
		if !l.AcquireWithRun("key-a", run) {
			t.Fatalf("AcquireWithRun #%d (%s) = false, want true (still under the cap of 3)", i+1, run)
		}
	}
	totalBefore, byRunBefore := l.InFlightByAgentRun("key-a")
	if totalBefore != 3 {
		t.Fatalf("setup: total = %d, want 3", totalBefore)
	}

	if l.AcquireWithRun("key-a", "run-4") {
		t.Fatal("4th AcquireWithRun(key-a, run-4) = true, want false -- key-a is already at its MaxInFlight cap of 3")
	}

	total, byRun := l.InFlightByAgentRun("key-a")
	if total != 3 {
		t.Fatalf("total after a rejected AcquireWithRun = %d, want still 3 (a rejected call must not reserve a slot)", total)
	}
	if _, ok := byRun["run-4"]; ok {
		t.Errorf("byRun[%q] is present after a rejected AcquireWithRun call, want it entirely absent as a map key -- got value %d", "run-4", byRun["run-4"])
	}
	if len(byRun) != len(byRunBefore) {
		t.Errorf("byRun has %d entries after the rejection, want still %d (no new agent_run_id keys introduced)", len(byRun), len(byRunBefore))
	}
}

// TestAcquireWithRunRejectionNeverIncrementsAnExistingAgentRunCount is
// the sharper sibling of the test above: the rejected call REUSES an
// agent_run_id that already holds some of the in-flight slots, proving
// that specific run's own count stays at its exact pre-rejection value,
// not incremented by even one.
func TestAcquireWithRunRejectionNeverIncrementsAnExistingAgentRunCount(t *testing.T) {
	l := NewConcurrencyLimiter([]ConcurrencyConfig{{ID: "key-a", MaxInFlight: 3}})

	if !l.AcquireWithRun("key-a", "run-1") {
		t.Fatal("AcquireWithRun(key-a, run-1) #1 = false, want true")
	}
	if !l.AcquireWithRun("key-a", "run-1") {
		t.Fatal("AcquireWithRun(key-a, run-1) #2 = false, want true")
	}
	if !l.AcquireWithRun("key-a", "run-2") {
		t.Fatal("AcquireWithRun(key-a, run-2) = false, want true")
	}

	// key-a is now at its cap of 3 (run-1 holds 2, run-2 holds 1). The
	// rejected 4th call reuses run-1, the exact scenario a naive
	// increment-then-fail-to-decrement bug would misreport.
	if l.AcquireWithRun("key-a", "run-1") {
		t.Fatal("4th AcquireWithRun(key-a, run-1) = true, want false -- key-a is already at its MaxInFlight cap of 3")
	}

	total, byRun := l.InFlightByAgentRun("key-a")
	if total != 3 {
		t.Fatalf("total after a rejected AcquireWithRun reusing run-1 = %d, want still 3", total)
	}
	if byRun["run-1"] != 2 {
		t.Errorf("byRun[run-1] after a rejected AcquireWithRun reusing run-1 = %d, want still 2 (unchanged by the rejection)", byRun["run-1"])
	}
	if byRun["run-2"] != 1 {
		t.Errorf("byRun[run-2] after an unrelated rejected call = %d, want still 1", byRun["run-2"])
	}
}

// TestInFlightByAgentRunReturnsSnapshotNotLiveReference proves
// InFlightByAgentRun's own doc comment's "fresh, independent copy, never
// a live reference" claim: a later Acquire/Release must never retroactively
// change an already-returned snapshot's contents. The load-bearing
// assertion is on byRun (a map, and therefore a reference type in Go --
// the only one of the two return values capable of exposing a live-
// reference bug at all; total is a plain int, copied by value
// regardless).
func TestInFlightByAgentRunReturnsSnapshotNotLiveReference(t *testing.T) {
	l := NewConcurrencyLimiter([]ConcurrencyConfig{{ID: "key-a", MaxInFlight: 10}})
	l.AcquireWithRun("key-a", "run-1")

	total, byRun := l.InFlightByAgentRun("key-a")
	if total != 1 || byRun["run-1"] != 1 {
		t.Fatalf("initial snapshot = total=%d byRun=%v, want total=1 byRun[run-1]=1", total, byRun)
	}

	// Mutate live state AFTER the snapshot above was taken.
	l.AcquireWithRun("key-a", "run-1")
	l.AcquireWithRun("key-a", "run-2")

	if byRun["run-1"] != 1 {
		t.Errorf("snapshot's byRun[run-1] = %d after a later AcquireWithRun, want still 1 -- InFlightByAgentRun must return a copy, not a live reference", byRun["run-1"])
	}
	if _, ok := byRun["run-2"]; ok {
		t.Errorf("snapshot's byRun unexpectedly gained run-2 (%v) after it was taken -- a live reference would show this, a copy must not", byRun)
	}

	// A fresh call now DOES reflect the mutations -- confirming the
	// mutations above were real, and the earlier snapshot's staleness was
	// because it's a copy, not because the mutations silently failed.
	total, byRun = l.InFlightByAgentRun("key-a")
	if total != 3 {
		t.Fatalf("fresh total = %d, want 3", total)
	}
	if byRun["run-1"] != 2 || byRun["run-2"] != 1 {
		t.Fatalf("fresh byRun = %v, want run-1=2 run-2=1", byRun)
	}
}
