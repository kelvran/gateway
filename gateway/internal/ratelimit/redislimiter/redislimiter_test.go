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

// TestAllowAndAllowTPMDoNotCollideForATPMPrefixedKeyID is the direct
// regression proof for a real HIGH-severity finding from this session's
// own end-to-end audit: keyID has no charset restriction anywhere, and
// Allow's own "ratelimit:" + keyID key used to collide byte-for-byte
// with AllowTPM("ratelimit:tpm:" + key) whenever keyID itself was
// "tpm:<suffix>" — but Allow's key is a Hash (HMGET/HSET) and AllowTPM's
// is a String (GET/SET), an incompatible-type collision. Proven here by
// exercising Allow first (creating the Hash), then AllowTPM against the
// key that used to alias it (which would fail outright with a Redis
// WRONGTYPE error if the collision still existed) — both must succeed
// independently. Break this by reverting Allow/AllowTPM/AdjustTPM's own
// url.QueryEscape calls: this test starts failing with a WRONGTYPE
// error from AllowTPM, not a wrong admission decision.
func TestAllowAndAllowTPMDoNotCollideForATPMPrefixedKeyID(t *testing.T) {
	l, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = l.Close() }()

	ctx := context.Background()
	suffix := uniqueKey(t)
	rpmKeyID := "tpm:" + suffix // Allow("tpm:"+suffix) used to alias AllowTPM(suffix)'s own key.

	if allowed, err := l.Allow(ctx, rpmKeyID, 3, 1); err != nil {
		t.Fatalf("Allow(%q) error = %v", rpmKeyID, err)
	} else if !allowed {
		t.Fatalf("Allow(%q) = false, want true (within burst capacity)", rpmKeyID)
	}

	allowed, _, err := l.AllowTPM(ctx, suffix, 10, 10, 1, 3)
	if err != nil {
		t.Fatalf("AllowTPM(%q) error = %v -- want no error, since a real collision would surface as a Redis WRONGTYPE error here", suffix, err)
	}
	if !allowed {
		t.Fatalf("AllowTPM(%q) = false, want true (well within its own separate burst)", suffix)
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

// TestAllowTPMWithinBurstSucceedsThenRejects proves the core GCRA
// admission math for a variable, non-1 cost: burst=10, rate=10
// tokens/sec, cost=3 per call — floor(10/3) = 3 calls admitted, the 4th
// rejected, since burst_offset (1000ms) is exhausted once the stored TAT
// advances past now+1000ms (3*300ms = 900ms fits, a 4th call's own
// +300ms would push it to 1200ms, past the 1000ms offset).
func TestAllowTPMWithinBurstSucceedsThenRejects(t *testing.T) {
	l, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = l.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)

	for i := 0; i < 3; i++ {
		allowed, _, err := l.AllowTPM(ctx, key, 10, 10, 1, 3)
		if err != nil {
			t.Fatalf("AllowTPM() #%d error = %v", i+1, err)
		}
		if !allowed {
			t.Fatalf("AllowTPM() #%d = false, want true (within burst)", i+1)
		}
	}

	allowed, retryAfterSec, err := l.AllowTPM(ctx, key, 10, 10, 1, 3)
	if err != nil {
		t.Fatalf("AllowTPM() error = %v", err)
	}
	if allowed {
		t.Fatal("AllowTPM() succeeded after burst exhausted")
	}
	if retryAfterSec <= 0 {
		t.Errorf("retryAfterSec = %v, want > 0 when rejected", retryAfterSec)
	}
}

// TestAllowTPMRejectsASingleCostExceedingBurst proves GCRA's real
// behavior for a single request whose own cost exceeds the entire
// configured burst: rejected outright, on the very first call — this
// implementation deliberately does not support partial admission
// (clamping cost down to whatever headroom remains), unlike
// redis_rate's own AllowAtMost variant.
func TestAllowTPMRejectsASingleCostExceedingBurst(t *testing.T) {
	l, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = l.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)

	allowed, _, err := l.AllowTPM(ctx, key, 10, 10, 1, 15)
	if err != nil {
		t.Fatalf("AllowTPM() error = %v", err)
	}
	if allowed {
		t.Fatal("AllowTPM() with cost (15) exceeding burst (10) succeeded, want rejected")
	}
}

