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

// hashWithinSideKey hashes key into the raw (unmodulused -- the caller
// takes % sideWeight itself, since sideWeight varies by call) value
// stickyPick's within-side pick uses, once the side itself
// (hashStickyKey) has already been decided. A DIFFERENT hash domain
// (an appended suffix, not just a different modulus of the same sum) so
// the side decision and the within-side decision are statistically
// independent even for adversarially-chosen keys.
func hashWithinSideKey(key string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte("|within"))
	return h.Sum64()
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
// ms.sumW is 0 (an empty group).
//
// Does NOT check exclude/health/cost-tier — SelectSticky applies those
// to whatever this returns, falling through to plain selectHealthy on
// any rejection, so a sticky pick can never bypass this package's
// existing health/ramp safety.
//
// **Fixed, a real bug found via a live -race reproduction**: the
// within-side pick used to walk ms's own shared WRR cursor (ms.next(),
// the exact same mutex-protected sequence plain Select/selectHealthy use
// for every OTHER concurrent caller of this model, including the
// fallback/re-pick call sites for THIS SAME model per SelectSticky's own
// doc comment). A bounded "up to sumW calls" scan against that cursor
// only actually visits every distinct group member when it runs as an
// unbroken sequence — true for a single goroutine in isolation, but not
// under real concurrency: another goroutine's own next() call (a plain
// Select, or a DIFFERENT key's stickyPick) can land between any two of
// THIS call's own iterations, consuming a slot in the global sequence
// without this call ever seeing it. A live 50-goroutine burst of
// concurrent Select calls interleaved with SelectSticky calls for known
// canary-hashing keys reproduced this directly: stickyPick's loop
// occasionally exhausted all sumW iterations without ever landing on a
// canary-side name, even though the group unambiguously had one —
// breaking this feature's own core promise ("the SAME key
// deterministically prefers the same side... on every call") under
// exactly the concurrent traffic this feature exists for.
//
// Fixed by making the within-side pick a second, independent pure hash
// (hashWithinSideKey, a different domain from the side-selection hash)
// bucketed via cumulative weight among the chosen side's own members —
// zero shared mutable state consulted at all, so there is no cursor left
// to race on. This is a STRONGER guarantee than the old design ever
// actually delivered even in the race-free case: the specific deployment
// within a side is now itself fully deterministic per key (not merely
// "some member of the correct side, WRR-distributed across calls"),
// still respecting each member's relative weight statistically across
// many distinct keys. Not required to be monotonic under a SetWeight
// change to one member's own weight (only the SIDE split has that
// requirement — see hashStickyKey's own doc comment); a key can
// legitimately move to a different canary among 2+ canaries after a
// weight edit, same as before this fix.
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

	sideWeight := stickyWeight
	if !wantSticky {
		sideWeight = ms.sumW - stickyWeight
	}
	pos := hashWithinSideKey(stickyKey) % uint64(sideWeight)
	var cumulative uint64
	for _, d := range ms.deps {
		if r.stickyDeployments[d.name] != wantSticky {
			continue
		}
		cumulative += uint64(d.weight)
		if pos < cumulative {
			return d.name, true
		}
	}
	// Unreachable: sideWeight is exactly the sum of this side's own
	// weights, and pos < sideWeight by construction (% above), so
	// cumulative must reach/exceed pos before this loop runs out of
	// members.
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

	if name, pickOK := r.stickyPick(ms, stickyKey); pickOK && !exclude[name] {
		// Corrected, a real bug an audit found: admitTurn mutates a
		// ramping deployment's own rampCredit accumulator UNCONDITIONALLY
		// (per its own doc comment — on every call, regardless of
		// whether it returns true or false), so this direct call already
		// consumes name's own "turn" for this logical selection. Falling
		// through to selectHealthy below WITHOUT excluding name let its
		// own scan (which calls admitTurn on every candidate it offers,
		// per selectHealthy's own doc comment) re-offer and re-admission-
		// check this SAME candidate — double-charging (or, across a
		// longer scan, charging even more times) a single real selection
		// decision, letting a just-recovered canary ramp to full traffic
		// admission faster than RecoveryRampSteps/RecoveryRampInitialPercent
		// configure. name must be excluded from selectHealthy's own scan
		// below in EVERY case once this admitTurn call has already run —
		// whether admitTurn admits it (and the tier check then rejects
		// it) or admitTurn itself declines to admit it this turn.
		admitted := r.admitTurn(name)
		if admitted {
			tier, tierFilterActive := r.activeCostTier(ms)
			if !tierFilterActive || r.costTiers[name] == tier {
				return name, true
			}
		}
		exclude = excludeWith(exclude, name)
	}
	return r.selectHealthy(ms, exclude)
}

// excludeWith returns a NEW map containing every entry of exclude (nil-
// safe) plus name — never mutates the caller's own exclude map, which
// SelectSticky's own caller may hold a reference to and expect
// unchanged.
func excludeWith(exclude map[string]bool, name string) map[string]bool {
	next := make(map[string]bool, len(exclude)+1)
	for k, v := range exclude {
		next[k] = v
	}
	next[name] = true
	return next
}
