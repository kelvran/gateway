package cachecorrelation

import (
	"testing"
	"time"
)

var baseTime = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

// TestAnalyzeCountsCrossInstanceAvoidableMiss is the load-bearing proof:
// two different instances, same tenant+key, instance B's miss arrives
// while instance A's own entry (written via its own earlier miss) is
// still within its TTL window — exactly the "duplicate work a shared
// cache would have avoided" phenomenon Finding 5 asks to measure.
func TestAnalyzeCountsCrossInstanceAvoidableMiss(t *testing.T) {
	events := []Event{
		{TenantID: "t1", Key: "k1", InstanceID: "a", Hit: false, Timestamp: baseTime, TTL: 5 * time.Minute},
		{TenantID: "t1", Key: "k1", InstanceID: "b", Hit: false, Timestamp: baseTime.Add(1 * time.Minute), TTL: 5 * time.Minute},
	}
	got := Analyze(events)
	want := Result{TotalMisses: 2, CrossInstanceAvoidable: 1}
	if got != want {
		t.Errorf("Analyze() = %+v, want %+v", got, want)
	}
}

// TestAnalyzeExcludesSameInstanceRepeats proves same-instance repeats are
// never counted as cross-instance-avoidable — that's the existing
// single-process singleflight/local-cache scope, not what this package
// measures.
func TestAnalyzeExcludesSameInstanceRepeats(t *testing.T) {
	events := []Event{
		{TenantID: "t1", Key: "k1", InstanceID: "a", Hit: false, Timestamp: baseTime, TTL: 5 * time.Minute},
		{TenantID: "t1", Key: "k1", InstanceID: "a", Hit: false, Timestamp: baseTime.Add(1 * time.Minute), TTL: 5 * time.Minute},
	}
	got := Analyze(events)
	want := Result{TotalMisses: 2, CrossInstanceAvoidable: 0}
	if got != want {
		t.Errorf("Analyze() = %+v, want %+v", got, want)
	}
}

// TestAnalyzeExcludesExpiredWindow proves a peer event whose TTL has
// already elapsed by the time of the miss does not count — the window
// must genuinely still be open.
func TestAnalyzeExcludesExpiredWindow(t *testing.T) {
	events := []Event{
		{TenantID: "t1", Key: "k1", InstanceID: "a", Hit: false, Timestamp: baseTime, TTL: 1 * time.Minute},
		{TenantID: "t1", Key: "k1", InstanceID: "b", Hit: false, Timestamp: baseTime.Add(2 * time.Minute), TTL: 5 * time.Minute},
	}
	got := Analyze(events)
	want := Result{TotalMisses: 2, CrossInstanceAvoidable: 0}
	if got != want {
		t.Errorf("Analyze() = %+v, want %+v", got, want)
	}
}

// TestAnalyzeExcludesFuturePeerEvent proves causality: a peer event that
// happens AFTER a given miss can never retroactively make THAT miss
// avoidable, even though the same peer event legitimately opens a window
// for a LATER miss of its own.
func TestAnalyzeExcludesFuturePeerEvent(t *testing.T) {
	// "b" misses first (baseTime-1m); "a" misses one minute later
	// (baseTime), while "b"'s window is still open.
	events := []Event{
		{TenantID: "t1", Key: "k1", InstanceID: "b", Hit: false, Timestamp: baseTime.Add(-1 * time.Minute), TTL: 5 * time.Minute},
		{TenantID: "t1", Key: "k1", InstanceID: "a", Hit: false, Timestamp: baseTime, TTL: 5 * time.Minute},
	}
	got := Analyze(events)
	// "a"'s miss legitimately finds "b"'s earlier, still-open event
	// (avoidable). "b"'s own miss has no earlier peer at all — "a"'s
	// event is in "b"'s future, so it must NOT count for "b".
	want := Result{TotalMisses: 2, CrossInstanceAvoidable: 1}
	if got != want {
		t.Errorf("Analyze() = %+v, want %+v", got, want)
	}
}

// TestAnalyzeCrossInstancePeerHitAlsoCounts proves an explicit peer HIT
// (not just a peer miss) also opens a valid window — the common,
// literal reading of Finding 5 ("a repeat of a HIT served on a different
// instance") stays a subset of what this package detects.
func TestAnalyzeCrossInstancePeerHitAlsoCounts(t *testing.T) {
	events := []Event{
		{TenantID: "t1", Key: "k1", InstanceID: "a", Hit: true, Timestamp: baseTime, TTL: 5 * time.Minute},
		{TenantID: "t1", Key: "k1", InstanceID: "b", Hit: false, Timestamp: baseTime.Add(1 * time.Minute), TTL: 5 * time.Minute},
	}
	got := Analyze(events)
	want := Result{TotalMisses: 1, CrossInstanceAvoidable: 1}
	if got != want {
		t.Errorf("Analyze() = %+v, want %+v", got, want)
	}
}

