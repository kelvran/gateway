package router

// HealthConfig configures the N-of-M consecutive-probe thresholds Router
// applies to decide whether a deployment is eligible for Select, per
// docs/rfcs/2026-09-07-gateway-active-health-probing.md, plus the
// post-recovery weight-ramp Select applies for a short window after a
// deployment transitions from unhealthy back to healthy (see
// deploymentHealth's ramping fields and admitRampedTurn, below) — closing
// the real gap evals/tests/fixtures/regression_corpus_routing_chaos.json's
// "chaos-recovery-instant-full-weight-no-ramp" case documented: a
// deployment that just barely recovered was previously handed its full
// configured traffic share the instant its Nth consecutive probe
// succeeded, a known real-world thundering-herd-on-recovery failure mode
// (Slack's 2021 postmortem: an automated recovery action itself became a
// second cascading-failure point against a shared dependency). The probe
// LOOP itself (issuing the actual request, on a timer) lives one layer
// up, in gateway/internal/gateway/dataplane — this package stays the
// adapter-agnostic leaf it already was, only tracking outcomes reported
// to it via ReportProbeResult.
//
// Zero-valued HealthConfig (the case whenever a caller never wires
// probing at all) normalizes to the same conservative defaults a caller
// who does wire probing but doesn't override the thresholds gets —
// deliberately NOT LiteLLM's own aggressive single-failure default,
// since Kelvran has no production traffic yet to calibrate a more
// aggressive threshold against.
type HealthConfig struct {
	// UnhealthyThreshold is the number of CONSECUTIVE failed probes
	// required before a deployment is excluded from Select's pool.
	// <= 0 defaults to 3.
	UnhealthyThreshold int
	// HealthyThreshold is the number of CONSECUTIVE successful probes
	// required before an excluded deployment is re-included. <= 0
	// defaults to 2.
	HealthyThreshold int
	// RecoveryRampSteps is the number of ADDITIONAL consecutive
	// successful probes required, after the one that re-includes a
	// deployment (see HealthyThreshold above), before that deployment's
	// effective routing weight finishes ramping back up to its full
	// configured Weight. <= 0 defaults to 4.
	//
	// Ramping is driven by subsequent PROBE OUTCOMES, not wall-clock
	// time, deliberately: this package is I/O-free by design (see the
	// package doc comment) and has no clock dependency anywhere —
	// injecting one here purely for this feature would be the first
	// exception, would complicate deterministic unit testing (fake
	// clocks or real sleeps instead of plain counter assertions), and
	// would lose a property probe-driven ramping gets for free: a
	// probe failure DURING the ramp window is immediately, structurally
	// meaningful (see ReportProbeResult) rather than requiring separate
	// "was there a failure during this time window" bookkeeping a
	// pure-time design would need to reconstruct from scratch. Since
	// probes already run on a fixed interval (dataplane.Pipeline's
	// RunHealthProbeLoop), N additional successful probes already
	// approximates N probe-intervals of real elapsed time in practice,
	// without the added complexity.
	RecoveryRampSteps int
	// RecoveryRampInitialPercent is the effective-weight percentage (of
	// the deployment's full configured Weight) a just-recovered
	// deployment starts at, before ramping linearly up to 100 over
	// RecoveryRampSteps further successful probes. <= 0 defaults to 20;
	// values above 100 are clamped to 100 (a no-op ramp).
	RecoveryRampInitialPercent int
}

const (
	defaultUnhealthyThreshold         = 3
	defaultHealthyThreshold           = 2
	defaultRecoveryRampSteps          = 4
	defaultRecoveryRampInitialPercent = 20
	// rampCreditFull is the Bresenham-style accumulator's fixed
	// denominator admitRampedTurn accumulates a ramping deployment's
	// current effective-weight percentage into on every offered turn —
	// see admitRampedTurn's own doc comment for why this yields an
	// exact, deterministic acceptance ratio rather than a statistical
	// approximation.
	rampCreditFull = 100
)

// normalized resolves <= 0 fields to their documented defaults — the
// same "zero means unset, resolve to an operational default" convention
// this codebase already uses throughout (CacheL2TTL, TelemetryConfig's
// Exporter, etc.).
func (c HealthConfig) normalized() HealthConfig {
	if c.UnhealthyThreshold <= 0 {
		c.UnhealthyThreshold = defaultUnhealthyThreshold
	}
	if c.HealthyThreshold <= 0 {
		c.HealthyThreshold = defaultHealthyThreshold
	}
	if c.RecoveryRampSteps <= 0 {
		c.RecoveryRampSteps = defaultRecoveryRampSteps
	}
	if c.RecoveryRampInitialPercent <= 0 {
		c.RecoveryRampInitialPercent = defaultRecoveryRampInitialPercent
	}
	if c.RecoveryRampInitialPercent > rampCreditFull {
		c.RecoveryRampInitialPercent = rampCreditFull
	}
	return c
}

