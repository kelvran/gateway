package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"testing"
	"time"
)

// Round-5 Tier S item 24 (2026-10-15): tokens_per_day is a second token
// dimension per virtual key on the same reserve-then-reconcile path as
// TPM -- a bucket of capacity D refilling at D/86400 tokens per second (a
// rolling 24 h allowance, never a midnight reset). These tests pin the
// in-memory bucket, the Redis-mode backend calls (key suffix, derived
// rate, cold-start cap, the wait handed back) and the Register rules.

const tpdTestKey = "team-day"

// dayBucketWithClock swaps keyID's in-memory day bucket for one on a fake
// clock, so refill over hours is deterministic (KeyLimiter has no clock
// injection; TokenBucket does).
func dayBucketWithClock(t *testing.T, l *KeyLimiter, keyID string, tokensPerDay float64, clock *fakeClock) *TokenBucket {
	t.Helper()
	bucket := NewTokenBucketWithClock(tokensPerDay, tokensPerDay/secondsPerDay, clock.now)
	l.mu.Lock()
	l.tpdBuckets[keyID] = bucket
	l.mu.Unlock()
	return bucket
}

func durationsClose(a, b time.Duration) bool {
	return math.Abs(float64(a-b)) <= float64(time.Millisecond)
}

// Two 8-token requests exhaust a 10-token day while the minute bucket
// (capacity 1000, frozen) keeps its balance; the third ReserveTPD is
// refused and reports the wait until the day bucket is positive again:
// 6 tokens of debt at 10/86400 tokens per second = 51 840 s.
func TestReserveTPDRejectsWhenTheDayBucketIsExhaustedThoughTheMinuteHasBalance(t *testing.T) {
	l := NewInMemoryKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TPMCapacity: 1000, TPMRefillPerSecond: 0, TokensPerDay: 10}})
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	dayBucketWithClock(t, l, tpdTestKey, 10, clock)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		allowed, reserved, tokens, epoch, wait, err := l.ReserveTPD(ctx, tpdTestKey, 0)
		if err != nil || !allowed || !reserved || wait != 0 {
			t.Fatalf("call %d: ReserveTPD() = (%v, %v, %v, _, %v, %v), want allowed and reserved with no wait", i+1, allowed, reserved, tokens, wait, err)
		}
		real := 8.0
		l.ReconcileTPD(ctx, tpdTestKey, tokens, epoch, &real)
	}

	allowed, reserved, tokens, _, wait, err := l.ReserveTPD(ctx, tpdTestKey, 0)
	if err != nil || allowed || reserved || tokens != 0 {
		t.Fatalf("call 3: ReserveTPD() = (%v, %v, %v, _, %v, %v), want (false, false, 0) -- the day bucket is at -6", allowed, reserved, tokens, wait, err)
	}
	want := time.Duration(6.0 / (10.0 / secondsPerDay) * float64(time.Second))
	if !durationsClose(wait, want) {
		t.Errorf("call 3 wait = %v, want %v (6 tokens of debt at 10/86400 per second)", wait, want)
	}
	if bal := l.tpmBuckets[tpdTestKey].tokens; bal != 1000 {
		t.Errorf("the minute bucket moved to %v; ReserveTPD must never touch it", bal)
	}
}

// The day bucket is a rolling allowance: half a day later the 6-token
// debt has shrunk by 5 but the balance is still at or below zero; a full
// day after the last settle it is positive again.
func TestReserveTPDRefillsAcrossTheDayNotAtMidnight(t *testing.T) {
	l := NewInMemoryKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TokensPerDay: 10}})
	clock := &fakeClock{t: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)}
	bucket := dayBucketWithClock(t, l, tpdTestKey, 10, clock)
	ctx := context.Background()
	real := 16.0
	bucket.ReconcileTPM(0, 0, &real) // balance 10 -> -6, one settled request of 16 tokens

	clock.Advance(12 * time.Hour) // -6 + 5 = -1
	if allowed, _, _, _, wait, _ := l.ReserveTPD(ctx, tpdTestKey, 0); allowed || !durationsClose(wait, time.Duration(1.0/(10.0/secondsPerDay)*float64(time.Second))) {
		t.Fatalf("after 12 h: allowed = %v, wait = %v; want refused with 8 640 s to go", allowed, wait)
	}
	clock.Advance(12 * time.Hour) // -6 + 10 = 4 (midnight passed long ago; only elapsed time matters)
	allowed, reserved, tokens, _, _, _ := l.ReserveTPD(ctx, tpdTestKey, 0)
	if !allowed || !reserved || tokens != 16 {
		t.Fatalf("after 24 h: ReserveTPD() = (%v, %v, %v), want allowed, reserving the running mean 16", allowed, reserved, tokens)
	}
}

