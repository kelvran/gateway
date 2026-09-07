package ratelimit

import "sync"

// ConcurrencyConfig is one virtual key's own in-flight-request cap, per
// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md's design (b) --
// mirrors KeyConfig's own shape and "0/negative = unlimited" convention.
type ConcurrencyConfig struct {
	ID string
	// MaxInFlight is the maximum number of this key's requests allowed to
	// be simultaneously outstanding (auth passed, response not yet
	// finalized). <= 0 (the default, and every config written before this
	// feature existed) means unlimited -- matching KeyConfig.TPMCapacity's
	// identical "<= 0 disables this dimension entirely" rule.
	MaxInFlight int
}

// ConcurrencyLimiter bounds how many requests from the same virtual key
// may be in flight at once -- a genuinely different dimension from
// KeyLimiter's RPM/TPM rate limiting, which bounds how FAST a key may
// issue new requests or spend tokens over time, never how MANY of its
// requests may be simultaneously outstanding. A key comfortably under its
// RPM cap can still open an unbounded number of long-running concurrent
// requests, each adding real load to a deployment that may already be
// struggling -- exactly the shape a retry storm from many parallel agent
// workers takes, and exactly what neither RPM nor TPM catches.
//
// In-memory, single-instance only in v1 -- the same "single-instance
// stepping stone" scope limit this project's rate-limiting/budget/cache-
// stampede-protection features have each started with (see
// docs/rfcs/2026-09-05-gateway-cache-stampede-protection.md's identical
// framing); a multi-instance deployment enforces this cap independently
// per instance, not globally, named explicitly as future work rather than
// silently assumed solved.
//
// Deliberately scoped to the virtual-key ID only, never agent_run_id --
// see the RFC's design (b) for why: agent_run_id is entirely client-
// supplied and unverified (telemetry.AgentRunIDFromContext, read
// directly, confirms this), so enforcing a cap on it would let a client
// trivially bypass this control by rotating the value per request.
type ConcurrencyLimiter struct {
	mu       sync.Mutex
	limits   map[string]int // keyID -> MaxInFlight; a keyID with MaxInFlight <= 0 is never stored here at all.
	inFlight map[string]int
}

// NewConcurrencyLimiter builds a ConcurrencyLimiter from keys -- the
// exact same "eagerly construct per-key state at startup" shape
// NewInMemoryKeyLimiter already uses.
func NewConcurrencyLimiter(keys []ConcurrencyConfig) *ConcurrencyLimiter {
	limits := make(map[string]int, len(keys))
	for _, k := range keys {
		if k.MaxInFlight > 0 {
			limits[k.ID] = k.MaxInFlight
		}
	}
	return &ConcurrencyLimiter{limits: limits, inFlight: map[string]int{}}
}

// Acquire attempts to reserve one in-flight slot for keyID. It returns
// false (and reserves nothing) if keyID is already at its configured
// MaxInFlight cap. A keyID with no configured limit (MaxInFlight <= 0, or
// never registered at all) always succeeds -- matching this codebase's
// "unconfigured means unlimited" convention for every other optional
// per-key control (TPM, per-model rate limits, budget).
//
// Every successful Acquire MUST be paired with exactly one Release call
// once the request finishes, normally via defer at the call site --
// ConcurrencyLimiter itself has no timeout or leak-detection mechanism;
// an Acquire without a matching Release permanently consumes one slot.
func (l *ConcurrencyLimiter) Acquire(keyID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	max, limited := l.limits[keyID]
	if !limited {
		return true
	}
	if l.inFlight[keyID] >= max {
		return false
	}
	l.inFlight[keyID]++
	return true
}

// Release frees one in-flight slot for keyID previously reserved by a
// successful Acquire. A no-op (never goes negative, never panics) for a
// keyID with no configured limit, or with no outstanding slots --
// defends against a Release without a matching successful Acquire, which
// correct calling code must never do, but which is cheap to guard
// against regardless.
func (l *ConcurrencyLimiter) Release(keyID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight[keyID] > 0 {
		l.inFlight[keyID]--
	}
}

// Register upserts keyID's MaxInFlight cap live -- mirroring
// KeyLimiter.Register's own live-update precedent for
// docs/rfcs/2026-09-05-gateway-admin-api.md's virtual-key mutation.
// Deliberately never resets inFlight: an admin update changing a key's
// limit while requests are genuinely in flight for it must not lose
// track of them, exactly like KeyLimiter.Register never resets a live
// TokenBucket's own in-progress refill state for unrelated fields.
//
// Not yet wired into dataplane.Pipeline.UpsertVirtualKey in this pass --
// a named, real v1 gap (see the RFC's risk assessment), not something
// this method's own existence implies is already fully connected.
func (l *ConcurrencyLimiter) Register(cfg ConcurrencyConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cfg.MaxInFlight > 0 {
		l.limits[cfg.ID] = cfg.MaxInFlight
	} else {
		delete(l.limits, cfg.ID)
	}
}

// InFlight reports keyID's current in-flight count -- exported so tests
// (and a future admin-status endpoint) can observe state directly,
// mirroring router.IsHealthy's own precedent of exposing read-only
// internal state for exactly that reason.
func (l *ConcurrencyLimiter) InFlight(keyID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inFlight[keyID]
}
