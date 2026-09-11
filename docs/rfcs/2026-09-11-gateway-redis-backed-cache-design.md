# RFC: Redis-backed L1/L2 cache — design only, no code

## Status

Design-only, explicitly deferred pending its own named trigger. Phase 4.1 of the round-4
upgrade plan. Mirrors the exact "designed in full, deferred" precedent already established for
the distributed `ConcurrencyLimiter` (`docs/upgrade-research/gateway-distributed-concurrency-limiter-2026-09-09.md`):
de-risk the "how" now, without committing to the "when."

## Context

Kelvran's `cache.Cache` interface (`gateway/internal/cache/port.go`) is deliberately narrow —
`Get(ctx, key) (resp, writtenAt, ok, err)` / `Put(ctx, key, resp, ttl) error`, value objects only,
no pointer into anything outside the package — specifically so a future adapter can satisfy it
without leaking in-process memory across a process boundary, per
`docs/decisions/0002-cache-embedded-in-gateway.md`. Today's only real implementation,
`inprocess.Cache` (and its sibling `inprocess.LexicalCache` for L3), is single-instance-only: an
in-memory map with jittered TTLs (`docs/rfcs/2026-09-10-gateway-cache-ttl-jitter.md`) and
`singleflight`-coalesced concurrent misses (`docs/rfcs/2026-09-05-gateway-cache-stampede-protection.md`'s
`dataplane.missGroup`, keyed on `l1Key`, inherently single-process). A second gateway instance
today has zero visibility into a sibling instance's cache contents — a real repeated-upstream-call
cost the moment more than one replica runs behind a load balancer without sticky routing.

**This is explicitly NOT a decision to extract Cache into a standalone service** —
`docs/decisions/0002-cache-embedded-in-gateway.md`'s own Revisit Triggers gate a much bigger
question (a separate deployable with its own auth/routing surface) that this RFC does not touch
at all. This RFC is scoped narrowly to swapping `cache.Cache`'s *storage backend* — from
in-process memory to a shared Redis instance — while `cache.Cache` stays embedded inside the
gateway binary, called in-process, exactly as it is today. The precedent for this exact kind of
swap already exists in this codebase: `gateway/internal/ratelimit/redislimiter` gives
`internal/ratelimit`'s rate limiter a second, Redis-backed, interface-compatible implementation
without touching the caller's own request-handling code at all, per
`docs/rfcs/2026-09-03-distributed-rate-limiting.md`.

**The real, already-fired-once, still-unfired-a-second-time trigger this design waits on**:
`docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md` built exactly the instrumentation
needed to answer "is cross-instance duplicate work or effective-hit-rate loss real and how
large" (`cachecorrelation.Analyze`, a pure function over `[]Event` a future log-parsing pass
would populate) — deliberately without building a live correlator, a log pipeline, or the
distributed cache itself, because Kelvran runs single-instance only today and the question can't
be measured yet. That RFC's own words: "decide whether to ever build a distributed/shared cache
only once that evidence exists." This trigger has not fired — Kelvran still runs single-instance
only (`docs/operations/DEPLOY.md` confirms no multi-replica deployment exists yet) — so this RFC
is deliberately design-only, matching that document's own stated intent, not a reversal of it.

## Design

### L1 (local, pure performance optimization) / L2 (shared Redis, sole coherence source of truth)