// A key without tokens_per_day never reserves against a day bucket, in
// either mode -- mirroring ReserveTPM's "no entry means unlimited" rule.
func TestReserveTPDUnlimitedWhenNotConfigured(t *testing.T) {
	ctx := context.Background()
	inMem := NewInMemoryKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TPMCapacity: 10, TPMRefillPerSecond: 0}})
	if allowed, reserved, tokens, _, wait, err := inMem.ReserveTPD(ctx, tpdTestKey, 0); !allowed || reserved || tokens != 0 || wait != 0 || err != nil {
		t.Fatalf("in-memory ReserveTPD() = (%v, %v, %v, _, %v, %v), want (true, false, 0, 0, nil)", allowed, reserved, tokens, wait, err)
	}
	inMem.ReconcileTPD(ctx, tpdTestKey, 0, 0, nil) // must not panic on a key with no day bucket

	backend := &fakeBackend{reserveTPMFunc: func(context.Context, string, float64, float64, float64, float64) (bool, float64, float64, error) {
		t.Fatal("the backend must not be called when tokens_per_day isn't configured")
		return false, 0, 0, nil
	}}
	redis := NewRedisKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TPMCapacity: 1000, TPMRefillPerSecond: 50}}, backend)
	if allowed, reserved, _, _, _, err := redis.ReserveTPD(ctx, tpdTestKey, 0); !allowed || reserved || err != nil {
		t.Fatalf("Redis-mode ReserveTPD() = (%v, %v, _, _, _, %v), want (true, false, nil)", allowed, reserved, err)
	}
}

// A cold day bucket (no settled history) reserves at most one hour's share
// of the day (tokens_per_day/24), or the same request's minute reservation
// when that is smaller -- so a cold key never reserves its whole day on
// one request -- and ignores both once it has history of its own.
func TestReserveTPDColdStartIsBoundedByTheCapUntilTheBucketHasHistory(t *testing.T) {
	l := NewInMemoryKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TokensPerDay: 24000}})
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	bucket := dayBucketWithClock(t, l, tpdTestKey, 24000, clock)
	ctx := context.Background()

	allowed, reserved, tokens, epoch, _, err := l.ReserveTPD(ctx, tpdTestKey, 100)
	if err != nil || !allowed || !reserved || tokens != 100 {
		t.Fatalf("cold ReserveTPD(minute 100) = (%v, %v, %v, _, _, %v), want 100 reserved (below the hour's share of 1000)", allowed, reserved, tokens, err)
	}
	if bucket.tokens != 23900 {
		t.Fatalf("balance after the capped reservation = %v, want 23900", bucket.tokens)
	}
	real := 30.0
	l.ReconcileTPD(ctx, tpdTestKey, tokens, epoch, &real)
	if bucket.tokens != 23970 {
		t.Fatalf("balance after settling 30 real tokens = %v, want 23970", bucket.tokens)
	}
	// With history the running mean (30) sizes the reservation, cap or not.
	if _, _, tokens, _, _, _ = l.ReserveTPD(ctx, tpdTestKey, 100); tokens != 30 {
		t.Errorf("warm ReserveTPD(minute 100) reserved %v, want the running mean 30", tokens)
	}
	// No minute reservation (TPM unset or failed open): one hour's share,
	// never the whole day.
	l2 := NewInMemoryKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TokensPerDay: 24000}})
	dayBucketWithClock(t, l2, tpdTestKey, 24000, clock)
	if _, _, tokens, _, _, _ := l2.ReserveTPD(ctx, tpdTestKey, 0); tokens != 1000 {
		t.Errorf("cold ReserveTPD(minute 0) reserved %v, want the hour's share 1000", tokens)
	}
	// A minute reservation above the hour's share is bounded by it.
	l3 := NewInMemoryKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TokensPerDay: 24000}})
	dayBucketWithClock(t, l3, tpdTestKey, 24000, clock)
	if _, _, tokens, _, _, _ := l3.ReserveTPD(ctx, tpdTestKey, 5000); tokens != 1000 {
		t.Errorf("cold ReserveTPD(minute 5000) reserved %v, want the hour's share 1000", tokens)
	}
}

