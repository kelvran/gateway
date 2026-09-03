> **For agentic executors:** Task 1 (the Lua script + `redislimiter` package) must land before Task 2 (`KeyLimiter`), which must land before Tasks 3/4. Task 5 is last.

---

**Goal:** A virtual key's burst/refill rate limit is enforced correctly across multiple gateway instances when `rate_limit.redis_addr` is configured, with zero behavior change when it isn't.

**Architecture:** A new `internal/ratelimit/redislimiter` package runs the existing `TokenBucket` algorithm atomically via a Lua script over `go-redis/v9`; a new `internal/ratelimit.KeyLimiter` type sits above both `TokenBucket` (in-memory) and `redislimiter.Limiter` (Redis) behind one small interface; `dataplane.Pipeline` swaps its `limiters map[string]*ratelimit.TokenBucket` field for a single `*ratelimit.KeyLimiter`, and both rate-limit call sites (buffered + streaming) route through one new `checkRateLimit` helper implementing this RFC's fail-open policy; `cmd/gateway` wires the config-driven choice.

**Tech Stack:** `github.com/redis/go-redis/v9` — the gateway's fourth external Go dependency, isolated to `internal/ratelimit/redislimiter` so it's never pulled into a build that doesn't set `rate_limit.redis_addr`. `github.com/testcontainers/testcontainers-go/modules/redis` (test-only dependency) for real-Redis integration tests, per `docs/testing/TESTING.md` §4's existing commitment.

**Spec:** `docs/rfcs/2026-09-03-distributed-rate-limiting.md` — the exact Lua script, type signatures, and the fail-open-vs-fail-closed reasoning live there; this plan implements them verbatim.

**Global Constraints** (inherited from the spec + `AGENTS.md`):
- `ratelimit.TokenBucket` itself must not change — zero churn to its existing API or tests.
- A bare `config.yaml` with no `rate_limit:` section must behave identically to before this feature existed (in-memory `KeyLimiter`, unchanged).
- The Redis-backed path must implement the *exact same* refill/cost semantics as `TokenBucket.Allow`/`refillLocked` — same formula, same 1-token cost, same "advance last-refill time even on a rejected call" behavior — so switching backends never changes rate-limiting behavior, only its distribution/durability properties.
- A Redis backend error must be logged (`ratelimit_backend_unavailable`, Warn) and must **allow** the request through (fail-open), never reject it — per the RFC's reasoning that `budget.Tracker` is the independent backstop that makes this safe.
- `internal/ratelimit`'s core package must not import `go-redis` — only `internal/ratelimit/redislimiter` does, mirroring `internal/budget`/`internal/budget/boltstore`'s existing separation.

---

## Task 1 — `go-redis/v9` + `internal/ratelimit/redislimiter`

**Files:**
- Modify: `gateway/go.mod`, `gateway/go.sum` (via `go get`)
- Create: `gateway/internal/ratelimit/redislimiter/redislimiter.go`
- Create: `gateway/internal/ratelimit/redislimiter/redislimiter_test.go`

