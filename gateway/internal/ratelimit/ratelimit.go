// Package ratelimit implements per-virtual-key token-bucket rate
// limiting: a real token-bucket algorithm (refill rate + burst
// capacity), not a naive fixed-window counter, so burst traffic is
// handled correctly.
//
// TokenBucket itself is single-process, in-memory only. KeyLimiter sits
// above it and is what callers actually use: backed by TokenBucket by
// default (NewInMemoryKeyLimiter), or by a Redis-backed RedisBackend
// (NewRedisKeyLimiter) when correctness across multiple gateway
// instances matters — see
// docs/rfcs/2026-09-03-distributed-rate-limiting.md and
// internal/ratelimit/redislimiter for the real implementation.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// TokenBucket is a single token bucket: burstCapacity tokens available
// immediately, refilling continuously at refillPerSecond tokens/second up
// to that same capacity.
//
// The clock is injectable (via NewTokenBucketWithClock) so tests never
// need to sleep on wall-clock time to exercise refill behavior, per
// docs/testing/TESTING.md §1.
type TokenBucket struct {
	mu sync.Mutex

	capacity        float64
	refillPerSecond float64

	tokens     float64
	lastRefill time.Time
	now        func() time.Time

	// billedTokens/billedCount are ReserveTPM/ReconcileTPM-only
	// bookkeeping (never read or written by Allow/HasBalance/Debit) for
	// the TPM dimension's historical-average reservation estimate, per
	// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md — the
	// TPM-specific analog of budget.Tracker's own billedCount/spent
	// average. billedTokens is the running sum of real (non-reservation)
	// token counts ReconcileTPM has applied; billedCount is how many
	// times.
	billedTokens float64
	billedCount  int64
}

// NewTokenBucket constructs a TokenBucket at full capacity using the real
// wall clock.
func NewTokenBucket(burstCapacity float64, refillPerSecond float64) *TokenBucket {
	return NewTokenBucketWithClock(burstCapacity, refillPerSecond, time.Now)
}

// NewTokenBucketWithClock constructs a TokenBucket at full capacity using
// the given clock function, for deterministic refill testing.
func NewTokenBucketWithClock(burstCapacity, refillPerSecond float64, now func() time.Time) *TokenBucket {
	return &TokenBucket{
		capacity:        burstCapacity,
		refillPerSecond: refillPerSecond,
		tokens:          burstCapacity,
		lastRefill:      now(),
		now:             now,
	}
}

// Allow attempts to consume one token. It returns true (and consumes a
// token) if one was available, false otherwise.
func (b *TokenBucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refillLocked()

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// HasBalance refills for elapsed time and reports whether the resulting
// balance is positive — a non-consuming check (unlike Allow, it never
// subtracts a token itself), for the TPM rate-limit dimension per
// docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md: a request's real token
// cost isn't known until after it completes, so the pre-request decision
// can only ask "has past usage already exhausted this bucket," never
// "does this specific request fit."
func (b *TokenBucket) HasBalance() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	return b.tokens > 0
}

// Debit refills for elapsed time, then subtracts n — deliberately
// allowed to leave tokens negative (an overdraft), since n (real token
// usage) is only known retrospectively, after HasBalance's own decision
// already let the request through. The bucket recovers via ordinary
// refill on subsequent calls, exactly like a positive balance would.
func (b *TokenBucket) Debit(n float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	b.tokens -= n
}

// ReserveTPM is HasBalance's concurrency-safe replacement for real
// request-handling code, per
// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md: it
// performs the exact same "is the balance positive" check HasBalance
// does, but — under the SAME lock acquisition, never released and
// re-acquired in between — immediately follows an "allowed" result with
// a provisional debit of a conservative reservation estimate, closing
// the TOCTOU window between HasBalance and Debit that let concurrent
// requests all pass the check before any of them committed a debit.
// allowed is false exactly when HasBalance would have returned false, in
// which case reservedTokens is 0 and nothing was mutated.
//
// Every true `allowed` return MUST be paired with exactly one
// ReconcileTPM call, even on an error/timeout path — see ReconcileTPM's
// own doc comment for why a leaked, never-reconciled reservation
// permanently shrinks the bucket's effective remaining balance.
func (b *TokenBucket) ReserveTPM() (allowed bool, reservedTokens float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	if b.tokens <= 0 {
		return false, 0
	}
	reservedTokens = b.reservationAmountLocked()
	b.tokens -= reservedTokens
	return true, reservedTokens
}

