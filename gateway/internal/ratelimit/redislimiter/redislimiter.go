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
	"net/url"
	"strconv"
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

// tpmLuaSrc implements GCRA (the generic cell rate algorithm, a.k.a.
// virtual scheduling) for the TPM dimension — a deliberately DIFFERENT
// algorithm shape from luaSrc's plain continuous-refill token bucket
// above, not a rewrite of it: TPM must support a variable,
// request-dependent cost (an LLM call's real token count), unlike RPM's
// fixed cost-of-1, and GCRA generalizes to a variable cost cleanly by
// scaling the per-unit emission_interval by that cost — the same
// pattern confirmed against real production implementations
// (go-redis/redis_rate, Scality Cloudserver's grantTokens.lua) that do
// exactly this, rather than a home-grown variant.
//
// Stores a single theoretical-arrival-time (TAT) scalar per key —
// GET/SET, never HMGET/HSET's multi-field shape luaSrc uses — since
// GCRA's whole state is one number, unlike a token bucket's
// tokens+last_refill_ms pair.
//
// Uses redis.call('TIME') (Redis's OWN server clock), not a
// client-supplied timestamp, so this script's notion of "now" stays
// consistent across concurrent replicas with skewed wall clocks —
// mirroring a real, live-discovered fix in a production LLM gateway's
// own TPM limiter (LiteLLM's parallel_request_limiter_v3.py comment:
// "Uses Redis server time... so that window resets are deterministic
// across replicas with skewed wall-clocks").
const tpmLuaSrc = `
local burst = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local period = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])

-- period is in SECONDS (matching AllowTPM's own periodSec parameter),
-- but now_ms/TAT arithmetic below is entirely in MILLISECONDS -- the
-- *1000 here is a real, required unit conversion, not decoration.
local emission_interval = (period * 1000) / rate
local increment = emission_interval * cost
local burst_offset = emission_interval * burst

local time = redis.call('TIME')
local now_ms = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)

local tat = redis.call('GET', KEYS[1])
if tat == false then
  tat = now_ms
else
  tat = tonumber(tat)
end
tat = math.max(tat, now_ms)

local new_tat = tat + increment
local allow_at = new_tat - burst_offset

if allow_at > now_ms then
  local retry_after_sec = (allow_at - now_ms) / 1000.0
  return {0, tostring(retry_after_sec)}
end

local ttl_ms = math.ceil(new_tat - now_ms) + 1000
redis.call('SET', KEYS[1], new_tat, 'PX', ttl_ms)
return {1, '0'}
`

// tpmAdjustLuaSrc reconciles a prior AllowTPM reservation with the real
// cost, once known: adds deltaIncrement (positive if the real cost
// exceeded the estimate, negative if it was less) onto the stored TAT.
// The result is clamped so TAT can never be pushed before "now" (via
// redis.call('TIME'), for the same clock-skew-safety reason as
// tpmLuaSrc) — time already consumed can't be un-consumed, so a large
// negative delta (a real cost far below the estimate) must not
// manufacture capacity from the past. A missing key (never reserved, or
// already expired) is a silent no-op — there is nothing to adjust.
const tpmAdjustLuaSrc = `
local delta_increment = tonumber(ARGV[1])

local tat = redis.call('GET', KEYS[1])
if tat == false then
  return redis.status_reply('OK')
end
tat = tonumber(tat)

local time = redis.call('TIME')
local now_ms = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)

local new_tat = math.max(tat + delta_increment, now_ms)
local ttl_ms = math.ceil(new_tat - now_ms) + 1000
redis.call('SET', KEYS[1], new_tat, 'PX', ttl_ms)
return redis.status_reply('OK')
`

