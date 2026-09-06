# RFC: TPM (tokens-per-minute) rate-limit dimension

## Status

Accepted, implemented 2026-09-05.

## Context

`gateway/ARCHITECTURE.md`'s Request Lifecycle prose and `PRD.md`'s v1 routing scope line both use "rpm/tpm" as a paired concept, but only RPM (`internal/ratelimit`'s existing `TokenBucket`, request-count-based) was ever built — `docs/rfcs/2026-09-02-virtual-keys-budgets.md` deferred TPM as a separate dimension outright. The fresh backlog audit re-surfaced it; a ground-truth research pass confirmed the gap and, critically, confirmed a hard constraint: **real token usage (`resp.Usage`) is only known after the upstream call completes** (`dataplane.finalize` is the first point it's read), not at the point a rate-limit decision must be made (`checkRateLimit`, before any upstream call). No tokenizer or token-count estimator exists anywhere in this codebase.

## Design

### Retrospective debit, not pre-call reservation

Given the above constraint, TPM cannot work like RPM's `Allow` (check-and-consume-for-this-request). Instead, per-key TPM state answers only "has past usage already exhausted this key's allowance" — a request is rejected purely from prior consumption, never from a prediction of its own cost. Two new operations on `KeyLimiter`:

- **`AllowTPM(keyID string) bool`** — a non-consuming check: does the bucket's current (refilled) balance remain positive? Called from `checkRateLimit`, after the existing RPM `Allow` check passes (RPM-exhausted still rejects for that reason first, matching this codebase's existing check-ordering discipline).
- **`RecordTokens(keyID string, tokens int)`** — debits the bucket by `tokens`, called from `finalize` once `resp.Usage.TotalTokens` is known, gated on the same `billable` flag `docs/rfcs/2026-09-05-gateway-cost-double-counting.md` introduced: a cache hit or coalesced singleflight follower incurred no real, incremental token usage, so it must not debit again for tokens already counted once — the identical double-counting hazard that RFC already closed for budget.

`TokenBucket` (the existing RPM primitive) gains two new methods reused for TPM rather than a parallel type: `HasBalance() bool` (refill, then read — never consumes, unlike `Allow`) and `Debit(n float64)` (refill, then subtract `n`, deliberately allowed to go negative — an overdraft, since the debited amount is only known after the fact). The bucket recovers via its existing, unconditional refill on subsequent calls, exactly like a positive balance would.

### `KeyConfig.TPMCapacity`/`TPMRefillPerSecond`: 0 = unlimited, matching existing convention