Unlike today's L1 (exact-match)/L2 (normalized-match)/L3 (lexical-fuzzy) three-layer split — a
*matching-strategy* dimension, unaffected by this RFC — this design introduces an orthogonal
*storage-tier* dimension **within** the exact-match layer specifically: a small, process-local L1
(an in-memory LRU, most likely `inprocess.Cache` reused as-is, holding only the hottest, most
recently seen keys) sits in front of a Redis-backed L2 that is the actual coherence source of
truth across every gateway instance. A read checks local L1 first (sub-microsecond, zero network
hop); on a local miss, it checks Redis; a Redis hit backfills local L1 before returning. A write
goes to Redis first, then updates local L1 — Redis is authoritative, local L1 is a pure cache-of-
a-cache with no correctness obligation of its own (a stale or evicted local L1 entry is
harmless — the next read simply pays one extra Redis round-trip). This is the standard two-tier
"local cache in front of a shared cache" shape (the same relationship the existing
L1-exact/L2-normalized split does NOT have today — those are two different matching strategies,
not two tiers of the same one), and requires no new interface: `cache.Cache`'s existing
`Get`/`Put` contract is unchanged; a new adapter package (`redisbackedcache`, sibling to
`inprocess`, mirroring `redislimiter`'s relationship to `ratelimit`) would internally compose an
`inprocess.Cache` and a Redis client behind the same two methods.

### Concrete Redis operations, mirroring `redislimiter.go`'s exact idiom

`redislimiter.go`'s pattern — a single atomic Lua script (EVALSHA, with go-redis's built-in
EVAL fallback on a NOSCRIPT miss), a Go-supplied clock as `ARGV` (never Lua's own `os.time()`),
and a defensive `EXPIRE` on every call so an idle key auto-expires — maps directly onto `Get`/
`Put` without needing anything as elaborate as the ConcurrencyLimiter design's lease+heartbeat
scheme (that design exists to solve *held-for-a-duration* semantics; a cache entry is not "held,"
it's simply present-with-a-TTL, a far simpler shape Redis's own native `SET key value EX ttl`
already models exactly):

- **`Put(ctx, key, resp, ttl)`**: a plain `SET key resp EX <ttl_seconds>` (or `PX` for millisecond
  precision, matching `inprocess.Cache`'s own jittered TTL granularity) — no Lua script needed at
  all; Redis's native per-key TTL already does exactly what `redislimiter`'s manual `EXPIRE` call
  exists to emulate on a hash that has no TTL semantics of its own.
- **`Get(ctx, key)`**: a plain `GET key`, `ok=false` on a Redis miss (`redis.Nil`), mapped to this
  package's own `(nil, time.Time{}, false, nil)` no-error-on-miss contract exactly as
  `inprocess.Cache.Get` already does. `writtenAt` needs one additional field: `Put` must store a
  small envelope (response bytes + a written-at timestamp), not raw bytes alone, since Redis's
  own `SET`/`GET` carry no metadata — a JSON or length-prefixed binary envelope, decided at
  implementation time, not this design pass.

### Invalidation: extend the existing guardrail-policy-version cache-key fold, not a new pub/sub channel

`cache.Key`/`cache.NormalizedKey` (`gateway/internal/cache/key.go`) already fold a
`guardrailPolicyVersion` string into every cache key's hash input — a policy change is invalidated
implicitly, for free, because it changes every future key, never by deleting old entries. The
same versioned-write pattern extends directly to Redis: no new invalidation mechanism, no pub/sub
fan-out to notify other instances a key changed, and no explicit `DEL` broadcast. A config change
that should invalidate cached content (a guardrail policy bump, a future price-table change that
should bust cost-sensitive cached answers, etc.) is expressed the same way it already is today —
fold a new version marker into the key — and Redis's own per-key TTL naturally reaps every
old-version entry without anyone deleting anything. This is a deliberate, structural choice: pub/
sub invalidation introduces exactly the new failure modes (partition, missed message, ordering)
`docs/upgrade-research/cache-2026-09-06.md` Finding 5 already names as the real cost of a
distributed cache — the versioned-key approach sidesteps needing it at all for every invalidation
case this codebase has needed so far.

### Cache stampede protection: `singleflight` stays local-only; Redis needs its own coalescing primitive if ever built

`dataplane.missGroup`'s `singleflight.Group` coalesces concurrent identical misses **within one
process** — it has no cross-instance awareness today, and this RFC does not change that. Two
different gateway instances can still independently miss on the same key at the same moment and
both call upstream, even with a shared Redis L2, for the same reason `docs/rfcs/2026-09-05-gateway-cache-stampede-protection.md`
already accepted this exact gap as "single-instance-only in v1; a distributed version remains
named future work, not built" — a cross-instance stampede lock (a short-lived Redis `SET key
lock_token NX PX lease_ms` per in-flight miss, released or expired before the real `Put` lands)
is a real, separate primitive this design deliberately does not build, named here as a distinct
follow-on rather than silently bundled into the storage-backend swap this RFC actually scopes.

### Why this needs no `docs/decisions/0002`-style extraction trigger

Re-stated for emphasis, since it is the single most important scope boundary of this whole
document: `docs/decisions/0002-cache-embedded-in-gateway.md`'s four Revisit Triggers all gate
extracting Cache into a **standalone service** with its own auth/routing surface — a materially
different, much larger question. Swapping `inprocess.Cache` for a `redisbackedcache.Cache` behind
the exact same `cache.Cache` interface, still called in-process by the exact same dataplane code,
changes zero of the properties those four triggers are about (no new auth surface, no new
routing surface, no change to who owns/deploys Cache). The only real gate this design needs is
the cross-instance-telemetry evidence trigger described above, which remains unfired.

## Alternatives considered

**Build the distributed cache now, skip the measurement step.** Rejected — this is precisely the
"introduces new failure modes without demonstrated benefit" pattern `docs/upgrade-research/cache-2026-09-06.md`
Finding 5 warns against, and the whole reason the cross-instance-telemetry RFC was built as a
separate, earlier, deliberately narrower pass.

**A Redis pub/sub invalidation channel instead of the versioned-key fold.** Rejected: introduces
a new failure mode (a missed/delayed pub/sub message leaves a stale entry alive past its intended
invalidation) that the existing versioned-key approach doesn't have, for no case this codebase
has actually needed yet — every real invalidation trigger so far (a guardrail policy change) is
already expressible as a new key-fold input.

**Extend `dataplane.missGroup`'s `singleflight` itself to be cluster-aware.** Rejected as
out-of-scope for a storage-backend design doc — `singleflight.Group` has no network-aware variant
in its own package, and building one is a distinct primitive (a distributed lock, not a cache
storage swap), named above as a real follow-on rather than folded into this design silently.

**A lease+heartbeat scheme mirroring the ConcurrencyLimiter design.** Rejected as unnecessary
complexity for this problem shape: a concurrency slot is *held* for an unbounded, unknown
duration (hence needing a renewable lease); a cache entry's TTL is known and fixed at write time
—Redis's own native per-key expiry already is the right primitive, with no held-duration ambiguity
to solve.

## Verification

None — this RFC is design-only, per its own Status line and this phase's own scope. No code
changes accompany it; a future implementation pass would need its own new `redisbackedcache`
package, tests mirroring `redislimiter_test.go`'s real-Redis-container integration pattern
(`testcontainers-go/modules/redis`, already a real dependency of this module today), and its own
RFC status update from "Design-only" to "Accepted, implemented."
