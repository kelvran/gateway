package budget

import (
	"sync"
	"testing"
)

func TestAllowWithNoRecordedSpendUnderPositiveCap(t *testing.T) {
	tr := NewTracker()
	if !tr.Allow("team-alpha", 10) {
		t.Error("Allow with no recorded spend and a positive cap = false, want true")
	}
}

func TestAllowZeroOrNegativeCapAlwaysUnlimited(t *testing.T) {
	tr := NewTracker()
	tr.Record("team-alpha", 1_000_000)
	if !tr.Allow("team-alpha", 0) {
		t.Error("Allow with capUSD=0 after huge spend = false, want true (unlimited)")
	}
	if !tr.Allow("team-alpha", -5) {
		t.Error("Allow with capUSD<0 after huge spend = false, want true (unlimited)")
	}
}

func TestAllowBoundaryExactlyAtCap(t *testing.T) {
	tr := NewTracker()
	tr.Record("team-alpha", 10)
	// Spend == cap must NOT be allowed: the cap is a strict upper bound,
	// not an inclusive one — a request that would push spend to exactly
	// the cap already happened; the NEXT request must be rejected.
	if tr.Allow("team-alpha", 10) {
		t.Error("Allow with spend == cap = true, want false (cap is a strict upper bound)")
	}
	// One cent under the cap must still be allowed.
	tr2 := NewTracker()
	tr2.Record("team-beta", 9.99)
	if !tr2.Allow("team-beta", 10) {
		t.Error("Allow with spend just under cap = false, want true")
	}
}

func TestAllowPastCapRejected(t *testing.T) {
	tr := NewTracker()
	tr.Record("team-alpha", 15)
	if tr.Allow("team-alpha", 10) {
		t.Error("Allow with spend > cap = true, want false")
	}
}

func TestRecordAccumulates(t *testing.T) {
	tr := NewTracker()
	tr.Record("team-alpha", 3)
	tr.Record("team-alpha", 4)
	if tr.Allow("team-alpha", 7) {
		t.Error("Allow after recording 3+4=7 against a cap of 7 = true, want false")
	}
	if !tr.Allow("team-alpha", 7.01) {
		t.Error("Allow after recording 3+4=7 against a cap of 7.01 = false, want true")
	}
}

func TestRecordNegativeCostIgnored(t *testing.T) {
	tr := NewTracker()
	tr.Record("team-alpha", 5)
	tr.Record("team-alpha", -100) // must not reduce recorded spend
	if tr.Allow("team-alpha", 5) {
		t.Error("Allow after a negative Record reduced spend below the cap = true, want false")
	}
}

func TestKeysTrackIndependently(t *testing.T) {
	tr := NewTracker()
	tr.Record("team-alpha", 100)
	if !tr.Allow("team-beta", 10) {
		t.Error("recording spend for team-alpha affected team-beta's Allow result — keys must track independently")
	}
}

func TestConcurrentRecordNeverLosesAnUpdate(t *testing.T) {
	tr := NewTracker()
	const goroutines = 100
	const perGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				tr.Record("team-alpha", 1)
			}
		}()
	}
	wg.Wait()

	const want = float64(goroutines * perGoroutine)
	// Allow(id, want) must be false (spend == cap, not < cap) if every one
	// of the 10,000 concurrent Record calls actually landed.
	if tr.Allow("team-alpha", want) {
		t.Errorf("after %v concurrent Record(1) calls, Allow(cap=%v) = true, want false (some updates were lost)", want, want)
	}
	if !tr.Allow("team-alpha", want+0.01) {
		t.Errorf("after %v concurrent Record(1) calls, Allow(cap=%v) = false, want true (some updates over-counted)", want, want+0.01)
	}
}
