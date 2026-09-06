package dataplane

import (
	"errors"
	"fmt"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
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

	dep, resp, err, attempted := p.attemptFallbackChain([]string{"b", "c"}, map[string]bool{"a": true}, call, func() bool { return false })
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

	_, _, err, attempted := p.attemptFallbackChain([]string{"b", "c", "d"}, map[string]bool{"a": true}, call, func() bool { return false })
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
	_, resp, err, attempted := p.attemptFallbackChain([]string{"a", "unknown", "c"}, map[string]bool{"a": true}, call, func() bool { return false })
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

	_, _, _, attempted := p.attemptFallbackChain([]string{"b", "c"}, map[string]bool{}, call, func() bool { return stopped })
	if attempted {
		t.Fatal("attempted = true, want false — stop() was already true before the first hop")
	}
}