// deploymentHealth is one deployment's own consecutive-probe counters,
// current health verdict, and post-recovery ramp state. Never
// constructed directly outside Router.ReportProbeResult's own lazy-init.
type deploymentHealth struct {
	healthy              bool
	consecutiveFailures  int
	consecutiveSuccesses int

	// ramping marks this deployment as being within its post-recovery
	// weight-ramp window — the ramp-in-progress marker threaded into
	// Select's weight-selection math via admitRampedTurn. false means
	// "use the full configured Weight," byte-identical to this
	// package's pre-ramp behavior — this is what makes a fully-settled
	// deployment (never ramped, or ramp long since completed)
	// indistinguishable from before this feature existed.
	ramping bool
	// rampStep is the number of additional successful probes counted
	// toward ramp completion so far, 0 at the moment of recovery
	// (HealthyThreshold's own Nth success), reset to 0 by any
	// intervening probe failure — a deployment that flaps during its
	// own recovery window restarts the ramp's caution rather than
	// continuing to advance toward full weight on the strength of a
	// streak that has already been broken.
	rampStep int
	// rampCredit is the current effective-weight percentage's
	// accumulator for admitRampedTurn's Bresenham-style thinning gate —
	// persists across Select calls exactly like modelState.cw persists
	// across next() calls, so the accept ratio stays exact over any
	// window that's a multiple of its period, not merely on average.
	rampCredit int
}

// ReportProbeResult records the outcome of one health probe for the
// deployment named name — success true for a probe that completed
// without error, false otherwise. name is excluded from Select's pool
// only once r.health.UnhealthyThreshold CONSECUTIVE failures have been
// reported for it (never a single one), and re-included only once
// r.health.HealthyThreshold CONSECUTIVE successes have been reported —
// any interleaved success resets the failure streak to zero and vice
// versa, so "N consecutive" always means genuinely consecutive, never a
// non-contiguous count.
//
// The success report that re-includes a previously-unhealthy deployment
// also starts its recovery weight-ramp (ramping=true, rampStep=0, at
// r.healthCfg.RecoveryRampInitialPercent of its full weight); each
// further success advances rampStep by one until it reaches
// r.healthCfg.RecoveryRampSteps, at which point ramping clears entirely
// and the deployment reverts to using its full configured Weight with no
// residual state, indistinguishable from a deployment that was never
// unhealthy at all. A failure while ramping resets rampStep/rampCredit to
// their starting values (still ramping, but back at the initial,
// most-cautious percentage) rather than letting it continue advancing —
// and a failure that actually re-trips UnhealthyThreshold clears ramping
// outright, since Select already excludes an unhealthy deployment
// entirely and the next recovery will re-initialize ramp state fresh
// regardless.
//
// A deployment never reported here at all (name absent from internal
// state) is always treated as healthy — this is what makes Select's
// pre-existing "every configured deployment is eligible" behavior
// exactly unchanged whenever health probing is not wired/configured at
// all (see dataplane.Pipeline.RunHealthProbeLoop's own "interval <= 0
// means disabled" gate).
//
// Returns the deployment's healthy state after applying this report, and
// whether that state changed from before this call — callers (e.g.
// dataplane.Pipeline.ProbeDeployments) use changed to decide whether a
// transition is worth logging, without needing a second call to inspect
// prior state themselves. Ramp-progress changes are deliberately not
// reflected in changed — that return value is, and stays, purely about
// the healthy/unhealthy verdict, exactly as it was before this feature.
func (r *Router) ReportProbeResult(name string, success bool) (healthy bool, changed bool) {
	r.healthMu.Lock()
	defer r.healthMu.Unlock()

	h, ok := r.health[name]
	if !ok {
		h = &deploymentHealth{healthy: true}
		r.health[name] = h
	}
	was := h.healthy

	if success {
		h.consecutiveFailures = 0
		h.consecutiveSuccesses++
		switch {
		case !h.healthy && h.consecutiveSuccesses >= r.healthCfg.HealthyThreshold:
			h.healthy = true
			h.ramping = true
			h.rampStep = 0
			h.rampCredit = 0
		case h.healthy && h.ramping:
			h.rampStep++
			if h.rampStep >= r.healthCfg.RecoveryRampSteps {
				h.ramping = false
				h.rampStep = 0
				h.rampCredit = 0
			}
		}
	} else {
		h.consecutiveSuccesses = 0
		h.consecutiveFailures++
		if h.ramping {
			h.rampStep = 0
			h.rampCredit = 0
		}
		if h.healthy && h.consecutiveFailures >= r.healthCfg.UnhealthyThreshold {
			h.healthy = false
			h.ramping = false
		}
	}

	return h.healthy, h.healthy != was
}

