package budget

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// fakeClock returns a func() time.Time that starts at t0 and advances
// exactly when advance moves it forward — mirroring
// internal/ratelimit.NewTokenBucketWithClock's own injectable-clock
// testing convention, so reset-window tests never sleep on wall-clock
// time.
type fakeClock struct {
	t time.Time
}

func newFakeClock(t0 time.Time) *fakeClock { return &fakeClock{t: t0} }

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestResetIntervalZeroNeverResetsEvenAcrossLargeTimeGaps(t *testing.T) {
	tr := NewTracker()
	clock := newFakeClock(time.Now())
	tr.now = clock.now

	tr.Record("team-alpha", d("10"), 0)
	clock.advance(365 * 24 * time.Hour)
	tr.Record("team-alpha", d("5"), 0)

	if got := tr.SpentUSD("team-alpha", 0); !got.Equal(d("15")) {
		t.Errorf("SpentUSD = %s, want 15 (resetInterval=0 must never reset, regardless of elapsed time)", got)
	}
}

func TestFirstAccessStartsWindowWithoutResettingExistingSpend(t *testing.T) {
	store := newFakeStore(map[string]decimal.Decimal{"team-alpha": d("42")})
	tr, err := NewTrackerWithStore(context.Background(), store, nil)
	if err != nil {
		t.Fatalf("NewTrackerWithStore: %v", err)
	}
	clock := newFakeClock(time.Now())
	tr.now = clock.now

	// The very first Allow/SpentUSD call for this key starts its window
	// now -- it must NOT treat "never seen a periodStart before" as "the
	// window already elapsed."
	if got := tr.SpentUSD("team-alpha", time.Hour); !got.Equal(d("42")) {
		t.Errorf("SpentUSD = %s, want 42 (a freshly-loaded key's real spend must survive its first access)", got)
	}
}

func TestSpendResetsAfterWindowElapses(t *testing.T) {
	tr := NewTracker()
	clock := newFakeClock(time.Now())
	tr.now = clock.now
	const window = time.Hour

	tr.Record("team-alpha", d("10"), window) // starts the window
	if tr.Allow("team-alpha", d("9.99"), window) {
		t.Fatal("Allow with a 9.99 cap against 10 spent = true, want false (before any reset, the real spend must still be enforced)")
	}

	clock.advance(window + time.Minute) // past the boundary

	if got := tr.SpentUSD("team-alpha", window); !got.IsZero() {
		t.Errorf("SpentUSD after the window elapsed = %s, want 0 (a real reset, not a stale total)", got)
	}
	if !tr.Allow("team-alpha", d("1"), window) {
		t.Error("Allow after reset = false, want true — a 1 dollar cap should easily cover a freshly-reset 0 spend")
	}
}

func TestSpendDoesNotResetBeforeWindowElapses(t *testing.T) {
	tr := NewTracker()
	clock := newFakeClock(time.Now())
	tr.now = clock.now
	const window = time.Hour

	tr.Record("team-alpha", d("10"), window)
	clock.advance(window - time.Minute) // still inside the window

	if got := tr.SpentUSD("team-alpha", window); !got.Equal(d("10")) {
		t.Errorf("SpentUSD one minute before the window elapses = %s, want 10 (must not reset early)", got)
	}
}

func TestRecordAfterResetAccumulatesOnFreshZeroNotStaleTotal(t *testing.T) {
	tr := NewTracker()
	clock := newFakeClock(time.Now())
	tr.now = clock.now
	const window = time.Hour

	tr.Record("team-alpha", d("100"), window)
	clock.advance(window + time.Second)
	tr.Record("team-alpha", d("3"), window)

	if got := tr.SpentUSD("team-alpha", window); !got.Equal(d("3")) {
		t.Errorf("SpentUSD after a post-reset Record = %s, want 3 (must accumulate on the fresh zero, not the stale 100)", got)
	}
}

func TestResetTriggeredByAllowPersistsZeroToStore(t *testing.T) {
	store := newFakeStore(map[string]decimal.Decimal{"team-alpha": d("50")})
	tr, err := NewTrackerWithStore(context.Background(), store, nil)
	if err != nil {
		t.Fatalf("NewTrackerWithStore: %v", err)
	}
	clock := newFakeClock(time.Now())
	tr.now = clock.now
	const window = time.Hour

	tr.Allow("team-alpha", d("1"), window) // starts the window, no reset yet
	clock.advance(window + time.Second)
	tr.Allow("team-alpha", d("1"), window) // crosses the boundary -- Allow alone, no Record

	if got, ok := store.data["team-alpha"]; !ok || !got.IsZero() {
		t.Errorf("store.data[team-alpha] = %v (present=%v), want 0 -- Allow's own reset must durably persist, not just update the in-memory map", got, ok)
	}
}

func TestResetTriggeredBySpentUSDPersistsZeroToStore(t *testing.T) {
	store := newFakeStore(map[string]decimal.Decimal{"team-alpha": d("50")})
	tr, err := NewTrackerWithStore(context.Background(), store, nil)
	if err != nil {
		t.Fatalf("NewTrackerWithStore: %v", err)
	}
	clock := newFakeClock(time.Now())
	tr.now = clock.now
	const window = time.Hour

	tr.SpentUSD("team-alpha", window) // starts the window
	clock.advance(window + time.Second)
	tr.SpentUSD("team-alpha", window) // crosses the boundary

	if got, ok := store.data["team-alpha"]; !ok || !got.IsZero() {
		t.Errorf("store.data[team-alpha] = %v (present=%v), want 0 -- SpentUSD's own reset must durably persist too", got, ok)
	}
}

func TestKeysWithDifferentResetIntervalsAreIndependent(t *testing.T) {
	tr := NewTracker()
	clock := newFakeClock(time.Now())
	tr.now = clock.now

	tr.Record("team-hourly", d("5"), time.Hour)
	tr.Record("team-daily", d("5"), 24*time.Hour)

	clock.advance(time.Hour + time.Minute) // past team-hourly's window, well inside team-daily's

	if got := tr.SpentUSD("team-hourly", time.Hour); !got.IsZero() {
		t.Errorf("team-hourly SpentUSD = %s, want 0 (its hourly window elapsed)", got)
	}
	if got := tr.SpentUSD("team-daily", 24*time.Hour); !got.Equal(d("5")) {
		t.Errorf("team-daily SpentUSD = %s, want 5 (its daily window has not elapsed yet)", got)
	}
}