// Register follows the TPM rules one dimension over: a positive
// tokens_per_day resets the EXISTING bucket object in place (an in-flight
// reservation must reconcile against the same object), and dropping it
// removes the bucket.
func TestRegisterTPDResetsInPlaceAndRemovesAStaleDayBucket(t *testing.T) {
	l := NewInMemoryKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TokensPerDay: 10}})
	before := l.tpdBuckets[tpdTestKey]
	if before == nil {
		t.Fatal("a key with tokens_per_day 10 must get a day bucket at construction")
	}
	l.Register(KeyConfig{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TokensPerDay: 20})
	after := l.tpdBuckets[tpdTestKey]
	if after != before {
		t.Fatal("Register replaced the day bucket object; it must Reset the existing one so an outstanding reservation still reconciles against it")
	}
	if after.capacity != 20 || after.refillPerSecond != 20/secondsPerDay {
		t.Errorf("Reset left capacity %v, refill %v; want 20 and 20/86400", after.capacity, after.refillPerSecond)
	}
	l.Register(KeyConfig{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100})
	if _, ok := l.tpdBuckets[tpdTestKey]; ok {
		t.Fatal("an update without tokens_per_day left a stale day bucket behind")
	}
	if allowed, reserved, _, _, _, _ := l.ReserveTPD(context.Background(), tpdTestKey, 0); !allowed || reserved {
		t.Fatal("after the cap is dropped the key must be unlimited on the day dimension")
	}
}

// A mid-stream top-up on the day dimension fits or is refused against the
// day bucket's own balance, exactly like IncreaseReservationTPM.
func TestIncreaseReservationTPDFollowsTheDayBucketsHeadroom(t *testing.T) {
	l := NewInMemoryKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TokensPerDay: 240}})
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	bucket := dayBucketWithClock(t, l, tpdTestKey, 240, clock)
	ctx := context.Background()
	_, _, tokens, epoch, _, _ := l.ReserveTPD(ctx, tpdTestKey, 4) // 240 -> 236 (the hour's share is 10, the minute reservation 4 is smaller)
	bucket.Debit(230)                                             // 6 left

	allowed, applied, _, err := l.IncreaseReservationTPD(ctx, tpdTestKey, tokens, 20, epoch)
	if err != nil || allowed || applied != 4 {
		t.Fatalf("top-up to 20 = (%v, %v, %v), want refused with the reservation kept at 4 (delta 16 exceeds the 6 left)", allowed, applied, err)
	}
	allowed, applied, newEpoch, err := l.IncreaseReservationTPD(ctx, tpdTestKey, tokens, 8, epoch)
	if err != nil || !allowed || applied != 8 || bucket.tokens != 2 {
		t.Fatalf("top-up to 8 = (%v, %v, %v), balance %v; want allowed, 8 applied, balance 2", allowed, applied, err, bucket.tokens)
	}
	l.ReconcileTPD(ctx, tpdTestKey, applied, newEpoch, nil)
	if bucket.tokens != 10 {
		t.Errorf("release after the top-up left %v, want the 8 back on the 2 (10)", bucket.tokens)
	}
	// Unconfigured: a no-op that keeps the caller's values.
	none := NewInMemoryKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100}})
	if allowed, applied, _, err := none.IncreaseReservationTPD(ctx, tpdTestKey, 1, 50, 7); !allowed || applied != 1 || err != nil {
		t.Errorf("unconfigured top-up = (%v, %v, %v), want (true, 1, nil)", allowed, applied, err)
	}
}

