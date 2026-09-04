package router

import "sync"

// weightedDeployment is one Deployment's routing-selection state within
// its model group: a plain name plus its normalized (>=1) weight.
type weightedDeployment struct {
	name   string
	weight int
}

// modelState is one canonical model's smooth-weighted-round-robin
// selection cursor — the LVS/IPVS wrr.c algorithm (real, existing
// production code, not invented here; see
// docs/rfcs/2026-09-04-weighted-routing.md's Detailed Design for the
// hand-traced degrade-to-plain-round-robin proof this exact shape
// depends on). Guarded by mu, mirroring
// gateway/internal/ratelimit.TokenBucket's "mutex + plain numeric
// fields, no goroutine" shape — every decision is computed inline at
// selection time, never on a background timer.
type modelState struct {
	mu sync.Mutex

	deps []weightedDeployment // preserves the exact input order — NEVER re-sorted; see the RFC's degrade proof
	i    int                  // last-selected index; -1 before the first call
	cw   int                  // current weight counter
	gcd  int                  // gcd of every deployment's weight in this group
	maxW int                  // max weight in this group
}

// newModelState builds a modelState from deps (in the caller's exact
// order), normalizing any zero weight to 1 (the "unset in config"
// default) before computing gcd/max.
func newModelState(deps []weightedDeployment) *modelState {
	normalized := make([]weightedDeployment, len(deps))
	copy(normalized, deps)
	for i := range normalized {
		if normalized[i].weight <= 0 {
			normalized[i].weight = 1
		}
	}

	g := normalized[0].weight
	maxW := normalized[0].weight
	for _, d := range normalized[1:] {
		g = gcd(g, d.weight)
		if d.weight > maxW {
			maxW = d.weight
		}
	}

	return &modelState{deps: normalized, i: -1, gcd: g, maxW: maxW}
}

// next returns the next selected deployment name. Returns ("", false)
// only if this modelState was built with zero deployments — New never
// constructs one that way, so this is a defensive, not a reachable,
// branch in practice.
func (m *modelState) next() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := len(m.deps)
	if n == 0 {
		return "", false
	}
	for {
		m.i = (m.i + 1) % n
		if m.i == 0 {
			m.cw -= m.gcd
			if m.cw <= 0 {
				m.cw = m.maxW
			}
		}
		if m.deps[m.i].weight >= m.cw {
			return m.deps[m.i].name, true
		}
	}
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
