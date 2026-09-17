package router

import "hash/fnv"

// stickyHashBuckets is the resolution of the threshold-hash split
// stickyPick uses — 10,000 gives 0.01% granularity on the canary
// percentage, comfortably finer than any weight ratio an operator would
// realistically configure by hand.
const stickyHashBuckets = 10000

// hashStickyKey hashes key into a value evenly distributed across
// [0, stickyHashBuckets). FNV-1a (hash/fnv, stdlib, zero new dependency)
// — this is a routing-bucket assignment, not security-sensitive material,
// so a fast non-cryptographic hash is the right choice, matching this
// codebase's own internal/cache.Key precedent of using SHA-256 only where
// collision-resistance actually matters and a cheaper hash everywhere
// else it doesn't.
func hashStickyKey(key string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key)) // fnv.Sum64's Write never returns an error.
	return h.Sum64() % stickyHashBuckets
}

// stickyPick chooses a deployment from ms's group for stickyKey,
// splitting the group into its Sticky-flagged ("canary") and
// non-flagged ("stable") members by weight, then bucketing stickyKey
// into one side via a monotonic THRESHOLD hash
// (hashStickyKey(key) < threshold), not an N-way cumulative-range scheme
// re-hashed against the group's current sumW.
//
// This choice is deliberate, not incidental: the whole point of "sticky
// canary" is that ramping the canary's own weight up (via
// Router.SetWeight) must only ever ADD newly-bucketed keys to the canary
// side, never bounce a key that's already landed there back to stable.
// A threshold hash has that monotonicity property for free — raising
// threshold only ever grows the set {key : hash(key) < threshold}, never
// shrinks it. A naive per-deployment cumulative-range scheme re-hashed
// against the CURRENT sumW does not: the modulus itself changes on every
// weight edit, so nearly every key's bucket can flip on any weight
// change, defeating the property this feature exists to provide.
//
// Scoped deliberately to the two-sided stable-vs-canary case, not fully
// general N-way sticky routing — see the doc comment on the WITHIN-side
// selection below for why a THIRD or later sticky-flagged deployment in
// the same group doesn't get its own independent monotonic boundary.
//
// Returns ("", false) if stickyKey's side has no real split to make
// (nobody in the group is Sticky, or everybone is — the same reason
// SelectSticky checks stickyGroups before ever calling this) or if
// ms.sumW is 0 (an empty group; ms.next() already handles that itself).
//
// Does NOT check exclude/health/cost-tier — SelectSticky applies those
// to whatever this returns, falling through to plain selectHealthy on
// any rejection, so a sticky pick can never bypass this package's
// existing health/ramp safety.
func (r *Router) stickyPick(ms *modelState, stickyKey string) (name string, ok bool) {
	if ms.sumW == 0 {
		return "", false
	}

	stickyWeight := 0
	for _, d := range ms.deps {
		if r.stickyDeployments[d.name] {
			stickyWeight += d.weight
		}
	}
	if stickyWeight == 0 || stickyWeight == ms.sumW {
		return "", false
	}

	threshold := uint64(stickyWeight) * stickyHashBuckets / uint64(ms.sumW)
	wantSticky := hashStickyKey(stickyKey) < threshold

	// WITHIN the matching side, still walk the group's own shared WRR
	// cursor (ms.next(), the exact same one plain Select uses) rather
	// than a flat first-match — preserving proportional distribution
	// across however many deployments share that side, exactly mirroring
	// selectHealthy's own "scan up to sumW offers, skip non-matching"
	// shape (health.go). This is also why a group with 3+ Sticky-flagged
	// deployments doesn't get independent monotonic boundaries between
	// them specifically: the threshold hash only ever decides sticky-side
	// membership as a whole, and the within-side WRR walk (like
	// selectHealthy's own health/tier skip) is not itself monotonic under
	// a weight change to one member of that side — an accepted v1 scope
	// limit for the N>2 case, disclosed, not silently glossed over.
	for i := 0; i < ms.sumW; i++ {
		name, ok = ms.next()
		if !ok {
			return "", false
		}
		if r.stickyDeployments[name] == wantSticky {
			return name, true
		}
	}
	return "", false
}

// SelectSticky is Select's sticky-routing sibling: for a model whose
// group has at least one Sticky-flagged deployment (stickyGroups[model]),
// and a non-empty stickyKey, the SAME key deterministically prefers the
// same side (canary vs. stable) of that group on every call — see
// stickyPick's own doc comment for the monotonic-threshold-hash design
// this guarantees.
//
// Select itself is completely untouched by this method's existence —
// every plain Select call (byte-for-byte, including every one of this
// package's own pre-existing pinned WRR tests) is unaffected.
//
// Falls through to plain selectHealthy (the exact same fail-open/health/
// ramp/cost-tier-aware scan Select itself uses) whenever: stickyKey is
// empty, the model's group has no Sticky-flagged deployment at all, OR
// stickyPick's own candidate fails exclude/health/cost-tier — a sticky
// pick can never bypass this package's existing safety checks, it can
// only ever narrow WHICH otherwise-valid candidate gets offered first.
func (r *Router) SelectSticky(model string, exclude map[string]bool, stickyKey string) (string, bool) {
	r.modelsMu.RLock()
	ms, ok := r.models[model]
	r.modelsMu.RUnlock()
	if !ok {
		return "", false
	}

	if stickyKey == "" || !r.stickyGroups[model] {
		return r.selectHealthy(ms, exclude)
	}

	if name, pickOK := r.stickyPick(ms, stickyKey); pickOK {
		if !exclude[name] && r.admitTurn(name) {
			tier, tierFilterActive := r.activeCostTier(ms)
			if !tierFilterActive || r.costTiers[name] == tier {
				return name, true
			}
		}
	}
	return r.selectHealthy(ms, exclude)
}
