package redisbudget

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestMarkWarnAlertedFiresOncePerWindow proves the core "set exactly once
// per window" contract: the first call for a keyID marks it and reports
// true, every subsequent call against the SAME window reports false. Runs
// against the real Redis container TestMain (in redisbudget_test.go, this
// same package) already starts -- no new setup invented here.
func TestMarkWarnAlertedFiresOncePerWindow(t *testing.T) {
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)

	marked, err := b.MarkWarnAlerted(ctx, key, 0)
	if err != nil {
		t.Fatalf("MarkWarnAlerted() #1 error = %v", err)
	}
	if !marked {
		t.Fatal("MarkWarnAlerted() against a never-warned key = not marked, want marked")
	}

	marked, err = b.MarkWarnAlerted(ctx, key, 0)
	if err != nil {
		t.Fatalf("MarkWarnAlerted() #2 error = %v", err)
	}
	if marked {
		t.Fatal("MarkWarnAlerted() a second time in the same window = marked, want not marked")
	}

	// A third call must still report not-marked -- this is a durable
	// per-window flag, not a one-shot toggle that flips back.
	marked, err = b.MarkWarnAlerted(ctx, key, 0)
	if err != nil {
		t.Fatalf("MarkWarnAlerted() #3 error = %v", err)
	}
	if marked {
		t.Fatal("MarkWarnAlerted() a third time in the same window = marked, want not marked")
	}
}

// TestMarkWarnAlertedKeysTrackIndependently proves one virtual key's
// warn-dedup state never leaks into another's.
func TestMarkWarnAlertedKeysTrackIndependently(t *testing.T) {
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	keyA := uniqueKey(t) + "-a"
	keyB := uniqueKey(t) + "-b"

	if marked, err := b.MarkWarnAlerted(ctx, keyA, 0); err != nil || !marked {
		t.Fatalf("MarkWarnAlerted(%q) = (%v, %v), want (true, nil)", keyA, marked, err)
	}
	if marked, err := b.MarkWarnAlerted(ctx, keyA, 0); err != nil || marked {
		t.Fatalf("MarkWarnAlerted(%q) second call = (%v, %v), want (false, nil)", keyA, marked, err)
	}
	if marked, err := b.MarkWarnAlerted(ctx, keyB, 0); err != nil || !marked {
		t.Fatalf("MarkWarnAlerted(%q) = (%v, %v), want (true, nil) -- %q's dedup state must not leak", keyB, marked, err, keyA)
	}
}

// TestMarkWarnAlertedWindowExpiresViaRedisTTL proves a short
// resetIntervalMs window, once it elapses, lets a fresh call mark the key
// again -- mirrors TestReserveWindowExpiresViaRedisTTL's identical
// TTL-expiry proof for the spend key.
func TestMarkWarnAlertedWindowExpiresViaRedisTTL(t *testing.T) {
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)
	const windowMs = 200

	marked, err := b.MarkWarnAlerted(ctx, key, windowMs)
	if err != nil {
		t.Fatalf("MarkWarnAlerted() #1 error = %v", err)
	}
	if !marked {
		t.Fatal("MarkWarnAlerted() #1 = not marked, want marked")
	}

	marked, err = b.MarkWarnAlerted(ctx, key, windowMs)
	if err != nil {
		t.Fatalf("MarkWarnAlerted() #2 error = %v", err)
	}
	if marked {
		t.Fatal("MarkWarnAlerted() #2 immediately after #1 = marked, want not marked")
	}

	time.Sleep(time.Duration(windowMs)*time.Millisecond*2 + 100*time.Millisecond)

	marked, err = b.MarkWarnAlerted(ctx, key, windowMs)
	if err != nil {
		t.Fatalf("MarkWarnAlerted() #3 (after the window expired) error = %v", err)
	}
	if !marked {
		t.Fatal("MarkWarnAlerted() #3 after the window expired = not marked, want marked -- a fresh window")
	}
}

// TestMarkWarnAlertedNeverCollidesWithAlertBucketNamespace is the direct
// regression proof for warnAlertKey's own doc comment: MarkWarnAlerted and
// MarkAlertBucket live in completely separate Redis key namespaces for the
// SAME keyID, so marking one must never affect the other.
func TestMarkWarnAlertedNeverCollidesWithAlertBucketNamespace(t *testing.T) {
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)

	if _, err := b.MarkAlertBucket(ctx, key, 0.9, 0); err != nil {
		t.Fatalf("MarkAlertBucket() error = %v", err)
	}

	marked, err := b.MarkWarnAlerted(ctx, key, 0)
	if err != nil {
		t.Fatalf("MarkWarnAlerted() error = %v", err)
	}
	if !marked {
		t.Fatal("MarkWarnAlerted() for a keyID whose alert-bucket ladder already fired = not marked, want marked -- the two namespaces must be independent")
	}

	// And the reverse: MarkAlertBucket must still be able to re-fire at a
	// higher bucket for the same keyID, unaffected by MarkWarnAlerted.
	marked2, err := b.MarkAlertBucket(ctx, key, 1.0, 0)
	if err != nil {
		t.Fatalf("MarkAlertBucket() second call error = %v", err)
	}
	if !marked2 {
		t.Fatal("MarkAlertBucket(1.0) after MarkWarnAlerted for the same keyID = not marked, want marked (1.0 exceeds the already-marked 0.9)")
	}
}
