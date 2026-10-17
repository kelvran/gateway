package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/ratelimit/admission"
)

// Round-5 item 10 phase 2b: KeyLimiter.Admit decides RPM, TPM and
// tokens-per-day in one step -- one backend call in Redis mode, the three
// buckets in memory -- and a refused request consumes nothing in any
// dimension in either mode. KeyLimiter.SettleTokens settles both token
// dimensions in one backend call.

func bucketTokens(b *TokenBucket) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokens
}

func TestAdmitInMemoryRefundsTheRPMTokenWhenALaterDimensionRefuses(t *testing.T) {
	// Two RPM tokens, a 50-token minute bucket: the first admission takes the
	// whole minute bucket (cold start), so the second is refused by TPM.
	l := NewInMemoryKeyLimiter([]KeyConfig{{ID: "k", Capacity: 2, RefillPerSecond: 0, TPMCapacity: 50, TPMRefillPerSecond: 0}})
	ctx := context.Background()
	a, err := l.Admit(ctx, "k", "")
	if err != nil || !a.Allowed || !a.TPM.Reserved || a.TPM.Tokens != 50 || a.TPD.Reserved {
		t.Fatalf("first Admit = %+v (%v), want allowed with the 50-token cold reservation and no day dimension", a, err)
	}
	a, err = l.Admit(ctx, "k", "")
	if err != nil || a.Allowed || a.RefusedBy != "tpm" || a.TPM.Reserved {
		t.Fatalf("second Admit = %+v (%v), want refused by tpm with nothing reserved", a, err)
	}
	if got := bucketTokens(l.buckets["k"]); got != 1 {
		t.Fatalf("RPM tokens after a TPM refusal = %v, want 1: the refused request's token is refunded (parity with the Redis gate)", got)
	}
}

func TestAdmitInMemoryReleasesTheMinuteReservationOnADayRefusal(t *testing.T) {
	const minute = 1_000_000_000.0
	l := NewInMemoryKeyLimiter([]KeyConfig{{ID: "k", Capacity: 2, RefillPerSecond: 0, TPMCapacity: minute, TPMRefillPerSecond: 0, TokensPerDay: 10}})
	ctx := context.Background()
	a, err := l.Admit(ctx, "k", "")
	if err != nil || !a.Allowed || !a.TPD.Reserved {
		t.Fatalf("first Admit = %+v (%v), want allowed with a day reservation", a, err)
	}
	// More than the whole day allowance, settled: the day balance goes
	// negative and the next day reservation must refuse (the in-memory bucket
	// admits while its balance is positive, so exactly the allowance would
	// still admit after a microsecond of refill).
	real := 15.0
	l.SettleTokens(ctx, "k", "", a.TPM, a.TPD, &real)
	if got := bucketTokens(l.tpmBuckets["k"]); got != minute-15 {
		t.Fatalf("minute tokens after settling 15 = %v, want %v", got, minute-15)
	}
	a, err = l.Admit(ctx, "k", "")
	if err != nil || a.Allowed || a.RefusedBy != "tpd" || a.TPM.Reserved || a.TPD.Reserved {
		t.Fatalf("second Admit = %+v (%v), want refused by tpd with nothing left reserved", a, err)
	}
	if a.RetryAfter < time.Hour {
		t.Fatalf("day refusal RetryAfter = %v, want the day bucket's hours-scale wait", a.RetryAfter)
	}
	if got := bucketTokens(l.tpmBuckets["k"]); got != minute-15 {
		t.Fatalf("minute tokens after a day refusal = %v, want %v: the minute reservation the refused request took is released", got, minute-15)
	}
	if got := bucketTokens(l.buckets["k"]); got != 1 {
		t.Fatalf("RPM tokens after a day refusal = %v, want 1 (refunded)", got)
	}
}

