# Multi-Region/Geo-Distributed Cache Coherence Strategies for LLM Response Caches (2026-09-11)

Scope: whether real-world companies/tools operating multi-region AI gateways have a documented
cache-coherence model for cross-region LLM-response caching, and whether that precedent (or its
absence) changes the trigger point for Kelvran's already-designed-but-deliberately-unbuilt
Redis-backed L1/L2 cache (`docs/rfcs/2026-09-11-gateway-redis-backed-cache-design.md`) to extend
to multi-region. Externally verified against an arXiv LLM-caching paper (Cortex), the foundational
Probabilistically Bounded Staleness (PBS) paper, Redis's own Active-Active (CRDB) docs, and the
public docs/source of three real LLM-gateway-adjacent caching tools (LiteLLM Proxy, Cloudflare AI
Gateway, Portkey) and one open-source semantic-cache library (GPTCache), each claim adversarially
3-vote-verified before inclusion here.

## Executive Summary

No documented case study of a company scaling an LLM gateway's response cache from single-instance
to true multi-region geo-distribution was found; every real tool surveyed (LiteLLM, Cloudflare AI
Gateway, Portkey, GPTCache) stops at single-region "shared cache across N workers/replicas" and
none discuss cross-region coherence, so RQ1 is **not_yet** — Kelvran would be pioneering, not
following precedent, if it built one today. The real-world trigger point actually used in practice
for the far more common "single-instance → shared cache" step is purely topological (LiteLLM: "the
right default for anything past a single worker"; GPTCache: any multi-node/multi-replica
deployment) — not a request-rate or volume figure — which validates the *shape* of Kelvran's own
gate (real multi-instance deployment exists) but confirms no source extends that gate to a
region-level threshold. On the coherence-model question, general distributed-systems evidence (the
PBS paper; Redis's own Active-Active default of Strong Eventual Consistency, not linearizable
read-your-own-writes) shows eventual consistency is an established, quantifiable, accepted
tradeoff industry-wide — but this is general-purpose evidence, not LLM-cache-specific, and the
one LLM-caching academic paper found (Cortex) only confirms cross-region latency/cost is the
motivating problem, not a validated coherence model or quantified WAN-round-trip-vs-recompute
number (its own quantified benefit claims did not survive verification). On validation methodology,
the one real precedent for *why* a caching tool added distributed-cache support (GPTCache) did so
on pure architectural necessity, not a measured duplicate-work study — meaning Kelvran's
measure-before-build approach (`cache_cross_instance_check`) is more rigorous than the one
documented precedent, not less.

## Findings

### Finding 1 — No real company/tool has documented a multi-region LLM-cache coherence model; every surveyed tool stops at single-region

**Confidence: high**

Three independently-checked production tools that sit closest to Kelvran's own shape — a
cache layer bolted onto an LLM gateway/proxy — were searched specifically for multi-region or
geo-distribution content and none had any: Portkey's AI Gateway cache docs contain zero mention of
multi-region, geo-distribution, or cross-instance coherence anywhere on the page, in its Enterprise
docs, or in its GitHub README; GPTCache's own distributed-cache feature (Redis-backed, added via
GitHub issue #496 and PR #518) is explicitly scoped to multi-node/multi-replica horizontal scaling
**within one deployment** with zero mention of multi-region, WAN latency, or cross-region
coherence anywhere in the repo; Cloudflare AI Gateway's cache is exact-match-only (SHA-256 of
provider + endpoint + model + auth header + full request body) with no semantic matching yet and
no documented cross-region unification — and Cloudflare explicitly documents that it does **not**
even coordinate two identical concurrent requests within its own cache (`Cache in AI Gateway is
volatile... the second request [may retrieve] data from the original source`), i.e. a major
production AI gateway accepts a race/miss rather than paying for coordination even at the simplest
possible coherence problem (same key, same instant, one region). Portkey's semantic-cache blog
post (cosine-similarity + exact-match KV two-tier design) likewise describes no replication,
invalidation propagation, or consistency model of any kind. The one academic paper found that
frames LLM caching explicitly as a cross-region problem (Cortex, arXiv:2509.17360) confirms only
the motivating problem statement — LLM agents querying remote knowledge sources across regions
creates real latency/cost bottlenecks — not a validated or benchmarked coherence model; its
specific quantified throughput/hit-rate claims did not survive adversarial verification (refuted
0-3), so no confirmed quantified benefit exists to cite.

