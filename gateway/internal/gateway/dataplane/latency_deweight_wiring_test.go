package dataplane

import (
	"context"
	"testing"
	"time"
)

// slowFastUpstream is a two-deployment UpstreamCaller fixture: requests
// routed to slowDeployment sleep slowDelay before succeeding; every
// other deployment succeeds immediately -- a real, measurable elapsed-
// time difference for updateLatencyDeweighting's own probe-timing signal
// to pick up, without needing to fake time.Now itself.
func slowFastUpstream(slowDeployment string, slowDelay time.Duration) UpstreamCaller {
	return func(ctx context.Context, dep Deployment, req any) (any, error) {
		if dep.Name == slowDeployment {
			time.Sleep(slowDelay)
		}
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}
}

// TestProbeDeploymentsSetsLatencyFactorForASlowerPeer is the end-to-end
// wiring proof for updateLatencyDeweighting: after probing a model
// group whose two deployments have a real, measurable latency
// difference, the slower one's traffic share (via the real router.Select
// path) must be measurably reduced -- never fully starved (it's still
// healthy) -- closing self-hosted-inference-server-integration-depth-
// 2026-09-22.md's own finding that Select had zero load/latency signal.
func TestProbeDeploymentsSetsLatencyFactorForASlowerPeer(t *testing.T) {
	deployments := twoDeploymentsSameModel() // "good", "bad" -- reused as "fast"/"slow" here, names don't matter for this test.
	p := newTestPipeline(t, slowFastUpstream("bad", 20*time.Millisecond), deployments)

	ctx := context.Background()
	// Two probe rounds: the first round is when EACH deployment gets its
	// first-ever latency sample (no peer comparison possible yet, since
	// ProbeDeployments' own goroutines race and a peer's EMA may not
	// exist yet); the second round has both peers' EMAs already
	// populated, so the real relative comparison (and therefore
	// SetLatencyFactor) actually fires.
	p.ProbeDeployments(ctx)
	p.ProbeDeployments(ctx)

	const totalCalls = 600
	counts := map[string]int{}
	for i := 0; i < totalCalls; i++ {
		dep, ok := p.nextDeployment("gpt-4o", nil)
		if !ok {
			t.Fatalf("call %d: nextDeployment returned ok=false, want true", i)
		}
		counts[dep.Name]++
	}

	// Equal configured weight would imply 300/300 with no de-weighting
	// at all. A real ~10x-or-more latency difference (20ms sleep vs.
	// near-zero) must produce a CLEARLY reduced share for "bad" -- this
	// test intentionally does not pin an exact count (unlike
	// internal/router's own deterministic tests): real wall-clock sleep
	// timing has natural jitter this package's own EMA smoothing
	// absorbs, so a generous band proves the real property (measurably
	// less traffic) without being flaky on exact ms-level timing.
	if counts["bad"] >= 250 {
		t.Errorf(`counts["bad"] = %d, want well below 300 (a real, measurable latency-based de-weighting effect)`, counts["bad"])
	}
	if counts["bad"] == 0 {
		t.Error(`counts["bad"] = 0, want nonzero -- latency de-weighting must never fully starve a healthy deployment`)
	}
}
