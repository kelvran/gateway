package dataplane

// Client-facing Retry-After attachment, per
// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md's design (a).
// Kept in its own file from dataplane.go for the same reason fallback.go
// and streaming.go already are — a single, independently-reasoned-about
// concern, not mixed into an already-large file.

import (
	"time"

	gatewayeventsv1 "github.com/kelvran/gateway/gateway/api/gatewayevents/v1"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

// RetryAfterError wraps a rejection/failure error with the backoff
// duration a client should wait before retrying, per the RFC's design
// (a). Wraps (never replaces) the original error — errors.Is/errors.As
// against ErrRateLimited, ErrBudgetExceeded, etc. still work unchanged
// through Unwrap, so finalize's own outcomeFor classification (and every
// existing test asserting on the underlying sentinel) sees the exact same
// classification whether or not a RetryAfterError got attached.
type RetryAfterError struct {
	err        error
	RetryAfter time.Duration
}

// Error implements the error interface, delegating to the wrapped error's
// own message — a RetryAfterError never changes what text a client sees,
// only what HTTP header cmd/gateway's writeErrorResponse adds alongside it.
func (e *RetryAfterError) Error() string { return e.err.Error() }

// Unwrap exposes the original error to errors.Is/errors.As.
func (e *RetryAfterError) Unwrap() error { return e.err }

// isRetryStormEligible reports whether err represents a genuine capacity/
// availability rejection this codebase can honestly suggest a bounded
// backoff for — reusing outcomeFor's existing classification rather than
// maintaining a second, parallel error taxonomy. Deliberately narrow: see
// the RFC's design (a) for exactly why budget/guardrail/model-not-allowed/
// auth are excluded rather than merely unconsidered.
//
// OUTCOME_DEPLOYMENT_CAPACITY is included deliberately, not merely left
// over from when it was OUTCOME_UPSTREAM_ERROR: a deployment-scoped
// capacity rejection (dataplane.DeploymentCapacityError) is arguably the
// most canonical case Retry-After exists for — a real "shed load, ask the
// client to wait" signal — so minting its own Outcome value must not
// silently drop it out of this eligible set.
func isRetryStormEligible(err error) bool {
	switch outcomeFor(err) {
	case gatewayeventsv1.GatewayDecisionEvent_OUTCOME_RATE_LIMITED,
		gatewayeventsv1.GatewayDecisionEvent_OUTCOME_UPSTREAM_ERROR,
		gatewayeventsv1.GatewayDecisionEvent_OUTCOME_DEPLOYMENT_CAPACITY:
		return true
	default:
		return false
	}
}

// attachRetryAfter is called from HandleChatCompletion's and
// HandleChatCompletionStream's own top-level deferred closure, BEFORE
// finalize runs, so finalize's logging/telemetry sees the exact same
// wrapped error the client ultimately receives rather than a second,
// divergent view of what happened. vk is nil when auth itself failed —
// there is no per-key streak to track in that case, and the error passes
// through unchanged (an auth failure is not a capacity signal regardless).
//
// A genuine success (err == nil) resets vk's streak — backoff only
// escalates while rejections are genuinely consecutive for this
// identity, never accumulating forever across occasional successes.
func (p *Pipeline) attachRetryAfter(vk *identity.VirtualKey, err error) error {
	if vk == nil {
		return err
	}
	if err == nil {
		p.retryBackoff.Reset(vk.ID)
		return nil
	}
	if !isRetryStormEligible(err) {
		return err
	}
	delay := p.retryBackoff.Record(vk.ID)
	return &RetryAfterError{err: err, RetryAfter: delay}
}
