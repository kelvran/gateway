- **Status**: accepted
- **Date**: 2026-09-03
- **Author(s)**: project founder + Claude Code

## Summary

Give the gateway an optional, Redis-backed rate limiter that enforces each virtual key's burst/refill cap correctly across multiple gateway instances, alongside the existing single-process, in-memory `ratelimit.TokenBucket`. Selection is a config-time choice: a new `rate_limit.redis_addr` field unset means today's exact in-memory behavior; set, it swaps to a Redis-backed limiter running the same token-bucket algorithm atomically via a Lua script. Same granularity as today — per virtual key, burst + refill-per-second — no hierarchical scope (org/team/user/session) added in this pass; that remains explicitly deferred per `docs/rfcs/2026-09-02-virtual-keys-budgets.md`'s own scope boundary.

## Motivation

`internal/ratelimit`'s own package doc comment has said, since the initial scaffolding, that distributed rate limiting is "Phase 1" and that today's `TokenBucket` is "deliberately single-process only." That's not a cosmetic gap: an in-memory token bucket is *local* state. Run two gateway instances behind a load balancer, and each instance independently grants a full burst to the same virtual key — the effective cap a key can spend against becomes `burst × instance count`, silently, with no error or warning. `THREAT_MODEL.md`'s LLM10 (Unbounded Consumption) row already names rate limiting as one of two controls (alongside budgets) that exist specifically to bound this — and this gap means that control quietly stops doing its job the moment Kelvran is ever run at more than one instance, which is the entire point of calling something a "gateway" rather than a single server.

This RFC closes that gap for the one dimension it currently exists for (per-key burst/refill), following the same "ship the minimal real thing, document the upgrade path" pattern as `docs/rfcs/2026-09-03-budget-persistence.md`.

## Detailed Design

### Algorithm: Lua token-bucket script, not GCRA

Three real options were checked for atomic distributed rate limiting over Redis, not two: a hand-rolled Lua script implementing token-bucket math (single EVALSHA round-trip); GCRA via the `redis-cell` Redis *module* (`CL.THROTTLE`); and `github.com/go-redis/redis_rate`, an official, well-tested, pure-Lua GCRA implementation from the go-redis org itself that needs no custom module. The third option is the one worth taking seriously — per this project's own "search before building" principle, reaching for a battle-tested library beats hand-rolling and re-verifying a Lua script, *if* it actually fits.

It doesn't, for one concrete, verified reason (checked against the library's real type definitions via context7, not assumed): `redis_rate.Limit{Rate, Burst int; Period time.Duration}` — `Rate` and `Burst` are `int`, not `float64`. Kelvran's `identity.VirtualKey.RateLimitBurst`/`RateLimitRefill` and `controlplane.VirtualKeyConfig`'s same fields are deliberately `float64`, matching the in-memory `TokenBucket`'s own float64 `capacity`/`refillPerSecond` fields — which exist specifically to support fractional rates (e.g. `refill_per_second: 0.5` for a very low-tier key), a real, already-possible config value with the in-memory limiter today. Routing that same config through `redis_rate` would silently round it to an integer, meaning the *exact same config value* would rate-limit differently depending on whether `rate_limit.redis_addr` is set — a direct violation of this RFC's own Global Constraint that switching backends must never change rate-limiting behavior, only its distribution/durability properties. A rescaling workaround (e.g. `Rate = refillPerSecond * 1000, Period = 1000×time.Second`) recovers some precision but is still lossy at the margins and adds more conceptual indirection than the hand-rolled script it would replace — not a clear win.

(For the record, correcting an earlier framing: GCRA is not disqualified by an inability to report remaining capacity — `redis_rate.Result.Remaining` exposes exactly that, verified against the library's real `Result` struct. The float64-vs-int type mismatch is the actual, narrower reason this option doesn't fit, not a general claim that GCRA implementations lack observability.)

`redis-cell` (the module-based GCRA option) is rejected for the reason already stated: it requires a custom Redis module most managed Redis offerings don't ship, a real deployment-portability cost neither of the other two options has.

This RFC ports the *exact* algorithm already implemented in `internal/ratelimit.TokenBucket.Allow`/`refillLocked` into a Lua script, so in-memory and Redis-backed modes are behaviorally identical (same refill formula, same 1-token-per-request cost, same "advance `lastRefill` even on a rejected call as long as time elapsed" behavior) — switching backends changes durability/distribution properties, never rate-limiting semantics.