`ratelimit.KeyConfig` (and `controlplane.VirtualKeyConfig`, and `internal/admin`'s `rateLimitRequest`) gain `TPMCapacity`/`TPMRefillPerSecond`. `TPMCapacity <= 0` means no TPM limit for that key. Implemented by simply never constructing a TPM bucket for that key (`tpmBuckets[keyID]` absent) rather than constructing a zero-capacity one — a zero-capacity bucket's balance is always `<= 0`, which would incorrectly read as "always blocked," not "unlimited." `AllowTPM`/`RecordTokens` both treat a missing entry as the unlimited/no-op case. `KeyLimiter.Register` (the live Admin API mutation path) deletes any existing TPM bucket when an update no longer configures one — an admin update disabling TPM must actually stop enforcing the old limit, not leave a stale bucket behind.

### `identity.VirtualKey` deliberately gets no TPM fields

Unlike every other per-key rate-limit/budget field, `identity.VirtualKey.RateLimitBurst`/`RateLimitRefill` were found, while implementing this, to be write-only in production — set at construction in both `cmd/gateway/main.go` and `internal/admin/admin.go`, but never read by any real (non-test) code afterward; only a test-only helper (`keyConfigsFromVirtualKeys`, explicitly commented "production code never does this conversion this way") derives a `ratelimit.KeyConfig` from them. Rather than mirroring that dead-weight pattern for a new field, TPM fields were added only where they're actually consumed: `ratelimit.KeyConfig` and `controlplane.VirtualKeyConfig`/`internal/admin`'s request struct, wired directly into `keyConfigs`/`rateLimitCfg` construction without an intermediate, unread `identity.VirtualKey` field.

### Scope limit: in-memory only in v1 — Redis mode is a deliberate no-op

Matching this project's own repeated "single-instance stepping stone, distributed version is later work" precedent (cache stampede protection; RPM rate limiting itself originally shipped in-memory-only before `docs/rfcs/2026-09-03-distributed-rate-limiting.md` added Redis support in a separate, later pass) — TPM ships in-memory-only. `NewRedisKeyLimiter`-built `KeyLimiter`s never build TPM buckets at all; `AllowTPM` returns `true` unconditionally and `RecordTokens` is a no-op whenever `l.backend != nil`. The `RedisBackend` interface is deliberately **not** extended (no new Lua script, no changes to the two existing test-fake implementers) — a materially larger change this pass doesn't need to make to deliver real value. `cmd/gateway.buildPipeline` logs a startup warning if a virtual key configures `TPMCapacity > 0` while `rate_limit.redis_addr` is also set, so this limitation is visible, never silent.

## Alternatives considered

**Pre-call reserve + reconcile** (estimate prompt tokens, treat `MaxTokens` as worst-case completion, reserve that sum, refund the delta once real usage is known) — rejected for this pass: requires building token estimation from scratch (no per-provider tokenizer exists), plus a policy for the common case where `MaxTokens` is unset. A real, larger undertaking than retrospective debit, named as possible future work if proactive (rather than one-request-lagged) throttling is ever needed.

**A second, parallel bucket type instead of reusing `TokenBucket`** — rejected; `TokenBucket`'s existing refill/capacity mechanics are exactly what TPM needs, just driven by `HasBalance`/`Debit` instead of `Allow`. A new type would duplicate the same math for no benefit.

**Extending `RedisBackend` for full Redis-mode TPM support in this same pass** — rejected; see "Scope limit" above.

## Verification

`go build ./... && go vet ./...` clean. `golangci-lint run ./...` → `0 issues`. `go run github.com/fe3dback/go-arch-lint@v1.18.0 check` → clean. `go mod tidy` → empty diff. `go test ./... -race` → every package `ok` except two pre-existing, environmental rootless-Docker failures — the already-documented `TestIntegrationTwoGatewayInstancesShareOneRedisRateLimit`, and a second, newly-surfaced-by-`go clean -testcache` instance: `internal/ratelimit/redislimiter`'s own `TestMain` also requires a real Docker daemon and had simply been test-cache-masked in every earlier run this session. Confirmed via the same stash-and-rerun discipline used earlier in this session: identical panic on completely unmodified code, unrelated to this change (this pass never touches `redislimiter` at all).

7 new unit tests in `internal/ratelimit` (`TestDebitCanOverdraftBelowZero`, `TestHasBalanceNeverConsumes`, `TestDebitRecoversViaOrdinaryRefill`, `TestAllowTPMUnlimitedWhenNotConfigured`, `TestRecordTokensExhaustsTPMBucketThenAllowTPMRejects`, `TestRecordTokensNoOpWhenTPMNotConfigured`, `TestAllowTPMAlwaysUnlimitedInRedisMode`, `TestRegisterDisablingTPMRemovesTheStaleBucket`). 3 new full-pipeline tests in `internal/gateway/dataplane/tpm_ratelimit_test.go`: `TestHandleChatCompletionRejectsOnceTPMBucketExhausted` (the load-bearing proof — 2 real calls exhaust a 10-token bucket via 8-token responses, the 3rd is rejected with `ErrRateLimited` before reaching upstream), `TestHandleChatCompletionCacheHitDoesNotDebitTPMBucket` (a repeat cache hit must not double-debit — verified by making a subsequent, genuinely different request's success/failure the distinguishing signal, since a boolean `AllowTPM` check alone can't otherwise tell "correctly stayed positive" apart from "incorrectly went negative but still failed for other reasons"), `TestHandleChatCompletionUnaffectedByTPMWhenNotConfigured`. Extended `controlplane/config_test.go`'s `TestLoadExampleConfig` with the new `config.example.yaml` fields' assertion. Sanity-checked twice: removed the `RecordTokens` call entirely — the exhaustion test failed with "want ErrRateLimited" but got nil; removed only the `billable` gate — the cache-hit test failed with the third call incorrectly rejected. Both restored.
