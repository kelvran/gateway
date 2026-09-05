# RFC: Cache concurrent-miss stampede protection (singleflight)

## Status

Accepted, implemented 2026-09-05.

## Context

`THREAT_MODEL.md`'s Cache Denial-of-Service row names a real, live gap, corrected into present-tense honesty on 2026-09-05 but never actually closed: "Cache-miss storms causing redundant expensive upstream calls... no request coalescing/singleflight/distributed-lock mechanism exists anywhere in the code... a miss returns immediately with no lock acquisition, so N concurrent requests for the same not-yet-cached key each independently call upstream today." A fresh backlog audit re-confirmed this directly against the live `checkCache` function and ranked it the highest-value actionable item in the repo: it sits at the center of the project's own stated cost-savings mission (`PRD.md`), and a real agent workload — the kind Kelvran is built for — routinely produces exactly this pattern (a burst of identical retries, or several concurrent agent instances asking the same question).

## Design

### Mechanism: `golang.org/x/sync/singleflight`, keyed on `l1Key`

`dataplane.Pipeline` gains a `missGroup singleflight.Group` field (zero value is ready to use — no constructor change needed). The entire cache-miss body — guardrail pre-call, router selection + single fallback, the real upstream call, guardrail post-call, and cache write-back — is extracted into a new `runMissPath` method and wrapped in `p.missGroup.Do(l1Key, fn)`. Only the first caller for a given `l1Key` actually executes `fn`; every other concurrent caller for the same key blocks and receives the identical result (`resp`, `dep`, `fallback`, `err`) once it completes.

Coalescing is keyed on `l1Key` — the exact-match cache key (`cache.Key`, already baking in tenant ID, model, exact serialized messages, temperature, max_tokens, and guardrail policy version) — not `l2Key` (the normalized-match key). This is deliberately narrower than it could be: two concurrent requests that are merely normalized-equivalent (and would already share an L2 hit once either one completes) are *not* coalesced with each other in this pass, only byte-identical ones. Broadening this to `l2Key` would require writing into two different L1 keys from one shared execution — a genuinely separate design question, named here as explicit future work, not solved in this pass.

### Why this can never coalesce across tenants

`l1Key` already bakes in `vk.ID` via `cache.Key`'s own existing signature. Two different virtual keys making the byte-identical request concurrently get two different `l1Key` values, and therefore two entirely separate `singleflight.Group` entries — there is no code path by which this change could cause one tenant's request to be served by (or share billing/guardrail-decision provenance with) another tenant's call. Verified with a dedicated test (`TestHandleChatCompletionNeverCoalescesAcrossDifferentTenants`), not just argued from the key's shape.

### A real, accepted tradeoff: the leader's context governs the shared call

Only the first (leader) caller's `ctx` is actually used for the shared guardrail checks and upstream call. If the leader's own request is canceled (client disconnect, its own timeout), every coalesced waiter's call fails too — even though their own individual request contexts may still be live. This is the same tradeoff every production use of `singleflight` for HTTP request coalescing accepts (e.g. groupcache's own well-known design); a context that outlives any single caller would need its own detached timeout policy this project has no expressed need for yet. Named explicitly, not hidden.

### An adjacent, pre-existing finding — explicitly out of scope for this pass

While tracing `finalize`'s real cost-recording logic to confirm this change wouldn't introduce a NEW correctness issue, a separate, already-existing behavior was found: `finalize` calls `p.budget.Record(vk.ID, cost, ...)` using `resp.Usage` for every request where `err == nil`, **including on an existing cache hit** — a cached response's stored `Usage` field still produces a nonzero computed cost, and today's code already re-records that same cost against budget on every repeat cache hit, not just the first real call. This predates this RFC entirely (verified: the same behavior applies to an L1/L2/L3 cache hit today, with or without this change) and this pass's own coalesced-miss path exhibits the identical, pre-existing pattern (N coalesced callers each independently record the same `cost` for the one real call they share) — consistent with, not a regression from, existing behavior. Whether repeat cache hits (or coalesced misses) *should* re-charge budget at all is a real, separate policy question, explicitly not decided or changed here — named for a future pass, not silently fixed as a scope-creeping side effect of this one.

## Alternatives considered

**A distributed lock (Redis-backed) instead of in-process `singleflight`** — rejected for v1: `singleflight.Group` only coalesces requests landing on the *same gateway instance* — a multi-instance deployment still allows redundant calls across instances. This project's existing distributed-rate-limiting RFC already established the precedent that single-instance-only mechanisms are an acceptable, named stepping stone (in-memory budget/rate-limiting both started single-instance-only before their Redis-backed siblings shipped); a cross-instance version is real future work, not designed here, since it would need its own transport/consistency design pass.

**Coalescing on `l2Key` instead of `l1Key`** — rejected for this pass; see "Mechanism" above.

## Verification

`internal/gateway/dataplane/cache_stampede_test.go`: `TestHandleChatCompletionCoalescesConcurrentIdenticalCacheMisses` (10 concurrent identical requests via a real Pipeline, a blocking fake upstream, and a barrier+bounded-wait synchronization pattern — exactly 1 real upstream call, every caller gets the correct response) and `TestHandleChatCompletionNeverCoalescesAcrossDifferentTenants` (2 different virtual keys, identical request bodies, concurrently — exactly 2 real upstream calls, never 1). `cmd/gateway/cache_stampede_integration_test.go`: a real end-to-end HTTP proof (`TestIntegrationConcurrentIdenticalRequestsCoalesceIntoOneUpstreamCall`) — 10 real, concurrent HTTP requests against a real `httptest.Server`-backed gateway and a real, blocking mock upstream server, confirming exactly 1 real upstream call and 10 successful `200` responses. All three passed consistently across repeated runs (`-count=3` to `5`) under `-race`. Sanity-checked by temporarily replacing the `singleflight.Group.Do` call with a direct, uncoalesced invocation of the same closure and re-running the unit-level coalescing test: it failed with `upstreamCalls = 10, want exactly 1` — the exact wrong number, for the exact right reason — then restored.