func TestAdmitRedisModeMakesOneBackendCallWithTheResolvedKeys(t *testing.T) {
	fb := &fakeBackend{admitFunc: func(req admission.Request) (admission.Result, error) {
		return admission.Result{Allowed: true, TPMCost: req.TPM.ColdStart, TPDCost: 7, TPDColdStart: 7}, nil
	}}
	l := NewRedisKeyLimiter([]KeyConfig{{
		ID: "k", Capacity: 100, RefillPerSecond: 5, TPMCapacity: 1000, TPMRefillPerSecond: 50, TokensPerDay: 86400,
		PerModel: map[string]ModelRateLimit{"gpt-4o": {Capacity: 7, RefillPerSecond: 1, TPMCapacity: 300, TPMRefillPerSecond: 3}},
	}}, fb)
	ctx := context.Background()

	a, err := l.Admit(ctx, "k", "gpt-4o")
	if err != nil || !a.Allowed || fb.admitCalls != 1 {
		t.Fatalf("Admit = %+v (%v), calls %d; want allowed from one backend call", a, err, fb.admitCalls)
	}
	req := fb.lastAdmit
	if req.RPMKey != "k\x00model=gpt-4o" || req.RPMCapacity != 7 || req.RPMRefillPerSecond != 1 {
		t.Errorf("RPM part = %q %v %v, want the per-model key, capacity 7, refill 1", req.RPMKey, req.RPMCapacity, req.RPMRefillPerSecond)
	}
	if req.TPM != (admission.Dimension{Key: "k\x00model=gpt-4o", Burst: 300, Rate: 3, ColdStart: 300}) {
		t.Errorf("TPM part = %+v, want the per-model override with the burst as cold start", req.TPM)
	}
	if req.TPD != (admission.Dimension{Key: "k\x00dim=day", Burst: 86400, Rate: 1, ColdStart: 3600}) {
		t.Errorf("TPD part = %+v, want the day key, 86400/1 and the 1/24 cold cap", req.TPD)
	}
	if a.TPM != (TokenReservation{Reserved: true, Tokens: 300}) || a.TPD != (TokenReservation{Reserved: true, Tokens: 7}) {
		t.Errorf("reservations = tpm %+v tpd %+v, want 300 and 7 reserved (epoch 0 in Redis mode)", a.TPM, a.TPD)
	}
	// Without a model the plain key and the key-level limits are used.
	if _, err := l.Admit(ctx, "k", ""); err != nil || fb.lastAdmit.RPMKey != "k" || fb.lastAdmit.RPMCapacity != 100 || fb.lastAdmit.TPM.Key != "k" || fb.lastAdmit.TPM.Burst != 1000 {
		t.Errorf("model-less Admit request = %+v (%v), want the key-level limits", fb.lastAdmit, err)
	}
	if fb.lastReserveKey != "" || fb.recordedKeyID != "" {
		t.Errorf("the gate must not fall back to the single-dimension Allow/ReserveTPM calls: %q %q", fb.recordedKeyID, fb.lastReserveKey)
	}
}

func TestAdmitRedisModeMapsRefusalsAndTheColdWait(t *testing.T) {
	cases := []struct {
		name   string
		result admission.Result
		want   Admission
	}{
		{"rpm", admission.Result{RefusedBy: "rpm"}, Admission{RefusedBy: "rpm"}},
		{"tpm keeps the backoff-only wait", admission.Result{RefusedBy: "tpm", RetryAfterSec: 5, TPMCost: 300}, Admission{RefusedBy: "tpm"}},
		{"tpd warm", admission.Result{RefusedBy: "tpd", RetryAfterSec: 3600, TPMCost: 300, TPDCost: 100, TPDColdStart: 3600}, Admission{RefusedBy: "tpd", RetryAfter: time.Hour}},
		{"tpd cold", admission.Result{RefusedBy: "tpd", RetryAfterSec: 3600, TPDCost: 3600, TPDColdStart: 3600}, Admission{RefusedBy: "tpd"}},
		// The tolerance itself: the script's %.14g rendering of 2 000 000/24 against Go's own float.
		{"tpd cold through %.14g", admission.Result{RefusedBy: "tpd", RetryAfterSec: 3600, TPDCost: 83333.333333333, TPDColdStart: 83333.33333333333}, Admission{RefusedBy: "tpd"}},
	}
	for _, c := range cases {
		fb := &fakeBackend{admitFunc: func(admission.Request) (admission.Result, error) { return c.result, nil }}
		l := NewRedisKeyLimiter([]KeyConfig{{ID: "k", Capacity: 100, RefillPerSecond: 100, TPMCapacity: 1000, TPMRefillPerSecond: 50, TokensPerDay: 2000000}}, fb)
		a, err := l.Admit(context.Background(), "k", "")
		if err != nil || a != c.want {
			t.Errorf("%s: Admit = %+v (%v), want %+v", c.name, a, err, c.want)
		}
	}
	failing := &fakeBackend{admitFunc: func(admission.Request) (admission.Result, error) { return admission.Result{}, errors.New("redis down") }}
	l := NewRedisKeyLimiter([]KeyConfig{{ID: "k", Capacity: 100, RefillPerSecond: 100}}, failing)
	if a, err := l.Admit(context.Background(), "k", ""); err == nil || a != (Admission{}) {
		t.Fatalf("a backend error must surface with an empty admission so the caller fails open: %+v, %v", a, err)
	}
}

