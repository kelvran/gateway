package redislimiter

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// One real Redis container shared by every test in this file — per
// docs/testing/TESTING.md §4's "real Redis via testcontainers, never
// mocked at this layer" commitment. Each test uses its own unique key
// (see uniqueKey) so tests never interfere with each other despite
// sharing one container.
var redisAddr string

func TestMain(m *testing.M) {
	ctx := context.Background()
	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		panic(fmt.Sprintf("redislimiter: starting test Redis container: %v", err))
	}
	defer func() { _ = container.Terminate(ctx) }()

	connStr, err := container.ConnectionString(ctx)
	if err != nil {
		panic(fmt.Sprintf("redislimiter: getting test Redis connection string: %v", err))
	}
	redisAddr = strings.TrimPrefix(connStr, "redis://")

	m.Run()
}

var keyCounter atomic.Uint64

// uniqueKey returns a fresh virtual-key ID per call so concurrently or
// sequentially run tests never share Redis state.
func uniqueKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-%s-%d", t.Name(), keyCounter.Add(1))
}

func TestAllowWithinCapacitySucceedsThenRejects(t *testing.T) {
	l, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = l.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)

	for i := 0; i < 3; i++ {
		allowed, err := l.Allow(ctx, key, 3, 1)
		if err != nil {
			t.Fatalf("Allow() #%d error = %v", i+1, err)
		}
		if !allowed {
			t.Fatalf("Allow() #%d = false, want true (within burst capacity)", i+1)
		}
	}

	if allowed, err := l.Allow(ctx, key, 3, 1); err != nil {
		t.Fatalf("Allow() error = %v", err)
	} else if allowed {
		t.Fatal("Allow() succeeded after burst capacity exhausted")
	}
}

func TestAllowRefillsOverTime(t *testing.T) {
	l, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = l.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)

	// Burst of 2, refill 10/sec — exhaust the burst, then wait for a
	// real refill (this package has no injectable clock, unlike
	// ratelimit.TokenBucket, since the Lua script's clock argument is
	// always the real wall clock passed in by Allow — sleeping a small,
	// deterministic amount is the simplest correct way to exercise this
	// without adding test-only surface area to the production Allow
	// signature).
	for i := 0; i < 2; i++ {
		allowed, err := l.Allow(ctx, key, 2, 10)
		if err != nil {
			t.Fatalf("Allow() #%d error = %v", i+1, err)
		}
		if !allowed {
			t.Fatalf("Allow() #%d = false, want true", i+1)
		}
	}

	if allowed, err := l.Allow(ctx, key, 2, 10); err != nil {
		t.Fatalf("Allow() error = %v", err)
	} else if allowed {
		t.Fatal("Allow() succeeded immediately after burst exhausted")
	}

	// 10 tokens/sec refill; 150ms is enough for >1 token, not enough to
	// exceed the capacity of 2.
	time.Sleep(150 * time.Millisecond)

	if allowed, err := l.Allow(ctx, key, 2, 10); err != nil {
		t.Fatalf("Allow() error = %v", err)
	} else if !allowed {
		t.Fatal("Allow() rejected after enough time elapsed for a token to refill")
	}
}

// TestTwoLimitersShareOneBucket is the load-bearing test for this whole
// RFC: two independent *Limiter instances (simulating two separate
// gateway processes, each with its own go-redis client) pointed at the
// same Redis address and the same key must share exactly one burst
// budget between them — the multi-instance correctness property the
// in-memory ratelimit.TokenBucket cannot provide, and the entire reason
// this package exists.
func TestTwoLimitersShareOneBucket(t *testing.T) {
	l1, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() #1 error = %v", err)
	}
	defer func() { _ = l1.Close() }()

	l2, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() #2 error = %v", err)
	}
	defer func() { _ = l2.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)

	allowedCount := 0
	for i := 0; i < 6; i++ {
		l := l1
		if i%2 == 1 {
			l = l2
		}
		allowed, err := l.Allow(ctx, key, 3, 0.001) // negligible refill: isolates burst-sharing from refill timing
		if err != nil {
			t.Fatalf("Allow() call #%d error = %v", i+1, err)
		}
		if allowed {
			allowedCount++
		}
	}

	if allowedCount != 3 {
		t.Fatalf("allowedCount = %d, want exactly 3 (the shared burst capacity) across both limiter instances combined", allowedCount)
	}
}

func TestOpenNeverFailsOnUnreachableAddr(t *testing.T) {
	// A port nothing is listening on. Open must still succeed — go-redis
	// dials lazily, and this RFC's fail-open policy depends on Open
	// itself never failing on a bad/unreachable address, only Allow.
	l, err := Open("127.0.0.1:1")
	if err != nil {
		t.Fatalf("Open() on an unreachable address returned an error = %v, want nil (dialing is lazy)", err)
	}
	defer func() { _ = l.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := l.Allow(ctx, "any-key", 1, 1); err == nil {
		t.Fatal("Allow() against an unreachable Redis address succeeded, want an error")
	}
}
