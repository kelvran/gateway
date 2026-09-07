# RFC: Multi-dimensional rate-limit matching (per-model overrides)

## Status

Accepted, implemented 2026-09-07.

## Context

`docs/upgrade-research/gateway-2026-09-06.md` Finding 5 names Kong's multi-dimensional rate-limit matching (consumer × model, provider, header, path, with AND logic) as a real capability Kelvran's own rate limiting doesn't have — today, `internal/ratelimit.KeyLimiter` scopes purely per-virtual-key: one RPM bucket (`Capacity`/`RefillPerSecond`) and one optional TPM bucket (`docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md`), both shared identically across every model that virtual key is allowed to call. There is no way to give one virtual key a tighter, separate limit for one specific model while leaving its other models on the shared default.

Kong's own cost-*computation* formula (the other half of Finding 5) is out of scope here — `internal/budget.Tracker` already does something conceptually similar (real Decimal-precision cost tracking against a per-key USD cap, from real provider usage); the genuinely missing piece this RFC closes is the *matching* dimension, not the cost-computation idea.

The plan's own scope note flagged this as Medium-Large and explicitly authorized scoping it down to a bounded, real first step rather than a fully general Kong-style rule-matching engine (provider/header/path dimensions) in one pass.

## Design

### Scope: the consumer × model dimension only

This RFC ships exactly one new dimension — a virtual key's own, optional per-model RPM override — not provider/header/path matching. Those three remain named, scoped-out future work (see Alternatives Considered), not silently dropped: nothing surveyed while implementing this made them cheaply addable in the same pass without a materially larger change (header/path matching in particular would need a new place in the request pipeline to even read that data, since `checkRateLimit` today only ever sees `vk`/`model`).

### `KeyConfig.PerModel map[string]ModelRateLimit`, consulted before the default bucket

`ratelimit.KeyConfig` gains `PerModel map[string]ModelRateLimit` (`ModelRateLimit{Capacity, RefillPerSecond}`). `KeyLimiter` gains a new `AllowForModel(ctx, keyID, model) (bool, error)` entry point alongside the existing `Allow(ctx, keyID)` — `Allow` is now a thin wrapper calling the same internal `allow(ctx, keyID, "")`, so nothing in this codebase that already calls `Allow` needed to change signature. `AllowForModel` checks `PerModel[model]` first; if a positive-capacity entry exists, it decides against **that** bucket instead of — never in addition to — the key's own default bucket. A key with zero `PerModel` entries (every config written before this feature existed) makes `AllowForModel` and `Allow` behave byte-identically, proven by `TestAllowForModelByteIdenticalToAllowWhenNoPerModelConfigured` and, end-to-end, `TestHandleChatCompletionWithoutPerModelBehavesUnchanged`.

`dataplane.Pipeline.checkRateLimit` gains a `model string` parameter, threaded from both `HandleChatCompletion` and `HandleChatCompletionStream`'s own `req.Model`, calling `AllowForModel` instead of `Allow`.

An entry with `Capacity <= 0` is treated as **absent** — falls through to the default bucket — never as "unlimited" for that model (`buildPerModelBuckets` skips it at construction; the Redis-mode `allow()` branch checks `override.Capacity > 0` before using it). A zero-capacity bucket would otherwise incorrectly read as "always blocked," the same hazard `KeyConfig.TPMCapacity`'s own `<= 0` convention already guards against.

### Enforced in BOTH in-memory and Redis mode — a real improvement over TPM's own precedent

Unlike `TPMCapacity` (in-memory-only in v1, a deliberate, named scope limit), `PerModel` is enforced in both rate-limit backends. `RedisBackend.Allow` already accepts an arbitrary per-call `(key, capacity, refillPerSecond)` — no interface change, no new Lua script, and no change to `internal/ratelimit/redislimiter` was needed. The Redis-mode key for an override is `keyID + "\x00model=" + model` (see `perModelBackendKey`) — a NUL-byte-tagged separator, mirroring `internal/cache/key.go`'s own `Key`/`NormalizedKey` precedent for the identical problem (two caller-controlled, variable-length strings naively `":"`-joined can collide, e.g. key `"a:model=b"` vs. key `"a"`, model `"b"`; a NUL byte can't appear in either component via this project's config surfaces).

