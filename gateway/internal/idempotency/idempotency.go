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

// ErrNonPositiveTTL is returned by Claim when ttl <= 0. **Fixed
// 2026-09-17, real bug**: inprocess.Store previously accepted this
// silently, computing expiresAt as time.Now() or a past instant --
// sweepExpiredLocked (which runs at the start of every Claim call, for
// ANY key) would then evict this exact entry almost immediately, well
// before the real in-flight request could ever call Complete. Any
// caller checking Claim again for the same key would see it as a brand
// new claim rather than in-flight/completed, silently defeating
// deduplication entirely rather than merely shortening its window. Not
// reachable via the one real caller today (dataplane.claimIdempotency
// always passes a fixed positive idempotencyKeyTTL), but this Store
// interface takes ttl directly from any future caller -- a clear error
// here is safer than either silently substituting an arbitrary positive
// default (guessing at intent this package has no basis for) or letting
// the dead-on-arrival claim through.
var ErrNonPositiveTTL = errors.New("idempotency: ttl must be positive")

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

// Token identifies exactly which underlying claim a StateNew ClaimResult
// refers to — the zero Token never means anything real (a real Store
// implementation's own token generator must never hand out the zero
// value), so a caller that never received a StateNew Claim has no way to
// forge one. Complete/Fail must be called with the SAME Token the
// StateNew Claim returned; a caller passing a stale Token (e.g. from a
// claim this Store has since swept as abandoned and replaced with a
// DIFFERENT claim for the same key, per Claim's own ttl/abandonment doc
// comment) gets a safe no-op, never a mutation of that unrelated, newer
// claim — closes the exact gap a live 2026-09-16 adversarial audit found:
// a key-only (no ownership check) Complete/Fail lookup could otherwise
// resolve a completely different caller's in-flight claim. Meaningless
// for StateInFlight/StateCompleted results — there is nothing left for
// THIS caller to resolve either way.
type Token uint64

// ClaimResult is Claim's own return value — exactly one of Response
// (StateCompleted), Done (StateInFlight), or Token (StateNew) is
// meaningful, per State.
type ClaimResult struct {
	State    State
	Response []byte
	Done     <-chan struct{}
	Token    Token
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
	// this exact key, and must pass that Claim's own Token back
	// unchanged. A no-op-shaped error (never panics) if key is unknown,
	// already resolved, or token no longer matches the claim currently
	// held for key (a stale token from an abandoned, since-superseded
	// claim) — Complete is expected to run from a defer, so it must
	// never be a second source of failure, and must never resolve a
	// claim it was not actually granted.
	Complete(ctx context.Context, key string, token Token, resp []byte) error
	// Fail releases key's claim with NO stored response, per this
	// package's own doc comment: a genuinely failed attempt may be
	// retried under the SAME key, by the original caller or by a caller
	// that was waiting on ClaimResult.Done — either becomes the new
	// StateNew owner via a subsequent Claim call, whichever calls it
	// first. Never an error for an unknown key or a stale token (same
	// no-op reasoning as Complete's own token check).
	Fail(ctx context.Context, key string, token Token) error
}