// TestAdjustTPMReconcilesEstimateDownToRealCost proves the reserve-then-
// reconcile flow: reserving a conservative estimate (8) then adjusting
// down to a smaller real cost (2) frees up exactly the difference —
// a subsequent call for the remaining headroom (7, filling burst=10 to
// 2+7=9) must now succeed, which it would NOT have if the original
// 8-token reservation had never been reconciled down.
func TestAdjustTPMReconcilesEstimateDownToRealCost(t *testing.T) {
	l, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = l.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)
	const burst, rate, period = 10.0, 10.0, 1.0
	const estimate, realCost = 8.0, 2.0

	allowed, _, err := l.AllowTPM(ctx, key, burst, rate, period, estimate)
	if err != nil {
		t.Fatalf("AllowTPM(estimate) error = %v", err)
	}
	if !allowed {
		t.Fatal("AllowTPM(estimate=8) against an empty burst=10 key was rejected, want allowed")
	}

	emissionInterval := 1000.0 * period / rate // milliseconds, matching tpmLuaSrc's own units
	deltaIncrement := emissionInterval * (realCost - estimate)
	if err := l.AdjustTPM(ctx, key, deltaIncrement); err != nil {
		t.Fatalf("AdjustTPM() error = %v", err)
	}

	allowed, _, err = l.AllowTPM(ctx, key, burst, rate, period, 7)
	if err != nil {
		t.Fatalf("AllowTPM(cost=7) error = %v", err)
	}
	if !allowed {
		t.Fatal("AllowTPM(cost=7) after reconciling the estimate down to 2 was rejected, want allowed (2+7=9 <= burst=10)")
	}
}

// TestAdjustTPMClampPreventsManufacturingCapacityFromExcessiveRelease is
// the regression proof for tpmAdjustLuaSrc's own "clamp so TAT can never
// be pushed before now" contract: releasing far more than was ever
// reserved (a real caller bug, or simply realTokens=0 combined with an
// unusually large estimate) must not push the stored TAT deep into the
// past, which would otherwise let MANY MORE than one burst's worth of
// full-cost calls all succeed rapid-fire before TAT caught back up to
// real time — "manufacturing capacity from the past."
func TestAdjustTPMClampPreventsManufacturingCapacityFromExcessiveRelease(t *testing.T) {
	l, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = l.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)
	const burst, rate, period = 10.0, 10.0, 1.0

	allowed, _, err := l.AllowTPM(ctx, key, burst, rate, period, 1)
	if err != nil {
		t.Fatalf("AllowTPM(cost=1) error = %v", err)
	}
	if !allowed {
		t.Fatal("AllowTPM(cost=1) against an empty burst=10 key was rejected, want allowed")
	}

	// A wildly excessive release -- far beyond what the single cost=1
	// reservation above could ever justify.
	if err := l.AdjustTPM(ctx, key, -1_000_000); err != nil {
		t.Fatalf("AdjustTPM() error = %v", err)
	}

	// Immediately after: exactly ONE full-burst call may succeed (the
	// clamp pinned TAT back to "now", not deep in the past) — a SECOND
	// one, right after, must be rejected.
	allowed, _, err = l.AllowTPM(ctx, key, burst, rate, period, burst)
	if err != nil {
		t.Fatalf("AllowTPM(cost=burst) #1 error = %v", err)
	}
	if !allowed {
		t.Fatal("AllowTPM(cost=burst) #1 immediately after the clamp was rejected, want allowed (full capacity restored)")
	}

	allowed, _, err = l.AllowTPM(ctx, key, burst, rate, period, burst)
	if err != nil {
		t.Fatalf("AllowTPM(cost=burst) #2 error = %v", err)
	}
	if allowed {
		t.Fatal("AllowTPM(cost=burst) #2 immediately after #1 succeeded, want rejected -- the excessive release must not have manufactured a SECOND full burst's worth of capacity from the past")
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
