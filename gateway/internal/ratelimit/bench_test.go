package ratelimit

import "testing"

// BenchmarkAllow is the rate limiter's performance-regression baseline
// referenced by docs/testing/TESTING.md §7. Allow() is on the gateway's
// hot path (called once per inbound request, per
// gateway/ARCHITECTURE.md's Request Lifecycle), so its steady-state cost
// under contention matters more than any single-threaded number — this
// benchmark is run with -cpu fan-out via RunParallel to exercise the
// mutex under real concurrent load, not just serially.
func BenchmarkAllow(b *testing.B) {
	// A large capacity/refill rate keeps the bucket from running dry
	// mid-benchmark, so this measures Allow's steady-state locking/refill
	// cost rather than the (uninteresting, for this benchmark's purpose)
	// cost of rejecting an exhausted bucket.
	bucket := NewTokenBucket(1_000_000_000, 1_000_000_000)

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			bucket.Allow()
		}
	})
}

// BenchmarkAllowSerial measures Allow's single-threaded cost, as a simpler
// baseline to compare BenchmarkAllow's contended numbers against.
func BenchmarkAllowSerial(b *testing.B) {
	bucket := NewTokenBucket(1_000_000_000, 1_000_000_000)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bucket.Allow()
	}
}
