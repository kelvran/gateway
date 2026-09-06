package router

// HealthConfig configures the N-of-M consecutive-probe thresholds Router
// applies to decide whether a deployment is eligible for Select, per
// docs/rfcs/2026-09-07-gateway-active-health-probing.md. The probe LOOP
// itself (issuing the actual request, on a timer) lives one layer up, in
// gateway/internal/gateway/dataplane — this package stays the
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
}

const (
	defaultUnhealthyThreshold = 3
	defaultHealthyThreshold   = 2
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
	return c
}

// deploymentHealth is one deployment's own consecutive-probe counters and
// current health verdict. Never constructed directly outside
// Router.ReportProbeResult's own lazy-init.
type deploymentHealth struct {
	healthy              bool
	consecutiveFailures  int
	consecutiveSuccesses int
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
// prior state themselves.
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
		if !h.healthy && h.consecutiveSuccesses >= r.healthCfg.HealthyThreshold {
			h.healthy = true
		}
	} else {
		h.consecutiveSuccesses = 0
		h.consecutiveFailures++
		if h.healthy && h.consecutiveFailures >= r.healthCfg.UnhealthyThreshold {
			h.healthy = false
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

// selectHealthy calls ms.next() up to ms.sumW times — the smooth-WRR
// schedule's full cycle length, guaranteed to visit every distinct
// deployment in the group at least once even under the most skewed
// weight configuration (see wrr.go's sumW doc comment) — returning the
// first candidate Router currently considers healthy.
//
// If every deployment in the group is currently unhealthy, this fails
// OPEN: it returns the last candidate examined rather than ("", false),
// mirroring this codebase's established fail-open precedent elsewhere
// (e.g. internal/ratelimit's Redis-backend-error path) — a known-bad
// deployment is still a better answer than "no deployment configured for
// this model at all," which would otherwise incorrectly surface as
// dataplane.ErrNoDeployment.
func (r *Router) selectHealthy(ms *modelState) (string, bool) {
	var name string
	var ok bool
	for i := 0; i < ms.sumW; i++ {
		name, ok = ms.next()
		if !ok {
			return "", false
		}
		if r.IsHealthy(name) {
			return name, true
		}
	}
	return name, ok
}