// Limiter is a Redis-backed distributed rate limiter. The zero value is
// not usable — construct with Open.
type Limiter struct {
	client          *redis.Client
	script          *redis.Script
	tpmScript       *redis.Script
	tpmAdjustScript *redis.Script
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
	return &Limiter{
		client:          client,
		script:          redis.NewScript(luaSrc),
		tpmScript:       redis.NewScript(tpmLuaSrc),
		tpmAdjustScript: redis.NewScript(tpmAdjustLuaSrc),
	}, nil
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
	// url.QueryEscape closes a real HIGH-severity finding from this
	// session's own end-to-end audit: keyID has no charset restriction
	// anywhere, and this key used to be "ratelimit:" + keyID raw --
	// AllowTPM's own "ratelimit:tpm:" + key convention (see that
	// method's own doc comment) meant a keyID like "tpm:foo" produced
	// the IDENTICAL Redis key ("ratelimit:tpm:foo") as
	// AllowTPM("foo", ...) — but Allow's key is a Hash (HMGET/HSET) and
	// AllowTPM's is a String (GET/SET), an incompatible-type collision
	// that fails outright with a Redis WRONGTYPE error the first time
	// both dimensions touch the aliased key. QueryEscape encodes every
	// ':' as "%3A" (not in its unreserved charset), so an escaped keyID
	// can never contain a raw ':' — meaning this key can never
	// coincidentally start with "ratelimit:tpm:" the way a raw keyID
	// could.
	key := "ratelimit:" + url.QueryEscape(keyID)
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

// AllowTPM reports whether cost (a variable, request-dependent token
// count) may be admitted against key's TPM budget right now, via a GCRA
// admission check — burst/rate/periodSec configure the same shape
// internal/ratelimit.TokenBucket's TPM dimension already exposes
// (capacity, refill-per-period), just enforced through Redis instead of
// an in-process *TokenBucket. key is the caller's own choice of Redis
// key suffix (a plain keyID for a key-level default, or a per-model
// variant) — this method itself only ever prefixes it with
// "ratelimit:tpm:", mirroring Allow's identical "ratelimit:" + keyID
// convention for the RPM dimension, just under a distinct namespace so
// the two dimensions never collide even when a caller reuses the same
// keyID/model pair for both. A non-nil error means the Redis call
// itself failed; callers should treat that the same fail-open way
// Allow's own doc comment already establishes.
func (l *Limiter) AllowTPM(ctx context.Context, key string, burst, rate, periodSec, cost float64) (allowed bool, retryAfterSec float64, err error) {
	// url.QueryEscape here for the identical reason Allow's own call
	// site now escapes keyID — see that comment for the full
	// Hash-vs-String collision this closes on both sides.
	redisKey := "ratelimit:tpm:" + url.QueryEscape(key)
	val, err := l.tpmScript.Run(ctx, l.client, []string{redisKey}, burst, rate, periodSec, cost).Result()
	if err != nil {
		return false, 0, fmt.Errorf("redislimiter: running TPM script for key %q: %w", key, err)
	}

	results, ok := val.([]interface{})
	if !ok || len(results) != 2 {
		return false, 0, fmt.Errorf("redislimiter: unexpected TPM script result shape %v for key %q", val, key)
	}
	allowedInt, ok := results[0].(int64)
	if !ok {
		return false, 0, fmt.Errorf("redislimiter: unexpected TPM script result[0] type %T for key %q", results[0], key)
	}
	retryAfterStr, ok := results[1].(string)
	if !ok {
		return false, 0, fmt.Errorf("redislimiter: unexpected TPM script result[1] type %T for key %q", results[1], key)
	}
	retryAfterSec, err = strconv.ParseFloat(retryAfterStr, 64)
	if err != nil {
		return false, 0, fmt.Errorf("redislimiter: parsing TPM retryAfterSec %q for key %q: %w", retryAfterStr, key, err)
	}
	return allowedInt == 1, retryAfterSec, nil
}

// AdjustTPM reconciles a prior AllowTPM reservation for key with the
// real cost, once known — see tpmAdjustLuaSrc's own doc comment for the
// exact semantics. deltaIncrement is the DIFFERENCE (emission_interval *
// (realCost - estimatedCost)) the caller computes, not the real cost
// itself.
func (l *Limiter) AdjustTPM(ctx context.Context, key string, deltaIncrement float64) error {
	redisKey := "ratelimit:tpm:" + url.QueryEscape(key)
	if _, err := l.tpmAdjustScript.Run(ctx, l.client, []string{redisKey}, deltaIncrement).Result(); err != nil {
		return fmt.Errorf("redislimiter: running TPM adjust script for key %q: %w", key, err)
	}
	return nil
}

// Close closes the underlying Redis client.
func (l *Limiter) Close() error {
	return l.client.Close()
}