### Config surface: static YAML and the live Admin API, both

`controlplane.VirtualKeyConfig.PerModelRateLimits map[string]ModelRateLimitConfig` (YAML key `rate_limit.per_model.<model>.{burst,refill_per_second}`) is parsed by a new `parsePerModelRateLimits`, requiring both fields **positive** — unlike the key's own top-level `burst`/`refill_per_second` (where `0` resolves to the gateway's own operational default), a per-model entry has no equivalent fallback to resolve to, so a missing or non-positive value is a config-load error, not a silent zero-capacity bucket.

`internal/admin`'s `rateLimitRequest` gains the identical `PerModel map[string]perModelRateLimitRequest` shape and the identical positive-value validation, applied inline in `upsertVirtualKeyHandler` — matching this project's own established completeness discipline (TPM, budget-warn, fallback chains) of never shipping a rate-limit-adjacent field in only one of the two mutation surfaces. Like every other field on the upsert request, `per_model` is a full replace, never a partial merge: omitting it on an update to an already-overridden key clears the override, exactly as omitting `rate_limit` entirely already resets burst/refill to the gateway's default.

## Alternatives considered

**A fully general Kong-style rule engine (consumer × model × provider × header × path, with AND logic) in this same pass** — rejected for now, per the plan's own explicit scope authorization. Provider/header/path matching would need a materially different integration point than `checkRateLimit`'s current `(vk, model)` view of a request, and no cheap way to add all three without a much larger change was found while implementing the model dimension. Left as named, scoped-out future work, not silently dropped.

**A separate, parallel `PerModelLimiter` type instead of extending `KeyLimiter`** — rejected; `TokenBucket`'s existing refill/capacity mechanics are exactly what a per-model override needs, and `KeyLimiter` already owns the `configs`/`buckets` maps a per-model extension naturally slots into, matching TPM's own precedent of extending the existing type rather than parallel-building a new one.

**Keeping `PerModel` in-memory-only, matching TPM's own scope limit** — considered and rejected specifically for this feature: unlike TPM (which needed a new Lua script/stateful bucket design for Redis mode that was a real, separate undertaking), `RedisBackend.Allow`'s existing per-call `(key, capacity, refill)` signature already supports this dimension for free — restricting it to in-memory mode would have been a needless scope limit with no corresponding implementation-cost savings.

## Verification

`go build ./... && go vet ./...` clean. `gofmt -l .` clean. `golangci-lint run ./...` → `0 issues`. `go run github.com/fe3dback/go-arch-lint@v1.18.0 check` → clean (no new component). `go mod tidy` → empty diff. `go test ./... -race` → every package `ok` except the same two pre-existing, already-documented environmental rootless-Docker failures (`TestIntegrationTwoGatewayInstancesShareOneRedisRateLimit`; `internal/ratelimit/redislimiter`'s own `TestMain` panic).

New tests: `internal/ratelimit` (5 — separate-bucket proof, byte-identical-with-no-override proof, non-positive-capacity-treated-as-absent proof, Redis-mode distinct-key-and-config proof, Register-disables-stale-override proof); `internal/gateway/controlplane` (2 — Load-time rejection of a non-positive per-model entry, nil-when-unconfigured); `internal/gateway/dataplane` (2 — a real `HandleChatCompletion` pipeline test proving a per-model override rejects only that model while a sibling model on the shared default bucket is unaffected, and a no-override-configured backward-compatibility proof); `internal/admin` (2 — the live Admin API mutation surface actually enforces an override it was POSTed, and rejects a non-positive one with 400).

Sanity-checked the single most load-bearing line (the in-memory `allow()` branch's per-model bucket lookup) by temporarily disabling it (`if false && model != ""`): exactly the two in-memory-mode tests that depend on it failed for the right reason (`TestAllowForModelUsesItsOwnBucketSeparateFromTheDefault`, `TestRegisterDisablingPerModelRemovesTheStaleBucket`), the two full-pipeline/Admin-API tests exercising the same code path also failed as expected, and — correctly — the Redis-mode test (`TestAllowForModelInRedisModePassesADistinctKeyAndConfigForTheOverride`) stayed green, since the temporary break was scoped to the in-memory branch only. Reverted; confirmed via `git diff` that zero trace of the temporary break remains.
