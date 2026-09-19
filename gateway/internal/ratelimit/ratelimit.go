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

	// resetEpoch mirrors budget.Tracker.periodEpoch's own contract
	// exactly, one epoch counter per bucket rather than per key: bumped
	// every Reset call, and threaded out of ReserveTPM/IncreaseReservation
	// as reservationEpoch for ReconcileTPM/IncreaseReservation's own
	// later call to compare against. Closes a real bug an audit found in
	// Reset's own first version: unconditionally force-setting
	// tokens = burstCapacity discarded any outstanding reservation's
	// already-taken debit, so a later ReconcileTPM call for that
	// reservation added reservedTokens back on top of the freshly-reset
	// full balance — silently granting free extra capacity that
	// refillLocked's own math.Min then clamped away, un-billing whatever
	// real usage the same call also tried to debit. When resetEpoch no
	// longer matches reservationEpoch, the reservation being reconciled
	// is against a balance that no longer exists (Reset already
	// overwrote it), so ReconcileTPM skips re-adding reservedTokens
	// (never driving the fresh balance up by a phantom amount) and
	// applies realTokens, if any, fresh against the current balance
	// instead — see ReconcileTPM's own doc comment.
	resetEpoch int64
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

// Reset reconfigures an EXISTING bucket to burstCapacity/refillPerSecond
// at full capacity — the exact same "admin update resets the key to
// full burst" effect KeyLimiter.Register has always documented as
// deliberate — but WITHOUT discarding this *TokenBucket's own object
// identity the way constructing a brand-new one via NewTokenBucket
// would. Added to close a real bug an audit found: KeyLimiter.Register
// previously replaced tpmBuckets[id] (and perModelTPMBuckets[id][model])
// outright, so a reservation whose ReserveTPM call had already resolved
// (and holds, only implicitly via keyID/model, never a pinned pointer)
// the OLD bucket would have its later ReconcileTPM call re-resolve keyID/
// model via a fresh map lookup and land on the NEW object instead —
// crediting/debiting a bucket that was never actually the one debited,
// while the real, correctly-debited old object was silently discarded,
// never reconciled. Reset lets Register update capacity/refill/tokens
// IN PLACE on the SAME object every outstanding reservation already
// resolved to, closing that window, while still producing the
// documented full-capacity-reset effect.
func (b *TokenBucket) Reset(burstCapacity, refillPerSecond float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.capacity = burstCapacity
	b.refillPerSecond = refillPerSecond
	b.tokens = burstCapacity
	b.lastRefill = b.now()
	b.billedTokens = 0
	b.billedCount = 0
	b.resetEpoch++
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
// reservationEpoch MUST be threaded through to that same ReconcileTPM
// call (and any IncreaseReservation call in between) unchanged — never
// re-derived — mirroring budget.Tracker.Reserve's own epoch contract;
// see resetEpoch's field comment for why.
func (b *TokenBucket) ReserveTPM() (allowed bool, reservedTokens float64, reservationEpoch int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	if b.tokens <= 0 {
		return false, 0, b.resetEpoch
	}
	reservedTokens = b.reservationAmountLocked()
	b.tokens -= reservedTokens
	return true, reservedTokens, b.resetEpoch
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
//
// reservationEpoch MUST be the exact value ReserveTPM (or the latest
// IncreaseReservation call for this same reservation) returned — see
// resetEpoch's own field comment. When it no longer matches b.resetEpoch,
// a Reset happened since this reservation was taken, so the reservedTokens
// debit this call would otherwise undo no longer exists in the current
// balance (Reset already overwrote it to full capacity) — skipping the
// addition here is what prevents driving that fresh balance up by a
// phantom amount that refillLocked's own capacity clamp would then
// silently eat, un-billing whatever realTokens this same call debits.
func (b *TokenBucket) ReconcileTPM(reservedTokens float64, reservationEpoch int64, realTokens *float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	if b.resetEpoch == reservationEpoch {
		b.tokens += reservedTokens
	}
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
//
// reservationEpoch/newReservationEpoch mirror
// budget.Tracker.IncreaseReservation's own epoch contract exactly (see
// resetEpoch's field comment): a long-running stream's original
// ReserveTPM call and its later, possibly-repeated top-ups here can
// straddle a Reset call triggered by any concurrent Register. When the
// epoch has rolled over, currentReservedTokens no longer represents a
// real outstanding debit against the (already-reset) current balance,
// so this reserves newReservedTokens FRESH against that balance instead
// of computing a delta against a stale amount. The caller MUST thread
// whichever epoch this returns into its eventual ReconcileTPM call,
// mirroring ReserveTPM's own contract, never the original pre-topup
// epoch.
func (b *TokenBucket) IncreaseReservation(currentReservedTokens, newReservedTokens float64, reservationEpoch int64) (allowed bool, appliedTokens float64, newReservationEpoch int64) {
	if newReservedTokens <= currentReservedTokens {
		return true, currentReservedTokens, reservationEpoch
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()

	if b.resetEpoch != reservationEpoch {
		if b.tokens < newReservedTokens {
			return false, 0, b.resetEpoch
		}
		b.tokens -= newReservedTokens
		return true, newReservedTokens, b.resetEpoch
	}

	delta := newReservedTokens - currentReservedTokens
	if b.tokens < delta {
		return false, currentReservedTokens, b.resetEpoch
	}
	b.tokens -= delta
	return true, newReservedTokens, b.resetEpoch
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