// reservationAmountLocked mirrors
// budget.Tracker.reservationAmountLocked: this bucket's own historical
// average real-token-usage per ReconcileTPM call with a non-nil
// realTokens (billedTokens / billedCount), once at least one such call
// has happened — or, before any usage history exists at all
// (billedCount == 0: a brand-new bucket, or one just reset via
// Register), the bucket's own full current balance, clamped to zero
// rather than negative (ReserveTPM's own check above already guarantees
// b.tokens > 0 by the time this runs, so the clamp is defensive, not
// reachable in practice). Callers must hold b.mu.
func (b *TokenBucket) reservationAmountLocked() float64 {
	if b.billedCount > 0 {
		return b.billedTokens / float64(b.billedCount)
	}
	if b.tokens < 0 {
		return 0
	}
	return b.tokens
}

// ReconcileTPM undoes a previous ReserveTPM call's provisional debit (its
// reservedTokens return value, passed back here unchanged) and, if
// realTokens is non-nil, debits the real usage in its place —
// atomically, under one lock acquisition, mirroring
// budget.Tracker.Reconcile exactly. Because this adds back EXACTLY the
// reservedTokens value ReserveTPM subtracted, the net effect on balance
// for a request that reserves then reconciles with no concurrent
// interleaving is byte-identical to the old HasBalance-then-Debit pair,
// regardless of what the reservation estimate happened to be.
//
// realTokens == nil releases the reservation with no replacement — the
// correct call for a request that errored, timed out, or turned out
// non-billable (a cache hit or coalesced singleflight follower, per
// docs/rfcs/2026-09-05-gateway-cost-double-counting.md) before real
// usage was ever known. This release path is what prevents a permanent
// capacity leak: every ReserveTPM that returns allowed == true MUST
// eventually reach a matching ReconcileTPM call, on every return path
// (including error).
func (b *TokenBucket) ReconcileTPM(reservedTokens float64, realTokens *float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	b.tokens += reservedTokens
	if realTokens != nil {
		b.tokens -= *realTokens
		b.billedTokens += *realTokens
		b.billedCount++
	}
}

// IncreaseReservation is ReserveTPM's mid-stream top-up sibling — see
// budget.Tracker.IncreaseReservation's doc comment for the shared design
// rationale (the streaming concurrent-sibling reservation gap). A no-op,
// always allowed (returns currentReservedTokens unchanged), when
// newReservedTokens is not actually larger than currentReservedTokens —
// callers are expected to check this cheaply themselves before calling,
// to avoid acquiring b.mu on every one of a stream's many chunks, but
// this method stays correct even if a caller doesn't bother. Otherwise
// atomically checks whether the delta fits under the bucket's current
// balance and, if so, debits it (b.tokens -= delta) and returns (true,
// newReservedTokens); if it doesn't fit, b.tokens is left completely
// unchanged and this returns (false, currentReservedTokens) — mirroring
// ReserveTPM's own "insufficient balance" outcome exactly.
func (b *TokenBucket) IncreaseReservation(currentReservedTokens, newReservedTokens float64) (allowed bool, appliedTokens float64) {
	if newReservedTokens <= currentReservedTokens {
		return true, currentReservedTokens
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	delta := newReservedTokens - currentReservedTokens
	if b.tokens < delta {
		return false, currentReservedTokens
	}
	b.tokens -= delta
	return true, newReservedTokens
}

// refillLocked adds tokens for elapsed time since the last refill, capped
// at capacity. Callers must hold b.mu.
func (b *TokenBucket) refillLocked() {
	now := b.now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed <= 0 {
		return
	}
	b.tokens = math.Min(b.capacity, b.tokens+elapsed*b.refillPerSecond)
	b.lastRefill = now
}
