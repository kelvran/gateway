# Cache Distributed/Multi-Instance Coherence Research — Round 4 (2026-09-11)

Scope: `gateway/internal/cache`'s distributed/multi-instance cache-coherence readiness.
Grounded against `gateway/ARCHITECTURE.md`'s cache/scaling sections,
`docs/decisions/0002-cache-embedded-in-gateway.md`,
`docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md`, and
`docs/upgrade-research/gateway-distributed-concurrency-limiter-2026-09-09.md`; externally
verified against go-redis/cache, Microsoft HybridCache, Redis client-side caching, Envoy load
balancing (Ring Hash/Maglev), envoyproxy/ratelimit, Redis Cluster, Azure multitenancy
guidance, and Facebook's memcache NSDI'13 paper, each claim adversarially 3-vote-verified
before inclusion here.

## Executive Summary

Kelvran's own `redislimiter` precedent generalizes cleanly to cache: a Redis-backed
L1(local)/L2(shared) split — local cache as a pure performance optimization, Redis as the
sole coherence source of truth — is exactly how go-redis/cache, Microsoft's HybridCache, and
Envoy's own `ratelimit` service are all built, so a "designed in full, deferred" RFC (the same
shape already used for the distributed `ConcurrencyLimiter`) is buildable NOW even though the
live-multi-instance trigger has not fired. For invalidation, HybridCache's own precedent is a
closer fit to Kelvran's already-shipped guardrail-policy-version-stamped cache keys than
pub/sub broadcast: a logical timestamp-cutoff/versioned-write, not a cross-instance notify.
Consistent-hash sticky routing (Ring Hash/Maglev) is a real, well-precedented technique, but
it structurally requires a load-balancer/gateway tier in front of the instance pool that
Kelvran's architecture does not document today, so it is NOT a drop-in intermediate step
absent that missing tier — and the framing that vendors position it as "an alternative to a
shared cache" did not survive adversarial verification. L3-lite's existing per-tenant cap sits
on the same isolation/cost axis a distributed cache would face, but the more specific claims
mapping that structure onto a "sharding is the natural next step for L3-lite" narrative were
refuted — that analogy is this research's own inference, not an industry-corroborated
pattern, and should be labeled as such if carried forward.

## Findings

### Finding 1 — A Redis-backed L1/L2 cache RFC is buildable now, mirroring the ConcurrencyLimiter precedent
**Confidence: high**

A Redis-backed L1(local)/L2(shared) cache directly analogous to `redislimiter` is concretely
specifiable today, even though the live-multi-instance trigger hasn't fired, following the
exact "designed in full, deferred" precedent already set for the distributed
`ConcurrencyLimiter`. go-redis/cache's `Options` struct pairs a `LocalCache` (TinyLFU) field
with a `Redis` field in one config, and Envoy's own `ratelimit` service independently
converges on the identical shape: a standalone decision path backed by shared Redis, with an
optional local `freecache` layer that only skips redundant reads for already-known negative
state (over-limit keys) — never the coherence mechanism itself. Kelvran's own
`gateway/internal/ratelimit/redislimiter/redislimiter.go` already implements exactly this
idiom (single atomic Lua script, Go-supplied clock, defensive EXPIRE) for rate limiting,
giving a real in-repo template to port to cache.

**Verdict: BUILD NOW** (design-only RFC).

### Finding 2 — Invalidation coherence: versioned-write/timestamp-cutoff fits Kelvran better than pub/sub
**Confidence: high**

HybridCache's own precedent (the closest real-world L1-in-process/L2-distributed analog
surveyed) does NOT propagate key/tag invalidation to other instances' local memory — it
updates only the current server + the shared distributed store, and tag-based invalidation is
a logical "ignore anything created before this point" timestamp cutoff, not physical deletion
or pub/sub broadcast. This shape is the closer fit to Kelvran's ALREADY-shipped mechanism (a
guardrail-policy/detector version bump busts cache entries via a stored, checked provenance
field) than adopting a new pub/sub-invalidation channel would be — no new broadcast
infrastructure is needed, only extending the existing version-stamp check to also compare
against a Redis-held cutoff. Separately, Redis's own client-side-caching (RESP3 tracking)
feature demonstrates push-based, server-driven invalidation as a real, shipped alternative
pattern — but its architecture is a local-cache-backed-by-Redis-as-source-of-truth pattern,
not a mechanism for multiple app instances to share one Redis-held value store the way
Kelvran's `redislimiter` or a future distributed cache would need; only the push-on-write
invalidation SEMANTICS transfer, not the storage/sharing architecture.

**Verdict: BUILD NOW** (mechanism choice — versioned-write, not pub/sub).

### Finding 3 — Consistent-hash sticky routing: real technique, no LB tier exists to hang it on
**Confidence: medium**