// Redis mode: the day dimension is the same GCRA script under a tagged key
// suffix (never the minute dimension's own key), with burst = D, rate =
// D/86400 per second at the 1 s period every other call uses, and the
// cold-start cost one hour's share of the day or the minute reservation
// when smaller.
func TestReserveTPDRedisModeUsesATaggedKeyTheDerivedRateAndTheCappedColdStart(t *testing.T) {
	backend := &fakeBackend{reserveTPMReserved: 125}
	l := NewRedisKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TPMCapacity: 1000, TPMRefillPerSecond: 50, TokensPerDay: 86400}}, backend)
	ctx := context.Background()

	allowed, reserved, tokens, epoch, wait, err := l.ReserveTPD(ctx, tpdTestKey, 100)
	if err != nil || !allowed || !reserved || tokens != 125 || epoch != 0 || wait != 0 {
		t.Fatalf("ReserveTPD() = (%v, %v, %v, %v, %v, %v), want (true, true, 125, 0, 0, nil)", allowed, reserved, tokens, epoch, wait, err)
	}
	if backend.lastReserveKey != tpdBackendKey(tpdTestKey) || backend.lastReserveKey != tpdTestKey+"\x00dim=day" {
		t.Errorf("backend key = %q, want %q (a NUL-tagged suffix distinct from the minute key)", backend.lastReserveKey, tpdTestKey+"\x00dim=day")
	}
	if backend.lastReserveBurst != 86400 || backend.lastReserveRate != 1 || backend.lastReservePeriodSec != 1 || backend.lastReserveColdStartCost != 100 {
		t.Errorf("backend saw burst %v, rate %v, period %v, cold start %v; want 86400, 1, 1, 100", backend.lastReserveBurst, backend.lastReserveRate, backend.lastReservePeriodSec, backend.lastReserveColdStartCost)
	}
	// No minute reservation: one hour's share (86400/24); a minute
	// reservation above it is bounded by it. Never the whole day.
	if _, _, _, _, _, _ = l.ReserveTPD(ctx, tpdTestKey, 0); backend.lastReserveColdStartCost != 3600 {
		t.Errorf("cold start with no minute reservation = %v, want 3600", backend.lastReserveColdStartCost)
	}
	if _, _, _, _, _, _ = l.ReserveTPD(ctx, tpdTestKey, 500000); backend.lastReserveColdStartCost != 3600 {
		t.Errorf("cold start with an oversized minute reservation = %v, want 3600", backend.lastReserveColdStartCost)
	}
}

// Redis mode: a refusal the key's history decided carries the script's own
// wait; a refusal sized at the cold-start cost (no history) carries none,
// because that wait counts siblings' in-flight reservations; a backend
// error fails open with the error passed through.
func TestReserveTPDRedisModeHandsBackTheBackendsWaitAndFailsOpenOnError(t *testing.T) {
	ctx := context.Background()
	refusing := &fakeBackend{reserveTPMFunc: func(context.Context, string, float64, float64, float64, float64) (bool, float64, float64, error) {
		return false, 1800, 11, nil // sized at the running mean 11, refused
	}}
	l := NewRedisKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TokensPerDay: 86400}}, refusing)
	allowed, reserved, tokens, _, wait, err := l.ReserveTPD(ctx, tpdTestKey, 0)
	if err != nil || allowed || reserved || tokens != 0 || wait != 30*time.Minute {
		t.Fatalf("refused ReserveTPD() = (%v, %v, %v, _, %v, %v), want (false, false, 0, 30m, nil)", allowed, reserved, tokens, wait, err)
	}
	cold := &fakeBackend{reserveTPMFunc: func(_ context.Context, _ string, _, _, _, coldStartCost float64) (bool, float64, float64, error) {
		return false, 86400, coldStartCost, nil // no history: sized at the cold-start cost, refused by a sibling's provisional reservation
	}}
	l = NewRedisKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TokensPerDay: 86400}}, cold)
	if allowed, _, _, _, wait, err := l.ReserveTPD(ctx, tpdTestKey, 0); err != nil || allowed || wait != 0 {
		t.Fatalf("cold refusal = (%v, wait %v, %v), want refused with NO wait (the backoff applies)", allowed, wait, err)
	}
	// The real script formats the cost with Lua's %.14g: for the shipped
	// example cap, 2000000/24, that is 83333.333333333 against the caller's
	// 83333.33333333333 -- an exact compare would miss the cold refusal.
	roundTripped := &fakeBackend{reserveTPMFunc: func(_ context.Context, _ string, _, _, _, coldStartCost float64) (bool, float64, float64, error) {
		cost, _ := strconv.ParseFloat(fmt.Sprintf("%.14g", coldStartCost), 64)
		return false, 3600, cost, nil
	}}
	l = NewRedisKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TokensPerDay: 2000000}}, roundTripped)
	if allowed, _, _, _, wait, err := l.ReserveTPD(ctx, tpdTestKey, 0); err != nil || allowed || wait != 0 {
		t.Fatalf("cold refusal through the %%.14g round-trip = (%v, wait %v, %v), want refused with NO wait", allowed, wait, err)
	}
	if !isColdRefusal(83333.333333333, 2000000.0/24) || isColdRefusal(1100, 1000) {
		t.Fatal("isColdRefusal must accept the script's 14-digit rendering of the cold cost and reject a running mean 10% away")
	}

	wantErr := errors.New("boom")
	failing := &fakeBackend{reserveTPMErr: wantErr}
	l = NewRedisKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TokensPerDay: 86400}}, failing)
	allowed, reserved, tokens, _, wait, err = l.ReserveTPD(ctx, tpdTestKey, 0)
	if !errors.Is(err, wantErr) || !allowed || reserved || tokens != 0 || wait != 0 {
		t.Fatalf("erroring ReserveTPD() = (%v, %v, %v, _, %v, %v), want (true, false, 0, 0, boom)", allowed, reserved, tokens, wait, err)
	}
}

