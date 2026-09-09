# Cache Next-Upgrade Scan — Deep Research, Round 2 (2026-09-09)

*(This file was reconstructed from the completed workflow's structured result after the underlying run did not itself write the file to disk — the content below is the verified research output verbatim, not re-derived.)*

## Question

Now that `gateway/v0.3.0` is tagged and the prior round's top finding
(`docs/upgrade-research/cache-next-upgrade-2026-09-09.md`'s cross-tenant
provider-cache-leakage risk) has shipped as the `SharedAcrossTenants`
deployment flag (`docs/rfcs/2026-09-09-gateway-cache-shared-tenant-flag.md`)
— what genuinely **new** gaps does a fresh 2026 semantic-caching/production-
cache-system survey surface for Kelvran's embedded Cache specifically,
comparing against GPTCache, Portkey's semantic cache, the CacheProbe/
SAGAI'26 line of work, LiteLLM, Kong, Envoy, and Helicone?

## Context (Kelvran's own real, current state)

L1 exact-match in-process cache; L2 normalized-match layer; L3-lite lexical
near-duplicate matching with a hard entity-fingerprint + freshness/
volatility-risk gate (never a bare similarity threshold); `isVolatileQuery`
scoped to `role: "user"` messages only; per-gate outcome instrumentation
(`kelvran.cache.l3.gate_outcome`); cache-hit provenance (layer/similarity/
age-ms); concurrent-miss stampede protection via tenant-scoped
`singleflight.Group`; cross-instance duplicate-work measurement-only
telemetry (`cachecorrelation.Analyze`, deliberately unwired); guardrail-
policy-version-aware cache keying; provider-side prompt-caching passthrough
for Anthropic/Bedrock with the new `SharedAcrossTenants` flag.

## Summary

This round found no new gap in Kelvran's shipped lexical-near-duplicate L3
design itself — a 2026 cluster of peer-reviewed security research (NDSS
2026's "Cache Me, Catch You"; ICML 2026's CacheAttack, arXiv:2601.23088;
Nature Scientific Reports' SAFE-CACHE) formalizes and empirically
demonstrates 66-92% poisoned-hit rates and an 86% black-box hijack rate
against embedding-vector cosine-similarity/LSH semantic caches specifically
— none of it targets lexical near-duplicate matching, entity-fingerprint
gating, or freshness gating, the class Kelvran actually ships. This is
confirmatory, not a to-do: it strengthens the case for never weakening the
entity/freshness hard gate toward a bare similarity threshold and for
continuing to defer a real embedding-based L3 (Finding 1).

The single most concrete, actionable finding is a direct code read: Kelvran
already has 100% of the raw per-request data needed for a real "$ saved by
cache tier" metric, and needs only an aggregation query, not new
instrumentation — `dataplane.go`'s `finalize()` already computes a real,
priced notional cost on every cache hit (regardless of the `billable`
flag), with a code comment explicitly naming this as intended for "a
cache-savings dashboard." This puts Kelvran ahead of every 2026 production
framework surveyed — LiteLLM has no per-cache-tier cost metric at all, and
a real production LLM-caching paper's own authors call deriving this from
their own aggregate proxy "unsolved future work" (Finding 2). A second
small, concrete, low-risk gap: `inprocess.go`'s `Put()` sets a fixed TTL
with no randomized jitter, the textbook thundering-herd risk AWS's own
caching-best-practices docs and Portkey's semantic-caching blog both
independently name as a standard fix (Finding 3). Negative caching remains
genuinely unresolved industry-wide — LiteLLM's docs are silent on it and no
clean 2026 precedent exists anywhere surveyed (Finding 4). This round did
not resolve whether a cheaper intermediate step exists short of a full
distributed cache to make the already-built-but-unwired cross-instance
telemetry actionable in a single-instance deployment — a genuine open
question, not answered either way.

## Findings

### Finding 1 — Kelvran's decision never to build an embedding-vector semantic L3 is validated (not challenged) by a 2026 cluster of cache-poisoning/hijack research targeting exactly that mechanism (confidence: high) — **confirmatory, not a new action item**

A 2026 cluster of peer-reviewed security research (NDSS Symposium 2026's
"Cache Me, Catch You"; ICML 2026's CacheAttack, arXiv:2601.23088; Nature
Scientific Reports' SAFE-CACHE) formalizes semantic-cache keys as fuzzy
hashes with an inherent locality-vs-collision-resistance tradeoff, and
empirically demonstrates 66-92% poisoned-hit rates (GPTCache testbed, 0.8
similarity threshold) and an 86% black-box hijack rate (CacheAttack,
transferable across embedding models, with a financial-agent case study) —
plus a real-world timing angle: chat platforms embedding daily timestamps
in system prompts create a narrow, predictable daily collision-attack
window. All of this targets embedding-vector cosine-similarity/LSH matching
specifically; none of it names or attacks lexical near-duplicate matching,
entity-fingerprint gating, or freshness gating — the mechanism Kelvran
actually ships. **Verdict**: not a new action item — the attack surface
described requires embedding vectors as cache keys, which Kelvran's
`PRD.md`/`THREAT_MODEL.md` hard-gate design already structurally avoids.
Value is confirmatory: it strengthens the case for never weakening that
hard gate toward a bare similarity threshold, and for continuing to defer a
real embedding-based L3 (consistent with the prior round, not
re-litigating it).

### Finding 2 — Kelvran already emits 100% of the raw data for a real "$ saved by cache tier" metric; the gap is purely an aggregation query, and this puts Kelvran ahead of surveyed 2026 production systems (confidence: high) — **BUILD NOW, aggregation/dashboard only**

Direct code read: `gateway/internal/gateway/dataplane/dataplane.go`'s
`finalize()` computes `cost` via `p.costCalc.Calculate(...)` from
`resp.Usage` whenever `err == nil`, **regardless** of the separate
`billable` flag (which is `false` on every cache hit and singleflight-
coalesced follower) — so on a cache hit, `CostUSD` in
`telemetry.ChatCompletionResult` is a real, priced notional "what this
would have cost upstream" figure, not zero. A code comment at
`dataplane.go` (~lines 1621-1624) states this explicitly: cost is still
computed and reported specifically as informational "what this would have
cost" data, e.g. for a cache-savings dashboard. Combined with the
already-shipped `kelvran.cache.hit`/`kelvran.cache.layer` attributes, a
simple `SUM(CostUSD) WHERE CacheHit GROUP BY CacheLayer` aggregation is the
only missing piece — no new telemetry field required. By contrast,
LiteLLM's proxy docs describe no cost-per-cache-tier metric at all, and a
real production LLM-caching research paper (pollinations.ai/OreoLook, Aug
2026) explicitly states its authors could **not** derive a per-query
cached-cost/savings figure from their own Redis keyspace hit-rate proxy,
naming request-level cache-hit/avoided-token measurement as unsolved future
work in their own deployed system. **Fix**: build the aggregation/dashboard
query over already-emitted OTel attributes (`kelvran.cost.usd`,
`kelvran.cache.hit`, `kelvran.cache.layer`) — this closes an intentionally-
left-open seam the code's own author already anticipated, rather than
inventing new scope.

### Finding 3 — L1's fixed (unjittered) TTL is a real, checkable thundering-herd gap; Kelvran's overall eviction maturity is otherwise on par with or ahead of mainstream practice (confidence: high) — **BUILD NOW, small surgical fix**

`gateway/internal/cache/inprocess/inprocess.go`'s `Put()` sets
`expiresAt := now.Add(ttl)` with no randomized jitter added, so a burst of
entries written together (e.g. after a deploy, or a traffic spike) expires
in near-lockstep — the exact "thundering herd" pattern AWS's own official
caching-best-practices docs (`ttl = base + rand()*jitter`) and Portkey's
semantic-caching blog independently name as a standard, trivial-to-
implement fix. This is narrower than Kelvran's overall eviction maturity,
which is otherwise architecturally comparable or cleaner than mainstream
practice: Portkey's hosted semantic cache is pure max-age TTL with no
LRU/size-based eviction at all, and GPTCache's default eviction is a plain
single-process LRU (`cachetools`-backed, no persistence, no cost-
awareness) that treats freshness as a bolted-on similarity-evaluation-time
filter rather than folding it into eviction proper — Kelvran's design
(recency-list LRU + `maxEntries` cap + TTL, with freshness/volatility
handled as its own hard gate) is comparable or cleaner, just missing the
jitter step. **Fix**: add bounded random jitter to the TTL computation in
`Put()`; check whether L2's normalized-match layer or any TTL carried by L3
lexical candidates need the same treatment. Note: previously-surfaced but
refuted claims about vLLM multi-signal eviction and GPTCache's line-count-
based eviction did not survive verification and are correctly excluded.

### Finding 4 — Negative caching remains genuinely unresolved industry-wide; no clean 2026 precedent exists for Kelvran to adopt (confidence: medium) — **not yet, unchanged**

LiteLLM's proxy caching docs contain no discussion of negative/error-
response caching anywhere, and a GitHub issue-tracker search on the same
project found no shipped feature and only one open, unrelated feature
request (caching failed AWS credential/IMDS lookups — an infra-layer
cache, not an LLM-response cache); other "negative" hits were unrelated
counter-underflow bugs. **Trigger, unchanged from the prior round**: either
a mature external pattern emerging elsewhere, or Kelvran's own production
traffic showing a materially expensive class of reliably-erroring/refused
requests worth short-circuiting — neither exists yet, and building this
without precedent risks caching a stale refusal past the point a
provider's moderation/availability state changes.

## Caveats

- Finding 1's embedding-cache attack papers are all Jan-Aug 2026 preprints
  or very-recent peer-reviewed publications; CacheAttack's 86% figure has
  no independent replication yet beyond the authors' own paper. Treat as
  current-but-young. Its relevance to Kelvran is confirmatory/defensive,
  not a to-do — do not read it as requiring new work.
- A related LiteLLM-documented multi-turn/agentic cache-staleness risk was
  **not** independently traced through Kelvran's own L3 gate behavior in
  this pass — flagged as worth a targeted follow-up, not confirmed as an
  actual Kelvran exposure. `isVolatileQuery`'s `role: "user"`-only scoping
  is a plausible partial mitigation but wasn't verified against this
  specific failure mode.
- Finding 2's grounding in `dataplane.go` is exact and load-bearing (a real
  code read, not inferred) — the strongest, most actionable finding this
  round.
- Refuted claims excluded from the findings above (checked, did not survive
  3-vote verification): a Portkey per-cache-tier cost dashboard, GPTCache
  line-count eviction, vLLM multi-signal eviction, a drift-triggered-
  model-update poisoning defense, and a "lowest perturbation = hardest to
  detect" inverse-correlation attack claim.

## Recommendation for Kelvran

1. **Build the cache-savings aggregation query** (Finding 2) — zero new
   instrumentation needed, purely a `SUM(CostUSD) WHERE CacheHit GROUP BY
   CacheLayer` over already-emitted OTel attributes.
2. **Add bounded TTL jitter to L1's `Put()`** (Finding 3) — small, surgical,
   well-precedented; check L2/L3 for the same treatment while there.
3. **No action needed, confirmatory only**: Finding 1 validates the
   existing decision to never build embedding-vector semantic L3 — nothing
   to build, nothing to reconsider.
4. **Not yet, correctly deferred**: negative caching (Finding 4) — still no
   trigger.

## Open Questions

- Does Kelvran's L3 entity-fingerprint+freshness gate actually get
  exercised on multi-turn/agentic conversation-style requests in a way
  that could replay a stale turn (LiteLLM's documented failure mode), or
  does the existing `role: "user"`-scoped `isVolatileQuery` check already
  close this — needs a targeted repro against
  `gateway/internal/cache/lexical.go`, not just external precedent.
- No claim survived this round's adversarial verification addressing
  whether a cheaper intermediate step exists short of a full distributed
  cache to make the already-built-but-unwired cross-instance telemetry
  (`cachecorrelation.Analyze`) actionable in a single-instance deployment —
  is that because no such step exists, or because the research didn't look
  in the right place (e.g. a single-instance backtest/simulation over
  already-logged request history)?
- Once the cache-savings aggregation in Finding 2 is actually built, is the
  $-saved-by-tier number large enough to justify further cache investment,
  or so small it deprioritizes further cache work relative to other
  roadmap items?
- Should the TTL-jitter fix in Finding 3 apply only to L1
  (`inprocess.go`), or do L2's normalized-match layer and any TTL carried
  by L3 lexical candidates need the same treatment?
