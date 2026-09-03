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
