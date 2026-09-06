package ratelimit

import (
	"context"
	"errors"
	"testing"
)

// fakeBackend is a small in-memory RedisBackend test double, keeping
// this package's own tests independent of internal/ratelimit/redislimiter
// (that package's real, Redis-backed tests live there) — the same
// dependency-direction discipline internal/budget's own tests use
// against a fake budget.Store, rather than boltstore.
type fakeBackend struct {
	allowFunc func(ctx context.Context, keyID string, capacity, refillPerSecond float64) (bool, error)
	closeErr  error
	closed    bool

	// recorded captures the last call's arguments, so tests can assert
	// KeyLimiter passed the right per-key Capacity/RefillPerSecond
	// through, not just that Allow was called at all.
	recordedKeyID           string
	recordedCapacity        float64
	recordedRefillPerSecond float64
}

func (f *fakeBackend) Allow(ctx context.Context, keyID string, capacity, refillPerSecond float64) (bool, error) {
	f.recordedKeyID = keyID
	f.recordedCapacity = capacity
	f.recordedRefillPerSecond = refillPerSecond
	if f.allowFunc != nil {
		return f.allowFunc(ctx, keyID, capacity, refillPerSecond)
	}
	return true, nil
}

func (f *fakeBackend) Close() error {
	f.closed = true
	return f.closeErr
}

func TestNewInMemoryKeyLimiterBehavesLikeDirectTokenBucket(t *testing.T) {
	l := NewInMemoryKeyLimiter([]KeyConfig{{ID: "team-alpha", Capacity: 2, RefillPerSecond: 0}})

	ctx := context.Background()

	for i := 0; i < 2; i++ {
		allowed, err := l.Allow(ctx, "team-alpha")
		if err != nil {
			t.Fatalf("Allow() #%d error = %v, want nil (in-memory TokenBucket.Allow never errors)", i+1, err)
		}
		if !allowed {
			t.Fatalf("Allow() #%d = false, want true (within capacity)", i+1)
		}
	}

	if allowed, err := l.Allow(ctx, "team-alpha"); err != nil {
		t.Fatalf("Allow() error = %v", err)
	} else if allowed {
		t.Fatal("Allow() succeeded after capacity exhausted, with zero refill configured")
	}
}

func TestNewRedisKeyLimiterPassesCorrectPerKeyConfigToBackend(t *testing.T) {
	backend := &fakeBackend{}
	l := NewRedisKeyLimiter([]KeyConfig{
		{ID: "team-alpha", Capacity: 20, RefillPerSecond: 10},
		{ID: "team-beta", Capacity: 5, RefillPerSecond: 2.5},
	}, backend)

	ctx := context.Background()

	if _, err := l.Allow(ctx, "team-beta"); err != nil {
		t.Fatalf("Allow() error = %v", err)
	}
	if backend.recordedKeyID != "team-beta" {
		t.Errorf("recordedKeyID = %q, want %q", backend.recordedKeyID, "team-beta")
	}
	if backend.recordedCapacity != 5 {
		t.Errorf("recordedCapacity = %v, want 5 (team-beta's own configured burst, not team-alpha's)", backend.recordedCapacity)
	}
	if backend.recordedRefillPerSecond != 2.5 {
		t.Errorf("recordedRefillPerSecond = %v, want 2.5", backend.recordedRefillPerSecond)
	}
}

func TestRedisModeErrorPropagatesUnchanged(t *testing.T) {
	wantErr := errors.New("boom")
	backend := &fakeBackend{allowFunc: func(ctx context.Context, keyID string, capacity, refillPerSecond float64) (bool, error) {
		return false, wantErr
	}}
	l := NewRedisKeyLimiter([]KeyConfig{{ID: "team-alpha", Capacity: 1, RefillPerSecond: 1}}, backend)

	_, err := l.Allow(context.Background(), "team-alpha")
	if !errors.Is(err, wantErr) {
		t.Fatalf("Allow() error = %v, want %v — KeyLimiter must pass backend errors through unchanged, not decide fail-open/closed itself", err, wantErr)
	}
}

