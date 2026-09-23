package anomaly

import "testing"

// TestObserveNeverFlagsBeforeFirstWindowCompletes proves no anomaly is
// ever reported until a full window's worth of observations has landed.
func TestObserveNeverFlagsBeforeFirstWindowCompletes(t *testing.T) {
	d := NewDetector(5, 0.1, 3.0)
	for i := 0; i < 4; i++ {
		if d.Observe("key-a", true) {
			t.Fatalf("Observe #%d flagged before the window (size 5) even completed", i+1)
		}
	}
}

// TestObserveNeverFlagsTheFirstCompletedWindow proves the first-ever
// completed window for a key never flags, since there is no baseline
// yet to compare against.
func TestObserveNeverFlagsTheFirstCompletedWindow(t *testing.T) {
	d := NewDetector(5, 0.1, 3.0)
	var flaggedAny bool
	for i := 0; i < 5; i++ {
		if d.Observe("key-a", true) {
			flaggedAny = true
		}
	}
	if flaggedAny {
		t.Fatal("Observe flagged on the very first completed window, want false (no baseline exists yet)")
	}
}

// TestObserveFlagsARealShiftAboveBothThresholds is the load-bearing
// positive case: a low-rate baseline window followed by a high-rate
// recent window, both above the absolute floor, must flag.
func TestObserveFlagsARealShiftAboveBothThresholds(t *testing.T) {
	d := NewDetector(10, 0.1, 3.0)

	// Baseline window: 1/10 flagged (10%).
	observeN(d, "key-a", 1, 9)

	// Recent window: 6/10 flagged (60%) -- 6x the baseline rate, well
	// above both the 3x shiftFactor and the 0.1 absolute floor.
	var flaggedAny bool
	for _, flagged := range []bool{true, true, true, true, true, true, false, false, false, false} {
		if d.Observe("key-a", flagged) {
			flaggedAny = true
		}
	}
	if !flaggedAny {
		t.Fatal("Observe did not flag a real 6x rate shift (10% baseline -> 60% recent) above both thresholds")
	}
}

// TestObserveDoesNotFlagWhenBelowAbsoluteFloor proves the minFlaggedRate
// floor suppresses noise even when the relative shift is large: a
// near-zero baseline followed by a still-tiny recent rate must not flag,
// even though the RATIO between them is large.
func TestObserveDoesNotFlagWhenBelowAbsoluteFloor(t *testing.T) {
	d := NewDetector(100, 0.1, 3.0)

	// Baseline: 1/100 flagged (1%).
	observeN(d, "key-a", 1, 99)

	// Recent: 4/100 flagged (4%) -- a real 4x shift, but still below the
	// 0.1 (10%) absolute floor.
	var flaggedAny bool
	for i := 0; i < 100; i++ {
		if d.Observe("key-a", i < 4) {
			flaggedAny = true
		}
	}
	if flaggedAny {
		t.Fatal("Observe flagged a shift whose recent rate (4%) is below the absolute floor (10%) -- the floor should have suppressed this")
	}
}

// TestObserveDoesNotFlagAStableRate proves a baseline stream never
// flags itself across window rotations -- the false-positive guard.
func TestObserveDoesNotFlagAStableRate(t *testing.T) {
	d := NewDetector(10, 0.1, 3.0)

	var flaggedAny bool
	for window := 0; window < 5; window++ {
		for _, flagged := range []bool{true, false, false, false, false, false, false, false, false, false} {
			if d.Observe("key-a", flagged) {
				flaggedAny = true
			}
		}
	}
	if flaggedAny {
		t.Fatal("Observe flagged a perfectly stable 10% rate across multiple window rotations, want no flag")
	}
}

// TestObserveTracksEachKeyIndependently proves one key's anomalous shift
// never affects another key's own windows.
func TestObserveTracksEachKeyIndependently(t *testing.T) {
	d := NewDetector(10, 0.1, 3.0)

	// key-a: stable low rate.
	observeN(d, "key-a", 1, 9)
	// key-b: about to spike.
	observeN(d, "key-b", 1, 9)

	if d.Observe("key-a", false) {
		t.Fatal("key-a's own stable second window incorrectly flagged")
	}

	var keyBFlagged bool
	for _, flagged := range []bool{true, true, true, true, true, true, false, false, false, false} {
		if d.Observe("key-b", flagged) {
			keyBFlagged = true
		}
	}
	if !keyBFlagged {
		t.Fatal("key-b's real spike was not flagged")
	}
}

// TestObserveEmptyKeyIsANoOp proves an empty key (e.g. auth failed, no
// virtual key resolved) never panics or accumulates state.
func TestObserveEmptyKeyIsANoOp(t *testing.T) {
	d := NewDetector(5, 0.1, 3.0)
	for i := 0; i < 20; i++ {
		if d.Observe("", true) {
			t.Fatal("Observe(\"\", ...) flagged, want always false")
		}
	}
}

// observeN feeds flagged observations followed by (total-flagged)
// unflagged ones into d for key, ignoring the return value -- a test
// helper for building up a window with an exact known rate.
func observeN(d *Detector, key string, flagged, unflagged int) {
	for i := 0; i < flagged; i++ {
		d.Observe(key, true)
	}
	for i := 0; i < unflagged; i++ {
		d.Observe(key, false)
	}
}