```lua
-- KEYS[1] = "ratelimit:{virtualKeyID}"
-- ARGV[1] = capacity (float, burst)
-- ARGV[2] = refill_per_second (float)
-- ARGV[3] = now_unix_millis (integer) -- passed from Go, never Lua's own
--           os.time() (1-second resolution, and Redis Lua execution must
--           stay deterministic across any replica, so wall-clock reads
--           happen only on the caller's side)
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
```

The `EXPIRE` on every call (idle keys' bucket state auto-expires instead of accumulating in Redis forever; a TTL of `max(60s, 2×capacity/refill)` guarantees a key's state outlives at least one full refill cycle, so a key that goes quiet doesn't lose its earned burst prematurely) and the single-round-trip atomicity (no read-then-write race between two gateway instances hitting the same key concurrently) are the two properties this whole RFC exists to add over the in-memory version.

### go-redis v9 wiring

```go
script := redis.NewScript(luaSrc) // client-side SHA-1, computed once at construction
val, err := script.Run(ctx, rdb, []string{key}, capacity, refillPerSecond, nowUnixMillis).Result()
allowed, ok := val.(int64) // Lua integer replies decode to int64 via go-redis's RESP reader, not int/float64
```

`Script.Run` already performs EVALSHA with automatic fallback to EVAL on a `NOSCRIPT` error internally — this RFC does not hand-roll that fallback. Verified against the real go-redis v9 source (not assumed from memory). Two real gotchas worth stating explicitly since they're the kind of thing that silently panics at runtime rather than failing a build: Lua integer replies decode to `int64`, not `int`/`float64` — the type assertion above must be exact; and Redis executes Lua scripts single-threaded, so the script body must stay short (this one is O(1) — no loops, no scans) since a slow script blocks every other Redis operation, not just this call.

### `internal/ratelimit`: `KeyLimiter` and the `RedisBackend` seam

`TokenBucket` itself is untouched — zero churn to its existing API or test suite. A new type sits one layer above it:

```go
// internal/ratelimit/limiter.go (new)

// KeyConfig is one virtual key's rate-limit parameters — the same
// RateLimitBurst/RateLimitRefill values controlplane.VirtualKeyConfig
// already carries, passed here without either package depending on the
// other (mirroring the existing budget/identity decoupling rule in
// gateway/ARCHITECTURE.md's dependency-direction table).
type KeyConfig struct {
    ID              string
    Capacity        float64
    RefillPerSecond float64
}

// RedisBackend is implemented by internal/ratelimit/redislimiter.Limiter.
// Optional — a KeyLimiter built via NewInMemoryKeyLimiter never touches
// this, exactly mirroring internal/budget.Store's relationship to
// internal/budget/boltstore.
type RedisBackend interface {
    Allow(ctx context.Context, keyID string, capacity, refillPerSecond float64) (bool, error)
    Close() error
}

// KeyLimiter is the single per-Pipeline rate limiter, backed by either
// in-memory TokenBuckets (default) or a RedisBackend (when configured).
// Callers never need to know which.
type KeyLimiter struct {
    configs map[string]KeyConfig
    buckets map[string]*TokenBucket // non-nil in in-memory mode only
    backend RedisBackend            // non-nil in Redis mode only
}

func NewInMemoryKeyLimiter(keys []KeyConfig) *KeyLimiter { /* today's exact eager-build behavior */ }
func NewRedisKeyLimiter(keys []KeyConfig, backend RedisBackend) *KeyLimiter { /* no buckets built; backend holds all state */ }

func (l *KeyLimiter) Allow(ctx context.Context, keyID string) (bool, error) {
    if l.backend != nil {
        cfg := l.configs[keyID]
        return l.backend.Allow(ctx, keyID, cfg.Capacity, cfg.RefillPerSecond)
    }
    return l.buckets[keyID].Allow(), nil // in-memory TokenBucket.Allow never errors
}

func (l *KeyLimiter) Close() error {
    if l.backend != nil {
        return l.backend.Close()
    }
    return nil
}
```

`internal/ratelimit/redislimiter` (new subpackage, mirroring `internal/budget/boltstore`'s precedent exactly) owns the go-redis dependency and the Lua script, and structurally satisfies `ratelimit.RedisBackend` without importing `internal/ratelimit` at all — the same "interface lives in the consumer package, implementation package doesn't import it" idiom `budget.Store`/`boltstore.Store` already established. This keeps `internal/ratelimit` itself free of the go-redis dependency in the common (in-memory) case, and keeps `gateway/ARCHITECTURE.md`'s existing dependency-direction rules intact.

### `dataplane.Pipeline` wiring and fail-open behavior

`Pipeline.limiters map[string]*ratelimit.TokenBucket` becomes `Pipeline.limiter *ratelimit.KeyLimiter` — one field, not a map, since `KeyLimiter` already holds its own per-key map internally. The two call sites (`dataplane.go`'s `HandleChatCompletion` and `streaming.go`'s `HandleChatCompletionStream`) both currently do `if !p.limiterFor(vk.ID).Allow() { err = ErrRateLimited; return }`; both become a call to one new shared helper:

```go
// checkRateLimit reports whether vk may proceed, applying this RFC's
// fail-open policy: a Redis backend error is logged and the request is
// allowed through, rather than rejected — see "Fail-open, not fail-closed"
// below for why that's the right default specifically for Kelvran.
func (p *Pipeline) checkRateLimit(ctx context.Context, vk *identity.VirtualKey) bool {
    allowed, err := p.limiter.Allow(ctx, vk.ID)
    if err != nil {
        p.logger.Warn("ratelimit_backend_unavailable", "key_id", vk.ID, "error", err.Error())
        return true
    }
    return allowed
}
```

### Fail-open, not fail-closed, on Redis errors

Industry precedent (Kong's Redis rate-limit policy, Envoy's global rate-limit filter) defaults to configurable-but-default-fail-open specifically because a rate-limiter *outage* shouldn't become a full gateway outage. The real counter-argument — if rate limiting is the *only* abuse control, failing open during an outage removes protection exactly when it's most needed — does not fully apply to Kelvran: `internal/budget.Tracker`'s per-key USD cap is a second, independent enforcement layer that never touches Redis at all. So failing open here is defensible specifically because the worst case (a Redis outage) still leaves the budget cap bounding total cost exposure — the two controls `THREAT_MODEL.md`'s LLM10 row names aren't fully redundant with each other, and this RFC leans on that. The `Warn`-level log on every fail-open decision makes an outage observable rather than silent, which is the actual mitigation for this tradeoff, not a decorative afterthought.

### Config

```yaml
rate_limit:
  redis_addr: "localhost:6379"   # optional; omit for pure in-memory (today's behavior)
```

```go
// controlplane/config.go
// RateLimitConfig configures distributed (Redis-backed) rate limiting,
// per docs/rfcs/2026-09-03-distributed-rate-limiting.md. Optional — a
// zero-valued RateLimitConfig (RedisAddr == "") means pure in-memory
// per-key token buckets, exactly as before this RFC.
type RateLimitConfig struct {
    RedisAddr string
}
```

`buildPipeline` in `cmd/gateway/main.go` gains a `newKeyLimiter(cfg controlplane.RateLimitConfig, keys []ratelimit.KeyConfig) (*ratelimit.KeyLimiter, error)` helper mirroring `newBudgetTracker`'s exact shape: `RedisAddr == ""` returns `ratelimit.NewInMemoryKeyLimiter(keys)` (no behavior change for any existing config/test); otherwise it opens a `redislimiter.Limiter` (which itself opens a `*redis.Client` against `RedisAddr` — connection is lazy, go-redis doesn't dial until the first command, so a misconfigured/unreachable address surfaces on the first real rate-limit check, not at startup, matching this RFC's fail-open policy rather than making gateway startup itself fail-closed on Redis unavailability) and wraps it via `ratelimit.NewRedisKeyLimiter`. `Pipeline.Close` gains `p.limiter.Close()` alongside its existing `p.budget.Close()`.

### Testing strategy

`docs/testing/TESTING.md` §4 already commits, in writing, before this RFC exists: *"real Redis/Postgres via testcontainers, never mocked at this layer."* This RFC follows that literally rather than re-deciding it: `github.com/testcontainers/testcontainers-go/modules/redis` spins up a real, ephemeral Redis container for integration tests exercising `redislimiter.Limiter` and a `cmd/gateway` end-to-end test proving two independent `*dataplane.Pipeline`s (simulating two gateway instances) sharing one Redis-backed limiter correctly share one burst cap — the load-bearing proof this RFC's whole motivation rests on, mirroring `TestIntegrationBudgetPersistsAcrossRestart`'s "two independent Pipeline instances against the same backing store" pattern. Unlike `evals`' `RUN_DOCKER_TESTS=1` gate (needed there because those tests require heavier Docker-sandbox capabilities), a single ephemeral Redis container is lightweight enough to run unconditionally in CI — GitHub's `ubuntu-latest` runners ship Docker preinstalled, and this repo's own local dev machine already has a running Docker daemon (verified: `docker version` succeeds). No new env-var gate needed; `go test ./...` in CI simply requires Docker, which it already effectively does not require today (no test currently needs it) but will after this RFC — worth stating plainly as a new CI precondition, not a silent one.

## Drawbacks

- Fourth external Go dependency family (after OTel, `shopspring/decimal`, `go.etcd.io/bbolt`) — `github.com/redis/go-redis/v9`, isolated to the new `internal/ratelimit/redislimiter` subpackage so it's never pulled into a build that doesn't configure `rate_limit.redis_addr`.
- Fail-open on Redis errors means a sustained Redis outage silently degrades rate limiting back to "unlimited" for every request during the outage — mitigated by the Warn-level log on every occurrence and by `budget.Tracker`'s independent cost cap, but not eliminated. A future alerting rule on that log line is the natural operational follow-up (tracked below, not built here).
- CI now requires a working Docker daemon to run `gateway`'s full test suite (for the new testcontainers-backed integration tests) — previously true in spirit but not in practice, since no existing test actually needed it.
- The multi-instance correctness problem this RFC exists to fix has never been exercised in this repo outside of tests — Kelvran has never been deployed or run as more than one process. The new integration test proves the mechanism works; it does not prove anything about real production behavior under real concurrent load from genuinely separate processes/hosts, which doesn't exist yet.

## Alternatives Considered

1. **GCRA via `redis-cell`** (module-based) — rejected: requires a custom Redis module most managed offerings don't support.
2. **GCRA via `github.com/go-redis/redis_rate`** (pure-Lua, no module, official go-redis-org library) — the strongest alternative seriously considered, and the one worth being precise about rejecting correctly. Not rejected for vocabulary (its `Limit{Rate, Period, Burst}` fields are a straightforward, non-breaking mapping from `RateLimitBurst`/`RateLimitRefill`) or for lacking observability (its `Result.Remaining` field reports exactly that). Rejected because `Limit.Rate`/`Limit.Burst` are typed `int`, while Kelvran's config fields are deliberately `float64` to support fractional refill rates the in-memory `TokenBucket` already allows — adopting this library would silently round those configs, making the same config value behave differently depending on which backend is active. See "Algorithm" above for the full reasoning.
3. **Hard-require Redis unconditionally (no in-memory fallback)** — rejected. `gateway/ARCHITECTURE.md`'s Tech Stack table frames Redis as the eventual target architecture, but the multi-instance correctness gap it fixes is currently hypothetical — Kelvran has never been run multi-instance anywhere in this repo. Forcing every local dev run and every CI invocation to require a live Redis just to start the gateway, for a failure mode that doesn't exist in practice yet, would be disproportionate — the same reasoning `docs/rfcs/2026-09-03-budget-persistence.md` already applied to Postgres. Opt-in, defaulting to unchanged in-memory behavior, is the right shape again here — for this RFC's own reason (the correctness gap is real in theory but unexercised in practice), not merely because the prior RFC set a precedent.
4. **Fail-closed on Redis errors (reject with 429/503 instead of allowing through)** — rejected as the default, per the "Fail-open, not fail-closed" section above; the budget cap is the reason this is safe to default to fail-open specifically for Kelvran, not a general claim that fail-open is always correct.
5. **Extend `TokenBucket` itself to optionally back onto Redis internally** — rejected: it would force go-redis into `internal/ratelimit`'s dependency graph unconditionally, exactly what the separate `redislimiter` subpackage exists to avoid, and would blur a well-tested, zero-dependency type with distributed-systems concerns it doesn't need to know about.

## Unresolved Questions

- No real production traffic exists yet to validate the Lua script's single-round-trip latency against Kelvran's actual request volume, or to validate the TTL formula's memory-growth characteristics at real key cardinality.
- Whether `rate_limit.redis_addr` should ever be shared with a future Redis-backed Cache L2/L3 layer (same Redis instance, different key namespace) rather than each feature independently configuring its own address — not decided here; `internal/ratelimit/redislimiter` and any future cache Redis adapter stay independent for now, revisit if running two separate Redis connections ever proves wasteful in practice.
- Hierarchical rate-limit scope (org → team → user → key → session, per `gateway/ARCHITECTURE.md`'s Request Lifecycle line) remains explicitly out of scope, same boundary `docs/rfcs/2026-09-02-virtual-keys-budgets.md` already drew.
- No alerting is wired to the `ratelimit_backend_unavailable` Warn log yet — logging it is the whole mitigation for the fail-open drawback above; turning that into a real alert is a natural but separate follow-up.
