package dataplane

// Tests for docs/rfcs/2026-09-07-gateway-active-health-probing.md's
// Pipeline-level integration: ProbeDeployments/RunHealthProbeLoop calling
// into router.Router.ReportProbeResult, and router.Router.Select (via
// nextDeployment/HandleChatCompletion) correctly routing around a
// deployment once — and only once — the N-of-M threshold trips. The pure
// N-of-M threshold logic itself is proven directly against
// router.Router in internal/router/health_test.go; this file proves the
// FULL pipeline wiring: a real (mocked) upstream, callDeployment, and
// HandleChatCompletion's own routing decision all compose correctly.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// intermittentUpstream is a two-deployment UpstreamCaller fixture:
// requests routed to failingDeployment always error; every other
// deployment always succeeds. Safe for ProbeDeployments' concurrent
// per-deployment goroutines — callsByDeployment is mutex-guarded.
type intermittentUpstream struct {
	mu                sync.Mutex
	failingDeployment string
	callsByDeployment map[string]int
}

func newIntermittentUpstream(failingDeployment string) *intermittentUpstream {
	return &intermittentUpstream{failingDeployment: failingDeployment, callsByDeployment: map[string]int{}}
}

func (u *intermittentUpstream) call(_ context.Context, dep Deployment, _ any) (any, error) {
	u.mu.Lock()
	u.callsByDeployment[dep.Name]++
	u.mu.Unlock()

	if dep.Name == u.failingDeployment {
		return nil, errors.New("intermittentUpstream: simulated upstream failure")
	}
	return fakeOpenAIResponse(dep.UpstreamModel), nil
}

func (u *intermittentUpstream) countFor(name string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.callsByDeployment[name]
}

// twoDeploymentsSameModel is this file's shared fixture: two deployments
// ("good", "bad") routing the same canonical model, equal (unset)
// weight — testRouter's own default N-of-M threshold (3 fail / 2
// success, router.HealthConfig{}'s normalized default).
func twoDeploymentsSameModel() []Deployment {
	return []Deployment{
		{Name: "good", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "bad", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
}

// TestProbeDeploymentsExcludesOnlyAfterThresholdThenHandleChatCompletionRoutesAround
// is the plan's own required integration proof: a mock upstream that
// fails intermittently (here, deterministically for one deployment),
// confirming the router correctly routes around it once — and only
// once — the N-of-M threshold trips.
func TestProbeDeploymentsExcludesOnlyAfterThresholdThenHandleChatCompletionRoutesAround(t *testing.T) {
	upstream := newIntermittentUpstream("bad")
	deployments := twoDeploymentsSameModel()
	p := newTestPipeline(t, upstream.call, deployments)
	ctx := context.Background()

	// Below threshold (default 3): "bad" must still be selectable.
	for i := 0; i < 2; i++ {
		p.ProbeDeployments(ctx)
	}
	if healthy := p.router.IsHealthy("bad"); !healthy {
		t.Fatal("\"bad\" became unhealthy after only 2 probe failures, want still healthy (threshold is 3)")
	}

	// The 3rd consecutive probe failure trips the threshold.
	p.ProbeDeployments(ctx)
	if healthy := p.router.IsHealthy("bad"); healthy {
		t.Fatal("\"bad\" is still healthy after 3 consecutive probe failures, want unhealthy")
	}

	// Drive 20 real client requests through the full pipeline — each with
	// distinct content, so every one is a genuine cache miss that forces
	// a fresh routing decision, never served from a prior call's cached
	// response. Every one must land on "good" — "bad" must never be
	// selected once excluded.
	badCallsBefore := upstream.countFor("bad")
	for i := 0; i < 20; i++ {
		req := adapter.ChatRequest{
			Model:    "gpt-4o",
			Messages: []adapter.Message{{Role: "user", Content: fmt.Sprintf("unique probe-routing question #%d", i)}},
		}
		resp, err := p.HandleChatCompletion(ctx, "Bearer test-key", req)
		if err != nil {
			t.Fatalf("HandleChatCompletion call %d: %v", i, err)
		}
		if resp.Model != "gpt-4o" {
			t.Fatalf("HandleChatCompletion call %d: resp.Model = %q, want %q", i, resp.Model, "gpt-4o")
		}
	}
	if got := upstream.countFor("bad") - badCallsBefore; got != 0 {
		t.Errorf("upstream calls routed to excluded deployment \"bad\" = %d, want 0", got)
	}
	if got := upstream.countFor("good"); got == 0 {
		t.Error("upstream calls routed to \"good\" = 0, want > 0 (every request should have landed here)")
	}
}

// TestProbeDeploymentsReincludesDeploymentAfterMConsecutiveSuccesses
// proves the recovery half end-to-end: once an excluded deployment's
// upstream starts succeeding again, M consecutive successful probes
// (not fewer) re-include it in real client routing.
func TestProbeDeploymentsReincludesDeploymentAfterMConsecutiveSuccesses(t *testing.T) {
	upstream := newIntermittentUpstream("bad")
	deployments := twoDeploymentsSameModel()
	p := newTestPipeline(t, upstream.call, deployments)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		p.ProbeDeployments(ctx)
	}
	if healthy := p.router.IsHealthy("bad"); healthy {
		t.Fatal("setup: \"bad\" should be unhealthy after 3 consecutive probe failures")
	}

	// "bad"'s upstream recovers.
	upstream.mu.Lock()
	upstream.failingDeployment = ""
	upstream.mu.Unlock()

	// Below the recovery threshold (default 2): still excluded.
	p.ProbeDeployments(ctx)
	if healthy := p.router.IsHealthy("bad"); healthy {
		t.Fatal("\"bad\" is healthy after only 1 consecutive probe success, want still unhealthy (threshold is 2)")
	}

	// The 2nd consecutive success re-includes it.
	p.ProbeDeployments(ctx)
	if healthy := p.router.IsHealthy("bad"); !healthy {
		t.Fatal("\"bad\" is still unhealthy after 2 consecutive probe successes, want healthy")
	}

	sawBad := false
	for i := 0; i < 40; i++ {
		if name, _ := p.router.Select("gpt-4o"); name == "bad" {
			sawBad = true
			break
		}
	}
	if !sawBad {
		t.Error("\"bad\" was never selected again after its 2nd consecutive recovery probe re-included it")
	}
}
