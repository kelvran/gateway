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
