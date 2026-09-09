package dataplane

// Direct unit tests for isRetryStormEligible — no prior test file existed
// for retry_after.go at all; closed while touching this exact function to
// add OUTCOME_DEPLOYMENT_CAPACITY eligibility.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

func TestIsRetryStormEligible(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil (success) is never eligible", nil, false},
		{"rate limited is eligible", ErrRateLimited, true},
		{"concurrency limit exceeded is eligible (classifies as OUTCOME_RATE_LIMITED)", ErrConcurrencyLimitExceeded, true},
		{"generic upstream error is eligible", errors.New("dialing upstream: connection refused"), true},
		{"deployment capacity (concurrency) is eligible", &DeploymentCapacityError{Deployment: "d1", Reason: "concurrency"}, true},
		{"deployment capacity (rate_limit) is eligible", &DeploymentCapacityError{Deployment: "d1", Reason: "rate_limit"}, true},
		{"wrapped deployment capacity is still eligible through errors.As", fmt.Errorf("upstream call to deployment %q: %w", "d1", &DeploymentCapacityError{Deployment: "d1", Reason: "concurrency"}), true},
		{"auth failure is never eligible", identity.ErrMissingHeader, false},
		{"model not allowed is never eligible", ErrModelNotAllowed, false},
		{"budget exceeded is never eligible", ErrBudgetExceeded, false},
		{"guardrail blocked is never eligible", ErrGuardrailBlocked, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryStormEligible(tt.err); got != tt.want {
				t.Errorf("isRetryStormEligible(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestAttachRetryAfterWrapsEligibleErrorsWithABackoffDuration proves the
// full attachRetryAfter path (not just the isRetryStormEligible
// classification in isolation): a deployment-capacity rejection gets
// wrapped in a real *RetryAfterError with a positive RetryAfter duration,
// and errors.Is/errors.As against the original error still work through
// the wrap.
func TestAttachRetryAfterWrapsEligibleErrorsWithABackoffDuration(t *testing.T) {
	p := &Pipeline{retryBackoff: ratelimit.NewRetryBackoff()}
	vk := &identity.VirtualKey{ID: "test-key"}
	original := &DeploymentCapacityError{Deployment: "d1", Reason: "concurrency"}

	got := p.attachRetryAfter(vk, original)

	var retryErr *RetryAfterError
	if !errors.As(got, &retryErr) {
		t.Fatalf("attachRetryAfter(%v) = %v, want it wrapped in a *RetryAfterError", original, got)
	}
	if retryErr.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want a positive duration", retryErr.RetryAfter)
	}
	var capErr *DeploymentCapacityError
	if !errors.As(got, &capErr) || capErr != original {
		t.Error("errors.As against the original *DeploymentCapacityError must still find it through the RetryAfterError wrap")
	}
}

// TestAttachRetryAfterLeavesIneligibleErrorsUnwrapped proves the negative
// case: an error outside the eligible set passes through byte-identical,
// never wrapped.
func TestAttachRetryAfterLeavesIneligibleErrorsUnwrapped(t *testing.T) {
	p := &Pipeline{retryBackoff: ratelimit.NewRetryBackoff()}
	vk := &identity.VirtualKey{ID: "test-key"}

	got := p.attachRetryAfter(vk, ErrGuardrailBlocked)
	if !errors.Is(got, ErrGuardrailBlocked) {
		t.Errorf("attachRetryAfter(ErrGuardrailBlocked) = %v, want the exact same, unwrapped error", got)
	}
	var retryErr *RetryAfterError
	if errors.As(got, &retryErr) {
		t.Errorf("attachRetryAfter(ErrGuardrailBlocked) wrapped it in a *RetryAfterError, want no wrap at all for an ineligible error")
	}
}

// TestAttachRetryAfterNilVirtualKeyPassesThroughUnchanged proves the
// auth-failure case (vk == nil, since no identity was ever resolved):
// the error passes through completely unchanged, no backoff tracking
// attempted.
func TestAttachRetryAfterNilVirtualKeyPassesThroughUnchanged(t *testing.T) {
	p := &Pipeline{retryBackoff: ratelimit.NewRetryBackoff()}
	got := p.attachRetryAfter(nil, identity.ErrMissingHeader)
	if !errors.Is(got, identity.ErrMissingHeader) {
		t.Errorf("attachRetryAfter(nil, ErrMissingHeader) = %v, want the exact same, unwrapped error", got)
	}
}