Consistent-hash sticky routing (Ring Hash/Maglev) is a real, production-proven technique with
a quantified bounded-disruption guarantee (adding/removing 1 host of N affects only ~1/N of
requests; Maglev trades ~2x more key movement on host removal for ~10x faster table build and
~5x faster lookup at 256K-entry scale, and Envoy's own docs recommend Maglev generally
including for Redis-adjacent use cases) — BUT it structurally requires an explicit
load-balancer/gateway policy resource (e.g. kgateway's `BackendConfigPolicy` under
`spec.loadBalancer.ringHash`/`.maglev`) sitting in front of the instance pool; it lives at
that LB tier, never inside an individual backend instance. Kelvran's own
`gateway/ARCHITECTURE.md` documents no separate load-balancer/proxy component today. A
further claim — that this pattern is explicitly framed by vendor docs as "an alternative to a
shared distributed cache" — did NOT survive adversarial verification (refuted 0-3) and should
be treated as this research's own inference, not an established industry framing.

**Verdict: NOT YET** buildable as an intermediate step for Kelvran absent a load-balancer
tier.

### Finding 4 — L3-lite's per-tenant structure: sound general theory, unproven specific mapping
**Confidence: medium**

L3-lite's structurally per-tenant cap does sit on a real, standard isolation/cost/complexity
axis (per-tenant-instance isolation = strongest data/performance isolation at highest
cost/complexity vs. a shared pooled instance = lowest isolation at lowest cost, per Azure's
own multitenancy guidance) that differs from L1/L2's single shared cap — and general
sharding best-practice (hash tenant ID rather than sequential/range assignment, use
consistent hashing to minimize key movement on shard-count changes) is solid, well-established
theory. HOWEVER, three more specific claims attempting to map this general theory directly
onto "L3-lite's per-tenant structure = a natural head start toward a sharded distributed
cache" did NOT survive adversarial verification (all refuted 0-3): that Azure names
tenant-sharding as the standard scale-out mitigation once a shared cache hits strain; that the
Storage/Data sharding pattern doesn't require duplicating a whole service's infrastructure;
and that lookup/directory-based sharding is "the standard multitenant sharding pattern." This
specific Kelvran-L3-lite analogy should be treated as unproven, not industry-corroborated.

**Verdict: NOT YET** as a validated build path — the general theory is sound but the specific
mapping to L3-lite is unsupported.

### Finding 5 — Additional 2026 precedents worth carrying forward
**Confidence: high**

Redis Cluster's native sharding model (16384 fixed hash slots, `HASH_SLOT = CRC16(key) mod
16384`, each master owning a subset, with NO server-side proxying — clients get a `-MOVED`
redirect and must reissue the query themselves) is the relevant groundwork if the shared
Redis store itself ever needs to scale beyond one node, and pushes routing-awareness into
Kelvran's own client code rather than into Redis. Facebook's memcache lease mechanism (a
rate-limited 64-bit per-key token, one grant per key per 10s) is a proven, quantified fix for
thundering herds in a multi-server cache (peak DB query rate 17K/s -> 1.3K/s, ~13x, over a
one-week measurement window on herd-prone keys) — directly relevant precedent for what a
Redis-backed L1/L2 would need on top of Kelvran's EXISTING single-instance singleflight-based
stampede protection (per `docs/rfcs/2026-09-05-gateway-cache-stampede-protection.md`) once
that protection needs to span multiple gateway instances.

## Caveats

This is a synthesis of already-verified claims plus targeted grounding against Kelvran's own
repo — no new web research was performed beyond reading those local files. The core trigger
question is unchanged: no live multi-instance Kelvran deployment exists to measure real
cross-instance duplicate work, so actually BUILDING a Redis-backed shared cache remains "not
yet" across every research question; only Finding 1's design-only RFC and Finding 2's
invalidation-mechanism CHOICE are "build now" in the sense of being specifiable without the
trigger. **Important nuance for a future RFC**: `docs/decisions/0002` gates extracting Cache
into its own standalone SERVICE (a different question) — it does NOT by itself gate giving
the embedded `cache.Cache` interface a new Redis-backed adapter (a storage-backend swap
analogous to `redislimiter`), so a Redis-backed L1/L2 need not wait on 0002's triggers, only
on the cross-instance-telemetry evidence trigger already established for cache specifically.
HybridCache's "no cross-instance L1 propagation" characterization sits behind open,
unimplemented GitHub feature requests (dotnet/extensions #5517/#7098/#7411) that could change
if Microsoft ships a backplane later.

## Open Questions

- If Kelvran's existing singleflight-based (single-instance) stampede protection is ever
  extended across instances via Redis, does it need an explicit Facebook-lease-style
  rate-limited-grant mechanism layered on top, or is a Redis-backed singleflight equivalent
  sufficient on its own?
- Should the Finding 1 RFC be commissioned now as a standalone deliverable, or does even the
  DESIGN work wait for the cross-instance telemetry to first show a nonzero
  `EffectiveHitRateLoss` signal?
- If Kelvran ever adds a load-balancer/gateway tier for unrelated reasons (TLS termination,
  multi-region, edge), should consistent-hash sticky cache-key routing be re-evaluated then?
- Does `docs/decisions/0002`'s extraction-trigger framing need an explicit addendum
  clarifying that a Redis-backed storage adapter for the embedded cache interface is a
  distinct, non-gated decision from extracting Cache into a standalone service?
