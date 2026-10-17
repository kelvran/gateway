package redislimiter

import (
	"context"
	"math"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/kelvran/gateway/gateway/internal/ratelimit/admission"
)

// Round-5 item 10 phase 2b: one gate script evaluates RPM, TPM and
// tokens-per-day and writes a reservation only when all three admit, so a
// refused request consumes nothing in any dimension and the pre-call cost is
// one round trip instead of three (four on a day refusal).

func rpmKeyOf(key string) string { return "ratelimit:" + url.QueryEscape(key) }
func tatKeyOf(key string) string { return "ratelimit:tpm:" + url.QueryEscape(key) }

func rpmTokens(t *testing.T, c *redis.Client, key string) float64 {
	t.Helper()
	s, err := c.HGet(context.Background(), rpmKeyOf(key), "tokens").Result()
	if err != nil {
		t.Fatalf("HGET tokens: %v", err)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func fullAdmit(key string) admission.Request {
	return admission.Request{
		RPMKey: key, RPMCapacity: 10, RPMRefillPerSecond: 0, // no refill between calls: token counts stay exact
		TPM: admission.Dimension{Key: key, Burst: 1_000, Rate: 1_000 / 60.0, ColdStart: 1_000},
		TPD: admission.Dimension{Key: key + "\x00dim=day", Burst: 100_000, Rate: 100_000 / 86400.0, ColdStart: 100_000 / 24.0},
	}
}

func TestAdmitReservesAllThreeDimensionsInOneCall(t *testing.T) {
	l, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	key := uniqueKey(t)

	r, err := l.Admit(ctx, fullAdmit(key))
	if err != nil || !r.Allowed || r.RefusedBy != "" {
		t.Fatalf("Admit on a fresh key = %+v (%v), want allowed", r, err)
	}
	if r.TPMCost != 1_000 {
		t.Fatalf("TPM cost %v, want the 1 000 cold-start cost (no history)", r.TPMCost)
	}
	// The day dimension's cold start is capped by the minute reservation.
	if want := 1_000.0; r.TPDCost != want || r.TPDColdStart != want {
		t.Fatalf("TPD cost %v, cold %v; want both %v (min(100 000/24, the 1 000 TPM reservation))", r.TPDCost, r.TPDColdStart, want)
	}
	if got := rpmTokens(t, c, key); got != 9 {
		t.Fatalf("RPM tokens after one admit = %v, want 9", got)
	}
	for _, k := range []string{tatKeyOf(key), tatKeyOf(key + "\x00dim=day")} {
		if n, _ := c.Exists(ctx, k).Result(); n != 1 {
			t.Fatalf("%s not written on a full admit", k)
		}
	}
}

func TestAdmitReservesNothingWhenADimensionRefuses(t *testing.T) {
	l, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	// TPM refuses: a 50-token burst is spent by the first (cold) reservation.
	key := uniqueKey(t)
	req := fullAdmit(key)
	req.TPM.Burst, req.TPM.ColdStart, req.TPM.Rate = 50, 50, 50/60.0
	if r, err := l.Admit(ctx, req); err != nil || !r.Allowed {
		t.Fatalf("first Admit = %+v (%v), want allowed", r, err)
	}
	dayTATBefore, _ := c.Get(ctx, tatKeyOf(key+"\x00dim=day")).Result()
	minuteTATAtAdmit, _ := c.Get(ctx, tatKeyOf(key)).Result()
	r, err := l.Admit(ctx, req)
	if err != nil || r.Allowed || r.RefusedBy != "tpm" {
		t.Fatalf("second Admit = %+v (%v), want refused by tpm", r, err)
	}
	if r.RetryAfterSec <= 0 {
		t.Fatalf("a TPM refusal must carry a retry-after, got %v", r.RetryAfterSec)
	}
	if got := rpmTokens(t, c, key); got != 9 {
		t.Fatalf("RPM tokens after a TPM refusal = %v, want 9: a refused request consumes no RPM token", got)
	}
	if dayTATAfter, _ := c.Get(ctx, tatKeyOf(key+"\x00dim=day")).Result(); dayTATAfter != dayTATBefore {
		t.Fatal("a TPM refusal must not touch the day dimension")
	}
	if minuteTATAfter, _ := c.Get(ctx, tatKeyOf(key)).Result(); minuteTATAfter != minuteTATAtAdmit {
		t.Fatal("a TPM refusal must not move the minute TAT it refused on")
	}

	// The day dimension refuses: a generous minute dimension, a 100-token day
	// allowance and a settled day history whose mean is the whole allowance,
	// so the first call fits and the second does not (a cold refusal needs
	// about 24 calls, since the cold start is the allowance over 24 h).
	day := uniqueKey(t)
	dreq := fullAdmit(day)
	dreq.TPM.Burst = 10_000
	dreq.TPD.Burst, dreq.TPD.Rate, dreq.TPD.ColdStart = 100, 100/86400.0, 100/24.0
	if err := c.HSet(ctx, tpmHistoryKey(day+"\x00dim=day"), "sum", "1000", "count", "1").Err(); err != nil {
		t.Fatal(err)
	}
	if r, err := l.Admit(ctx, dreq); err != nil || !r.Allowed || r.TPDCost != 100 {
		t.Fatalf("first day Admit = %+v (%v), want allowed with the day mean clamped to the 100-token allowance", r, err)
	}
	minuteTATBefore, _ := c.Get(ctx, tatKeyOf(day)).Result()
	r, err = l.Admit(ctx, dreq)
	if err != nil || r.Allowed || r.RefusedBy != "tpd" {
		t.Fatalf("second day Admit = %+v (%v), want refused by tpd", r, err)
	}
	// The cold start comes back through Lua's %.14g tostring: compare with a
	// tolerance, never exactly.
	if r.RetryAfterSec <= 0 || r.TPDCost != 100 || math.Abs(r.TPDColdStart-100/24.0) > 1e-9 {
		t.Fatalf("a warm day refusal reports its cost (%v, want 100), the cold start it did not use (%v, want %v) and a retry-after %v > 0", r.TPDCost, r.TPDColdStart, 100/24.0, r.RetryAfterSec)
	}
	if got := rpmTokens(t, c, day); got != 9 {
		t.Fatalf("RPM tokens after a day refusal = %v, want 9", got)
	}
	if minuteTATAfter, _ := c.Get(ctx, tatKeyOf(day)).Result(); minuteTATAfter != minuteTATBefore {
		t.Fatal("a day refusal must not move the minute TAT: nothing was reserved, so nothing is released")
	}

	// RPM refuses: capacity 1, the second call finds no token and reserves nothing.
	rpm := uniqueKey(t)
	rreq := fullAdmit(rpm)
	rreq.RPMCapacity = 1
	if r, err := l.Admit(ctx, rreq); err != nil || !r.Allowed {
		t.Fatalf("first RPM Admit = %+v (%v), want allowed", r, err)
	}
	minuteTATBefore, _ = c.Get(ctx, tatKeyOf(rpm)).Result()
	r, err = l.Admit(ctx, rreq)
	if err != nil || r.Allowed || r.RefusedBy != "rpm" {
		t.Fatalf("second RPM Admit = %+v (%v), want refused by rpm", r, err)
	}
	if minuteTATAfter, _ := c.Get(ctx, tatKeyOf(rpm)).Result(); minuteTATAfter != minuteTATBefore {
		t.Fatal("an RPM refusal must not move the minute TAT")
	}
}

func TestAdmitSkipsDimensionsThatAreOff(t *testing.T) {
	l, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	key := uniqueKey(t)
	req := admission.Request{RPMKey: key, RPMCapacity: 10, RPMRefillPerSecond: 0}
	r, err := l.Admit(ctx, req)
	if err != nil || !r.Allowed || r.TPMCost != 0 || r.TPDCost != 0 {
		t.Fatalf("RPM-only Admit = %+v (%v), want allowed with no token reservations", r, err)
	}
	if n, _ := c.Exists(ctx, tatKeyOf(key), tatKeyOf(key+"\x00dim=day")).Result(); n != 0 {
		t.Fatal("off dimensions must not create keys")
	}
	if got := rpmTokens(t, c, key); got != 9 {
		t.Fatalf("RPM tokens = %v, want 9", got)
	}
}

func TestSettleTokensSettlesBothDimensionsInOneCall(t *testing.T) {
	l, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	key := uniqueKey(t)
	req := fullAdmit(key)
	r, err := l.Admit(ctx, req)
	if err != nil || !r.Allowed {
		t.Fatal(err)
	}
	real := 120.0
	// Both dimensions settle to 120 real tokens: the minute reservation of
	// 1 000 and the day reservation of 1 000 both shrink by 880 tokens' worth.
	minuteEmission := 1000.0 / req.TPM.Rate
	dayEmission := 1000.0 / req.TPD.Rate
	if err := l.SettleTokens(ctx, []admission.Settlement{
		{Key: key, DeltaIncrement: minuteEmission * (real - r.TPMCost), RealTokens: &real},
		{Key: key + "\x00dim=day", DeltaIncrement: dayEmission * (real - r.TPDCost), RealTokens: &real},
	}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{key, key + "\x00dim=day"} {
		sum, _ := c.HGet(ctx, tpmHistoryKey(k), "sum").Result()
		count, _ := c.HGet(ctx, tpmHistoryKey(k), "count").Result()
		if sum != "120" || count != "1" {
			t.Fatalf("%s history = sum %q count %q, want 120 / 1", k, sum, count)
		}
	}
	// The next minute reservation is the 120-token mean, not the cold burst.
	r2, err := l.Admit(ctx, req)
	if err != nil || !r2.Allowed || r2.TPMCost != 120 {
		t.Fatalf("Admit after a settlement = %+v (%v), want the 120-token mean reserved", r2, err)
	}
	// A single-dimension settlement is the same call with one entry.
	if err := l.SettleTokens(ctx, []admission.Settlement{{Key: key, DeltaIncrement: minuteEmission * (real - r2.TPMCost), RealTokens: &real}}); err != nil {
		t.Fatal(err)
	}
	if count, _ := c.HGet(ctx, tpmHistoryKey(key), "count").Result(); count != "2" {
		t.Fatalf("minute history count = %q after two settlements, want 2", count)
	}
	// No entries: no call, no error.
	if err := l.SettleTokens(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

// The RPM hash's refreshed state is persisted on every call, a refusal
// included, so a hammered key never lets its hash expire -- only the token
// waits for the admit. And the gate's RPM write is the single RPM script's:
// Admit and Allow on twin keys leave the same hash and the same TTL.
func TestAdmitPersistsTheRPMStateOnARefusalAndWritesWhatAllowWrites(t *testing.T) {
	l, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	twinA, twinB := uniqueKey(t), uniqueKey(t)
	if ok, err := l.Allow(ctx, twinA, 10, 2); err != nil || !ok {
		t.Fatalf("Allow = %v, %v", ok, err)
	}
	if r, err := l.Admit(ctx, admission.Request{RPMKey: twinB, RPMCapacity: 10, RPMRefillPerSecond: 2}); err != nil || !r.Allowed {
		t.Fatalf("Admit = %+v, %v", r, err)
	}
	ha, _ := c.HGetAll(ctx, rpmKeyOf(twinA)).Result()
	hb, _ := c.HGetAll(ctx, rpmKeyOf(twinB)).Result()
	if ha["tokens"] != hb["tokens"] || ha["tokens"] != "9" || ha["last_refill_ms"] == "" || hb["last_refill_ms"] == "" {
		t.Fatalf("twin hashes differ: Allow %v, Admit %v; want tokens 9 and a last_refill_ms on both", ha, hb)
	}
	ttlA, _ := c.TTL(ctx, rpmKeyOf(twinA)).Result()
	ttlB, _ := c.TTL(ctx, rpmKeyOf(twinB)).Result()
	// max(60, ceil(2 * 10 / 2)) = 60 s on both.
	if ttlA < 55*time.Second || ttlA > 60*time.Second || ttlB < 55*time.Second || ttlB > 60*time.Second {
		t.Fatalf("TTLs = %v (Allow) and %v (Admit), want both about 60 s", ttlA, ttlB)
	}

	// A refusal persists the refreshed state: last_refill_ms advances, the TTL
	// is refreshed, the refilled tokens are kept and none is taken.
	key := uniqueKey(t)
	req := fullAdmit(key)
	req.RPMRefillPerSecond = 2
	req.TPM.Burst, req.TPM.ColdStart, req.TPM.Rate = 50, 50, 50/60.0
	if r, err := l.Admit(ctx, req); err != nil || !r.Allowed {
		t.Fatalf("first Admit = %+v (%v)", r, err)
	}
	before, _ := c.HGet(ctx, rpmKeyOf(key), "last_refill_ms").Result()
	if err := c.Expire(ctx, rpmKeyOf(key), 5*time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	r, err := l.Admit(ctx, req)
	if err != nil || r.Allowed || r.RefusedBy != "tpm" {
		t.Fatalf("second Admit = %+v (%v), want refused by tpm", r, err)
	}
	after, _ := c.HGet(ctx, rpmKeyOf(key), "last_refill_ms").Result()
	if after == "" || after <= before {
		t.Fatalf("last_refill_ms before %q, after %q: a refusal must persist the refreshed state", before, after)
	}
	if ttl, _ := c.TTL(ctx, rpmKeyOf(key)).Result(); ttl < 50*time.Second {
		t.Fatalf("TTL after a refusal = %v, want refreshed to about 60 s (it was set to 5 s before the call)", ttl)
	}
	if tokens := rpmTokens(t, c, key); tokens < 9 || tokens >= 10 {
		t.Fatalf("tokens after a refusal = %v, want the refilled 9.0x: refilled, not consumed", tokens)
	}
}
