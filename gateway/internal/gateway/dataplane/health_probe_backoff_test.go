package dataplane

// Tests for docs/rfcs/2026-09-08-gateway-health-probe-backoff.md:
// probeDueDeployments/rescheduleDeployment's per-deployment
// probe-interval backoff. Closes the honest FAIL case recorded in
// evals/tests/fixtures/regression_corpus_routing_chaos.json's
// "chaos-health-probe-no-backoff-on-repeated-failure" — a persistently
// failing deployment must be probed at a GROWING interval, not the
// fixed configured cadence forever, while a healthy deployment must
// never be affected at all.
//
// Uses a fake clock (p.now overridden directly, mirroring
// internal/budget.Tracker.now's own injectable-clock testing
// convention exactly — see internal/budget/reset_test.go) so backoff
// growth is proven against exact elapsed-time thresholds, never real
// sleeps.

import (
	"context"
	"testing"
	"time"
)

// fakeClock is this file's own injectable clock. A separate,
// package-local type rather than a shared cross-package helper — no
// such helper exists anywhere in this codebase today (internal/budget's
// own fakeClock, the closest precedent, is itself unexported and
// package-local); introducing one here would be exactly the kind of
// premature shared-infrastructure YAGNI this project's own conventions
// avoid for a single test file's need.
type fakeClock struct {
	t time.Time
}

func newFakeClock(t0 time.Time) *fakeClock { return &fakeClock{t: t0} }

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// TestProbeDueDeploymentsBacksOffIncreasinglyWhileDeploymentStaysUnhealthy
// is the plan's own required proof: a deployment that keeps failing
// every probe is probed at a GROWING interval (2x, then 4x the
// configured interval — healthProbeBackoffGrowthFactor doubling), never
// the fixed configured cadence forever. Drives probeDueDeployments
// directly against a fake clock advanced by exact, known amounts, and
// asserts on real upstream call counts — not just internal schedule
// state — so this proves the actual eligibility gating skips a call,
// not merely that some field changed.
func TestProbeDueDeploymentsBacksOffIncreasinglyWhileDeploymentStaysUnhealthy(t *testing.T) {
	upstream := newIntermittentUpstream("bad")
	deployments := twoDeploymentsSameModel()
	p := newTestPipeline(t, upstream.call, deployments)
	clock := newFakeClock(time.Now())
	p.now = clock.now
	ctx := context.Background()

	const interval = 10 * time.Second

	// 3 consecutive failed probes, one per plain interval, trips "bad"
	// unhealthy (default N-of-M threshold is 3) — every one of these 3
	// calls is due at the plain cadence, since "bad" is still healthy
	// (per router.IsHealthy) going into all 3 of them; only the 3rd call
	// itself flips it unhealthy.
	for i := 0; i < 3; i++ {
		p.probeDueDeployments(ctx, interval)
		clock.advance(interval)
	}
	if p.router.IsHealthy("bad") {
		t.Fatal("setup: \"bad\" should be unhealthy after 3 consecutive probe failures")
	}
	callsAfterTrip := upstream.countFor("bad")
	if callsAfterTrip != 3 {
		t.Fatalf("setup: \"bad\" probe count after tripping unhealthy = %d, want 3", callsAfterTrip)
	}

	// The clock is now exactly 1 plain interval past the trip. Without
	// backoff, "bad" would be due again right now — but its own
	// schedule just backed off to 2x the configured interval, so it
	// must NOT be probed yet.
	p.probeDueDeployments(ctx, interval)
	if got := upstream.countFor("bad"); got != callsAfterTrip {
		t.Errorf("\"bad\" probed after only 1 plain interval while unhealthy = %d calls, want %d (should still be backed off to 2x)", got, callsAfterTrip)
	}

	// Advancing the REST of the 2x backoff window (one more interval)
	// makes it due again.
	clock.advance(interval)
	p.probeDueDeployments(ctx, interval)
	if got, want := upstream.countFor("bad"), callsAfterTrip+1; got != want {
		t.Fatalf("\"bad\" probe count after the full 2x backoff window elapsed = %d, want %d (should be due)", got, want)
	}
	callsAfterTrip++

	// This probe was ALSO a failure (upstream still failing), so backoff
	// grows again: 2x -> 4x. One more plain interval is now nowhere near
	// enough.
	clock.advance(interval)
	p.probeDueDeployments(ctx, interval)
	if got := upstream.countFor("bad"); got != callsAfterTrip {
		t.Errorf("\"bad\" probed after only 1 more interval (3x total elapsed since last probe) = %d calls, want %d (needs 4x total, backoff should have grown again)", got, callsAfterTrip)
	}

	// Advancing the rest of the 4x window (3 more intervals) makes it
	// due once more.
	clock.advance(3 * interval)
	p.probeDueDeployments(ctx, interval)
	if got, want := upstream.countFor("bad"), callsAfterTrip+1; got != want {
		t.Errorf("\"bad\" probe count after the full 4x backoff window elapsed = %d, want %d (should be due)", got, want)
	}

	// Throughout all of this, "good" (healthy the entire time) must
	// still have been probed on every single call above — backoff must
	// never touch a currently-healthy deployment. 7 calls to
	// probeDueDeployments happened total in this test.
	if got, want := upstream.countFor("good"), 7; got != want {
		t.Errorf("\"good\" probe count = %d, want %d (a healthy deployment must be probed on every call, never backed off)", got, want)
	}
}

