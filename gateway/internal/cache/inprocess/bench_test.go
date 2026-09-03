package inprocess

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// These benchmarks are the cache's performance-regression baseline
// referenced by docs/testing/TESTING.md §7 ("Track p50/p95/p99 latency
// ... as trend lines, not just pass/fail thresholds"). They are not a
// pass/fail gate on their own — go test -bench has no built-in
// threshold — but a recorded ns/op baseline any future change to the L1
// cache's locking or copy behavior can be compared against.

// BenchmarkPut measures the cost of storing one entry, including the
// mandatory defensive copy Put makes of the input bytes (see inprocess.go's
// doc comment on why Cache.Get/Put never hand back a pointer into shared
// memory).
func BenchmarkPut(b *testing.B) {
	c := New(0)
	ctx := context.Background()
	value := []byte(`{"id":"chatcmpl-bench","choices":[{"message":{"role":"assistant","content":"hello"}}]}`)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		key := "bench-key-" + strconv.Itoa(i%1000)
		if err := c.Put(ctx, key, value, time.Minute); err != nil {
			b.Fatalf("Put: %v", err)
		}
	}
}

// BenchmarkGetHit measures the cost of a cache hit, including the
// defensive copy Get makes on the way out.
func BenchmarkGetHit(b *testing.B) {
	c := New(0)
	ctx := context.Background()
	value := []byte(`{"id":"chatcmpl-bench","choices":[{"message":{"role":"assistant","content":"hello"}}]}`)

	const numKeys = 1000
	for i := 0; i < numKeys; i++ {
		if err := c.Put(ctx, "bench-key-"+strconv.Itoa(i), value, time.Hour); err != nil {
			b.Fatalf("Put: %v", err)
		}
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		key := "bench-key-" + strconv.Itoa(i%numKeys)
		if _, ok, err := c.Get(ctx, key); err != nil || !ok {
			b.Fatalf("Get(%q): ok=%v err=%v", key, ok, err)
		}
	}
}

// BenchmarkGetMiss measures the cost of a cache miss (the common case on
// an empty/cold cache, and the path every never-before-seen request takes
// before falling through to the upstream call).
func BenchmarkGetMiss(b *testing.B) {
	c := New(0)
	ctx := context.Background()

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok, err := c.Get(ctx, "never-set-key"); err != nil || ok {
			b.Fatalf("Get: ok=%v err=%v, want a miss", ok, err)
		}
	}
}