**Verdict: NOT_YET.** Exact trigger: a documented, verifiable case study or vendor commitment
showing a real multi-region LLM-cache coherence model in production does not exist yet in any
source found; this finding itself is the evidence gap, not a blocker Kelvran can resolve — it
should not wait on this precedent appearing, since it may never appear, but should also not treat
"Cortex" or any surveyed vendor as a validated template to copy.

### Finding 2 — The real trigger point in practice is topological (>1 worker/replica), not a volume/request-rate number — but no source extends this to a region-level threshold

**Confidence: high**

LiteLLM Proxy's official docs give an explicit, non-numeric trigger: "Redis is the right default
for anything past a single worker," with the mechanism stated directly — an in-memory cache lives
inside one worker process, so N workers keep N separate caches and the hit rate drops roughly
proportional to worker count. GPTCache's own distributed-cache feature request (issue #496) gives
the identical shape of trigger with zero volume/rate justification: "the caching eviction is done
in memory... which makes caching limited to the local system... distributed cache using Redis or
Memcached will allow gptcache to scale horizontally" — accepted by the maintainer on architectural
grounds alone, no benchmark or usage study attached. Both real precedents converge on "more than
one process/replica" as the trigger, matching the *shape* of Kelvran's own cross-instance-telemetry
gate (a real multi-instance deployment must exist) — but neither source distinguishes multiple
instances *within one region* from multiple instances *across regions*; no source anywhere gives a
region-count, latency-budget, or request-volume threshold specific to when cross-region
coordination (versus independent per-region caches) becomes worth its cost.

**Verdict: NOT_YET for the region-level threshold specifically** — the single-region "more than one
instance" trigger is well-precedented and already matches Kelvran's existing single-region RFC's
own gate; extending that gate to "more than one region" has no precedent to borrow and would need
to be defined from first principles if Kelvran ever operates in more than one region.

### Finding 3 — Eventual consistency is an established, quantifiable, industry-accepted tradeoff — but this is general distributed-systems evidence, not LLM-cache-specific validation

**Confidence: high** (for the general distributed-systems claim) / **not directly answered** (for
the LLM-cache-specific RYOW question)

The foundational Probabilistically Bounded Staleness (PBS) paper's own central, quantified finding
is that eventually-consistent Dynamo-style partial-quorum stores "frequently return consistent data
within tens of milliseconds... while offering significant latency benefits" despite having no
formal consistency guarantee. Redis's own flagship multi-region product (Active-Active/CRDB)
independently converges on the same choice at the vendor level: its default and documented model is
Strong Eventual Consistency (SEC) — guaranteed convergence without a consensus protocol — explicitly
*not* strict linearizable or read-your-own-writes consistency; stronger causal ordering exists only
as an opt-in add-on layered on top of the SEC baseline. Together these show that for general
key-value data, eventual consistency with a bounded (sub-100ms) staleness window is a mature,
well-quantified, and industry-default choice, not a niche compromise. However, neither source is
about LLM response caches specifically, and no confirmed claim in this research directly answers
whether a given tenant's own repeated queries need read-your-own-writes guarantees against an
LLM cache — the candidate evidence for that specific sub-question (Cosmos DB session-consistency
framing, sticky-session-routing as a cheap RYOW substitute) did not survive adversarial
verification and is listed under Refuted claims. This sub-question remains genuinely open.