// IsHealthy reports whether name is currently eligible for selection —
// exported so callers outside this package (e.g.
// dataplane.Pipeline's own tests) can observe health state directly
// rather than only inferring it indirectly through many Select calls.
func (r *Router) IsHealthy(name string) bool {
	r.healthMu.Lock()
	defer r.healthMu.Unlock()
	h, ok := r.health[name]
	if !ok {
		return true
	}
	return h.healthy
}

// admitRampedTurn reports whether name's currently-offered turn should be
// admitted, applying the post-recovery weight-ramp gate: a deployment not
// currently ramping always admits (the exact pre-ramp behavior, zero
// overhead, no state touched) — this is what keeps every model group with
// no active recovery ramp completely unaffected by this feature. A
// ramping deployment instead runs a Bresenham-style (pixel-rasterization
// / rate-limiter) thinning gate: its persistent rampCredit accumulates
// the deployment's CURRENT effective-weight percentage on every offered
// turn, and admits (returning true, then subtracting rampCreditFull)
// only once that accumulator reaches rampCreditFull — carrying any
// remainder forward rather than discarding it, so no fractional share is
// ever lost across calls. This is deliberately layered OUTSIDE
// modelState/wrr.go's own cursor math (selectHealthy just treats a
// rejected offer the same way it already treats an unhealthy one: skip
// and keep searching) rather than reconstructing modelState's gcd/maxW/
// sumW cursor with adjusted weights — the smooth-WRR schedule itself
// (wrr.go) is never touched, so its own byte-identical-to-before
// guarantee for every non-ramping deployment holds with zero special
// casing.
//
// The percentage itself is r.healthCfg.RecoveryRampInitialPercent at
// rampStep 0, increasing linearly to (but never quite reaching, from
// this gate's perspective — see ReportProbeResult) 100 as rampStep
// advances toward RecoveryRampSteps.
func (r *Router) admitRampedTurn(name string) bool {
	r.healthMu.Lock()
	defer r.healthMu.Unlock()

	h, ok := r.health[name]
	if !ok || !h.ramping {
		return true
	}

	cfg := r.healthCfg
	step := h.rampStep
	if step > cfg.RecoveryRampSteps {
		step = cfg.RecoveryRampSteps
	}
	span := rampCreditFull - cfg.RecoveryRampInitialPercent
	percent := cfg.RecoveryRampInitialPercent + span*step/cfg.RecoveryRampSteps

	h.rampCredit += percent
	if h.rampCredit >= rampCreditFull {
		h.rampCredit -= rampCreditFull
		return true
	}
	return false
}

// selectHealthy calls ms.next() up to ms.sumW times — the smooth-WRR
// schedule's full cycle length, guaranteed to OFFER every distinct
// deployment in the group at least once even under the most skewed
// weight configuration (see wrr.go's sumW doc comment) — returning the
// first offered candidate Router currently considers both healthy and
// (if it's within its post-recovery weight ramp, see admitRampedTurn)
// admitted for this particular turn.
//
// If every deployment in the group is currently unhealthy, or every
// offer within this bounded search happens to be ramp-rejected despite
// one or more candidates being healthy, this fails OPEN: it returns the
// last candidate examined rather than ("", false), mirroring this
// codebase's established fail-open precedent elsewhere (e.g.
// internal/ratelimit's Redis-backend-error path) — a known-bad or
// momentarily-throttled deployment is still a better answer than "no
// deployment configured for this model at all," which would otherwise
// incorrectly surface as dataplane.ErrNoDeployment. In practice this
// fail-open ramp case is rare and self-correcting: rampCredit persists
// across calls, so a rejected offer only delays, never permanently
// denies, that deployment's eventual admission.
func (r *Router) selectHealthy(ms *modelState) (string, bool) {
	var name string
	var ok bool
	for i := 0; i < ms.sumW; i++ {
		name, ok = ms.next()
		if !ok {
			return "", false
		}
		if !r.IsHealthy(name) {
			continue
		}
		if r.admitRampedTurn(name) {
			return name, true
		}
	}
	return name, ok
}
