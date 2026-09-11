package router

import (
	"sync"
	"testing"
)

// TestAdmitTurnRejectsWhenHealthyIsFalseRegardlessOfRampingState is the
// direct, deterministic proof for a round-4 backlog-audit finding:
// selectHealthy used to call r.IsHealthy(name) and then, only if that
// returned true, a SEPARATE r.admitRampedTurn(name) call -- two
// independent lock acquisitions with no lock held across both.
// ReportProbeResult's own failure branch sets h.healthy=false AND
// h.ramping=false together in one locked write whenever
// UnhealthyThreshold trips -- so the split-call code's second call would
// see h.ramping==false and admit unconditionally via its own "not
// ramping -> always admit" shortcut, WITHOUT ever re-checking h.healthy,
// if that transition landed in the gap between the two calls. admitTurn
// (the merged replacement) must never do this: constructing the exact
// state ReportProbeResult's failure branch produces (healthy=false,
// ramping=false) and calling admitTurn directly proves it correctly
// returns false, not the old split-call code's would-be true.
func TestAdmitTurnRejectsWhenHealthyIsFalseRegardlessOfRampingState(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
	}, HealthConfig{UnhealthyThreshold: 1, HealthyThreshold: 2, RecoveryRampSteps: 4, RecoveryRampInitialPercent: 20})

	// Get "a" into a ramping state, then immediately trip it back
	// unhealthy -- ReportProbeResult's failure branch clears h.ramping to
	// false in the SAME locked write that sets h.healthy to false,
	// exactly reproducing the state the split-call race could expose.
	r.ReportProbeResult("a", false)
	r.ReportProbeResult("a", true)
	healthy, changed := r.ReportProbeResult("a", true) // 2nd success -> recovers, ramp starts
	if !healthy || !changed {
		t.Fatalf("setup: recovery ReportProbeResult = (healthy=%v, changed=%v), want (true, true)", healthy, changed)
	}
	healthy, changed = r.ReportProbeResult("a", false) // UnhealthyThreshold=1 -> trips again, clearing ramping too
	if healthy || !changed {
		t.Fatalf("setup: re-failure ReportProbeResult = (healthy=%v, changed=%v), want (false, true)", healthy, changed)
	}

	if r.admitTurn("a") {
		t.Fatal(`admitTurn("a") = true, want false -- "a" is genuinely unhealthy right now, regardless of its ramping state having just been cleared to false by the same transition`)
	}
}

// TestSelectConcurrentWithReportProbeResultNeverRacesUnderRaceDetector is
// a stress test proving the merged admitTurn has no data race under
// heavy concurrent Select/ReportProbeResult traffic -- run under `go test
// -race`. This does not (and cannot) deterministically reproduce the
// original nanosecond-scale TOCTOU window, but does prove the fix
// introduces no new synchronization hazard under real contention -- the
// same background-probe-loop-vs-request-handling-goroutine shape that
// exists in the live topology today (dataplane.RunHealthProbeLoop
// running concurrently with real request handling).
func TestSelectConcurrentWithReportProbeResultNeverRacesUnderRaceDetector(t *testing.T) {
	r := New([]Deployment{
		{Name: "a", Model: "gpt-4o"},
		{Name: "b", Model: "gpt-4o"},
	}, HealthConfig{UnhealthyThreshold: 2, HealthyThreshold: 2, RecoveryRampSteps: 3, RecoveryRampInitialPercent: 25})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.ReportProbeResult("a", i%2 == 0)
		}(i)
	}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Select("gpt-4o")
		}()
	}
	wg.Wait()
}
