package dataplane

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/router"
)

func TestClassifyFallbackError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "context window exceeded, openai-style code",
			err:  &UpstreamHTTPError{StatusCode: 400, Body: `{"error":{"code":"context_length_exceeded","message":"This model's maximum context length is 8192 tokens."}}`},
			want: FallbackClassContextWindowExceeded,
		},
		{
			name: "context window exceeded, free-text message only",
			err:  &UpstreamHTTPError{StatusCode: 400, Body: `{"error":{"type":"invalid_request_error","message":"prompt is too long: 200000 tokens > 195000 maximum"}}`},
			want: FallbackClassContextWindowExceeded,
		},
		{
			name: "content policy violation, openai-style code",
			err:  &UpstreamHTTPError{StatusCode: 400, Body: `{"error":{"code":"content_policy_violation","message":"Your request was rejected by our content policy."}}`},
			want: FallbackClassContentPolicy,
		},
		{
			name: "content policy, anthropic-style free text under a generic type",
			err:  &UpstreamHTTPError{StatusCode: 400, Body: `{"error":{"type":"invalid_request_error","message":"This content violates our usage policies and was blocked by our content filter."}}`},
			want: FallbackClassContentPolicy,
		},
		{
			name: "rate limit is generic, not its own class",
			err:  &UpstreamHTTPError{StatusCode: 429, Body: `{"error":{"type":"rate_limit_error","message":"Rate limit exceeded"}}`},
			want: FallbackClassGeneric,
		},
		{
			name: "unrelated 500 is generic",
			err:  &UpstreamHTTPError{StatusCode: 500, Body: `{"error":{"type":"api_error","message":"internal server error"}}`},
			want: FallbackClassGeneric,
		},
		{
			name: "wrapped UpstreamHTTPError is still classified through errors.As",
			err:  fmt.Errorf("upstream call to deployment %q: %w", "primary", &UpstreamHTTPError{StatusCode: 400, Body: "context_length_exceeded"}),
			want: FallbackClassContextWindowExceeded,
		},
		{
			name: "non-UpstreamHTTPError (local adapter/network error) is generic",
			err:  errors.New("dialing upstream: connection refused"),
			want: FallbackClassGeneric,
		},
		{
			name: "DeploymentCapacityError is generic, never ContentPolicy/ContextWindowExceeded",
			err:  &DeploymentCapacityError{Deployment: "shared", Reason: "concurrency"},
			want: FallbackClassGeneric,
		},
		{
			name: "wrapped DeploymentCapacityError is still classified through errors.As",
			err:  fmt.Errorf("callDeploymentWithCapacityCheck: %w", &DeploymentCapacityError{Deployment: "shared", Reason: "rate_limit"}),
			want: FallbackClassGeneric,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyFallbackError(tt.err); got != tt.want {
				t.Errorf("classifyFallbackError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestFallbackTargets(t *testing.T) {
	contentPolicyErr := &UpstreamHTTPError{StatusCode: 400, Body: "content_policy_violation"}
	contextWindowErr := &UpstreamHTTPError{StatusCode: 400, Body: "context_length_exceeded"}
	genericErr := errors.New("network timeout")

	t.Run("unconfigured deployment reports configured=false", func(t *testing.T) {
		dep := Deployment{Name: "primary"}
		targets, configured := fallbackTargets(dep, genericErr)
		if configured {
			t.Fatal("configured = true for a deployment with no FallbackChains at all, want false")
		}
		if targets != nil {
			t.Errorf("targets = %v, want nil", targets)
		}
	})

	t.Run("each class routes to its own configured list", func(t *testing.T) {
		dep := Deployment{
			Name: "primary",
			FallbackChains: map[string][]string{
				FallbackClassContentPolicy:         {"safety-alt"},
				FallbackClassContextWindowExceeded: {"large-context-alt"},
				FallbackClassGeneric:               {"generic-alt"},
			},
		}

		cases := []struct {
			err  error
			want []string
		}{
			{contentPolicyErr, []string{"safety-alt"}},
			{contextWindowErr, []string{"large-context-alt"}},
			{genericErr, []string{"generic-alt"}},
		}
		for _, c := range cases {
			targets, configured := fallbackTargets(dep, c.err)
			if !configured {
				t.Fatalf("configured = false for %v, want true", c.err)
			}
			if len(targets) != len(c.want) || targets[0] != c.want[0] {
				t.Errorf("fallbackTargets(%v) = %v, want %v", c.err, targets, c.want)
			}
		}
	})

	t.Run("unconfigured specific class falls through to generic", func(t *testing.T) {
		dep := Deployment{
			Name: "primary",
			FallbackChains: map[string][]string{
				FallbackClassGeneric: {"generic-alt"},
			},
		}
		targets, configured := fallbackTargets(dep, contentPolicyErr)
		if !configured {
			t.Fatal("configured = false, want true")
		}
		if len(targets) != 1 || targets[0] != "generic-alt" {
			t.Errorf("targets = %v, want [generic-alt] (fallthrough to the generic chain)", targets)
		}
	})

	t.Run("configured but neither specific nor generic list set means no fallback", func(t *testing.T) {
		dep := Deployment{
			Name: "primary",
			FallbackChains: map[string][]string{
				FallbackClassContextWindowExceeded: {"large-context-alt"},
			},
		}
		targets, configured := fallbackTargets(dep, contentPolicyErr)
		if !configured {
			t.Fatal("configured = false, want true (dep DID opt into fallback_chains)")
		}
		if targets != nil {
			t.Errorf("targets = %v, want nil (content_policy has no configured list, and there's no generic fallthrough either)", targets)
		}
	})
}

// TestIsCandidateHealthFailure is the direct proof for
// isCandidateHealthFailure's own status-class/origin filter, per
// docs/upgrade-research/gateway-router-health-real-traffic-2026-09-09.md's
// Finding 2/3 — a real backend-health signal is a genuine 5xx or a
// local/connection-level failure, never an ordinary client-caused 4xx
// (including the content-policy/context-window classes classifyFallbackError
// already recognizes) and never Kelvran's own DeploymentCapacityError
// self-throttling rejection.
func TestIsCandidateHealthFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error is never a candidate", nil, false},
		{"500 is a candidate", &UpstreamHTTPError{StatusCode: 500, Body: "internal server error"}, true},
		{"502 is a candidate", &UpstreamHTTPError{StatusCode: 502, Body: "bad gateway"}, true},
		{"503 is a candidate", &UpstreamHTTPError{StatusCode: 503, Body: "service unavailable"}, true},
		{"599 is a candidate", &UpstreamHTTPError{StatusCode: 599, Body: "network connect timeout error"}, true},
		{"400 is never a candidate", &UpstreamHTTPError{StatusCode: 400, Body: "bad request"}, false},
		{"404 is never a candidate", &UpstreamHTTPError{StatusCode: 404, Body: "not found"}, false},
		{"429 is never a candidate", &UpstreamHTTPError{StatusCode: 429, Body: "rate_limit_error"}, false},
		{"content_policy 4xx is never a candidate", &UpstreamHTTPError{StatusCode: 400, Body: "content_policy_violation"}, false},
		{"context_window 4xx is never a candidate", &UpstreamHTTPError{StatusCode: 400, Body: "context_length_exceeded"}, false},
		{"DeploymentCapacityError is never a candidate (self-throttling, not a real backend signal)", &DeploymentCapacityError{Deployment: "d1", Reason: "concurrency"}, false},
		{"wrapped DeploymentCapacityError is still excluded through errors.As", fmt.Errorf("callDeploymentWithCapacityCheck: %w", &DeploymentCapacityError{Deployment: "d1", Reason: "rate_limit"}), false},
		{"local/connection-level error (never got a response) is a candidate", errors.New("dialing upstream: connection refused"), true},
		{"wrapped 5xx is still a candidate through errors.As", fmt.Errorf("upstream call to deployment %q: %w", "primary", &UpstreamHTTPError{StatusCode: 500, Body: "internal server error"}), true},
		{"wrapped 4xx is still excluded through errors.As", fmt.Errorf("upstream call to deployment %q: %w", "primary", &UpstreamHTTPError{StatusCode: 400, Body: "bad request"}), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCandidateHealthFailure(tt.err); got != tt.want {
				t.Errorf("isCandidateHealthFailure(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// alwaysAllowRateLimit is the permissive rateLimitOK closure every
// pre-existing test in this file (written before the rate-limit-bypass
// fix) passes, so each one keeps testing exactly what it tested before —
// never a real target's rate-limit status, which is
// TestAttemptFallbackChainSkipsRateLimitedTargetsWithoutAttemptingThem's
// own, separately-added job below.
func alwaysAllowRateLimit(string) bool { return true }

// alwaysAllowDeploymentCapacity is the permissive deploymentCapacityOK
// closure every pre-existing test in this file (written before the
// per-deployment concurrency/rate-ceiling feature) passes, mirroring
// alwaysAllowRateLimit's own precedent exactly —
// TestAttemptFallbackChainSkipsDeploymentCapacityConstrainedTargetsWithoutAttemptingThem
// is the one test that exercises a real deploymentCapacityOK rejection.
func alwaysAllowDeploymentCapacity(string) bool { return true }

func TestAttemptFallbackChainStopsAtFirstSuccess(t *testing.T) {
	p := &Pipeline{deploymentsByName: map[string]Deployment{
		"b": {Name: "b"},
		"c": {Name: "c"},
	}}

	var calls []string
	call := func(d Deployment) (adapter.ChatResponse, error) {
		calls = append(calls, d.Name)
		if d.Name == "b" {
			return adapter.ChatResponse{}, errors.New("b failed")
		}
		return adapter.ChatResponse{Model: "served-by-" + d.Name}, nil
	}

	dep, resp, err, attempted := p.attemptFallbackChain(context.Background(), []string{"b", "c"}, map[string]bool{"a": true}, call, func() bool { return false }, alwaysAllowRateLimit, alwaysAllowDeploymentCapacity)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !attempted {
		t.Fatal("attempted = false, want true")
	}
	if dep.Name != "c" || resp.Model != "served-by-c" {
		t.Errorf("dep=%q resp.Model=%q, want dep=c resp.Model=served-by-c", dep.Name, resp.Model)
	}
	if len(calls) != 2 || calls[0] != "b" || calls[1] != "c" {
		t.Fatalf("calls = %v, want [b c] in that exact order", calls)
	}
}

func TestAttemptFallbackChainExhaustsInOrderBeforeGivingUp(t *testing.T) {
	p := &Pipeline{deploymentsByName: map[string]Deployment{
		"b": {Name: "b"},
		"c": {Name: "c"},
		"d": {Name: "d"},
	}}

	var calls []string
	call := func(d Deployment) (adapter.ChatResponse, error) {
		calls = append(calls, d.Name)
		return adapter.ChatResponse{}, errors.New(d.Name + " failed too")
	}

	_, _, err, attempted := p.attemptFallbackChain(context.Background(), []string{"b", "c", "d"}, map[string]bool{"a": true}, call, func() bool { return false }, alwaysAllowRateLimit, alwaysAllowDeploymentCapacity)
	if err == nil {
		t.Fatal("err = nil, want the last hop's error")
	}
	if err.Error() != "d failed too" {
		t.Errorf("err = %v, want the LAST hop's (d's) error", err)
	}
	if !attempted {
		t.Fatal("attempted = false, want true")
	}
	if len(calls) != 3 || calls[0] != "b" || calls[1] != "c" || calls[2] != "d" {
		t.Fatalf("calls = %v, want [b c d] — every configured hop attempted, in order, before giving up", calls)
	}
}

func TestAttemptFallbackChainSkipsAlreadyTriedAndUnknownNames(t *testing.T) {
	p := &Pipeline{deploymentsByName: map[string]Deployment{
		"a": {Name: "a"},
		"c": {Name: "c"},
	}}

	var calls []string
	call := func(d Deployment) (adapter.ChatResponse, error) {
		calls = append(calls, d.Name)
		return adapter.ChatResponse{Model: "served-by-" + d.Name}, nil
	}

	// "a" is already tried (the original, failed deployment); "unknown"
	// is not a real deployment at all (defends against a config typo
	// that startup validation should have already caught).
	_, resp, err, attempted := p.attemptFallbackChain(context.Background(), []string{"a", "unknown", "c"}, map[string]bool{"a": true}, call, func() bool { return false }, alwaysAllowRateLimit, alwaysAllowDeploymentCapacity)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !attempted {
		t.Fatal("attempted = false, want true")
	}
	if resp.Model != "served-by-c" {
		t.Errorf("resp.Model = %q, want served-by-c", resp.Model)
	}
	if len(calls) != 1 || calls[0] != "c" {
		t.Fatalf("calls = %v, want exactly [c] — 'a' skipped as already-tried, 'unknown' skipped as unresolvable", calls)
	}
}

func TestAttemptFallbackChainRespectsStop(t *testing.T) {
	p := &Pipeline{deploymentsByName: map[string]Deployment{
		"b": {Name: "b"},
		"c": {Name: "c"},
	}}

	stopped := true
	call := func(d Deployment) (adapter.ChatResponse, error) {
		t.Fatalf("call must never run once stop() is already true, but was called for %q", d.Name)
		return adapter.ChatResponse{}, nil
	}

	_, _, _, attempted := p.attemptFallbackChain(context.Background(), []string{"b", "c"}, map[string]bool{}, call, func() bool { return stopped }, alwaysAllowRateLimit, alwaysAllowDeploymentCapacity)
	if attempted {
		t.Fatal("attempted = true, want false — stop() was already true before the first hop")
	}
}

// TestAttemptFallbackChainSkipsRouterUnhealthyTargetsWithoutAttemptingThem
// proves design (c)'s composition with active health-probing: a chain
// target router.IsHealthy already reports unhealthy (from real,
// cross-request probe failures) is skipped entirely, never reaching
// call — closing the real gap this RFC's own grounding research found
// (attemptFallbackChain used to resolve targets via p.deploymentsByName
// directly, never consulting IsHealthy at all).
func TestAttemptFallbackChainSkipsRouterUnhealthyTargetsWithoutAttemptingThem(t *testing.T) {
	depRouter := router.New([]router.Deployment{{Name: "b", Model: "m"}, {Name: "c", Model: "m"}}, router.HealthConfig{})
	// Default UnhealthyThreshold is 3 consecutive failures.
	depRouter.ReportProbeResult("b", false)
	depRouter.ReportProbeResult("b", false)
	depRouter.ReportProbeResult("b", false)
	if depRouter.IsHealthy("b") {
		t.Fatal("setup: expected 'b' to be unhealthy after 3 consecutive probe failures")
	}

	p := &Pipeline{
		router: depRouter,
		deploymentsByName: map[string]Deployment{
			"b": {Name: "b"},
			"c": {Name: "c"},
		},
	}

	var calls []string
	call := func(d Deployment) (adapter.ChatResponse, error) {
		calls = append(calls, d.Name)
		return adapter.ChatResponse{Model: "served-by-" + d.Name}, nil
	}

	_, resp, err, attempted := p.attemptFallbackChain(context.Background(), []string{"b", "c"}, map[string]bool{"a": true}, call, func() bool { return false }, alwaysAllowRateLimit, alwaysAllowDeploymentCapacity)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !attempted {
		t.Fatal("attempted = false, want true")
	}
	if resp.Model != "served-by-c" {
		t.Errorf("resp.Model = %q, want served-by-c", resp.Model)
	}
	if len(calls) != 1 || calls[0] != "c" {
		t.Fatalf("calls = %v, want exactly [c] — 'b' skipped as router-unhealthy, never attempted", calls)
	}
}

// TestAttemptFallbackChainSkipsRateLimitedTargetsWithoutAttemptingThem is
// the load-bearing proof for the fix to
// evals/tests/fixtures/regression_corpus_cost_abuse.json's
// costabuse-permodel-ratelimit-bypassed-via-crossmodel-fallback-chain
// case: a candidate target whose rateLimitOK closure reports false is
// skipped exactly like a router-unhealthy one — never attempted, and
// never charged against the consecutiveFailures circuit breaker or
// realAttempts (proven indirectly here by the chain still succeeding at
// "c" with zero inter-hop backoff — a real attempt at "b" would have
// forced at least one delay before "c").
func TestAttemptFallbackChainSkipsRateLimitedTargetsWithoutAttemptingThem(t *testing.T) {
	p := &Pipeline{deploymentsByName: map[string]Deployment{
		"b": {Name: "b", Model: "gpt-4o"},
		"c": {Name: "c", Model: "gpt-4o-mini"},
	}}

	var rateLimitChecked []string
	rateLimitOK := func(model string) bool {
		rateLimitChecked = append(rateLimitChecked, model)
		return model != "gpt-4o"
	}

	var calls []string
	call := func(d Deployment) (adapter.ChatResponse, error) {
		calls = append(calls, d.Name)
		return adapter.ChatResponse{Model: "served-by-" + d.Name}, nil
	}

	start := time.Now()
	_, resp, err, attempted := p.attemptFallbackChain(context.Background(), []string{"b", "c"}, map[string]bool{"a": true}, call, func() bool { return false }, rateLimitOK, alwaysAllowDeploymentCapacity)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !attempted {
		t.Fatal("attempted = false, want true")
	}
	if resp.Model != "served-by-c" {
		t.Errorf("resp.Model = %q, want served-by-c", resp.Model)
	}
	if len(calls) != 1 || calls[0] != "c" {
		t.Fatalf("calls = %v, want exactly [c] — 'b' skipped as rate-limited, never attempted", calls)
	}
	if len(rateLimitChecked) != 2 || rateLimitChecked[0] != "gpt-4o" || rateLimitChecked[1] != "gpt-4o-mini" {
		t.Fatalf("rateLimitChecked = %v, want [gpt-4o gpt-4o-mini] — every candidate target's OWN Deployment.Model, in order", rateLimitChecked)
	}
	// A real attempt at "b" followed by a real attempt at "c" would incur
	// at least one inter-hop backoff sleep (fallbackChainInterHopBackoffBase's
	// floor); a rate-limited skip must never charge that delay, so the
	// walk here — one real attempt total — completes near-instantly.
	if elapsed >= fallbackChainInterHopBackoffBase {
		t.Errorf("elapsed = %v, want well under %v — a rate-limited skip must never charge the inter-hop backoff delay", elapsed, fallbackChainInterHopBackoffBase)
	}
}

// TestAttemptFallbackChainStopsAfterMaxConsecutiveFailuresWithinOneRequest
// is the per-request circuit breaker's own load-bearing proof: a chain
// configured with MORE than maxConsecutiveChainFailures targets, all
// failing, stops calling call once that many real attempts have failed
// in a row, even though further targets remain configured.
func TestAttemptFallbackChainStopsAfterMaxConsecutiveFailuresWithinOneRequest(t *testing.T) {
	p := &Pipeline{deploymentsByName: map[string]Deployment{
		"b": {Name: "b"},
		"c": {Name: "c"},
		"d": {Name: "d"},
		"e": {Name: "e"},
		"f": {Name: "f"},
	}}

	var calls []string
	call := func(d Deployment) (adapter.ChatResponse, error) {
		calls = append(calls, d.Name)
		return adapter.ChatResponse{}, errors.New(d.Name + " failed")
	}

	_, _, err, attempted := p.attemptFallbackChain(context.Background(), []string{"b", "c", "d", "e", "f"}, map[string]bool{"a": true}, call, func() bool { return false }, alwaysAllowRateLimit, alwaysAllowDeploymentCapacity)
	if err == nil {
		t.Fatal("err = nil, want the last attempted hop's error")
	}
	if !attempted {
		t.Fatal("attempted = false, want true")
	}
	if len(calls) != maxConsecutiveChainFailures {
		t.Fatalf("calls = %v (len %d), want exactly %d — the breaker must stop the walk after that many consecutive real failures, even though 'e' and 'f' remain configured and untried", calls, len(calls), maxConsecutiveChainFailures)
	}
	if calls[0] != "b" || calls[1] != "c" || calls[2] != "d" {
		t.Fatalf("calls = %v, want [b c d] in that exact order before the breaker trips", calls)
	}
}

// TestAttemptFallbackChainInsertsRealMeasurableDelayBetweenHops proves the
// inter-hop backoff is a genuine time.Sleep/timer-based pause the
// function actually executes, measured against real elapsed wall-clock
// time — not merely that a delay value is computed and discarded. A
// 3-hop, all-failing chain inserts a delay before hop 2 and hop 3 (never
// before hop 1), so the real elapsed time must be at least the sum of
// both hops' theoretical equal-jitter floors.
func TestAttemptFallbackChainInsertsRealMeasurableDelayBetweenHops(t *testing.T) {
	p := &Pipeline{deploymentsByName: map[string]Deployment{
		"b": {Name: "b"},
		"c": {Name: "c"},
		"d": {Name: "d"},
	}}

	call := func(d Deployment) (adapter.ChatResponse, error) {
		return adapter.ChatResponse{}, errors.New(d.Name + " failed")
	}

	// Theoretical equal-jitter floor for attempt N: half of
	// base*2^(N-1), capped. Backoff is inserted before hop 2 (attempt 1,
	// relative to the PRECEDING hop) and hop 3 (attempt 2).
	floorHop2 := fallbackChainInterHopBackoffBase / 2
	floorHop3 := fallbackChainInterHopBackoffBase // base*2^1 / 2 == base
	wantFloor := floorHop2 + floorHop3

	start := time.Now()
	_, _, _, attempted := p.attemptFallbackChain(context.Background(), []string{"b", "c", "d"}, map[string]bool{"a": true}, call, func() bool { return false }, alwaysAllowRateLimit, alwaysAllowDeploymentCapacity)
	elapsed := time.Since(start)

	if !attempted {
		t.Fatal("attempted = false, want true")
	}
	if elapsed < wantFloor {
		t.Errorf("elapsed = %v, want >= %v (the two inter-hop backoff floors) — the delay must be a real, measured pause, not a computed-and-discarded value", elapsed, wantFloor)
	}
	// Upper bound generous enough to never flake on a loaded CI runner,
	// tight enough to catch a regression that multiplies the delay by a
	// large, wrong factor (e.g. summing all consecutiveFailures instead
	// of the per-hop attempt index).
	wantCeiling := 5 * fallbackChainInterHopBackoffCap
	if elapsed > wantCeiling {
		t.Errorf("elapsed = %v, want <= %v", elapsed, wantCeiling)
	}
}
