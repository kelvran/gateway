package inprocess

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/cache"
)

// staticClock advances only when Advance is called — this is what lets the
// TTL-expiry test run in microseconds instead of sleeping on wall-clock
// time, per docs/testing/TESTING.md §1.
type staticClock struct {
	t time.Time
}

func (c *staticClock) now() time.Time          { return c.t }
func (c *staticClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// compile-time check that Cache satisfies cache.Cache.
var _ cache.Cache = (*Cache)(nil)

func TestGetMiss(t *testing.T) {
	c := New(0)
	_, _, ok, err := c.Get(context.Background(), "test-tenant", "never-set")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if ok {
		t.Fatal("Get on never-set key returned ok=true")
	}
}

// TestDeleteRemovesEntry proves Delete makes a subsequent Get miss.
// Regression test for
// docs/upgrade-research/ai-compliance-regulatory-readiness-2026-09-14.md
// Finding 4 (no way to service a GDPR erasure request against one
// cached entry before this method existed).
func TestDeleteRemovesEntry(t *testing.T) {
	c := New(0)
	ctx := context.Background()

	if err := c.Put(ctx, "test-tenant", "key1", []byte("payload"), time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Delete(ctx, "test-tenant", "key1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, ok, err := c.Get(ctx, "test-tenant", "key1"); err != nil {
		t.Fatalf("Get after Delete: %v", err)
	} else if ok {
		t.Fatal("Get after Delete returned ok=true, want a miss")
	}
}

// TestDeleteNeverSetKeyIsNoOp proves deleting a key that was never
// written is a no-op, not an error — the caller cares only that the key
// is absent afterward.
func TestDeleteNeverSetKeyIsNoOp(t *testing.T) {
	c := New(0)
	if err := c.Delete(context.Background(), "test-tenant", "never-set"); err != nil {
		t.Fatalf("Delete on never-set key returned error: %v", err)
	}
}

// TestDeleteDoesNotAffectOtherEntries proves Delete removes only the
// named key, leaving every other entry (and the recency list they live
// in) intact.
func TestDeleteDoesNotAffectOtherEntries(t *testing.T) {
	c := New(0)
	ctx := context.Background()

	if err := c.Put(ctx, "test-tenant", "keep", []byte("keep-me"), time.Minute); err != nil {
		t.Fatalf("Put keep: %v", err)
	}
	if err := c.Put(ctx, "test-tenant", "remove", []byte("remove-me"), time.Minute); err != nil {
		t.Fatalf("Put remove: %v", err)
	}
	if err := c.Delete(ctx, "test-tenant", "remove"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, _, ok, err := c.Get(ctx, "test-tenant", "keep")
	if err != nil {
		t.Fatalf("Get keep: %v", err)
	}
	if !ok || string(got) != "keep-me" {
		t.Errorf("Get keep = (%q, ok=%v), want (%q, ok=true)", got, ok, "keep-me")
	}
}

func TestPutThenGet(t *testing.T) {
	c := New(0)
	ctx := context.Background()
	want := []byte(`{"id":"resp-1"}`)

	if err := c.Put(ctx, "test-tenant", "key1", want, time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, _, ok, err := c.Get(ctx, "test-tenant", "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get after Put returned ok=false")
	}
	if string(got) != string(want) {
		t.Errorf("Get returned %q, want %q", got, want)
	}
}

// TestGetReturnsWriteTimeAsWrittenAt proves Get surfaces the most recent
// Put's write time, per docs/upgrade-research/cache-2026-09-06.md
// Finding 6 — closing the telemetry asymmetry with L3's own
// LexicalCandidate.WrittenAt.
func TestGetReturnsWriteTimeAsWrittenAt(t *testing.T) {
	clock := &staticClock{t: time.Now()}
	c := NewWithClock(0, clock.now)
	ctx := context.Background()

	if err := c.Put(ctx, "test-tenant", "key1", []byte("value"), time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}

	_, writtenAt, ok, err := c.Get(ctx, "test-tenant", "key1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if !writtenAt.Equal(clock.t) {
		t.Errorf("writtenAt = %v, want %v (the clock's time at Put)", writtenAt, clock.t)
	}

	clock.Advance(time.Hour)
	if err := c.Put(ctx, "test-tenant", "key1", []byte("value2"), time.Hour); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	_, writtenAt, ok, err = c.Get(ctx, "test-tenant", "key1")
	if err != nil || !ok {
		t.Fatalf("Get after second Put: ok=%v err=%v", ok, err)
	}
	if !writtenAt.Equal(clock.t) {
		t.Errorf("writtenAt after overwrite = %v, want %v (the second Put's time, not the first)", writtenAt, clock.t)
	}
}

func TestGetAfterTTLExpiry(t *testing.T) {
	clock := &staticClock{t: time.Now()}
	c := NewWithClock(0, clock.now)
	ctx := context.Background()

	if err := c.Put(ctx, "test-tenant", "key1", []byte("value"), 10*time.Second); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Not yet expired.
	if _, _, ok, _ := c.Get(ctx, "test-tenant", "key1"); !ok {
		t.Fatal("Get before TTL expiry returned ok=false")
	}

	clock.Advance(11 * time.Second)

	_, _, ok, err := c.Get(ctx, "test-tenant", "key1")
	if err != nil {
		t.Fatalf("Get after expiry returned error: %v", err)
	}
	if ok {
		t.Fatal("Get after TTL expiry returned ok=true")
	}
}

// TestGetAtExactExpiryInstantStillHits pins down the exact boundary
// TestGetAfterTTLExpiry above never isolates on its own (that test
// advances a full second PAST expiry, not to the exact instant): Get's
// own check is c.now().After(entry.expiresAt), a strict ">", so
// now == expiresAt must still be a hit, matching this documented
// behavior explicitly rather than leaving the exact boundary untested.
func TestGetAtExactExpiryInstantStillHits(t *testing.T) {
	clock := &staticClock{t: time.Now()}
	c := NewWithClock(0, clock.now)
	ctx := context.Background()

	const ttl = 10 * time.Second
	if err := c.Put(ctx, "test-tenant", "key1", []byte("value"), ttl); err != nil {
		t.Fatalf("Put: %v", err)
	}

	clock.Advance(ttl) // now == expiresAt exactly, not one instant past it.

	_, _, ok, err := c.Get(ctx, "test-tenant", "key1")
	if err != nil {
		t.Fatalf("Get at the exact expiry instant returned error: %v", err)
	}
	if !ok {
		t.Fatal("Get at the exact expiry instant (now == expiresAt) returned ok=false, want true — expiry is a strict After, not >=")
	}
}

func TestPutCopiesData(t *testing.T) {
	c := New(0)
	ctx := context.Background()
	data := []byte("original")

	if err := c.Put(ctx, "test-tenant", "key1", data, time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}
	data[0] = 'X' // mutate caller's slice after Put

	got, _, ok, err := c.Get(ctx, "test-tenant", "key1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if string(got) != "original" {
		t.Errorf("Put did not copy data: Get returned %q after caller mutation", got)
	}
}

// TestEvictionRemovesLeastRecentlyUsed is the load-bearing proof for
// docs/rfcs/2026-09-03-cache-l2-normalized-match.md's capacity-bound
// prerequisite: eviction on overflow must remove the least-recently
// *fetched* entry, not merely the least-recently *written* one — so a
// Get-touched old entry survives while a genuinely-idle one is evicted.
func TestEvictionRemovesLeastRecentlyUsed(t *testing.T) {
	c := New(2)
	ctx := context.Background()

	if err := c.Put(ctx, "test-tenant", "a", []byte("A"), time.Hour); err != nil {
		t.Fatalf("Put(a): %v", err)
	}
	if err := c.Put(ctx, "test-tenant", "b", []byte("B"), time.Hour); err != nil {
		t.Fatalf("Put(b): %v", err)
	}

	// Touch "a" so it becomes more recently used than "b".
	if _, _, ok, _ := c.Get(ctx, "test-tenant", "a"); !ok {
		t.Fatal("Get(a) before overflow returned ok=false")
	}

	// A third entry overflows the cap of 2 — "b" (untouched since its
	// Put) must be evicted, not "a" (just touched).
	if err := c.Put(ctx, "test-tenant", "c", []byte("C"), time.Hour); err != nil {
		t.Fatalf("Put(c): %v", err)
	}

	if _, _, ok, _ := c.Get(ctx, "test-tenant", "a"); !ok {
		t.Error("Get(a) after overflow = false, want true (recently touched, should survive eviction)")
	}
	if _, _, ok, _ := c.Get(ctx, "test-tenant", "b"); ok {
		t.Error("Get(b) after overflow = true, want false (least-recently-used, should have been evicted)")
	}
	if _, _, ok, _ := c.Get(ctx, "test-tenant", "c"); !ok {
		t.Error("Get(c) after overflow = false, want true (just inserted)")
	}
}

// TestEvictionWithMaxEntriesOfOneEvictsImmediately is
// TestEvictionRemovesLeastRecentlyUsed's own degenerate-boundary sibling:
// a cap of exactly 1 (never previously tested — every other test uses a
// cap of 2+) must still evict correctly on a genuinely different second
// key, while a same-key overwrite under that same cap of 1 must never
// evict the key it's overwriting.
func TestEvictionWithMaxEntriesOfOneEvictsImmediately(t *testing.T) {
	c := New(1)
	ctx := context.Background()

	if err := c.Put(ctx, "test-tenant", "a", []byte("A"), time.Hour); err != nil {
		t.Fatalf("Put(a): %v", err)
	}
	if err := c.Put(ctx, "test-tenant", "b", []byte("B"), time.Hour); err != nil {
		t.Fatalf("Put(b): %v", err)
	}

	if _, _, ok, _ := c.Get(ctx, "test-tenant", "a"); ok {
		t.Error("Get(a) after a second Put with maxEntries=1 = true, want false (evicted)")
	}
	if _, _, ok, _ := c.Get(ctx, "test-tenant", "b"); !ok {
		t.Error("Get(b) after a second Put with maxEntries=1 = false, want true (just inserted)")
	}

	// Overwriting "b" with a new value, still under the same cap of 1,
	// must never evict "b" itself -- Put's own "update existing" branch
	// (elem, found := c.entries[key]; found) never touches c.recency's
	// length at all.
	if err := c.Put(ctx, "test-tenant", "b", []byte("B2"), time.Hour); err != nil {
		t.Fatalf("Put(b, overwrite): %v", err)
	}
	got, _, ok, err := c.Get(ctx, "test-tenant", "b")
	if err != nil || !ok {
		t.Fatalf("Get(b) after a same-key overwrite under maxEntries=1: ok=%v err=%v", ok, err)
	}
	if string(got) != "B2" {
		t.Errorf("Get(b) = %q after overwrite, want %q", got, "B2")
	}
}

// expiresAtOf is a white-box helper (this file is `package inprocess`,
// not `inprocess_test`) reaching into a Cache's own internal map to read
// back the exact expiresAt a Put computed — the jitter tests below need
// this since Get only ever surfaces writtenAt, never expiresAt, per
// cache.Cache's own contract.
func expiresAtOf(c *Cache, tenantID, key string) time.Time {
	elem := c.tenants[tenantID].entries[key]
	return elem.Value.(*cacheEntry).expiresAt
}

// TestPutAppliesJitterWithinConfiguredFraction proves the real jitter
// formula: with rand pinned to its maximum (1.0) and jitterFraction=0.10,
// expiresAt must be exactly writtenAt + ttl*1.10 — the additive-only
// upper bound, per docs/rfcs/2026-09-10-gateway-cache-ttl-jitter.md.
func TestPutAppliesJitterWithinConfiguredFraction(t *testing.T) {
	clock := &staticClock{t: time.Now()}
	c := NewWithClockAndJitter(0, clock.now, 0.10, func() float64 { return 1.0 })
	ctx := context.Background()

	if err := c.Put(ctx, "test-tenant", "key1", []byte("value"), time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}

	want := clock.t.Add(time.Minute + time.Duration(0.10*float64(time.Minute)))
	if got := expiresAtOf(c, "test-tenant", "key1"); !got.Equal(want) {
		t.Errorf("expiresAt = %v, want %v (writtenAt + ttl*1.10, max jitter)", got, want)
	}
}

// TestPutZeroRandProducesNoJitter proves the un-jittered floor: even with
// jitterFraction configured non-zero, a rand() of exactly 0.0 must leave
// expiresAt at exactly writtenAt+ttl, never less (jitter is additive-only,
// per Put's own doc comment).
func TestPutZeroRandProducesNoJitter(t *testing.T) {
	clock := &staticClock{t: time.Now()}
	c := NewWithClockAndJitter(0, clock.now, 0.10, func() float64 { return 0.0 })
	ctx := context.Background()

	if err := c.Put(ctx, "test-tenant", "key1", []byte("value"), time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}

	want := clock.t.Add(time.Minute)
	if got := expiresAtOf(c, "test-tenant", "key1"); !got.Equal(want) {
		t.Errorf("expiresAt = %v, want %v (writtenAt + ttl, zero jitter)", got, want)
	}
}

// TestNewWithClockStillHasZeroJitter is a regression guard: NewWithClock
// itself (the pre-existing, documented "deterministic TTL testing"
// constructor every other test in this file relies on) must still
// produce byte-exact writtenAt+ttl expiry with no jitter at all, even
// after New's own jitter-by-default change.
func TestNewWithClockStillHasZeroJitter(t *testing.T) {
	clock := &staticClock{t: time.Now()}
	c := NewWithClock(0, clock.now)
	ctx := context.Background()

	if err := c.Put(ctx, "test-tenant", "key1", []byte("value"), time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}

	want := clock.t.Add(time.Minute)
	if got := expiresAtOf(c, "test-tenant", "key1"); !got.Equal(want) {
		t.Errorf("expiresAt = %v, want %v (NewWithClock must stay jitter-free)", got, want)
	}
}

// TestZeroOrNegativeMaxEntriesDefaultsToDefaultMaxEntries proves New(0)/
// New(negative) never means "unbounded" — there is deliberately no such
// mode, per this package's own doc comment.
func TestZeroOrNegativeMaxEntriesDefaultsToDefaultMaxEntries(t *testing.T) {
	for _, maxEntries := range []int{0, -1, -100} {
		c := New(maxEntries)
		ctx := context.Background()

		for i := 0; i < defaultMaxEntries+1; i++ {
			if err := c.Put(ctx, "test-tenant", strconv.Itoa(i), []byte("v"), time.Hour); err != nil {
				t.Fatalf("Put(%d): %v", i, err)
			}
		}
		if got := c.tenants["test-tenant"].recency.Len(); got != defaultMaxEntries {
			t.Errorf("New(%d): after inserting %d entries, recency.Len() = %d, want %d (the default cap)", maxEntries, defaultMaxEntries+1, got, defaultMaxEntries)
		}
	}
}

// TestEvictionIsPerTenant is the regression proof for a real, HIGH-
// severity end-to-end audit finding (2026-09-20): before this fix, L1/L2
// shared ONE global map + ONE global LRU list across every tenant, with
// zero per-tenant eviction boundary — tenant separation existed only at
// the hash-key level (cache.Key/NormalizedKey fold tenantID into the
// hash), never at the eviction level. A single tenant sending enough
// distinct cacheable requests to overflow the shared cap could silently
// evict every OTHER tenant's warm entries — a real noisy-neighbor/
// DoS-adjacent gap, inconsistent with LexicalCache (L3)'s own
// pre-existing per-tenant partitioning (see
// TestLexicalEvictionIsPerTenant, this fix's own direct mirror).
func TestEvictionIsPerTenant(t *testing.T) {
	c := New(1) // cap of 1 entry PER TENANT
	ctx := context.Background()

	if err := c.Put(ctx, "team-alpha", "key1", []byte("alpha-1"), time.Hour); err != nil {
		t.Fatalf("Put(team-alpha, key1): %v", err)
	}
	if err := c.Put(ctx, "team-beta", "key1", []byte("beta-1"), time.Hour); err != nil {
		t.Fatalf("Put(team-beta, key1): %v", err)
	}
	// Overflow team-alpha's own cap of 1 — must evict team-alpha's entry
	// only, leaving team-beta's untouched.
	if err := c.Put(ctx, "team-alpha", "key2", []byte("alpha-2"), time.Hour); err != nil {
		t.Fatalf("Put(team-alpha, key2): %v", err)
	}

	got, _, ok, err := c.Get(ctx, "team-beta", "key1")
	if err != nil {
		t.Fatalf("Get(team-beta, key1): %v", err)
	}
	if !ok || string(got) != "beta-1" {
		t.Errorf("team-beta's entry was evicted by team-alpha's overflow — eviction is not per-tenant: ok=%v got=%q", ok, got)
	}
}

// TestGetNeverCrossesTenantBoundary proves the mirror-image property:
// two tenants writing the exact same key never see each other's value —
// tenant isolation holds even setting the pre-existing hash-key-folding
// mechanism aside, at the Cache-internal storage level this fix added.
func TestGetNeverCrossesTenantBoundary(t *testing.T) {
	c := New(0)
	ctx := context.Background()

	if err := c.Put(ctx, "team-alpha", "shared-key", []byte("alpha-value"), time.Hour); err != nil {
		t.Fatalf("Put(team-alpha): %v", err)
	}

	if _, _, ok, err := c.Get(ctx, "team-beta", "shared-key"); err != nil {
		t.Fatalf("Get(team-beta, shared-key): %v", err)
	} else if ok {
		t.Error("Get(team-beta, shared-key) = ok=true, want a miss — team-beta never wrote this key, team-alpha's own write must not be visible to it")
	}
}

// TestConcurrentPutAndDeleteSameKeyLeavesConsistentState is the
// load-bearing concurrency proof for this package's own mutex: N
// goroutines repeatedly racing Put and Delete against the SAME key must
// never panic (the real signal here is `go test -race` itself finding
// no data race) and must always leave c.entries/c.recency in a mutually
// consistent state -- a key present in one must be present in the other,
// and vice versa, no matter which operation "won" the race.
func TestConcurrentPutAndDeleteSameKeyLeavesConsistentState(t *testing.T) {
	c := New(0)
	ctx := context.Background()
	const key = "racing-key"
	const rounds = 500

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_ = c.Put(ctx, "test-tenant", key, []byte("v"), time.Minute)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_ = c.Delete(ctx, "test-tenant", key)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_, _, _, _ = c.Get(ctx, "test-tenant", key)
		}
	}()
	wg.Wait()

	// Direct, locked inspection of internal state -- c.mu is this
	// package's own real lock, not a test-only shortcut. Put/Delete
	// always add to or remove from a bucket's entries and recency
	// TOGETHER (removeLocked's own doc comment: "deletes elem from both
	// bucket's map and its own recency list") -- their lengths must
	// always match exactly. The bucket is guaranteed to exist by now: at
	// least one of the 500 concurrent Puts above has created it.
	c.mu.Lock()
	bucket := c.tenants["test-tenant"]
	entriesLen, recencyLen := len(bucket.entries), bucket.recency.Len()
	c.mu.Unlock()
	if entriesLen != recencyLen {
		t.Fatalf("len(bucket.entries) = %d, bucket.recency.Len() = %d -- inconsistent after concurrent Put/Delete/Get", entriesLen, recencyLen)
	}
}
