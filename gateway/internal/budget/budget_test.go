package budget

import (
	"sync"
	"testing"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestAllowWithNoRecordedSpendUnderPositiveCap(t *testing.T) {
	tr := NewTracker()
	if !tr.Allow("team-alpha", d("10")) {
		t.Error("Allow with no recorded spend and a positive cap = false, want true")
	}
}

// TestUnrecordedKeyZeroValueBehavesAsZero explicitly verifies (rather
// than assumes) that decimal.Decimal's zero value — what a never-recorded
// map key yields — behaves as the number 0 for both Allow and Add,
// mirroring the implicit-zero behavior the old float64 map had.
func TestUnrecordedKeyZeroValueBehavesAsZero(t *testing.T) {
	tr := NewTracker()
	var zero decimal.Decimal // the exact zero value, never constructed via decimal.New*
	if !zero.IsZero() {
		t.Fatalf("decimal.Decimal{} zero value .IsZero() = false, want true")
	}
	if !tr.Allow("never-seen", d("0.01")) {
		t.Error("Allow for a never-recorded key under a tiny positive cap = false, want true")
	}
}

func TestAllowZeroOrNegativeCapAlwaysUnlimited(t *testing.T) {
	tr := NewTracker()
	tr.Record("team-alpha", d("1000000"))
	if !tr.Allow("team-alpha", decimal.Zero) {
		t.Error("Allow with capUSD=0 after huge spend = false, want true (unlimited)")
	}
	if !tr.Allow("team-alpha", d("-5")) {
		t.Error("Allow with capUSD<0 after huge spend = false, want true (unlimited)")
	}
}

func TestAllowBoundaryExactlyAtCap(t *testing.T) {
	tr := NewTracker()
	tr.Record("team-alpha", d("10"))
	// Spend == cap must NOT be allowed: the cap is a strict upper bound,
	// not an inclusive one — a request that would push spend to exactly
	// the cap already happened; the NEXT request must be rejected.
	if tr.Allow("team-alpha", d("10")) {
		t.Error("Allow with spend == cap = true, want false (cap is a strict upper bound)")
	}
	// One cent under the cap must still be allowed.
	tr2 := NewTracker()
	tr2.Record("team-beta", d("9.99"))
	if !tr2.Allow("team-beta", d("10")) {
		t.Error("Allow with spend just under cap = false, want true")
	}
}

func TestAllowPastCapRejected(t *testing.T) {
	tr := NewTracker()
	tr.Record("team-alpha", d("15"))
	if tr.Allow("team-alpha", d("10")) {
		t.Error("Allow with spend > cap = true, want false")
	}
}

func TestRecordAccumulates(t *testing.T) {
	tr := NewTracker()
	tr.Record("team-alpha", d("3"))
	tr.Record("team-alpha", d("4"))
	if tr.Allow("team-alpha", d("7")) {
		t.Error("Allow after recording 3+4=7 against a cap of 7 = true, want false")
	}
	if !tr.Allow("team-alpha", d("7.01")) {
		t.Error("Allow after recording 3+4=7 against a cap of 7.01 = false, want true")
	}
}

// TestRecordAccumulatesExactlyAcrossManySmallAdditions is the load-bearing
// precision test for docs/rfcs/2026-09-02-decimal-cost-accounting.md:
// the same accumulation pattern verified (empirically, before writing
// that RFC) to drift under float64 — 10,000 additions of a realistic
// small per-request cost fragment — must sum to EXACTLY the decimal
// total here, not an approximation.
func TestRecordAccumulatesExactlyAcrossManySmallAdditions(t *testing.T) {
	tr := NewTracker()
	fragment := d("0.0000075")
	const n = 10000
	for i := 0; i < n; i++ {
		tr.Record("team-alpha", fragment)
	}

	exact := d("0.075") // 10000 * 0.0000075, exact
	if tr.Allow("team-alpha", exact) {
		t.Errorf("Allow(cap=%v) = true after accumulating exactly that amount, want false (spend == cap is a strict boundary — this also proves the accumulated sum is exactly %v, not merely close to it)", exact, exact)
	}
	justOver := exact.Add(d("0.0000001"))
	if !tr.Allow("team-alpha", justOver) {
		t.Errorf("Allow(cap=%v) = false, want true — accumulated spend must be exactly %v, not drifted above it", justOver, exact)
	}
}

func TestRecordNegativeCostIgnored(t *testing.T) {
	tr := NewTracker()
	tr.Record("team-alpha", d("5"))
	tr.Record("team-alpha", d("-100")) // must not reduce recorded spend
	if tr.Allow("team-alpha", d("5")) {
		t.Error("Allow after a negative Record reduced spend below the cap = true, want false")
	}
}

func TestKeysTrackIndependently(t *testing.T) {
	tr := NewTracker()
	tr.Record("team-alpha", d("100"))
	if !tr.Allow("team-beta", d("10")) {
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
				tr.Record("team-alpha", decimal.NewFromInt(1))
			}
		}()
	}
	wg.Wait()

	want := decimal.NewFromInt(goroutines * perGoroutine)
	// Allow(id, want) must be false (spend == cap, not < cap) if every one
	// of the 10,000 concurrent Record calls actually landed.
	if tr.Allow("team-alpha", want) {
		t.Errorf("after %v concurrent Record(1) calls, Allow(cap=%v) = true, want false (some updates were lost)", want, want)
	}
	if !tr.Allow("team-alpha", want.Add(d("0.01"))) {
		t.Errorf("after %v concurrent Record(1) calls, Allow(cap=%v) = false, want true (some updates over-counted)", want, want.Add(d("0.01")))
	}
}