**Steps:**
- [ ] `go get github.com/redis/go-redis/v9`; `go get github.com/testcontainers/testcontainers-go/modules/redis` (test dependency); `go mod tidy`; confirm (don't assume) the actual net-new transitive dependency count.
- [ ] Embed the RFC's exact Lua script as a Go string constant; construct it once via `redis.NewScript` at `Open` time.
- [ ] `Open(addr string) (*Limiter, error)`: constructs a `*redis.Client` (lazy dial — go-redis doesn't connect until the first command, so this must not itself fail on an unreachable address; confirm this empirically, don't assume it from the RFC's claim).
- [ ] `(*Limiter) Allow(ctx, keyID string, capacity, refillPerSecond float64) (bool, error)`: builds the Redis key (`"ratelimit:" + keyID`), passes `capacity`, `refillPerSecond`, and `time.Now().UnixMilli()` as script args, runs it, type-asserts the result as `int64` explicitly (per the RFC's documented go-redis gotcha — get this assertion wrong and it panics at runtime, not at compile time), returns `result == 1`.
- [ ] `(*Limiter) Close() error`: closes the underlying `*redis.Client`.
- [ ] Confirm structurally (not by import — this package must NOT import `internal/ratelimit`) that `*Limiter` satisfies `ratelimit.RedisBackend`'s method set exactly.
- [ ] Tests, against a real Redis container via `testcontainers-go/modules/redis` (per the RFC's testing-strategy section — no env-var gate, this runs unconditionally): a fresh key with no prior state allows exactly `capacity` calls before rejecting; refill is exercised deterministically by manually advancing the passed-in "now" argument across calls (not by sleeping on wall-clock time, matching this project's existing `fakeClock` testing convention in `ratelimit_test.go`) and confirming tokens replenish at the configured rate, capped at capacity; **the load-bearing test**: two independent `*redislimiter.Limiter` instances (simulating two separate gateway processes) pointed at the same Redis container and the same `keyID` correctly share one burst budget — the exact multi-instance correctness property this whole RFC exists to deliver, proven here at the backend layer before Task 4 proves it end-to-end.

**Verify:** `cd gateway && go build ./internal/ratelimit/redislimiter/... && go test ./internal/ratelimit/redislimiter/... -v`

## Task 2 — `internal/ratelimit.KeyLimiter` and the `RedisBackend` seam

**Files:**
- Modify: `gateway/internal/ratelimit/limiter.go` (new file)
- Modify: `gateway/internal/ratelimit/limiter_test.go` (new file)

**Steps:**
- [ ] Define `KeyConfig{ID string; Capacity, RefillPerSecond float64}` and the `RedisBackend` interface exactly as specified in the RFC.
- [ ] `KeyLimiter{configs map[string]KeyConfig; buckets map[string]*TokenBucket; backend RedisBackend}`.
- [ ] `NewInMemoryKeyLimiter(keys []KeyConfig) *KeyLimiter`: builds `buckets` eagerly from `keys`, exactly matching today's `dataplane.NewPipeline`'s existing eager-build loop (this is a lift-and-shift of that logic, not new behavior).
- [ ] `NewRedisKeyLimiter(keys []KeyConfig, backend RedisBackend) *KeyLimiter`: stores `configs` (keyed by `ID`) for later per-call lookup; `buckets` stays nil.
- [ ] `(*KeyLimiter) Allow(ctx, keyID string) (bool, error)`: routes to `backend.Allow(ctx, keyID, cfg.Capacity, cfg.RefillPerSecond)` when `backend != nil`, else `buckets[keyID].Allow(), nil`.
- [ ] `(*KeyLimiter) Close() error`: no-op when `backend == nil`; else `backend.Close()`.
- [ ] Tests (using a small fake `RedisBackend` defined in this test file — keeping `internal/ratelimit`'s own tests independent of `redislimiter`, matching the `budget`/`boltstore` precedent): `NewInMemoryKeyLimiter` behaves identically to constructing `TokenBucket`s directly (same Allow sequence produces the same true/false sequence); `NewRedisKeyLimiter` passes the correct per-key `Capacity`/`RefillPerSecond` through to the fake backend's `Allow` call (assert the exact arguments received, not just the return value); a fake backend that returns an error propagates that error through `KeyLimiter.Allow` unchanged (fail-open is `Pipeline`'s job in Task 3, not `KeyLimiter`'s — this layer must stay a faithful pass-through); `Close` calls through to the fake backend's `Close` only in Redis mode, and is a no-op in in-memory mode.

**Verify:** `cd gateway && go build ./internal/ratelimit/... && go test ./internal/ratelimit/... -race`

## Task 3 — `dataplane.Pipeline` wiring + fail-open `checkRateLimit`

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go`
- Modify: `gateway/internal/gateway/dataplane/streaming.go`
- Modify: `gateway/internal/gateway/dataplane/dataplane_test.go`

**Steps:**
- [ ] `Pipeline.limiters map[string]*ratelimit.TokenBucket` → `Pipeline.limiter *ratelimit.KeyLimiter`. `Config` gains a way to inject a pre-built `*ratelimit.KeyLimiter` (mirroring how `Config.Budget` is already a pre-built `*budget.Tracker`, not raw config) — `NewPipeline` requires it non-nil, same validation pattern as every other required dependency.
- [ ] Delete `limiterFor` (no longer needed — `KeyLimiter` already does its own per-key lookup internally).
- [ ] Add `checkRateLimit(ctx, vk *identity.VirtualKey) bool` exactly as specified in the RFC (calls `p.limiter.Allow`, logs+allows on error).
- [ ] Update both call sites (`dataplane.go`'s `HandleChatCompletion`, `streaming.go`'s `HandleChatCompletionStream`) from `if !p.limiterFor(vk.ID).Allow() { err = ErrRateLimited; return }` to `if !p.checkRateLimit(ctx, vk) { err = ErrRateLimited; return }`.
- [ ] `Pipeline.Close`: add `p.limiter.Close()` alongside the existing `p.budget.Close()`; if either returns an error, wrap/join both (don't silently drop one) — check how `errors.Join` is used elsewhere in this module, if at all, for consistency; if nowhere yet, this is the first, and should be a clean, well-commented instance.
- [ ] Tests: a fake `RedisBackend` (or the same fake from Task 2, reused) that errors on every call proves `checkRateLimit` still allows the request through and logs a Warn (assert against a `slog.Logger` writing to a buffer, matching the existing `budget`-persistence-failure test's pattern); confirm the existing rate-limit-rejection tests (in-memory mode) still pass unmodified, proving this refactor is behavior-preserving for the default path.

**Verify:** `cd gateway && go build ./internal/gateway/dataplane/... && go test ./internal/gateway/dataplane/... -race`

## Task 4 — `controlplane` config + `cmd/gateway` wiring + the real multi-instance integration test (depends on Tasks 1-3)

**Files:**
- Modify: `gateway/internal/gateway/controlplane/config.go`
- Modify: `gateway/internal/gateway/controlplane/config_test.go`
- Modify: `gateway/cmd/gateway/main.go`
- Create: `gateway/cmd/gateway/distributed_ratelimit_integration_test.go`
- Modify: `gateway/config.example.yaml` (add a commented-out `rate_limit:` section)

**Steps:**
- [ ] `RateLimitConfig{RedisAddr string}`; `Config.RateLimit RateLimitConfig`; parse a `rate_limit:` section with `redis_addr` (plain `getString`).
- [ ] Test: a config with a `rate_limit: { redis_addr: "..." }` section parses correctly; a config without one leaves `Config.RateLimit` at its zero value (mirroring `TestLoadWithoutTelemetrySectionDefaultsToZeroValue`'s existing pattern).
- [ ] `buildPipeline`: build `[]ratelimit.KeyConfig` from `cfg.VirtualKeys` (lift-and-shift of today's existing loop); if `cfg.RateLimit.RedisAddr != ""`, open a `redislimiter.Limiter` and wrap via `ratelimit.NewRedisKeyLimiter`; else `ratelimit.NewInMemoryKeyLimiter(keys)` exactly as today. Wrap and return any open error clearly (note: per the RFC, opening should not itself fail on an unreachable Redis — confirm this at the integration-test level, not just assumed).
- [ ] **The load-bearing new integration test**: `TestIntegrationTwoGatewayInstancesShareOneRedisRateLimit` — start a real Redis container (testcontainers), build **two independent** `*dataplane.Pipeline`s (via `buildPipeline`, simulating two gateway instances) from configs pointing at the **same** Redis address and the **same** virtual key with a small burst (e.g. 3), wire each into its own `httptest.Server`, and send requests alternating between the two servers — assert the *combined* request count across both instances that succeed equals the configured burst (e.g. exactly 3 of the first 6 requests succeed, regardless of which server each landed on), proving the cap is shared across processes, not per-process. Also test the fail-open path end-to-end: point a `Pipeline` at a Redis address nothing is listening on, send a request, and confirm it succeeds (not rejected) — the real proof of this RFC's fail-open policy through the full HTTP stack, not just at the unit-test level from Task 3.

**Verify:** `cd gateway && go build ./... && go vet ./... && go test ./... -v -race && golangci-lint run ./...`

## Task 5 — Docs, Changelog, Wrap-Up

**Files:**
- Modify: `gateway/internal/ratelimit/ratelimit.go`'s package doc comment (remove the now-fulfilled "distributed... is Phase 1" note, replace with what's real)
- Modify: `gateway/ARCHITECTURE.md` (mark distributed rate limiting as real-but-opt-in; note the go-redis dependency and the fail-open policy)
- Modify: `gateway/changelog/unreleased.md` (Added entry)
- Modify: `DECISIONS.md` (one line: Lua-token-bucket over GCRA, opt-in scope, fail-open reasoning)
- Modify: `docs/agents/LOGS.md` (new append-only entry)
- Modify: `STATUS.md` (Current Phase, Verification State, Last Completed Task, Next Action)
- Modify: `THREAT_MODEL.md`'s LLM10 row if its current wording implies rate limiting is already distributed-safe (check first — don't assume it needs a change)

**Verify:** re-run Task 4's full verify command once more after doc edits; cross-reference grep for every new doc's referenced paths; confirm CI (`.github/workflows/ci.yml`) still only requires what it already requires (Docker is implicitly available on `ubuntu-latest` — confirm this is actually true rather than assumed, since Task 1/4's new tests depend on it) before pushing.
