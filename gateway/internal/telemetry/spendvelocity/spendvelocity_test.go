package spendvelocity

import "testing"

func TestNewRejectsNonPositiveSigma(t *testing.T) {
	for _, sigma := range []float64{0, -1} {
		if _, err := New(100, sigma, 0.5, 4); err == nil {
			t.Errorf("New with sigma=%v: want an error, got nil", sigma)
		}
	}
}

func TestNewAcceptsPositiveSigma(t *testing.T) {
	d, err := New(100, 10, 0.5, 4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d.Target != 100 || d.Sigma != 10 {
		t.Errorf("Target/Sigma = %v/%v, want 100/10", d.Target, d.Sigma)
	}
}

// TestObserveNeverAlarmsOnStableSeriesNearTarget proves ordinary noise
// around Target never accumulates into a false alarm — the whole point
// of SlackSigma existing at all.
func TestObserveNeverAlarmsOnStableSeriesNearTarget(t *testing.T) {
	d, err := New(100, 10, 0.5, 4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Oscillates within +-1 sigma of Target, well inside SlackSigma's
	// own +-0.5-sigma tolerance band once averaged over many points.
	series := []float64{102, 98, 105, 95, 101, 99, 103, 97, 100, 104, 96, 100}
	for i, x := range series {
		alarmed, _ := d.Observe(x)
		if alarmed {
			t.Fatalf("Observe(%v) at index %d: alarmed=true on a stable series, want false", x, i)
		}
	}
}

// TestObserveAlarmsOnSustainedIncrease proves a real, sustained upward
// shift eventually crosses ThresholdSigma and alarms with the correct
// direction.
func TestObserveAlarmsOnSustainedIncrease(t *testing.T) {
	d, err := New(100, 10, 0.5, 4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A sustained +2-sigma shift, per the source research's own cited
	// "1-sigma shift caught in ~6 points at h=4sigma" -- a 2-sigma shift
	// should alarm at least that fast, well within 20 observations.
	var alarmed bool
	var direction Direction
	for i := 0; i < 20 && !alarmed; i++ {
		alarmed, direction = d.Observe(120)
		_ = i
	}
	if !alarmed {
		t.Fatal("Observe never alarmed on a sustained +2-sigma shift within 20 observations")
	}
	if direction != DirectionIncrease {
		t.Errorf("direction = %v, want DirectionIncrease", direction)
	}
}

// TestObserveAlarmsOnSustainedDecrease proves the detector is genuinely
// two-sided -- a sustained DOWNWARD shift alarms too, not just an
// upward one. Real for spend velocity specifically: an unexpected spend
// drop (e.g. a fallback chain silently routing to a misconfigured free
// deployment) is itself a real signal worth catching.
func TestObserveAlarmsOnSustainedDecrease(t *testing.T) {
	d, err := New(100, 10, 0.5, 4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var alarmed bool
	var direction Direction
	for i := 0; i < 20 && !alarmed; i++ {
		alarmed, direction = d.Observe(80)
		_ = i
	}
	if !alarmed {
		t.Fatal("Observe never alarmed on a sustained -2-sigma shift within 20 observations")
	}
	if direction != DirectionDecrease {
		t.Errorf("direction = %v, want DirectionDecrease", direction)
	}
}

// TestObserveResetsAutomaticallyAfterAlarm proves Observe clears both
// cumulative sums immediately after alarming, so it is ready to detect
// the NEXT shift rather than continuing to alarm on every subsequent
// observation while the sum stays past threshold.
func TestObserveResetsAutomaticallyAfterAlarm(t *testing.T) {
	d, err := New(100, 10, 0.5, 4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 20; i++ {
		if alarmed, _ := d.Observe(120); alarmed {
			break
		}
	}
	if d.upper != 0 || d.lower != 0 {
		t.Errorf("upper/lower = %v/%v after an alarm, want 0/0 (auto-reset)", d.upper, d.lower)
	}
	// Immediately back to Target -- a fresh detector (post-reset) must
	// not alarm on the very next in-band observation.
	if alarmed, _ := d.Observe(100); alarmed {
		t.Error("Observe(Target) immediately after an auto-reset alarmed, want false")
	}
}

// TestResetClearsStateManually proves the exposed Reset method behaves
// identically to the automatic post-alarm reset, for a caller absorbing
// a known, expected spend-rate change (e.g. a deliberate price-table
// update) without that change itself counting as an anomaly.
func TestResetClearsStateManually(t *testing.T) {
	d, err := New(100, 10, 0.5, 4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.Observe(115)
	d.Observe(118)
	if d.upper == 0 {
		t.Fatal("upper == 0 before Reset, want non-zero accumulated evidence to reset")
	}
	d.Reset()
	if d.upper != 0 || d.lower != 0 {
		t.Errorf("upper/lower = %v/%v after Reset, want 0/0", d.upper, d.lower)
	}
}
