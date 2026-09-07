package ratelimit

// Exponential backoff with equal jitter, per
// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md's design (a).
// Deliberately equal jitter, not AWS's own "most effective" full-jitter
// recommendation: full jitter (random(0, capped)) has no deterministic
// floor -- a near-zero delay is always a legitimate outcome for any
// attempt count, which would make "prove backoff genuinely spaces things
// out over real elapsed time" impossible to assert without a large-sample
// statistical test. Equal jitter keeps most of full jitter's
// desynchronizing benefit (the upper half of the delay is still fully
// randomized) while guaranteeing a real floor to test against.

import (
	"math"
	"math/rand"
	"sync"
	"time"
)

// retryBackoffBase/Cap/MaxStreak bound RetryBackoff's escalation: 500ms
// doubling per consecutive rejection, capped at 30s -- long enough to
// meaningfully desynchronize a thundering herd of retrying agents, short
// enough that a client which DOES back off correctly isn't left waiting
// unreasonably long once whatever caused the rejection clears. streak is
// capped at 10 purely so the exponent computation stays small; 2^10 *
// base already exceeds the cap many times over, so nothing beyond that
// streak value changes the result.
const (
	retryBackoffBase      = 500 * time.Millisecond
	retryBackoffCap       = 30 * time.Second
	retryBackoffMaxStreak = 10
)

// EqualJitterBackoff computes the equal-jitter exponential backoff delay
// for the given attempt (1 = first retry/hop, 2 = second, ...): half of
// the capped exponential value (base * 2^(attempt-1), capped at cap) is
// always applied -- a deterministic floor, so backoff genuinely grows
// with repeated attempts even under worst-case jitter -- the other half
// is randomized by frac, a caller-supplied value in [0, 1). frac is
// caller-supplied, rather than read from a package-global math/rand
// source internally, specifically so callers can inject their own
// randomness (production: rand.Float64) and tests can pin frac to its
// extremes to assert exact bounds. attempt < 1 is treated as 1 -- there
// is no such thing as a "zeroth" backoff.
func EqualJitterBackoff(attempt int, base, maxDelay time.Duration, frac float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	exp := float64(base) * math.Pow(2, float64(attempt-1))
	if !(exp < float64(maxDelay)) { // also true for +Inf from a very large attempt/base
		exp = float64(maxDelay)
	}
	half := exp / 2
	return time.Duration(half + frac*half)
}

// RetryBackoff tracks each virtual key's own consecutive-rejection streak
// and computes the Retry-After delay to suggest, per
// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md's design (a).
// Mirrors two precedents already established elsewhere in this codebase
// rather than inventing a third shape: the map-plus-mutex, per-key-state
// structure KeyLimiter itself uses, and the "N consecutive" streak-
// counter idiom gateway/internal/router/health.go already uses for
// health-probing -- a genuinely different mechanism, deliberately
// reusing only the idiom, never its state.
type RetryBackoff struct {
	mu      sync.Mutex
	streaks map[string]int
	rand    func() float64
}

// NewRetryBackoff constructs a RetryBackoff using the real math/rand
// source.
func NewRetryBackoff() *RetryBackoff {
	return NewRetryBackoffWithRand(rand.Float64)
}

// NewRetryBackoffWithRand constructs a RetryBackoff using randFn as the
// jitter source -- for deterministic testing, mirroring
// NewTokenBucketWithClock's identical "injectable non-determinism" reason
// for existing.
func NewRetryBackoffWithRand(randFn func() float64) *RetryBackoff {
	return &RetryBackoff{streaks: map[string]int{}, rand: randFn}
}

// Record increments keyID's consecutive-rejection streak and returns the
// backoff duration to suggest via Retry-After. Every virtual key's streak
// is tracked independently -- one key's repeated rejections never affect
// another's, matching this codebase's existing per-key tenant-isolation
// discipline (KeyLimiter, budget.Tracker) elsewhere.
func (b *RetryBackoff) Record(keyID string) time.Duration {
	b.mu.Lock()
	b.streaks[keyID]++
	streak := b.streaks[keyID]
	frac := b.rand()
	b.mu.Unlock()

	if streak > retryBackoffMaxStreak {
		streak = retryBackoffMaxStreak
	}
	return EqualJitterBackoff(streak, retryBackoffBase, retryBackoffCap, frac)
}

// Reset clears keyID's consecutive-rejection streak -- called on every
// genuine success, so backoff only escalates while rejections are
// genuinely consecutive for this identity, never accumulating forever
// across occasional successes. A no-op for a keyID with no existing
// streak (never rejected, or already reset).
func (b *RetryBackoff) Reset(keyID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.streaks, keyID)
}