func TestAdmitRedisModeLeavesUnconfiguredDimensionsOff(t *testing.T) {
	fb := &fakeBackend{admitFunc: func(admission.Request) (admission.Result, error) { return admission.Result{Allowed: true}, nil }}
	l := NewRedisKeyLimiter([]KeyConfig{{ID: "k", Capacity: 100, RefillPerSecond: 100}}, fb)
	a, err := l.Admit(context.Background(), "k", "")
	if err != nil || !a.Allowed || a.TPM.Reserved || a.TPD.Reserved {
		t.Fatalf("Admit = %+v (%v), want allowed with both token dimensions off", a, err)
	}
	if fb.lastAdmit.TPM.Burst != 0 || fb.lastAdmit.TPD.Burst != 0 {
		t.Fatalf("request = %+v, want Burst 0 (off) for both token dimensions", fb.lastAdmit)
	}
}

func TestSettleTokensRedisModeSettlesTheConfiguredDimensionsInOneCall(t *testing.T) {
	fb := &fakeBackend{}
	l := NewRedisKeyLimiter([]KeyConfig{{ID: "k", Capacity: 100, RefillPerSecond: 100, TPMCapacity: 1000, TPMRefillPerSecond: 100, TokensPerDay: 86400}}, fb)
	ctx := context.Background()
	real := 100.0
	l.SettleTokens(ctx, "k", "", TokenReservation{Reserved: true, Tokens: 1000}, TokenReservation{Reserved: true, Tokens: 3600}, &real)
	if fb.settleTokensCalls != 1 || len(fb.lastSettlements) != 2 {
		t.Fatalf("settle calls = %d with %d entries, want one call carrying both dimensions", fb.settleTokensCalls, len(fb.lastSettlements))
	}
	m, d := fb.lastSettlements[0], fb.lastSettlements[1]
	if m.Key != "k" || m.DeltaIncrement != (1000.0/100)*(100-1000) || m.RealTokens == nil || *m.RealTokens != 100 {
		t.Errorf("minute settlement = %+v, want key k, delta -9000 ms, real 100", m)
	}
	if d.Key != "k\x00dim=day" || d.DeltaIncrement != (1000.0/1)*(100-3600) || d.RealTokens == nil || *d.RealTokens != 100 {
		t.Errorf("day settlement = %+v, want the day key, delta -3 500 000 ms, real 100", d)
	}
	// A release: both reserved, no real count.
	l.SettleTokens(ctx, "k", "", TokenReservation{Reserved: true, Tokens: 1000}, TokenReservation{Reserved: true, Tokens: 3600}, nil)
	if fb.settleTokensCalls != 2 || fb.lastSettlements[0].RealTokens != nil || fb.lastSettlements[0].DeltaIncrement != -10000 || fb.lastSettlements[1].DeltaIncrement != -3600000 {
		t.Errorf("release = %+v, want -10000 and -3 600 000 ms with no real count", fb.lastSettlements)
	}
	// A dimension with no reservation and no real count has nothing to settle;
	// one with a real count but no reservation (it failed open) still records it.
	l.SettleTokens(ctx, "k", "", TokenReservation{}, TokenReservation{}, nil)
	if fb.settleTokensCalls != 2 {
		t.Errorf("nothing to settle must make no call, got %d calls", fb.settleTokensCalls)
	}
	l.SettleTokens(ctx, "k", "", TokenReservation{}, TokenReservation{Reserved: true, Tokens: 3600}, &real)
	if fb.settleTokensCalls != 3 || len(fb.lastSettlements) != 2 || fb.lastSettlements[0].DeltaIncrement != (1000.0/100)*100 {
		t.Errorf("fail-open minute + reserved day = %+v, want two entries, the minute one recording 100 real tokens against no reservation", fb.lastSettlements)
	}
	// Unconfigured dimensions are never in the call.
	only := &fakeBackend{}
	lo := NewRedisKeyLimiter([]KeyConfig{{ID: "k", Capacity: 100, RefillPerSecond: 100, TPMCapacity: 1000, TPMRefillPerSecond: 100}}, only)
	lo.SettleTokens(ctx, "k", "", TokenReservation{Reserved: true, Tokens: 1000}, TokenReservation{}, &real)
	if only.settleTokensCalls != 1 || len(only.lastSettlements) != 1 || only.lastSettlements[0].Key != "k" {
		t.Errorf("TPM-only key settlement = %+v, want one entry for the minute key", only.lastSettlements)
	}
	if fb.lastSettleKey != "" || only.lastSettleKey != "" {
		t.Errorf("SettleTokens must not fall back to the single-dimension SettleTPM: %q %q", fb.lastSettleKey, only.lastSettleKey)
	}
}
