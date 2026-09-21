package budget

import (
	"context"
	"testing"
	"time"
)

// TestCheckAndMarkBudgetAlertBucketBelowLowestBucketDoesNotFire proves the
// negative case: spend that hasn't reached even the lowest bucket (50%)
// must never report a crossing.
func TestCheckAndMarkBudgetAlertBucketBelowLowestBucketDoesNotFire(t *testing.T) {
	tr := NewTracker()
	if _, crossed := tr.CheckAndMarkBudgetAlertBucket(context.Background(), "team-alpha", 0.2, 0); crossed {
		t.Error("CheckAndMarkBudgetAlertBucket at 20%% used = crossed, want no crossing")
	}
}

// TestCheckAndMarkBudgetAlertBucketFiresEachBucketExactlyOnce proves each
// of the four buckets fires exactly once as spend climbs through them, and
// never re-fires for a percentUsed that stays within an already-alerted
// bucket.
func TestCheckAndMarkBudgetAlertBucketFiresEachBucketExactlyOnce(t *testing.T) {
	tr := NewTracker()

	bucket, crossed := tr.CheckAndMarkBudgetAlertBucket(context.Background(), "team-alpha", 0.5, 0)
	if !crossed || bucket != 0.5 {
		t.Fatalf("first crossing at 50%% used: got (bucket=%v, crossed=%v), want (0.5, true)", bucket, crossed)
	}

	// Staying within the 50% bucket (e.g. a second request at 60% used,
	// still below the next 75% rung) must not re-fire.
	if _, crossed := tr.CheckAndMarkBudgetAlertBucket(context.Background(), "team-alpha", 0.6, 0); crossed {
		t.Error("CheckAndMarkBudgetAlertBucket at 60%% used, after already alerting 50%%, = crossed, want no crossing")
	}

	bucket, crossed = tr.CheckAndMarkBudgetAlertBucket(context.Background(), "team-alpha", 0.75, 0)
	if !crossed || bucket != 0.75 {
		t.Fatalf("crossing at 75%% used: got (bucket=%v, crossed=%v), want (0.75, true)", bucket, crossed)
	}

	bucket, crossed = tr.CheckAndMarkBudgetAlertBucket(context.Background(), "team-alpha", 0.9, 0)
	if !crossed || bucket != 0.9 {
		t.Fatalf("crossing at 90%% used: got (bucket=%v, crossed=%v), want (0.9, true)", bucket, crossed)
	}

	bucket, crossed = tr.CheckAndMarkBudgetAlertBucket(context.Background(), "team-alpha", 1.0, 0)
	if !crossed || bucket != 1.0 {
		t.Fatalf("crossing at 100%% used: got (bucket=%v, crossed=%v), want (1.0, true)", bucket, crossed)
	}

	// Nothing left to cross past 100%.
	if _, crossed := tr.CheckAndMarkBudgetAlertBucket(context.Background(), "team-alpha", 1.0, 0); crossed {
		t.Error("CheckAndMarkBudgetAlertBucket at 100%% used a second time = crossed, want no crossing")
	}
}

// TestCheckAndMarkBudgetAlertBucketOneLargeJumpFiresOnlyHighestBucket
// proves a single request that jumps spend straight past several buckets
// at once reports only the highest newly-crossed bucket, not every rung it
// skipped over — this is a "how close are we now" signal, not an audit
// trail of every threshold a request happened to leap past.
func TestCheckAndMarkBudgetAlertBucketOneLargeJumpFiresOnlyHighestBucket(t *testing.T) {
	tr := NewTracker()
	bucket, crossed := tr.CheckAndMarkBudgetAlertBucket(context.Background(), "team-alpha", 0.95, 0)
	if !crossed || bucket != 0.9 {
		t.Fatalf("jump straight to 95%% used: got (bucket=%v, crossed=%v), want (0.9, true)", bucket, crossed)
	}
}

// TestCheckAndMarkBudgetAlertBucketKeysTrackIndependently proves one key's
// alert state never leaks into another's.
func TestCheckAndMarkBudgetAlertBucketKeysTrackIndependently(t *testing.T) {
	tr := NewTracker()
	if _, crossed := tr.CheckAndMarkBudgetAlertBucket(context.Background(), "team-alpha", 0.9, 0); !crossed {
		t.Fatal("team-alpha at 90%% used = no crossing, want crossing")
	}
	bucket, crossed := tr.CheckAndMarkBudgetAlertBucket(context.Background(), "team-beta", 0.5, 0)
	if !crossed || bucket != 0.5 {
		t.Fatalf("team-beta at 50%% used: got (bucket=%v, crossed=%v), want (0.5, true) -- team-alpha's alert state must not leak", bucket, crossed)
	}
}

// TestCheckAndMarkBudgetAlertBucketRefiresAfterWindowReset proves a key
// correctly re-alerts every bucket again once its rolling window resets --
// the entire reason dedup state is keyed by periodEpoch, not just keyID.
func TestCheckAndMarkBudgetAlertBucketRefiresAfterWindowReset(t *testing.T) {
	tr := NewTracker()
	clock := newFakeClock(time.Now())
	tr.now = clock.now
	const window = time.Hour

	// Establish the window and alert the top bucket.
	tr.Record("team-alpha", d("1"), window)
	if _, crossed := tr.CheckAndMarkBudgetAlertBucket(context.Background(), "team-alpha", 1.0, 0); !crossed {
		t.Fatal("first-window crossing at 100%% used = no crossing, want crossing")
	}
	if _, crossed := tr.CheckAndMarkBudgetAlertBucket(context.Background(), "team-alpha", 1.0, 0); crossed {
		t.Fatal("repeat check within the same window at 100%% used = crossed, want no crossing (already alerted)")
	}

	// Roll the window over.
	clock.advance(2 * window)
	tr.Record("team-alpha", d("0.01"), window) // triggers resetIfNeeded, bumping periodEpoch

	bucket, crossed := tr.CheckAndMarkBudgetAlertBucket(context.Background(), "team-alpha", 0.5, 0)
	if !crossed || bucket != 0.5 {
		t.Fatalf("post-reset crossing at 50%% used: got (bucket=%v, crossed=%v), want (0.5, true) -- a new window must re-alert from scratch", bucket, crossed)
	}
}
