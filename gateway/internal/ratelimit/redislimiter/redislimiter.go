// Package redislimiter implements internal/ratelimit.RedisBackend over
// Redis, running the exact same continuous-refill token-bucket algorithm
// as internal/ratelimit.TokenBucket, but atomically across any number of
// gateway instances via a single Lua script (EVALSHA, with go-redis's
// built-in fallback to EVAL on a NOSCRIPT miss).
//
// Per docs/rfcs/2026-09-03-distributed-rate-limiting.md, this package
// deliberately does not import internal/ratelimit — *Limiter satisfies
// that package's RedisBackend interface structurally, the same
// interface-lives-in-the-consumer idiom internal/budget/boltstore
// already established for internal/budget.Store.
package redislimiter

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// luaSrc ports internal/ratelimit.TokenBucket.Allow/refillLocked's exact
// math into a single atomic script: refill for elapsed time (capped at
// capacity), then attempt to consume one token. The elapsed-time clock
// (ARGV[3]) is always supplied by the Go caller, never Lua's own
// os.time() (1-second resolution, and this script's execution must stay
// deterministic given its inputs) — matching TokenBucket's own injectable
// clock, which exists for exactly the same determinism reason.
//
// The EXPIRE on every call — including a rejected one, so an actively
// contested key's TTL keeps refreshing — lets an idle key's bucket state
// auto-expire from Redis rather than accumulating forever; the TTL is at
// least one full refill cycle (2×capacity/refillPerSecond) so a key that
// goes briefly quiet doesn't lose its earned burst before it would have
// refilled anyway.
const luaSrc = `
local capacity = tonumber(ARGV[1])
local refill_per_second = tonumber(ARGV[2])
local now = tonumber(ARGV[3])

local state = redis.call('HMGET', KEYS[1], 'tokens', 'last_refill_ms')
local tokens = tonumber(state[1])
local last_refill_ms = tonumber(state[2])
if tokens == nil then
  tokens = capacity
  last_refill_ms = now
end

local elapsed_seconds = math.max(0, (now - last_refill_ms) / 1000.0)
tokens = math.min(capacity, tokens + elapsed_seconds * refill_per_second)

local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end

redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'last_refill_ms', now)
redis.call('EXPIRE', KEYS[1], math.max(60, math.ceil(2 * capacity / math.max(refill_per_second, 0.001))))
return allowed
`

// Limiter is a Redis-backed distributed rate limiter. The zero value is
// not usable — construct with Open.
type Limiter struct {
	client *redis.Client
	script *redis.Script
}

// Open constructs a Limiter against the Redis server at addr
// ("host:port"). go-redis dials lazily — the first real connection
// attempt happens on the first Allow call, not here — so an unreachable
// addr does not make Open itself fail; this is confirmed by
// TestOpenNeverFailsOnUnreachableAddr, not merely assumed, since it's the
// property this RFC's fail-open policy depends on (Open must succeed
// even if Redis is down, so the gateway itself can still start).
func Open(addr string) (*Limiter, error) {
	client := redis.NewClient(&redis.Options{Addr: addr})
	return &Limiter{client: client, script: redis.NewScript(luaSrc)}, nil
}

// Allow reports whether one request against keyID may proceed, given
// that key's configured capacity (burst) and refillPerSecond — the exact
// same parameters internal/ratelimit.NewTokenBucket takes. A non-nil
// error means the Redis call itself failed (network error, timeout,
// script error); callers implementing this RFC's fail-open policy should
// treat that as "allow, but log," not "reject" — see
// docs/rfcs/2026-09-03-distributed-rate-limiting.md's "Fail-open, not
// fail-closed" section for why that's the right default for Kelvran.
func (l *Limiter) Allow(ctx context.Context, keyID string, capacity, refillPerSecond float64) (bool, error) {
	key := "ratelimit:" + keyID
	now := time.Now().UnixMilli()

	val, err := l.script.Run(ctx, l.client, []string{key}, capacity, refillPerSecond, now).Result()
	if err != nil {
		return false, fmt.Errorf("redislimiter: running script for key %q: %w", keyID, err)
	}

	// Lua integer replies decode to int64 via go-redis's RESP reader, not
	// int or float64 — a wrong type assertion here would panic at
	// runtime rather than fail a build, so this is asserted explicitly
	// rather than blindly cast.
	allowed, ok := val.(int64)
	if !ok {
		return false, fmt.Errorf("redislimiter: unexpected script result type %T for key %q", val, keyID)
	}
	return allowed == 1, nil
}

// Close closes the underlying Redis client.
func (l *Limiter) Close() error {
	return l.client.Close()
}