// Redis mode: the settle delta uses the derived rate (D/86400 per second,
// so 1000/rate ms per token) on the tagged key; nil real releases without
// teaching the history.
func TestReconcileTPDRedisModeSettlesTheTaggedKeyWithTheDerivedRate(t *testing.T) {
	backend := &fakeBackend{}
	l := NewRedisKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TokensPerDay: 86400}}, backend)
	ctx := context.Background()

	real := 40.0
	l.ReconcileTPD(ctx, tpdTestKey, 100, 0, &real)
	if backend.lastSettleKey != tpdBackendKey(tpdTestKey) || backend.lastSettleDelta != 1000.0*(40-100) || backend.lastSettleReal == nil || *backend.lastSettleReal != 40 {
		t.Errorf("settle = key %q delta %v real %v; want the tagged key, -60000 ms, real 40", backend.lastSettleKey, backend.lastSettleDelta, backend.lastSettleReal)
	}
	l.ReconcileTPD(ctx, tpdTestKey, 100, 0, nil)
	if backend.lastSettleDelta != -100000 || backend.lastSettleReal != nil {
		t.Errorf("release = delta %v real %v; want -100000 ms and nil", backend.lastSettleDelta, backend.lastSettleReal)
	}
	if backend.adjustCalls != 0 {
		t.Errorf("AdjustTPM was called %d times; the day dimension settles through SettleTPM only", backend.adjustCalls)
	}
}

// Redis mode: a top-up is a fresh AllowTPM for the delta on the tagged key.
func TestIncreaseReservationTPDRedisModeIssuesAllowTPMForTheDeltaOnTheTaggedKey(t *testing.T) {
	var gotKey string
	var gotBurst, gotRate, gotPeriod, gotCost float64
	backend := &fakeBackend{allowTPMFunc: func(_ context.Context, key string, burst, rate, periodSec, cost float64) (bool, float64, error) {
		gotKey, gotBurst, gotRate, gotPeriod, gotCost = key, burst, rate, periodSec, cost
		return true, 0, nil
	}}
	l := NewRedisKeyLimiter([]KeyConfig{{ID: tpdTestKey, Capacity: 100, RefillPerSecond: 100, TokensPerDay: 86400}}, backend)

	allowed, applied, epoch, err := l.IncreaseReservationTPD(context.Background(), tpdTestKey, 100, 150, 0)
	if err != nil || !allowed || applied != 150 || epoch != 0 {
		t.Fatalf("IncreaseReservationTPD() = (%v, %v, %v, %v), want (true, 150, 0, nil)", allowed, applied, epoch, err)
	}
	if gotKey != tpdBackendKey(tpdTestKey) || gotBurst != 86400 || gotRate != 1 || gotPeriod != 1 || gotCost != 50 {
		t.Errorf("AllowTPM saw key %q burst %v rate %v period %v cost %v; want the tagged key, 86400, 1, 1, 50", gotKey, gotBurst, gotRate, gotPeriod, gotCost)
	}
}

// TokenBucket.ReserveTokens is ReserveTPM plus the cold-start cap and the
// wait until the balance is positive again; ReserveTPM itself is unchanged
// (its own tests pin that).
func TestReserveTokensReportsTheWaitUntilTheBalanceIsPositive(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	b := NewTokenBucketWithClock(10, 1, clock.now)
	b.Debit(15) // -5 at 1 token per second

	allowed, reserved, _, wait := b.ReserveTokens(0)
	if allowed || reserved != 0 || wait != 5*time.Second {
		t.Fatalf("ReserveTokens() on a -5 balance = (%v, %v, _, %v), want (false, 0, 5s)", allowed, reserved, wait)
	}
	clock.Advance(5 * time.Second) // exactly 0: still refused, nothing left to wait for but the next token
	if allowed, _, _, wait := b.ReserveTokens(0); allowed || wait != 0 {
		t.Fatalf("ReserveTokens() at balance 0 = (%v, _, _, %v), want refused with a zero wait", allowed, wait)
	}
	frozen := NewTokenBucketWithClock(10, 0, clock.now)
	frozen.Debit(15)
	if allowed, _, _, wait := frozen.ReserveTokens(0); allowed || wait != 0 {
		t.Fatalf("a bucket that never refills reports no wait, got (%v, %v)", allowed, wait)
	}
}
