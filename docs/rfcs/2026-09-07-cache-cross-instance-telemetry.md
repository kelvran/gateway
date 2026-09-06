# RFC: Cache cross-instance duplicate-work / effective-hit-rate telemetry

## Status

Accepted, implemented 2026-09-07.

## Context

`docs/upgrade-research/cache-2026-09-06.md` Finding 5 is explicit that distributed cache coordination (a shared/Redis-backed cache, or extending `golang.org/x/sync/singleflight` coalescing across processes) is consistently described in the literature as *introducing* new failure modes — network partitions, lease expiry, fencing, split-brain — never as a low-risk scope upgrade of the same in-process mechanism `dataplane.go`'s `missGroup` already uses (keyed on `l1Key`, inherently single-process). The burden is on demonstrating the benefit — real cross-instance duplicate work, or effective-hit-rate loss when replicas are load-balanced without sticky routing — before paying that cost, not on assuming it exists.

Kelvran currently runs single-instance only: `docs/operations/DEPLOY.md` and `gateway/ARCHITECTURE.md` both confirm no multi-replica deployment exists yet (`ARCHITECTURE.md`'s own cache section: "Single-instance-only (in-process) in v1; a distributed version across multiple gateway instances is named future work"). So the real cross-instance phenomenon cannot be measured directly today. Finding 5's own "concrete next step" is explicit about what to build instead: instrument now, so that whenever a real multi-instance deployment does exist, the data needed to answer the question is already flowing — and decide whether to ever build a distributed/shared cache only once that evidence exists.

This RFC covers the "instrument half" of that item only (per the approved phased-roadmap plan text) — it is measurement infrastructure, never a change to caching, matching, or coalescing behavior on any path.

## Design

### `telemetry.InstanceID`: the smallest reasonable unique-per-process identifier, added to a genuinely empty gap

Grepped first, per the plan's own instruction: no UUID, no hostname-based identifier, and no OTel `service.instance.id` resource attribute existed anywhere in this codebase before this RFC — `resource.New`'s only attribute was `service.name`. `telemetry.InstanceID` is a new package-level var, computed once at package-init time as `hostname:pid` (`os.Hostname()` + `os.Getpid()`, both stdlib). Wired into two places:

- The OTel `Resource` in `telemetry.Init`, as the standard `service.instance.id` semantic-conventions attribute — every exported span/metric now automatically carries it.
- `telemetry.RecordCacheL3GateOutcome`'s existing attributes (see "L3 gets no exact-key correlation event" below).
- `dataplane.logCacheCrossInstanceCheck`'s log fields (see next section).
- Logged once at gateway startup (`cmd/gateway/main.go`'s `run()`), so an operator can confirm a given process's own identity without waiting for request traffic.

Hostname+pid, not a random UUID: zero new runtime dependency — `os.Hostname`/`os.Getpid` are stdlib, while `github.com/google/uuid` is only an *indirect* dependency today (pulled in transitively, never imported by any production code path — confirmed via `go mod why`), and promoting it to a direct dependency for this alone would be a real, avoidable new production dependency edge. Kelvran's real deployment model (one container/pod per replica) already gives every instance its own hostname; pid only guards the narrower, non-containerized case of two gateway processes sharing one host.

### The correlation logic: a pure, standalone package — `telemetry/cachecorrelation`

`cachecorrelation.Event` is exactly the six-field tuple Finding 5 names: `TenantID`, `Key`, `InstanceID`, `Hit`, `Timestamp`, `TTL`. `cachecorrelation.Analyze([]Event) Result` groups events by `(TenantID, Key)` and, for every miss (`Hit == false`), checks whether a DIFFERENT instance has any event (hit or miss) in the same group whose own `[Timestamp, Timestamp+TTL]` window was still open at the miss's own `Timestamp`. `Result.TotalMisses`/`CrossInstanceAvoidable` and a derived `EffectiveHitRateLoss()` fraction are the output.

Two deliberate choices worth naming:

1. **Every peer event counts, not only peer HIT events**, even though Finding 5's own one-line framing says "a repeat of a hit served on a different instance." This pipeline is write-through (`dataplane.writeCache` runs immediately after every genuine miss, before the response returns), so a peer's own first-ever MISS already means that instance's cache holds the value from approximately that event's own timestamp until `timestamp+ttl`. Restricting this to peer HIT events only would undercount the dominant real case: two near-simultaneous first requests for the same content landing on two different instances, neither of which has yet produced a second, hit-classified request of its own. See Alternatives considered for the narrower reading and why it was rejected.
2. **The tenant boundary is checked explicitly** (`groupKey{tenantID, key}`), never derived from `Key` alone — in practice this can never matter for real events (every `Key` already has `tenantID` folded into its own hash input, per `internal/cache.Key`/`NormalizedKey`), but `Analyze` doesn't depend on that upstream guarantee silently. The same cross-tenant-isolation discipline `AGENTS.md` requires of the cache itself is applied here, even though this is measurement code, not the cache.

This package takes no live dependency on the running process, a log pipeline, or any storage backend — it is a pure function over an in-memory `[]Event` slice a future analysis pass (not built by this RFC) would populate by parsing the log lines below. No live distributed lookup, no live correlator process, no new runtime dependency — exactly the plan's explicit scope limit.

### Live emission: `checkCache` (L1/L2) and `checkLexicalCache` (L3)

`dataplane.checkCache` now logs one `cache_cross_instance_check` structured line (via the existing `p.logger`, matching this codebase's uniform "log everything, no verbosity toggle" convention — no new level/config knob) per layer actually checked, but ONLY when the backend genuinely answered (`getErr == nil`): a backend I/O error tells this telemetry nothing about whether the key was really present, and must never be misreported as a definite miss. Fields: `tenant_id`, `cache_key` (the layer's own real key — `l1Key`/`l2Key`, both SHA-256 hex, safe to log — no raw prompt content), `cache_layer`, `instance_id`, `hit`, `ttl_ms` (the layer's own currently-configured store TTL). `checkCache`'s existing hit/miss/promotion behavior is completely unchanged — this is purely an additional side effect at points that already exist.

Deliberately a structured **log line, not an OTel metric**: `cache_key` is a per-request SHA-256 hash — unbounded cardinality — and attaching an unbounded-cardinality value as a metric attribute is a well-documented cardinality-explosion anti-pattern, unlike the small, fixed vocabularies (`gate`, `outcome`, `layer`, `instance_id`) this codebase's existing OTel counters already use as attributes.

**L3 (`checkLexicalCache`) gets no exact-key correlation event of its own — its own share of "instrument this call site" is narrower.** L3's match is a fuzzy near-duplicate search (`Search(tenantID, signature, k)`), never a single deterministic lookup key — there is no natural "exact key" the way L1/L2 have. Two options were considered:

- Emit the same `Event` shape anyway, reusing the request's own `l1Key` as the correlation identity (available in `HandleChatCompletion`/`HandleChatCompletionStream` before either cache check runs). This is what was actually built — `checkLexicalCache` now takes `l1Key` as a parameter and calls `logCacheCrossInstanceCheck(vk.ID, l1Key, "L3", hit, p.cacheL3TTL)` on the two paths that genuinely completed a real search (a full-gate-passing hit, or exhausting every candidate without one) — never on the volatile-bypass or search-error paths, which never learned whether a valid entry existed. This answers the same question L1/L2's own events answer ("was this exact request servable from any cache resource on this instance"), just via L3's different mechanism, and lets `Analyze` treat an L3 hit as a legitimate cross-instance peer for a sibling instance's later L1/L2/L3 miss on the same `l1Key` — which is correct: it genuinely means *some* cache resource on that instance could answer this exact request around that time, regardless of which layer.
- Add `instance_id` to the *already-shipped* per-gate ablation counter (`telemetry.RecordCacheL3GateOutcome`, Finding 1) instead/in addition. Done as well — a real, low-effort, correctly-scoped enhancement (once traffic from more than one `InstanceID` value appears there, that alone is a first, cheap signal that multi-instance deployment has begun), and safe as a metric attribute since `InstanceID` has small, fixed cardinality (one value per running process), unlike a raw key.

Both were shipped; neither is mutually exclusive with the other, and each closes a different gap.

### Why `Analyze` is not wired to consume the live log stream

Explicitly out of scope, per the plan text: no live correlator process, no log-parsing pipeline, no new runtime dependency (no new database, no Redis, no message queue). `cachecorrelation.Analyze` is ready to be handed real parsed events whenever that need arises — building the parser itself is deferred until a real multi-instance deployment exists and someone actually wants the answer, at which point it's a small, mechanical glue script (parse `cache_cross_instance_check` JSON lines into `[]Event`, call `Analyze`), not a design problem.

## Alternatives considered

**Narrower correlation: only correlate a miss against an explicit peer HIT event.** The literal reading of Finding 5's own one-line phrasing. Rejected — see "The correlation logic" above: it would systematically undercount the dominant real case (two near-simultaneous first misses on two different instances), since neither instance's *own* first miss is itself classified as a "hit" by anything.

**A live distributed lookup or shared cache, built now.** Explicitly rejected by the finding itself and the approved plan — the whole point of this item is to measure before paying the new-failure-modes cost the research consistently documents (partitions, lease expiry, fencing, split-brain). Not attempted.

**A random UUID (`github.com/google/uuid`) for `InstanceID` instead of hostname+pid.** Rejected: `google/uuid` is only an indirect dependency today (no production code imports it), and promoting it to direct for this alone would be a real, avoidable new dependency edge — hostname+pid needs nothing beyond `os`, and Kelvran's real deployment model already gives every replica a distinct hostname.

**Extending the L1/L2 correlation `Event` schema down into `checkLexicalCache`'s per-CANDIDATE loop (one event per searched candidate, not one per request).** Rejected as overreach: a candidate-level event would need its own stored identity distinct from `l1Key` (the candidate's own, different original request), which `LexicalCandidate` doesn't currently expose, and Finding 5 asks about the requesting side's own duplicate work, not an audit of every candidate L3 ever considered.

**Wiring `cachecorrelation.Analyze` into a live in-process background job that periodically re-reads this process's own recent log lines.** Rejected as a "live correlator process" — exactly what the plan explicitly says not to build in this pass, and it would only ever see this ONE instance's own events anyway, defeating the entire cross-instance point.

## Verification

`go build ./... && go vet ./... && go test ./... -race` from `gateway/` — every package `ok` except the two pre-existing, already-documented, environmental rootless-Docker failures (`TestIntegrationTwoGatewayInstancesShareOneRedisRateLimit`, `internal/ratelimit/redislimiter`'s own `TestMain`), confirmed unrelated (this pass touches neither package). `golangci-lint run ./...` → `0 issues`. `go run github.com/fe3dback/go-arch-lint@v1.18.0 check` → clean (new `cache-correlation` component registered for `telemetry/cachecorrelation`, a pure leaf like `telemetry` itself). `go mod tidy` → empty diff (stdlib-only addition). `gofmt -l .` → clean.

New tests: `internal/telemetry/cachecorrelation/cachecorrelation_test.go` — 11 unit tests over synthetic multi-instance `Event` fixtures, per the plan's own Verify line ("unit test on the correlation logic using synthetic multi-instance log fixtures"): a real cross-instance-avoidable miss is detected; same-instance repeats are excluded (today's own `missGroup` scope, not this metric's); an expired peer TTL window excludes; a peer event strictly in the future never retroactively opens a window; an explicit peer HIT also counts (the narrower reading stays a correctly-handled subset); two different tenants sharing a literal key string never correlate; two different keys for the same tenant never correlate; hits never contribute to `TotalMisses`; `EffectiveHitRateLoss` is a real computed fraction (0.4 for 2/5, verified by hand) and reports 0 (not a false "definitely zero loss") when there is no evidence; multiple qualifying peers still count a single miss exactly once. `internal/telemetry/result_test.go`'s existing `TestRecordCacheL3GateOutcomeIncrementsPerGateAndOutcome` was left as the pre-existing per-gate-ablation proof (the `instance_id` attribute addition doesn't change what that test already asserts, and is exercised implicitly by every call already made in it). `internal/gateway/dataplane/cache_cross_instance_telemetry_test.go` — 4 new full-pipeline tests proving the live emission side actually produces the tuple the correlation logic needs: an L1 miss then hit on a byte-identical repeat; an L2 hit (with L2's own distinct TTL, not L1's) on a normalized-but-not-exact repeat; an L3 hit reusing the SAME `cache_key` value as that request's own L1 miss (the load-bearing cross-layer identity proof); and confirmation that a volatile-bypass request never logs an L3 event at all (it never learned anything about presence).

Sanity-checked per this project's own established discipline: temporarily removed the `peer.InstanceID == miss.InstanceID` same-instance guard from `hasOpenCrossInstanceWindow` — 7 of the 11 `cachecorrelation` tests failed immediately, each for the exact expected reason (every test asserting same-instance/wrong-tenant/wrong-key/expired/future-peer exclusion now incorrectly counted those cases as cross-instance-avoidable), then restored; `git diff` on the package afterward shows zero trace of the temporary break.
