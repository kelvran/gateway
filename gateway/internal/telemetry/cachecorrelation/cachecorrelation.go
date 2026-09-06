// Package cachecorrelation is the pure, offline-analyzable correlation
// logic for docs/upgrade-research/cache-2026-09-06.md Finding 5 and
// docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md: "instrument
// now, decide later" — before ever building a distributed/shared cache,
// measure whether cross-instance duplicate work is real at Kelvran's
// actual self-hosted replica counts.
//
// This package takes no live dependency on the running gateway process,
// a log pipeline, or any storage backend — it operates entirely on an
// in-memory []Event slice, which a future analysis pass (not built by
// this package) would populate by parsing the structured
// "cache_cross_instance_check" log lines
// gateway/internal/gateway/dataplane emits at every real cache check
// (see dataplane.logCacheCrossInstanceCheck). Deliberately no live
// distributed lookup and no new runtime dependency here, matching the
// RFC's own explicit scope limit.
package cachecorrelation

import "time"

// Event is one cache-check observation from a single gateway instance —
// exactly the (tenant, exact key, instance ID, hit-or-miss, timestamp,
// ttl) tuple docs/upgrade-research/cache-2026-09-06.md Finding 5 names as
// what a retroactive correlation needs. Key is the exact-match cache key
// that identifies the request being checked (L1's own cache.Key, L2's
// own cache.NormalizedKey, or — for an L3 check — the request's own L1
// key, per the RFC's Design section on why L3 reuses it rather than
// having its own exact-key concept). TTL is the checked layer's own
// configured storage TTL at the time of the check, not a per-entry
// remaining-lifetime value — this package has no access to the latter
// from a log line alone.
type Event struct {
	TenantID   string
	Key        string
	InstanceID string
	Hit        bool
	Timestamp  time.Time
	TTL        time.Duration
}

// Result summarizes one Analyze run.
type Result struct {
	// TotalMisses is every Event with Hit == false across the analyzed
	// stream.
	TotalMisses int
	// CrossInstanceAvoidable is the subset of TotalMisses for which some
	// OTHER instance had a still-valid (per its own reported TTL) event
	// — hit or miss — for the exact same (TenantID, Key) at or before
	// this miss's own Timestamp. Per the RFC's Design section: a
	// same-instance's own singleflight/local coalescing already handles
	// concurrent duplicate work today (see
	// gateway/internal/gateway/dataplane/dataplane.go's own missGroup),
	// so only a DIFFERENT instance's event counts here — this metric is
	// specifically about the work a shared, cross-instance cache would
	// have avoided that today's single-instance coalescing cannot.
	CrossInstanceAvoidable int
}

// EffectiveHitRateLoss returns the fraction of misses that were
// cross-instance-avoidable, or 0 when TotalMisses is 0 — the latter is
// "no evidence either way," never a claim that loss is genuinely zero.
func (r Result) EffectiveHitRateLoss() float64 {
	if r.TotalMisses == 0 {
		return 0
	}
	return float64(r.CrossInstanceAvoidable) / float64(r.TotalMisses)
}

// groupKey identifies one (tenant, key) correlation group. Grouping by
// Key alone would risk two different tenants' otherwise-identical keys
// colliding; in practice this can never happen for real events emitted
// by dataplane (every Key already has tenantID folded into its own hash
// input — see internal/cache.Key/NormalizedKey), but Analyze keeps the
// tenant boundary explicit anyway rather than depending on that upstream
// guarantee silently — the same cross-tenant-isolation discipline
// AGENTS.md requires of the cache itself applies here, even though this
// is measurement code, not the cache.
type groupKey struct {
	tenantID string
	key      string
}

// Analyze scans events (any order, any mix of tenants/keys/instances)
// and computes Result. Real events, once a multi-instance deployment
// exists, are unbounded in volume; this implementation is O(n^2) within
// each (tenant, key) group, which is deliberately acceptable for a
// retrospective, offline analysis tool over the realistically small
// number of repeats any single exact key sees within one TTL window —
// not a hot-path or live-serving concern.
func Analyze(events []Event) Result {
	groups := make(map[groupKey][]Event)
	for _, e := range events {
		gk := groupKey{tenantID: e.TenantID, key: e.Key}
		groups[gk] = append(groups[gk], e)
	}

	var result Result
	for _, group := range groups {
		for _, e := range group {
			if e.Hit {
				continue
			}
			result.TotalMisses++
			if hasOpenCrossInstanceWindow(group, e) {
				result.CrossInstanceAvoidable++
			}
		}
	}
	return result
}

// hasOpenCrossInstanceWindow reports whether group (all events sharing
// miss's own TenantID+Key) contains an event from a DIFFERENT instance
// whose own [Timestamp, Timestamp+TTL] window was still open at miss's
// Timestamp.
//
// Deliberately checks every peer event (hit AND miss), not only peer
// HIT events, even though Finding 5's own one-line framing says "a
// repeat of a hit served on a different instance": this pipeline is
// write-through (gateway/internal/gateway/dataplane/dataplane.go's
// writeCache runs immediately after every genuine miss, before the
// response is returned), so a peer's own first-ever MISS already means
// that instance's cache holds the value from approximately that peer
// event's own Timestamp until Timestamp+TTL — restricting this to only
// peer HIT events would undercount the dominant real case (two
// near-simultaneous first requests for the same content landing on two
// different instances, neither of which has yet produced a second,
// hit-classified request of its own). See the RFC's Alternatives
// considered for the narrower, hits-only reading and why it was
// rejected.
func hasOpenCrossInstanceWindow(group []Event, miss Event) bool {
	for _, peer := range group {
		if peer.InstanceID == miss.InstanceID {
			continue
		}
		if peer.Timestamp.After(miss.Timestamp) {
			// A peer event strictly after this miss cannot have
			// retroactively made it avoidable — causality only runs
			// forward.
			continue
		}
		windowEnd := peer.Timestamp.Add(peer.TTL)
		if miss.Timestamp.After(windowEnd) {
			continue
		}
		return true
	}
	return false
}
