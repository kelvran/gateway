package dataplane

// The tokens-per-day dimension of a virtual key's rate limit (round-5
// Tier S item 24, 2026-10-15; OWASP LLM06:2026's tokens-per-day limit):
// the types checkRateLimit, finalize, the mid-stream top-up and
// attachRetryAfter share. The limiter side lives in
// internal/ratelimit (KeyConfig.TokensPerDay, ReserveTPD/
// IncreaseReservationTPD/ReconcileTPD). Kept in its own file like
// retry_after.go: one concern, not more lines in dataplane.go.

import (
	"errors"
	"time"
)

// tokenReservation is one token dimension's outstanding reservation for a
// request -- the (reserved, tokens, epoch) triple ReserveTPM/ReserveTPD
// return and ReconcileTPM/ReconcileTPD take back. The TPM dimension still
// travels as three named locals (the shape every test and the streaming
// top-up pointers were written against); the day dimension travels as this
// struct, and the mid-stream top-up updates it through a pointer exactly
// like the TPM pointers in midStreamReservation.
type tokenReservation struct {
	reserved bool
	tokens   float64
	epoch    int64
}

// rateLimitDecision is checkRateLimit's result: whether the request may
// proceed (ok), whether a backend error made that answer a fail-open one
// (failedOpen, already logged and counted once -- it means "admitted
// because of a backend error", so it is false on every rejection, even
// one whose minute check errored before the day check refused), the two token
// reservations the caller MUST thread to finalize on every return path,
// and -- on a tokens-per-day refusal -- the bucket's own wait, which
// rejection() turns into the error attachRetryAfter floors the client's
// Retry-After on.
type rateLimitDecision struct {
	ok            bool
	failedOpen    bool
	tpm           tokenReservation
	tpd           tokenReservation
	rejectedByDay bool
	retryAfter    time.Duration
}

// rejection is the error a !ok decision becomes: the plain ErrRateLimited
// for an RPM or TPM refusal (unchanged), a *tokensPerDayError carrying the
// day bucket's wait for a tokens-per-day refusal.
func (d rateLimitDecision) rejection() error {
	if d.rejectedByDay {
		return &tokensPerDayError{wait: d.retryAfter}
	}
	return ErrRateLimited
}

// maxLimiterRetryAfter caps the Retry-After a limiter's own computed wait
// may set: a day bucket's wait is at most its full refill time, 24 h, plus
// any settle overshoot -- this keeps the header honest for a client that
// reads it (SDKs that cap their own honoured value at 60 s fall back to
// their backoff) while never promising more than the dimension's own
// window. Deliberately above maxUpstreamRetryAfter's 60 s: that cap
// bounds what an UPSTREAM may tell our clients; this wait is Kelvran's
// own, trusted computation. Per the owner's 2026-10-15 decision
// (DECISIONS.md, the `[2026-10-15]` round-5 Tier S item 24 line).
const maxLimiterRetryAfter = 24 * time.Hour

// tokensPerDayError is ErrRateLimited for a tokens-per-day refusal, with
// the day bucket's wait attached. It unwraps to ErrRateLimited so the
// envelope (429 rate_limit_error / rate_limit_exceeded), outcomeFor
// (OUTCOME_RATE_LIMITED) and every errors.Is check see the same rejection
// they always did; only the message names the dimension and only
// attachRetryAfter reads the wait.
type tokensPerDayError struct {
	wait time.Duration
}

func (e *tokensPerDayError) Error() string { return ErrRateLimited.Error() + " (tokens_per_day)" }

func (e *tokensPerDayError) Unwrap() error { return ErrRateLimited }

// limiterRetryAfter returns the limiter's own wait carried by err -- today
// only a tokens-per-day refusal carries one -- capped at
// maxLimiterRetryAfter; zero when err carries none.
func limiterRetryAfter(err error) time.Duration {
	var dayErr *tokensPerDayError
	if !errors.As(err, &dayErr) || dayErr.wait <= 0 {
		return 0
	}
	return min(dayErr.wait, maxLimiterRetryAfter)
}