**Verdict: BUILD_NOW is defensible only for "assume eventual consistency is acceptable by default,"
given the strength of the general precedent** — but this is an inference this research draws by
analogy (an LLM cache miss is cheaply re-computable, unlike a lost financial write, which if
anything makes the case for eventual consistency easier than Redis's own general-purpose use case),
not a claim any surveyed source makes about LLM caches directly. Treat as a reasoned default, not a
verified fact, until Kelvran actually has multi-region traffic to observe.

### Finding 4 — Kelvran's measure-before-build (`cache_cross_instance_check`) approach is more rigorous than the one real precedent found, not less

**Confidence: high**

GPTCache is the only real precedent found for *why* a team actually added distributed/shared-cache
support to an LLM-caching tool, and its own GitHub issue #496 and merged PR #518 show the decision
was made on pure architectural necessity — "caching eviction is done in memory using `cachetools`...
which makes caching limited to the local system" — accepted by the maintainer in a same-day reply
with zero mention of a measured duplicate-work rate, cross-instance miss-rate study, or any
benchmark preceding the build decision. LiteLLM's docs similarly frame the Redis-vs-local decision
as a structural inevitability ("the right default for anything past a single worker") rather than
something to measure first. This means the one concrete, verifiable answer to RQ3 is: **no**, real
teams did not validate ROI with a duplicate-work measurement before building — they built on
architectural/topological grounds alone. Kelvran's own approach (`docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md`'s
`cache_cross_instance_check` structured log line, deliberately built to measure before deciding)
is therefore *more* rigorous than the only documented precedent, not validated by a better-known
industry pattern it is merely replicating.

**Verdict: BUILD_NOW for keeping the telemetry-first approach as-is** — there is no better-precedented
validation method to switch to; if anything, Kelvran's approach exceeds the bar the one real
precedent (GPTCache) actually cleared. The exact trigger for consuming that telemetry remains
unchanged: a real multi-instance deployment plus a non-zero measured `EffectiveHitRateLoss`/
duplicate-work signal from `cachecorrelation.Analyze`, which has not fired.

## Caveats

No production company (as opposed to an OSS tool's docs, or a single academic paper) operating a
genuinely multi-region AI gateway with a documented cache-coherence story was found in this pass —
the closest thing (Cortex) is a September 2025 academic paper, not a production track record, and
its own quantified benefit numbers were refuted on verification, leaving only its motivating-problem
framing as confirmed. Several searches for harder, more specific claims — Azure Cosmos DB's
session-consistency framing as a RYOW precedent, quantified WAN-round-trip costs for strong
cross-region consistency, sticky-session routing as a cheap RYOW substitute, Redis's own
"local-latency-everywhere" claim for Active-Active, and a purpose-built article on Redis multi-region
architecture — were pursued and did **not** survive adversarial verification (see Refuted claims in
the source synthesis); their absence from this document is a verification failure, not an oversight,
and the underlying ideas may still be directionally correct even though the specific supporting
quotes did not hold up. Search-tool availability was degraded throughout this research pass (Exa
and Tavily rate-limited, DuckDuckGo/Bing/Google returning bot-challenge pages on several attempts),
so "no case study was found" should be read as "none was found via the tools available in this
pass," not as proof none exists anywhere. This document is scoped narrowly to LLM-response-cache
coherence across **regions**; it deliberately does not re-litigate the already-answered
single-region distributed-cache design question, which is fully covered by
`docs/rfcs/2026-09-11-gateway-redis-backed-cache-design.md` and
`docs/upgrade-research/cache-distributed-coherence-round4-2026-09-11.md`.

## Open Questions

- Does read-your-own-writes consistency actually matter for a given tenant's own repeated queries
  against an LLM response cache, or is plain eventual consistency provably sufficient? No claim
  that survived verification answers this directly — it remains the single biggest open gap in
  this research pass.
- What is the actual measured (not vendor-marketing) WAN round-trip cost of a cross-region cache
  lookup for an LLM-sized payload (multi-KB request/response, not a small KV row) versus simply
  accepting a regional miss and recomputing? Every candidate quantified answer found in this pass
  was refuted on verification.
- Is there a production company (not an OSS tool's public docs) anywhere that has actually built
  and operated a coherent multi-region LLM-response cache, documented in a blog post, conference
  talk, or postmortem this research's available search tools simply failed to surface due to
  rate-limiting/bot-blocking?
- If Kelvran ever does add a second region for reasons unrelated to caching (latency, data
  residency, DR), should the existing single-region Redis L1/L2 design
  (`docs/rfcs/2026-09-11-gateway-redis-backed-cache-design.md`) simply run as independent,
  uncoordinated per-region caches by default (accepting the miss on cross-region requests) rather
  than attempting any cross-region coherence mechanism at all — given Finding 1 found no
  precedent this would even be following?