// TestAnalyzeDifferentTenantsNeverCorrelate proves tenant isolation: two
// different tenants sharing the same literal Key string must never
// correlate with each other, mirroring this codebase's own cross-tenant
// cache-isolation discipline (internal/cache.Key's tenantID folding)
// even though this is measurement code, not the cache itself.
func TestAnalyzeDifferentTenantsNeverCorrelate(t *testing.T) {
	events := []Event{
		{TenantID: "tenant-A", Key: "same-key", InstanceID: "a", Hit: false, Timestamp: baseTime, TTL: 5 * time.Minute},
		{TenantID: "tenant-B", Key: "same-key", InstanceID: "b", Hit: false, Timestamp: baseTime.Add(1 * time.Minute), TTL: 5 * time.Minute},
	}
	got := Analyze(events)
	want := Result{TotalMisses: 2, CrossInstanceAvoidable: 0}
	if got != want {
		t.Errorf("Analyze() = %+v, want %+v", got, want)
	}
}

// TestAnalyzeDifferentKeysNeverCorrelate proves two different keys for
// the same tenant never correlate with each other.
func TestAnalyzeDifferentKeysNeverCorrelate(t *testing.T) {
	events := []Event{
		{TenantID: "t1", Key: "k1", InstanceID: "a", Hit: false, Timestamp: baseTime, TTL: 5 * time.Minute},
		{TenantID: "t1", Key: "k2", InstanceID: "b", Hit: false, Timestamp: baseTime.Add(1 * time.Minute), TTL: 5 * time.Minute},
	}
	got := Analyze(events)
	want := Result{TotalMisses: 2, CrossInstanceAvoidable: 0}
	if got != want {
		t.Errorf("Analyze() = %+v, want %+v", got, want)
	}
}

// TestAnalyzeHitsNeverCountTowardTotalMisses proves a Hit event itself
// never contributes to TotalMisses, regardless of any peer.
func TestAnalyzeHitsNeverCountTowardTotalMisses(t *testing.T) {
	events := []Event{
		{TenantID: "t1", Key: "k1", InstanceID: "a", Hit: true, Timestamp: baseTime, TTL: 5 * time.Minute},
		{TenantID: "t1", Key: "k1", InstanceID: "b", Hit: true, Timestamp: baseTime.Add(1 * time.Minute), TTL: 5 * time.Minute},
	}
	got := Analyze(events)
	want := Result{TotalMisses: 0, CrossInstanceAvoidable: 0}
	if got != want {
		t.Errorf("Analyze() = %+v, want %+v", got, want)
	}
}

// TestResultEffectiveHitRateLossZeroWhenNoMisses proves the "no evidence
// either way" zero-value case is distinguishable from "measured, and
// genuinely zero" only by also checking TotalMisses — EffectiveHitRateLoss
// alone reports 0.0 either way, by design (documented on the method).
func TestResultEffectiveHitRateLossZeroWhenNoMisses(t *testing.T) {
	r := Result{TotalMisses: 0, CrossInstanceAvoidable: 0}
	if got := r.EffectiveHitRateLoss(); got != 0 {
		t.Errorf("EffectiveHitRateLoss() = %v, want 0", got)
	}
}

// TestResultEffectiveHitRateLossComputesRealFraction proves the actual
// division for a non-trivial case, using values computed by hand (2/5 =
// 0.4), not merely asserted to be "some fraction."
func TestResultEffectiveHitRateLossComputesRealFraction(t *testing.T) {
	r := Result{TotalMisses: 5, CrossInstanceAvoidable: 2}
	if got := r.EffectiveHitRateLoss(); got != 0.4 {
		t.Errorf("EffectiveHitRateLoss() = %v, want 0.4", got)
	}
}

// TestAnalyzeMultipleQualifyingPeersStillCountsMissOnce proves a miss
// with several qualifying peer events (any of which alone would open a
// window) still only ever increments CrossInstanceAvoidable by 1 for
// that one miss — a binary "was this avoidable," not a peer count.
func TestAnalyzeMultipleQualifyingPeersStillCountsMissOnce(t *testing.T) {
	events := []Event{
		{TenantID: "t1", Key: "k1", InstanceID: "a", Hit: false, Timestamp: baseTime, TTL: 5 * time.Minute},
		{TenantID: "t1", Key: "k1", InstanceID: "b", Hit: false, Timestamp: baseTime.Add(30 * time.Second), TTL: 5 * time.Minute},
		{TenantID: "t1", Key: "k1", InstanceID: "c", Hit: false, Timestamp: baseTime.Add(1 * time.Minute), TTL: 5 * time.Minute},
	}
	got := Analyze(events)
	// "a" has no earlier peer (0). "b" has "a" as an open peer (1). "c"
	// has both "a" and "b" as open peers, but still counts once (1).
	want := Result{TotalMisses: 3, CrossInstanceAvoidable: 2}
	if got != want {
		t.Errorf("Analyze() = %+v, want %+v", got, want)
	}
}
