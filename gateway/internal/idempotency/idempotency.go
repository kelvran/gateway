// Package idempotency provides request deduplication for Kelvran's
// client-facing chat-completions endpoint, per
// docs/upgrade-research/request-lifecycle-reliability-2026-09-15.md:
// the same client-supplied Idempotency-Key, replayed within its TTL,
// either (a) returns the first attempt's own response verbatim, if it
// already completed; (b) blocks the caller until that in-flight first
// attempt finishes, then resolves the same way; or (c) is rejected with
// ErrFingerprintMismatch, if the replay's request body doesn't match the
// original — mirroring Stripe's and OpenAI's own documented
// Idempotency-Key contracts.
//
// Deliberately NOT internal/cache.Cache: that interface's Get/Put pair
// has no claim-or-wait concurrency primitive (a second concurrent
// request with the same key must block/replay, never race a bare Get
// then Put) and no fingerprint-mismatch concept at all — bolting either
// onto cache.Cache would leak idempotency-specific coordination state
// into every OTHER cache.Cache consumer (the L1/L2 response cache),
// which has no use for it.
package idempotency

import (
	"context"
	"errors"
	"time"
)

// ErrFingerprintMismatch is returned by Claim when key was already
// claimed by a request whose body hashed to a DIFFERENT fingerprint —
// the documented Stripe/OpenAI edge case of reusing an idempotency key
// with different parameters. The caller must reject the request, never
// silently proceed with either body.
var ErrFingerprintMismatch = errors.New("idempotency: key already claimed with a different request body")

// State is Claim's own outcome for a given key/fingerprint pair.
type State int

const (
	// StateNew means this call is now the sole owner of key — no prior
	// claim existed (or the prior one failed and was released; see
	// Fail's own doc comment). The caller must eventually call Complete
	// or Fail for the SAME key.
	StateNew State = iota
	// StateInFlight means another caller already owns key and has not
	// yet finished — wait on ClaimResult.Done, then call Claim again for
	// the authoritative outcome (which may itself be StateNew again, if
	// the original attempt failed and was released in the meantime).
	StateInFlight
	// StateCompleted means a prior call for key (with the same
	// fingerprint) already finished successfully — ClaimResult.Response
	// is that attempt's own stored response, to replay verbatim.
	StateCompleted
)

// ClaimResult is Claim's own return value — exactly one of Response
// (StateCompleted) or Done (StateInFlight) is meaningful, per State.
type ClaimResult struct {
	State    State
	Response []byte
	Done     <-chan struct{}
}

// Store is the concurrency-safe claim/complete/fail primitive this
// package exists to provide. See internal/idempotency/inprocess for the
// real, TTL-aware implementation.
type Store interface {
	// Claim attempts to become (or check the status of) the owner of key
	// for a request whose body hashes to fingerprint. ttl bounds how
	// long a StateNew claim stays valid before this Store may treat it
	// as abandoned and let a later Claim start fresh.
	Claim(ctx context.Context, key string, fingerprint [32]byte, ttl time.Duration) (ClaimResult, error)
	// Complete records resp as the successful, replayable result for key
	// — the caller MUST be the one that received StateNew from Claim for
	// this exact key. A no-op-shaped error (never panics) if key is
	// unknown or already resolved — Complete is expected to run from a
	// defer, so it must never be a second source of failure.
	Complete(ctx context.Context, key string, resp []byte) error
	// Fail releases key's claim with NO stored response, per this
	// package's own doc comment: a genuinely failed attempt may be
	// retried under the SAME key, by the original caller or by a caller
	// that was waiting on ClaimResult.Done — either becomes the new
	// StateNew owner via a subsequent Claim call, whichever calls it
	// first. Never an error for an unknown key.
	Fail(ctx context.Context, key string) error
}
