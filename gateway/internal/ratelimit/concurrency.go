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
// trivially bypass this control by rotating the value per request. This
// remains true and is NOT being revisited by the observability addition
// below -- per docs/upgrade-research/multi-agent-fanout-concurrency-
// composition-2026-09-23.md, no production LLM gateway surveyed trusts a
// client-supplied identifier for ENFORCEMENT, but several (Cloudflare,
// LiteLLM) do trust one for pure tagging/attribution, exactly the
// distinction runCounts below draws.
//
// runCounts is a SECOND, entirely separate bookkeeping dimension added
// for pure OBSERVABILITY, never enforcement: keyID -> agentRunID ->
// in-flight count for that pair. Nothing in this file's own admission
// logic (AcquireWithRun's cap check below) ever reads runCounts -- only
// InFlightByAgentRun does, and that method exists solely so an external
// caller (the admin API) can show an operator, for one virtual key, how
// its current in-flight load breaks down by agent_run_id, so a human can
// notice "most of this key's concurrency is one runaway agent run" and
// decide -- manually, out of band -- whether to mint that run its own
// separate virtual key. Populated for EVERY keyID unconditionally, on
// every AcquireWithRun/ReleaseWithRun call, regardless of whether that
// keyID has a configured MaxInFlight cap in `limits` -- this is the
// critical correctness point: unlike `inFlight` (which Acquire's own
// early return for an unconfigured/unlimited key never touches at all,
// see AcquireWithRun below), gating runCounts behind "only when this key
// is limited" would make this feature silently report zero/nothing for
// an uncapped key's own agent-run breakdown -- exactly the key an
// operator most needs this signal for -- which is actively misleading,
// not just incomplete. A blank agentRunID (the caller never set one, or
// called the plain Acquire/Release wrappers) is tracked under the ""
// map key, the same convention this codebase uses for every other
// absent optional dimension (see KeyConfig's own per-model map
// convention). A per-(keyID, agentRunID) count is deleted from its inner
// map the moment it reaches zero (see recordRunReleaseLocked), so a
// client rotating agent_run_id per request -- the exact spoofing
// behavior this file already documents as the reason agent_run_id is
// never trusted for enforcement -- churns through short-lived map
// entries rather than growing this map without bound.
type ConcurrencyLimiter struct {
	mu        sync.Mutex
	limits    map[string]int // keyID -> MaxInFlight; a keyID with MaxInFlight <= 0 is never stored here at all.
	inFlight  map[string]int
	runCounts map[string]map[string]int
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
	return &ConcurrencyLimiter{limits: limits, inFlight: map[string]int{}, runCounts: map[string]map[string]int{}}
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
//
// A thin wrapper around AcquireWithRun(keyID, "") -- see that method for
// the real, single implementation of the admission/bookkeeping logic
// below. Kept as its own function, with its own unchanged signature,
// specifically so every existing call site (this package's own
// concurrency_test.go, dataplane's deployment-scoped
// deployment_capacity_test.go, and DeploymentConcurrency in
// cmd/gateway/main.go, where agent_run_id has no meaning at all) never
// has to thread an irrelevant parameter through for no benefit.
func (l *ConcurrencyLimiter) Acquire(keyID string) bool {
	return l.AcquireWithRun(keyID, "")
}

// Release frees one in-flight slot for keyID previously reserved by a
// successful Acquire. A no-op (never goes negative, never panics) for a
// keyID with no configured limit, or with no outstanding slots --
// defends against a Release without a matching successful Acquire, which
// correct calling code must never do, but which is cheap to guard
// against regardless.
//
// A thin wrapper around ReleaseWithRun(keyID, "") -- see Acquire's own
// doc comment for why this stays its own function with its own
// unchanged signature rather than gaining a new parameter.
func (l *ConcurrencyLimiter) Release(keyID string) {
	l.ReleaseWithRun(keyID, "")
}

// AcquireWithRun is Acquire's real implementation: the exact same
// limits/inFlight admission logic Acquire has always had (every branch
// below matches Acquire's own pre-existing behavior unchanged), plus one
// new, purely additive step -- recordRunAcquireLocked -- that updates the
// SECOND, observation-only runCounts bookkeeping described on
// ConcurrencyLimiter's own doc comment. That second step runs on every
// successful acquisition, including for a keyID with no configured
// MaxInFlight cap at all (the `if !limited { ...; return true }` branch
// below) -- this is deliberate and load-bearing: InFlightByAgentRun's own
// doc comment explains why an uncapped key must still get accurate
// per-agent-run tracking. agentRunID is never validated, signed, or
// trusted for this method's own admission decision (the cap check above
// it) -- it is read-and-recorded only, per this file's package-level
// doc comment's observability-only rule.
func (l *ConcurrencyLimiter) AcquireWithRun(keyID, agentRunID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	max, limited := l.limits[keyID]
	if !limited {
		l.recordRunAcquireLocked(keyID, agentRunID)
		return true
	}
	if l.inFlight[keyID] >= max {
		return false
	}
	l.inFlight[keyID]++
	l.recordRunAcquireLocked(keyID, agentRunID)
	return true
}

// ReleaseWithRun is Release's real implementation -- mirrors
// AcquireWithRun's own "unchanged inFlight logic plus one new,
// observation-only step" shape exactly.
func (l *ConcurrencyLimiter) ReleaseWithRun(keyID, agentRunID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight[keyID] > 0 {
		l.inFlight[keyID]--
	}
	l.recordRunReleaseLocked(keyID, agentRunID)
}

// recordRunAcquireLocked increments runCounts[keyID][agentRunID],
// lazily allocating the inner map on first use -- callers must already
// hold l.mu. Never consulted by any admission decision; see
// ConcurrencyLimiter's own doc comment for the full observability-only
// rule this helper exists to implement.
func (l *ConcurrencyLimiter) recordRunAcquireLocked(keyID, agentRunID string) {
	if l.runCounts[keyID] == nil {
		l.runCounts[keyID] = map[string]int{}
	}
	l.runCounts[keyID][agentRunID]++
}

// recordRunReleaseLocked decrements runCounts[keyID][agentRunID],
// deleting the entry entirely once it reaches zero rather than leaving a
// stale zero behind -- callers must already hold l.mu. This deletion is
// what keeps a client that rotates agent_run_id on every single request
// (the exact spoofing behavior this file already documents as the
// reason agent_run_id is never trusted for enforcement) from growing
// this map without bound: each such request's own acquire+release pair
// nets to a fully removed entry, not an ever-growing history. A no-op
// (never goes negative) for a (keyID, agentRunID) pair with no
// outstanding count, mirroring Release's own defensive convention for
// inFlight.
func (l *ConcurrencyLimiter) recordRunReleaseLocked(keyID, agentRunID string) {
	counts := l.runCounts[keyID]
	if counts == nil || counts[agentRunID] <= 0 {
		return
	}
	counts[agentRunID]--
	if counts[agentRunID] == 0 {
		delete(counts, agentRunID)
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

// InFlightByAgentRun reports keyID's current in-flight load broken down
// by agent_run_id -- the read-only surface for the observability feature
// described on ConcurrencyLimiter's own doc comment. total is the sum of
// every count in keyID's runCounts entry, so it is accurate for keyID
// whether or not it has a configured MaxInFlight cap (unlike InFlight
// above, which only ever reflects a capped key's own bookkeeping);
// byAgentRun is the same data broken down per agent_run_id, with a blank
// "" key meaning "no agent_run_id was set on that request". Both
// returned values are a fresh, independent copy taken while l.mu is
// held -- never a live reference into l.runCounts -- so a caller can
// read and hold onto this snapshot with no risk of a data race with a
// concurrent AcquireWithRun/ReleaseWithRun, and no risk of the snapshot
// itself changing under the caller after this method returns. This
// method is READ-ONLY and is never, itself, consulted by
// AcquireWithRun's own admission decision -- it exists purely so an
// external caller (the admin API's GET .../inflight route) can show an
// operator this signal for a human to act on out of band; see this
// file's own package-level doc comment for why that boundary is never
// crossed in the other direction.
func (l *ConcurrencyLimiter) InFlightByAgentRun(keyID string) (total int, byAgentRun map[string]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	counts := l.runCounts[keyID]
	byAgentRun = make(map[string]int, len(counts))
	for run, n := range counts {
		byAgentRun[run] = n
		total += n
	}
	return total, byAgentRun
}