// TestProbeDueDeploymentsProbesHealthyDeploymentEveryIntervalWithNoBackoff
// is the plan's own required mirror-image proof: a deployment that is
// (and stays) healthy is probed on every single tick at the plain
// configured interval — never skipped, never backed off, regardless of
// how many times it has already been probed.
func TestProbeDueDeploymentsProbesHealthyDeploymentEveryIntervalWithNoBackoff(t *testing.T) {
	upstream := newIntermittentUpstream("") // never fails
	deployments := []Deployment{
		{Name: "solo", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	p := newTestPipeline(t, upstream.call, deployments)
	clock := newFakeClock(time.Now())
	p.now = clock.now
	ctx := context.Background()

	const interval = 5 * time.Second

	// First call: due (no schedule entry yet).
	p.probeDueDeployments(ctx, interval)
	if got := upstream.countFor("solo"); got != 1 {
		t.Fatalf("\"solo\" probe count after 1st call = %d, want 1", got)
	}

	// A moment before the interval elapses: must NOT be probed again.
	clock.advance(interval - time.Millisecond)
	p.probeDueDeployments(ctx, interval)
	if got := upstream.countFor("solo"); got != 1 {
		t.Errorf("\"solo\" probed before its own interval fully elapsed: count = %d, want 1", got)
	}

	// 5 further consecutive intervals: every single one must probe it
	// again — a perfectly healthy deployment never accumulates any
	// backoff at all.
	for i := 0; i < 5; i++ {
		clock.advance(time.Millisecond) // completes the interval exactly
		p.probeDueDeployments(ctx, interval)
		if got, want := upstream.countFor("solo"), i+2; got != want {
			t.Errorf("tick %d: \"solo\" probe count = %d, want %d (every tick must probe a healthy deployment)", i, got, want)
		}
		clock.advance(interval - time.Millisecond)
	}
}

// TestProbeDueDeploymentsRecoveredDeploymentResumesNormalIntervalImmediately
// is the plan's own required recovery proof: once a backed-off,
// previously-unhealthy deployment crosses back to healthy (per
// router.HealthConfig's own M-consecutive-success threshold), its very
// next scheduling decision reverts straight to the plain configured
// interval — never a lingering, still-growing (or still-large)
// backoff.
func TestProbeDueDeploymentsRecoveredDeploymentResumesNormalIntervalImmediately(t *testing.T) {
	upstream := newIntermittentUpstream("bad")
	deployments := twoDeploymentsSameModel()
	p := newTestPipeline(t, upstream.call, deployments)
	clock := newFakeClock(time.Now())
	p.now = clock.now
	ctx := context.Background()

	const interval = 10 * time.Second

	// Trip "bad" unhealthy: 3 consecutive failures, one per plain
	// interval (t=0,10,20 — the loop advances the clock AFTER each
	// call, so the clock sits at t=30 once the loop exits).
	for i := 0; i < 3; i++ {
		p.probeDueDeployments(ctx, interval)
		clock.advance(interval)
	}
	if p.router.IsHealthy("bad") {
		t.Fatal("setup: \"bad\" should be unhealthy after 3 consecutive probe failures")
	}
	// The trip probe (at t=20) backed "bad" off to 2x interval, due at
	// t=40. At the current t=30, it must not be due yet.
	p.probeDueDeployments(ctx, interval)
	if got, want := upstream.countFor("bad"), 3; got != want {
		t.Fatalf("\"bad\" probed while still inside its own 2x backoff window: count = %d, want %d", got, want)
	}

	// "bad"'s upstream recovers.
	upstream.mu.Lock()
	upstream.failingDeployment = ""
	upstream.mu.Unlock()

	// t=40: due. This is "bad"'s 1st consecutive SUCCESS, but
	// router.HealthConfig's default recovery threshold is 2 — still
	// unhealthy, so backoff grows again (2x -> 4x) rather than
	// resetting on a single success.
	clock.advance(interval)
	p.probeDueDeployments(ctx, interval)
	if p.router.IsHealthy("bad") {
		t.Fatal("setup: \"bad\" should still be unhealthy after only 1 consecutive successful probe (recovery threshold is 2)")
	}
	if got, want := upstream.countFor("bad"), 4; got != want {
		t.Fatalf("\"bad\" probe count after its 4x backoff window elapsed = %d, want %d", got, want)
	}

	// t=80 (t=40 + the new 4x=40s window): due again. This is the 2nd
	// consecutive success, crossing the recovery threshold.
	clock.advance(4 * interval)
	p.probeDueDeployments(ctx, interval)
	if !p.router.IsHealthy("bad") {
		t.Fatal("setup: \"bad\" should now be healthy after 2 consecutive successful probes")
	}
	if got, want := upstream.countFor("bad"), 5; got != want {
		t.Fatalf("\"bad\" probe count after recovering = %d, want %d", got, want)
	}

	// The load-bearing assertion: immediately after this recovery probe,
	// "bad"'s own schedule must show zero carried-over backoff and a
	// next-probe time exactly ONE plain interval away — not the 8x (or
	// even larger) value it would have kept growing toward had it
	// stayed unhealthy.
	p.probeMu.Lock()
	sched := p.probeSchedule["bad"]
	p.probeMu.Unlock()
	if sched == nil {
		t.Fatal("\"bad\" has no probe schedule entry after being probed")
	}
	if sched.backoff != 0 {
		t.Errorf("\"bad\" backoff after recovery = %v, want 0 (no lingering backoff on a currently-healthy deployment)", sched.backoff)
	}
	if want := clock.now().Add(interval); !sched.nextProbeAt.Equal(want) {
		t.Errorf("\"bad\" nextProbeAt after recovery = %v, want %v (exactly the plain configured interval, not a backed-off multiple)", sched.nextProbeAt, want)
	}

	// Behavioral confirmation of the same fact: a moment before that
	// one plain interval elapses, "bad" must not be probed; the instant
	// it does, it must be.
	clock.advance(interval - time.Millisecond)
	p.probeDueDeployments(ctx, interval)
	if got, want := upstream.countFor("bad"), 5; got != want {
		t.Errorf("\"bad\" probed before its post-recovery plain interval fully elapsed: count = %d, want %d", got, want)
	}
	clock.advance(time.Millisecond)
	p.probeDueDeployments(ctx, interval)
	if got, want := upstream.countFor("bad"), 6; got != want {
		t.Errorf("\"bad\" probe count exactly 1 plain interval after recovery = %d, want %d", got, want)
	}
}