func TestCloseOnlyCallsBackendInRedisMode(t *testing.T) {
	backend := &fakeBackend{}
	redisLimiter := NewRedisKeyLimiter([]KeyConfig{{ID: "k", Capacity: 1, RefillPerSecond: 1}}, backend)
	if err := redisLimiter.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !backend.closed {
		t.Error("Close() on a Redis-backed KeyLimiter did not close its backend")
	}

	inMemoryLimiter := NewInMemoryKeyLimiter([]KeyConfig{{ID: "k", Capacity: 1, RefillPerSecond: 1}})
	if err := inMemoryLimiter.Close(); err != nil {
		t.Fatalf("Close() on an in-memory KeyLimiter error = %v, want nil (no-op)", err)
	}
}

// TestInMemoryAllowOnUnregisteredKeyDeniesRatherThanPanics proves the real
// hazard docs/rfcs/2026-09-05-gateway-admin-api.md's Register method
// exists to close: before that pass, Allow on a keyID nothing ever built
// a bucket for dereferenced a nil *TokenBucket's own mutex and panicked.
// This can only happen once a caller (the admin API) can make identity
// and the rate limiter diverge — never possible with a config-built
// KeyLimiter alone, since it and identity.Verifier are always built from
// the same key list in lockstep.
func TestInMemoryAllowOnUnregisteredKeyDeniesRatherThanPanics(t *testing.T) {
	l := NewInMemoryKeyLimiter([]KeyConfig{{ID: "team-alpha", Capacity: 5, RefillPerSecond: 1}})

	allowed, err := l.Allow(context.Background(), "never-registered")
	if err != nil {
		t.Fatalf("Allow() error = %v, want nil", err)
	}
	if allowed {
		t.Fatal("Allow() on an unregistered key = true, want false (deny, never a fabricated allow)")
	}
}

func TestRegisterInMemoryModeMakesANewKeyImmediatelyUsable(t *testing.T) {
	l := NewInMemoryKeyLimiter([]KeyConfig{{ID: "team-alpha", Capacity: 5, RefillPerSecond: 1}})

	l.Register(KeyConfig{ID: "team-beta", Capacity: 2, RefillPerSecond: 0})

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		allowed, err := l.Allow(ctx, "team-beta")
		if err != nil || !allowed {
			t.Fatalf("Allow() #%d = (%v, %v), want (true, nil) — Register should have given team-beta its own 2-token bucket", i+1, allowed, err)
		}
	}
	if allowed, _ := l.Allow(ctx, "team-beta"); allowed {
		t.Fatal("Allow() succeeded after team-beta's registered 2-token capacity was exhausted")
	}
}

func TestRegisterInMemoryModeResetsAnExistingKeysBucketToFullCapacity(t *testing.T) {
	l := NewInMemoryKeyLimiter([]KeyConfig{{ID: "team-alpha", Capacity: 1, RefillPerSecond: 0}})
	ctx := context.Background()

	if allowed, _ := l.Allow(ctx, "team-alpha"); !allowed {
		t.Fatal("first Allow() should have succeeded (fresh bucket)")
	}
	if allowed, _ := l.Allow(ctx, "team-alpha"); allowed {
		t.Fatal("second Allow() should have failed (capacity exhausted)")
	}

	// An admin update to the same key ID -- even with the identical
	// capacity -- is a deliberate reset to full capacity, not a
	// no-op merge with whatever tokens happened to be left.
	l.Register(KeyConfig{ID: "team-alpha", Capacity: 1, RefillPerSecond: 0})

	if allowed, _ := l.Allow(ctx, "team-alpha"); !allowed {
		t.Fatal("Allow() after Register() = false, want true (Register resets to full capacity)")
	}
}

func TestRegisterRedisModeUpdatesTheConfigBackendSees(t *testing.T) {
	backend := &fakeBackend{}
	l := NewRedisKeyLimiter([]KeyConfig{{ID: "team-alpha", Capacity: 1, RefillPerSecond: 1}}, backend)

	l.Register(KeyConfig{ID: "team-beta", Capacity: 9, RefillPerSecond: 3})

	if _, err := l.Allow(context.Background(), "team-beta"); err != nil {
		t.Fatalf("Allow() error = %v", err)
	}
	if backend.recordedCapacity != 9 || backend.recordedRefillPerSecond != 3 {
		t.Fatalf("backend saw capacity=%v refill=%v, want 9/3 — Register() must reach the backend-facing configs map, not just the in-memory bucket map",
			backend.recordedCapacity, backend.recordedRefillPerSecond)
	}
}

