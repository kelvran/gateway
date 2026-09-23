package budget

import (
	"context"
	"testing"
	"time"
)

// TestCheckAndMarkBudgetWarnAlertedFiresOncePerEpoch proves the core
// dedup contract: the first call within a rolling-window epoch reports
// newlyCrossed, a second call within the SAME epoch does not, and a call
// after the epoch advances (a fresh rolling window) reports newlyCrossed
// again — mirrors
// TestCheckAndMarkBudgetAlertBucketRefiresAfterWindowReset's own
// epoch-advance mechanism (Record + a fake clock) exactly.
func TestCheckAndMarkBudgetWarnAlertedFiresOncePerEpoch(t *testing.T) {
	tr := NewTracker()
	clock := newFakeClock(time.Now())
	tr.now = clock.now
	const window = time.Hour

	// Establish the window.
	tr.Record("team-alpha", d("1"), window)

	if !tr.CheckAndMarkBudgetWarnAlerted(context.Background(), "team-alpha", window) {
		t.Fatal("first CheckAndMarkBudgetWarnAlerted call = false, want true (first warn this window)")
	}
	if tr.CheckAndMarkBudgetWarnAlerted(context.Background(), "team-alpha", window) {
		t.Error("second CheckAndMarkBudgetWarnAlerted call within the same epoch = true, want false (already warned)")
	}
	// A third call, still within the same window, must still be
	// suppressed — this isn't a one-shot latch that flips back after a
	// single check.
	if tr.CheckAndMarkBudgetWarnAlerted(context.Background(), "team-alpha", window) {
		t.Error("third CheckAndMarkBudgetWarnAlerted call within the same epoch = true, want false (still already warned)")
	}

	// Roll the window over.
	clock.advance(2 * window)
	tr.Record("team-alpha", d("0.01"), window) // triggers resetIfNeeded, bumping periodEpoch

	if !tr.CheckAndMarkBudgetWarnAlerted(context.Background(), "team-alpha", window) {
		t.Error("CheckAndMarkBudgetWarnAlerted after the epoch advanced = false, want true (a new window must re-warn)")
	}
	if tr.CheckAndMarkBudgetWarnAlerted(context.Background(), "team-alpha", window) {
		t.Error("second CheckAndMarkBudgetWarnAlerted call in the NEW window = true, want false (already warned in this new window)")
	}
}

// TestCheckAndMarkBudgetWarnAlertedFiresOnFirstCallEvenAtEpochZero is the
// direct regression proof for why the in-memory branch must use the
// two-value map form (t.warnAlertedEpoch[keyID]) rather than a bare index
// expression: a lifetime-cap key (resetInterval <= 0) never advances
// periodEpoch past its zero value, which is ALSO map[string]int64's own
// zero value. A naive `warnAlertedEpoch[keyID] == currentEpoch` check
// would therefore treat the very first call for such a key as
// "already warned" and never fire at all. Break this by reverting
// CheckAndMarkBudgetWarnAlerted's in-memory branch to a bare index
// expression instead of the comma-ok form: this test starts failing
// because the very first call returns false.
func TestCheckAndMarkBudgetWarnAlertedFiresOnFirstCallEvenAtEpochZero(t *testing.T) {
	tr := NewTracker()

	if !tr.CheckAndMarkBudgetWarnAlerted(context.Background(), "team-alpha", 0) {
		t.Fatal("first CheckAndMarkBudgetWarnAlerted call against a lifetime-cap key (resetInterval=0, periodEpoch always 0) = false, want true")
	}
	if tr.CheckAndMarkBudgetWarnAlerted(context.Background(), "team-alpha", 0) {
		t.Error("second call = true, want false (already warned -- a lifetime cap never advances its epoch)")
	}
}

// TestCheckAndMarkBudgetWarnAlertedKeysTrackIndependently proves one
// key's warn-dedup state never leaks into another's.
func TestCheckAndMarkBudgetWarnAlertedKeysTrackIndependently(t *testing.T) {
	tr := NewTracker()

	if !tr.CheckAndMarkBudgetWarnAlerted(context.Background(), "team-alpha", 0) {
		t.Fatal("team-alpha first call = false, want true")
	}
	if tr.CheckAndMarkBudgetWarnAlerted(context.Background(), "team-alpha", 0) {
		t.Fatal("team-alpha second call = true, want false")
	}
	if !tr.CheckAndMarkBudgetWarnAlerted(context.Background(), "team-beta", 0) {
		t.Error("team-beta first call = false, want true -- team-alpha's warn state must not leak")
	}
}

// TestCheckAndMarkBudgetWarnAlertedDoesNotShareStateWithAlertBucketLadder
// proves the two dedup mechanisms are genuinely independent: marking the
// alert-bucket ladder for a key must not affect that same key's
// warn-alert dedup, and vice versa.
func TestCheckAndMarkBudgetWarnAlertedDoesNotShareStateWithAlertBucketLadder(t *testing.T) {
	tr := NewTracker()

	if _, crossed := tr.CheckAndMarkBudgetAlertBucket(context.Background(), "team-alpha", 0.9, 0); !crossed {
		t.Fatal("CheckAndMarkBudgetAlertBucket(0.9) = no crossing, want crossing")
	}
	if !tr.CheckAndMarkBudgetWarnAlerted(context.Background(), "team-alpha", 0) {
		t.Error("CheckAndMarkBudgetWarnAlerted after the alert ladder already fired = false, want true -- the two mechanisms must not share dedup state")
	}
}
