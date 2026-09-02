package inprocess

import (
	"context"
	"testing"
	"time"

	"github.com/kelvran/gateway/internal/cache"
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
	c := New()
	_, ok, err := c.Get(context.Background(), "never-set")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if ok {
		t.Fatal("Get on never-set key returned ok=true")
	}
}

func TestPutThenGet(t *testing.T) {
	c := New()
	ctx := context.Background()
	want := []byte(`{"id":"resp-1"}`)

	if err := c.Put(ctx, "key1", want, time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok, err := c.Get(ctx, "key1")
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

func TestGetAfterTTLExpiry(t *testing.T) {
	clock := &staticClock{t: time.Now()}
	c := NewWithClock(clock.now)
	ctx := context.Background()

	if err := c.Put(ctx, "key1", []byte("value"), 10*time.Second); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Not yet expired.
	if _, ok, _ := c.Get(ctx, "key1"); !ok {
		t.Fatal("Get before TTL expiry returned ok=false")
	}

	clock.Advance(11 * time.Second)

	_, ok, err := c.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get after expiry returned error: %v", err)
	}
	if ok {
		t.Fatal("Get after TTL expiry returned ok=true")
	}
}

func TestPutCopiesData(t *testing.T) {
	c := New()
	ctx := context.Background()
	data := []byte("original")

	if err := c.Put(ctx, "key1", data, time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}
	data[0] = 'X' // mutate caller's slice after Put

	got, ok, err := c.Get(ctx, "key1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if string(got) != "original" {
		t.Errorf("Put did not copy data: Get returned %q after caller mutation", got)
	}
}