// TestAllowTPMUnlimitedWhenNotConfigured proves the "0/absent =
// unlimited" convention: a key with no TPMCapacity configured must
// never be blocked by the TPM dimension at all.
func TestAllowTPMUnlimitedWhenNotConfigured(t *testing.T) {
	l := NewInMemoryKeyLimiter([]KeyConfig{{ID: "team-alpha", Capacity: 100, RefillPerSecond: 100}})
	for i := 0; i < 5; i++ {
		if !l.AllowTPM("team-alpha") {
			t.Fatalf("AllowTPM() call #%d = false, want true (TPM not configured)", i+1)
		}
	}
}

// TestRecordTokensExhaustsTPMBucketThenAllowTPMRejects is the
// load-bearing proof for docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md:
// once RecordTokens has debited more than TPMCapacity's worth of
// tokens, AllowTPM must reject the next request — purely from past
// usage, since no request's own future cost is ever known in advance.
func TestRecordTokensExhaustsTPMBucketThenAllowTPMRejects(t *testing.T) {
	l := NewInMemoryKeyLimiter([]KeyConfig{{ID: "team-alpha", Capacity: 100, RefillPerSecond: 100, TPMCapacity: 1000, TPMRefillPerSecond: 0}})

	if !l.AllowTPM("team-alpha") {
		t.Fatal("AllowTPM() = false before any usage, want true")
	}
	l.RecordTokens("team-alpha", 1500) // more than TPMCapacity — a real overdraft
	if l.AllowTPM("team-alpha") {
		t.Fatal("AllowTPM() = true after debiting more tokens than TPMCapacity, want false")
	}
}

// TestRecordTokensNoOpWhenTPMNotConfigured proves RecordTokens never
// panics or has any effect for a key with no TPM bucket.
func TestRecordTokensNoOpWhenTPMNotConfigured(t *testing.T) {
	l := NewInMemoryKeyLimiter([]KeyConfig{{ID: "team-alpha", Capacity: 100, RefillPerSecond: 100}})
	l.RecordTokens("team-alpha", 999999)
	if !l.AllowTPM("team-alpha") {
		t.Fatal("AllowTPM() = false after RecordTokens on an unconfigured key, want true (no-op)")
	}
}

// TestAllowTPMAlwaysUnlimitedInRedisMode proves the RFC's explicit v1
// scope limit: TPM is in-memory-only, and a Redis-mode KeyLimiter never
// enforces it, even if TPMCapacity is configured.
func TestAllowTPMAlwaysUnlimitedInRedisMode(t *testing.T) {
	backend := &fakeBackend{}
	l := NewRedisKeyLimiter([]KeyConfig{{ID: "team-alpha", Capacity: 1, RefillPerSecond: 1, TPMCapacity: 1, TPMRefillPerSecond: 0}}, backend)

	l.RecordTokens("team-alpha", 999999)
	if !l.AllowTPM("team-alpha") {
		t.Fatal("AllowTPM() = false in Redis mode, want true — TPM is a deliberate no-op in Redis mode in v1")
	}
}

// TestRegisterDisablingTPMRemovesTheStaleBucket proves an update that
// stops configuring TPM (TPMCapacity <= 0) actually removes the old
// limit, rather than leaving a stale bucket that keeps enforcing it.
func TestRegisterDisablingTPMRemovesTheStaleBucket(t *testing.T) {
	l := NewInMemoryKeyLimiter([]KeyConfig{{ID: "team-alpha", Capacity: 100, RefillPerSecond: 100, TPMCapacity: 10, TPMRefillPerSecond: 0}})
	l.RecordTokens("team-alpha", 20) // exhaust the TPM bucket
	if l.AllowTPM("team-alpha") {
		t.Fatal("AllowTPM() = true before Register(), want false (bucket should be exhausted)")
	}

	l.Register(KeyConfig{ID: "team-alpha", Capacity: 100, RefillPerSecond: 100}) // TPMCapacity omitted = disabled

	if !l.AllowTPM("team-alpha") {
		t.Fatal("AllowTPM() = false after Register() disabled TPM, want true — the stale bucket must be removed, not left enforcing an old limit")
	}
}
