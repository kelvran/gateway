package redislimiter

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis-mode TPM used to reserve the full burst on every request (the
// 2026-10-08 live run's F2: one in-flight request per key). ReserveTPM now
// sizes the reservation from the key's own running mean of settled real
// usage, kept in Redis beside the arrival time; a key with no history still
// reserves the cold-start cost the caller passes (the full burst).

func openLimiter(t *testing.T) *Limiter {
	t.Helper()
	l, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// freshKey is uniqueKey plus a per-process nonce and a cleanup of both Redis
// keys: the history hash lives 24 h and uniqueKey's counter restarts with
// every test binary, so without this a second run against the same Redis
// found the previous run's history and started warm (the Go review lane hit
// it; CI never does, each run gets a fresh container).
func freshKey(t *testing.T, l *Limiter) string {
	t.Helper()
	key := fmt.Sprintf("%s-%d", uniqueKey(t), time.Now().UnixNano())
	t.Cleanup(func() {
		_ = l.client.Del(context.Background(), "ratelimit:tpm:"+url.QueryEscape(key), tpmHistoryKey(key)).Err()
	})
	return key
}

func TestReserveTPMReservesTheColdStartCostWithoutHistory(t *testing.T) {
	l := openLimiter(t)
	ctx, key := context.Background(), freshKey(t, l)
	allowed, _, reserved, err := l.ReserveTPM(ctx, key, 1000, 100, 1.0, 1000)
	if err != nil || !allowed || reserved != 1000 {
		t.Fatalf("cold reserve = (%v, %v, %v), want allowed with the full 1000 reserved", allowed, reserved, err)
	}
	// The full burst is out: a sibling is refused until the arrival time passes.
	if allowed, _, _, err := l.ReserveTPM(ctx, key, 1000, 100, 1.0, 1000); err != nil || allowed {
		t.Fatalf("second cold reserve = (%v, %v), want refused", allowed, err)
	}
}

func TestSettleTPMRecordsRealUsageAndReserveTPMUsesTheMean(t *testing.T) {
	l := openLimiter(t)
	ctx, key := context.Background(), freshKey(t, l)
	const burst, rate = 1000.0, 100.0
	emission := 1000.0 / rate
	_, _, reserved, err := l.ReserveTPM(ctx, key, burst, rate, 1.0, burst)
	if err != nil || reserved != burst {
		t.Fatalf("cold reserve: %v %v", reserved, err)
	}
	// Settle at 100 real tokens: the arrival time moves back by 900 tokens and the history starts.
	real := 100.0
	if err := l.SettleTPM(ctx, key, emission*(real-reserved), &real); err != nil {
		t.Fatal(err)
	}
	allowed, _, reserved, err := l.ReserveTPM(ctx, key, burst, rate, 1.0, burst)
	if err != nil || !allowed || reserved != 100 {
		t.Fatalf("reserve after one settlement = (%v, %v, %v), want allowed with the mean 100 reserved", allowed, reserved, err)
	}
	real2 := 300.0
	if err := l.SettleTPM(ctx, key, emission*(real2-reserved), &real2); err != nil {
		t.Fatal(err)
	}
	_, _, reserved, err = l.ReserveTPM(ctx, key, burst, rate, 1.0, burst)
	if err != nil || reserved != 200 {
		t.Fatalf("reserve after two settlements = %v (%v), want the mean 200", reserved, err)
	}
}

func TestConcurrentReservesAreAdmittedOnceTheMeanIsSmall(t *testing.T) {
	// The F2 shape at the backend: one warm-up settled at 50 tokens, then ten
	// reserves in a row against a 1000-token burst -- all admitted (10 x 50 =
	// 500), where the full-burst estimate admitted exactly one.
	l := openLimiter(t)
	ctx, key := context.Background(), freshKey(t, l)
	const burst, rate = 1000.0, 100.0
	_, _, reserved, _ := l.ReserveTPM(ctx, key, burst, rate, 1.0, burst)
	real := 50.0
	if err := l.SettleTPM(ctx, key, (1000.0/rate)*(real-reserved), &real); err != nil {
		t.Fatal(err)
	}
	admitted := 0
	for i := 0; i < 10; i++ {
		allowed, _, r, err := l.ReserveTPM(ctx, key, burst, rate, 1.0, burst)
		if err != nil {
			t.Fatal(err)
		}
		if allowed {
			admitted++
		}
		if r != 50 {
			t.Fatalf("reserve %d sized %v, want the mean 50", i, r)
		}
	}
	if admitted != 10 {
		t.Fatalf("admitted %d of 10 after a 50-token warm-up on a 1000-token burst, want 10", admitted)
	}
}

func TestSettleTPMWithNoRealUsageLeavesTheHistoryAlone(t *testing.T) {
	l := openLimiter(t)
	ctx, key := context.Background(), freshKey(t, l)
	_, _, reserved, _ := l.ReserveTPM(ctx, key, 1000, 100, 1.0, 1000)
	if err := l.SettleTPM(ctx, key, (1000.0/100)*(0-reserved), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, r, _ := l.ReserveTPM(ctx, key, 1000, 100, 1.0, 1000); r != 1000 {
		t.Fatalf("a release must not start a history; reserve sized %v, want the cold 1000", r)
	}
}

func TestReserveTPMClampsTheMeanToTheBurst(t *testing.T) {
	l := openLimiter(t)
	ctx, key := context.Background(), freshKey(t, l)
	_, _, reserved, _ := l.ReserveTPM(ctx, key, 1000, 100, 1.0, 1000)
	real := 5000.0 // a settlement far above the burst (the cap was raised later, say)
	if err := l.SettleTPM(ctx, key, (1000.0/100)*(real-reserved), &real); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, _, r, _ := l.ReserveTPM(ctx, key, 1000, 100, 1.0, 1000); r != 1000 {
		t.Fatalf("mean 5000 over a 1000 burst reserved %v, want the clamp 1000", r)
	}
}

func TestTPMHistoryExpiresAfterADay(t *testing.T) {
	l := openLimiter(t)
	ctx, key := context.Background(), freshKey(t, l)
	_, _, reserved, _ := l.ReserveTPM(ctx, key, 1000, 100, 1.0, 1000)
	real := 100.0
	if err := l.SettleTPM(ctx, key, (1000.0/100)*(real-reserved), &real); err != nil {
		t.Fatal(err)
	}
	ttl, err := l.client.PTTL(ctx, tpmHistoryKey(key)).Result()
	if err != nil || ttl < 23*time.Hour || ttl > 24*time.Hour {
		t.Fatalf("history TTL = %v (%v), want about 24 h", ttl, err)
	}
}

func TestSettleTPMRecordsUsageEvenWhenTheArrivalTimeKeyIsGone(t *testing.T) {
	// A long request whose arrival-time key expired before it settled still
	// teaches the history, so the next reservation is right-sized.
	l := openLimiter(t)
	ctx, key := context.Background(), freshKey(t, l)
	real := 120.0
	if err := l.SettleTPM(ctx, key, 0, &real); err != nil {
		t.Fatal(err)
	}
	if _, _, r, _ := l.ReserveTPM(ctx, key, 1000, 100, 1.0, 1000); r != 120 {
		t.Fatalf("reserve after a settlement with no arrival-time key sized %v, want 120", r)
	}
}
