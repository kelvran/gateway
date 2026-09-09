package inprocess

import (
	"context"
	"strconv"
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
	_, _, ok, err := c.Get(context.Background(), "never-set")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if ok {
		t.Fatal("Get on never-set key returned ok=true")
	}
}

func TestPutThenGet(t *testing.T) {
	c := New(0)
	ctx := context.Background()
	want := []byte(`{"id":"resp-1"}`)

	if err := c.Put(ctx, "key1", want, time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, _, ok, err := c.Get(ctx, "key1")
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

	if err := c.Put(ctx, "key1", []byte("value"), time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}

	_, writtenAt, ok, err := c.Get(ctx, "key1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if !writtenAt.Equal(clock.t) {
		t.Errorf("writtenAt = %v, want %v (the clock's time at Put)", writtenAt, clock.t)
	}

	clock.Advance(time.Hour)
	if err := c.Put(ctx, "key1", []byte("value2"), time.Hour); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	_, writtenAt, ok, err = c.Get(ctx, "key1")
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

	if err := c.Put(ctx, "key1", []byte("value"), 10*time.Second); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Not yet expired.
	if _, _, ok, _ := c.Get(ctx, "key1"); !ok {
		t.Fatal("Get before TTL expiry returned ok=false")
	}

	clock.Advance(11 * time.Second)

	_, _, ok, err := c.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get after expiry returned error: %v", err)
	}
	if ok {
		t.Fatal("Get after TTL expiry returned ok=true")
	}
}

func TestPutCopiesData(t *testing.T) {
	c := New(0)
	ctx := context.Background()
	data := []byte("original")

	if err := c.Put(ctx, "key1", data, time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}
	data[0] = 'X' // mutate caller's slice after Put

	got, _, ok, err := c.Get(ctx, "key1")
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

	if err := c.Put(ctx, "a", []byte("A"), time.Hour); err != nil {
		t.Fatalf("Put(a): %v", err)
	}
	if err := c.Put(ctx, "b", []byte("B"), time.Hour); err != nil {
		t.Fatalf("Put(b): %v", err)
	}

	// Touch "a" so it becomes more recently used than "b".
	if _, _, ok, _ := c.Get(ctx, "a"); !ok {
		t.Fatal("Get(a) before overflow returned ok=false")
	}

	// A third entry overflows the cap of 2 — "b" (untouched since its
	// Put) must be evicted, not "a" (just touched).
	if err := c.Put(ctx, "c", []byte("C"), time.Hour); err != nil {
		t.Fatalf("Put(c): %v", err)
	}

	if _, _, ok, _ := c.Get(ctx, "a"); !ok {
		t.Error("Get(a) after overflow = false, want true (recently touched, should survive eviction)")
	}
	if _, _, ok, _ := c.Get(ctx, "b"); ok {
		t.Error("Get(b) after overflow = true, want false (least-recently-used, should have been evicted)")
	}
	if _, _, ok, _ := c.Get(ctx, "c"); !ok {
		t.Error("Get(c) after overflow = false, want true (just inserted)")
	}
}

// expiresAtOf is a white-box helper (this file is `package inprocess`,
// not `inprocess_test`) reaching into a Cache's own internal map to read
// back the exact expiresAt a Put computed — the jitter tests below need
// this since Get only ever surfaces writtenAt, never expiresAt, per
// cache.Cache's own contract.
func expiresAtOf(c *Cache, key string) time.Time {
	elem := c.entries[key]
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

	if err := c.Put(ctx, "key1", []byte("value"), time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}

	want := clock.t.Add(time.Minute + time.Duration(0.10*float64(time.Minute)))
	if got := expiresAtOf(c, "key1"); !got.Equal(want) {
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

	if err := c.Put(ctx, "key1", []byte("value"), time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}

	want := clock.t.Add(time.Minute)
	if got := expiresAtOf(c, "key1"); !got.Equal(want) {
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

	if err := c.Put(ctx, "key1", []byte("value"), time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}

	want := clock.t.Add(time.Minute)
	if got := expiresAtOf(c, "key1"); !got.Equal(want) {
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
			if err := c.Put(ctx, strconv.Itoa(i), []byte("v"), time.Hour); err != nil {
				t.Fatalf("Put(%d): %v", i, err)
			}
		}
		if got := c.recency.Len(); got != defaultMaxEntries {
			t.Errorf("New(%d): after inserting %d entries, recency.Len() = %d, want %d (the default cap)", maxEntries, defaultMaxEntries+1, got, defaultMaxEntries)
		}
	}
}
