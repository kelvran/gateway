package ratelimit

import "context"

// KeyConfig is one virtual key's rate-limit parameters — the same
// burst/refill values controlplane.VirtualKeyConfig carries, passed here
// without either package depending on the other, matching this
// project's existing budget/identity decoupling rule
// (gateway/ARCHITECTURE.md's dependency-direction table).
type KeyConfig struct {
	ID              string
	Capacity        float64
	RefillPerSecond float64
}

// RedisBackend is implemented by internal/ratelimit/redislimiter.Limiter.
// Optional — a KeyLimiter built via NewInMemoryKeyLimiter never touches
// this, mirroring internal/budget.Store's relationship to
// internal/budget/boltstore: the interface lives here, in the consumer
// package, and the implementation package does not import this one.
type RedisBackend interface {
	Allow(ctx context.Context, keyID string, capacity, refillPerSecond float64) (bool, error)
	Close() error
}

// KeyLimiter is the single per-Pipeline rate limiter, backed by either
// in-memory TokenBuckets (default, per NewInMemoryKeyLimiter) or a
// RedisBackend (per NewRedisKeyLimiter, per
// docs/rfcs/2026-09-03-distributed-rate-limiting.md). Callers never need
// to know which — both are driven through the same Allow method.
type KeyLimiter struct {
	configs map[string]KeyConfig
	buckets map[string]*TokenBucket // non-nil in in-memory mode only
	backend RedisBackend            // non-nil in Redis mode only
}

// NewInMemoryKeyLimiter builds a KeyLimiter backed by one TokenBucket per
// key, eagerly constructed here — the exact behavior
// dataplane.NewPipeline built inline before this RFC.
func NewInMemoryKeyLimiter(keys []KeyConfig) *KeyLimiter {
	buckets := make(map[string]*TokenBucket, len(keys))
	for _, k := range keys {
		buckets[k.ID] = NewTokenBucket(k.Capacity, k.RefillPerSecond)
	}
	return &KeyLimiter{buckets: buckets}
}

// NewRedisKeyLimiter builds a KeyLimiter that delegates every Allow call
// to backend, passing each key's own Capacity/RefillPerSecond through on
// every call (backend holds no per-key state itself — Redis does).
func NewRedisKeyLimiter(keys []KeyConfig, backend RedisBackend) *KeyLimiter {
	configs := make(map[string]KeyConfig, len(keys))
	for _, k := range keys {
		configs[k.ID] = k
	}
	return &KeyLimiter{configs: configs, backend: backend}
}

// Allow reports whether one request against keyID may proceed. In Redis
// mode, a non-nil error means the backend call itself failed (e.g. a
// network error) — internal/ratelimit.KeyLimiter does not decide what to
// do about that; per docs/rfcs/2026-09-03-distributed-rate-limiting.md,
// that policy decision (fail-open, with a warning log) belongs to the
// caller (dataplane.Pipeline.checkRateLimit), not this package, so this
// method stays a faithful pass-through rather than baking in one
// caller's specific policy.
func (l *KeyLimiter) Allow(ctx context.Context, keyID string) (bool, error) {
	if l.backend != nil {
		cfg := l.configs[keyID]
		return l.backend.Allow(ctx, keyID, cfg.Capacity, cfg.RefillPerSecond)
	}
	return l.buckets[keyID].Allow(), nil
}

// Close releases the backend's resources, if any. A no-op in in-memory
// mode (TokenBucket owns nothing that needs closing).
func (l *KeyLimiter) Close() error {
	if l.backend != nil {
		return l.backend.Close()
	}
	return nil
}
