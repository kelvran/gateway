# RFC: Cache hits and coalesced followers never re-charge budget

## Status

Accepted, implemented 2026-09-05.

## Context

Fresh backlog audit item 17: a real, pre-existing pattern found (not introduced) while shipping cache concurrent-miss stampede protection — `finalize` unconditionally called `p.budget.Record(vk.ID, cost, ...)` whenever `err == nil`, regardless of whether the response came from a genuine, unshared upstream call. Two distinct cases both re-charged the same notional cost for zero incremental upstream spend:

- **A repeat cache hit** (L1/L2/L3) — the cached response's `resp.Usage` still produces a nonzero `cost`, and every repeat hit recorded it again against the virtual key's budget, inflating tracked spend far beyond what was actually incurred with the upstream provider.
- **A singleflight-coalesced follower** (per `docs/rfcs/2026-09-05-gateway-cache-stampede-protection.md`) — N concurrent identical cache misses share exactly one real upstream call, but before this change every one of the N callers independently ran `finalize` and independently charged the same cost, so N coalesced requests could charge N× the real dollar amount actually spent.

Resolved via an explicit brainstorming check-in (`AskUserQuestion`): "zero charge" — only genuine, unshared upstream calls debit budget; cache hits and coalesced followers cost $0 against the ledger — chosen over a reduced/discounted notional charge (would need a new pricing concept in `internal/costaccounting`) or keeping the existing behavior.

## Design

### `billable`: a new signal distinguishing "this call's own real execution" from "a shared/replayed result"

`finalize` gains a `billable bool` parameter. `p.budget.Record` is now gated on `vk != nil && billable`, not `vk != nil` alone. `cost` itself is still always computed and still always reaches `telemetry.RecordChatCompletionResult`/`logRequest` unconditionally when `err == nil` — this is deliberate: `kelvran.cost.usd` remains real, informational "what this would have cost" data (useful for a cache-savings dashboard, e.g. "we avoided $X in upstream spend via cache hits this hour"), only the *budget ledger* itself stops double-counting it.

`billable` is computed differently on each of the three paths that can produce a successful response:

- **Cache hit (L1/L2/L3)**: `HandleChatCompletion`/`HandleChatCompletionStream` return immediately on a hit without ever setting `billable` — it stays the zero value, `false`. No special-casing needed; this falls out of the existing early-return structure.
- **Genuine, unshared cache-miss (buffered path)**: `runMissPath`'s closure (the one `singleflight.Group.Do` actually invokes) sets `billable = true` as its first statement. Since `Group.Do` never invokes a follower's own closure at all — only ever the one in-flight leader's — each caller's own stack-local `billable` variable is only ever written by that same caller's own closure. A follower's `billable` simply stays `false`, with no shared mutable state and no synchronization required: this falls directly out of `singleflight`'s own documented semantics (confirmed via `go doc golang.org/x/sync/singleflight Group.Do`), not a new mechanism layered on top of it.
- **Streaming path**: `HandleChatCompletionStream` has no singleflight coalescing at all (a separate, already-named scope limit — `docs/rfcs/2026-09-05-gateway-cache-stampede-protection.md` covers only the buffered path). Every completed stream is therefore its own real, unshared upstream call — `billable = true` is set unconditionally right after `streamDeploymentWithFallback` succeeds.

### Why not derive `billable` from `singleflight.Group.Do`'s own `shared` return value

`Do`'s `shared bool` return indicates "whether v was given to multiple callers" — confirmed via `go doc`, this is the **same value for the leader and every follower** when there were 2+ total callers; it does not distinguish "I personally executed the closure" from "I received someone else's result." A closure-local flag, set only when the closure itself actually runs, is the only correct signal for this — `shared` was considered and rejected as insufficient, not overlooked.

## Alternatives considered

**Reduced notional charge** (hits/followers debit a smaller flat/discounted amount) — rejected by the user's explicit choice; would need a new pricing concept in `internal/costaccounting` with no clear "right" discount rate.

**Keep current behavior** — rejected; the audit's own finding was that this inflates tracked spend for zero incremental cost, a real correctness gap in a budget-enforcement feature whose whole purpose is accurate spend tracking.

**Deriving billable from `cacheInfo.Hit()` alone, ignoring the coalescing case** — rejected; would have fixed only the smaller of the two real double-charging cases found, leaving the stampede-protection-introduced follower case unfixed.

## Verification

`go build ./... && go vet ./...` clean. `golangci-lint run ./...` → `0 issues`. `go run github.com/fe3dback/go-arch-lint@v1.18.0 check` → clean. `go test ./... -race` → every package `ok` except the same pre-existing, already-documented rootless-Docker `TestIntegrationTwoGatewayInstancesShareOneRedisRateLimit` failure.

New `internal/gateway/dataplane/cost_double_counting_test.go`: `TestHandleChatCompletionCacheHitDoesNotRechargeBudget` (a real nonzero-priced pipeline; asserts tracked spend after a cache-hit second call equals spend after the first real call, not double) and `TestHandleChatCompletionCoalescedFollowerDoesNotRechargeBudget` (10 concurrent identical cache misses, barrier-synchronized exactly like the stampede-protection tests; asserts total tracked spend equals exactly one real charge, not ten). Sanity-checked by temporarily removing the `&& billable` gate: both tests failed with the exact precise wrong dollar amounts (`0.022` instead of `0.011` for the double-charge case; `0.11` instead of `0.011` for the ten-way case), then restored.
