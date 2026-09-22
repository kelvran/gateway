# Real-Time Cost/Capacity Arbitrage Routing Across Clouds/Regions — Research Report

**Date:** 2026-09-22
**Scope:** Cross-cutting research question, not a specific `gateway/` module — informs whether any future multi-region work (see the sibling `gateway-multiregion-dr-2026-09-13.md` report) should ever be framed around **cost arbitrage** (route to whichever cloud/region has the cheapest available capacity/lowest queue latency for the *same model*) as opposed to latency-based failover or disaster recovery, which that report already covers.
**Method:** 3-vote adversarial verification against primary sources — one actively-maintained open-source router's own code/docs/tests (GitHub, commit-pinned), three current arXiv systems papers, four independently-worded AWS primary documentation pages plus one AWS ML blog post, and OpenRouter's own vendor docs (fetched live, cross-checked via two independent tool paths). 26 candidate claims were put to adversarial vote; 15 confirmed, 11 refuted.

---

## Ground Truth (Kelvran, confirmed real, not a research finding)

- Kelvran's Kubernetes manifests (`deploy/k8s/base/`) are real and committed as of 2026-09-14 (updated 2026-09-17/18 with a dedicated `serviceaccount.yaml` and an `overlays/eks-irsa/`) — this **supersedes** the 2026-09-13 DR report's ground truth that the K8s section was "intended shape, not built." They have never been applied against a live cluster and describe a **single-service, single-region** deployment shape only.
- No region dimension exists anywhere in Kelvran's deployment surface today: `docker-compose.yml` runs one gateway container; the only region-shaped config value in the whole repo is a single `AWS_REGION` env var consumed by `evals`' Bedrock judge calls (`docs/operations/DEPLOY.md`) — a single fixed region for one subsystem's provider calls, not a routable dimension.
- Kelvran's router (`gateway/internal/router`) has exactly one fallback attempt on error and no cost-based or region-based selection logic of any kind (per `gateway-2026-09-06.md` Finding 2 and `gateway/ARCHITECTURE.md`'s own "still not built, deliberately" list).
- These facts mean this report's findings are evaluated against a project with **zero existing infrastructure to arbitrage across** — same framing as the sibling DR report, carried forward rather than re-verified here.

---

## Executive Summary

Real-time cost/capacity arbitrage routing — automatically sending a request to whichever cloud/region has the cheapest available capacity or lowest queue latency for the *same model*, as distinct from latency-based failover or disaster recovery — essentially does not exist in shipping production infrastructure today, and barely exists as a research topic. Every region-aware routing mechanism examined across a comprehensive open-source router (128 named strategies, code-level inspection), two academic multi-cluster/multi-instance routing papers, and one model-selection-routing paper keys on latency, health/residency, or carbon intensity — never live price — and the one production layer with genuine same-model regional price variance, AWS Bedrock's cross-region inference (CRIS), deliberately decouples its real, dynamic, capacity-based routing decision from billing (which is pinned to the caller's *source* region, not whichever destination region actually serves the request), so no cost signal ever reaches that router either. The premise's own fixed-pricing-vs-real-variance distinction holds up: single-vendor hosted-model APIs genuinely have no destination-region price to arbitrage against, while Bedrock-style cross-region infrastructure has real regional price variance but no mechanism that routes on it. The closest real-world analogue to true cost arbitrage found anywhere is OpenRouter's default provider routing — a probabilistic, price-weighted load balance across *competing inference providers* hosting the same open model — but that is provider-substitution, not cloud/region-level arbitrage, and even OpenRouter's deterministic "always cheapest" mode is an explicit opt-in that disables the normal load-balancing behavior. Net: the research question's implicit hypothesis (a real gap between what's theoretically possible and what's shipped) is correct — the gap is not close to closing.

---

## Findings, ranked

### 1. No production or research routing system found implements real-time cross-region/cross-cloud cost arbitrage for a single model — every region-aware strategy examined keys on latency, health/residency, or carbon, never live price
**Confidence: high** (6 merged claims, 4 independent primary sources — one full code-level repo audit, three current arXiv papers, all unanimous or 2-1 votes independently re-verified)

- **nexus-llm-router** (github.com/Francis1998/nexus-llm-router — real, active: 125 stars, 35 forks, commits through 2026-09-22) enumerates 128 named `RoutingStrategyName` values. Every region-scoped strategy (`geo-region`, `region-tier-affinity`, `sticky-region-failover`, `region-failover-hysteresis`, `region-latency-p99-shed`, `multi-region-latency-hedge`, `region-carbon-blend`, `sticky-region-warmup`, `sticky-region-drain`) keys off latency percentiles, health/circuit-breaker state, residency/compliance tags, or carbon intensity. Two use estimated cost only as a *tie-breaker* between models within an already-eligible pool — never as a region-vs-region price comparison. Critically, `ModelCandidate` (the data model backing every strategy) has **no region-indexed pricing field at all** — a flat `input_cost_per_1k`/`output_cost_per_1k` per model, decoupled from `supported_regions`. A full-text grep for `arbitrage`, `spot_price`, `cross-region price`, `cheapest region`, and `cheapest cloud` across the strategy source returned zero matches. The only strategy the repo itself frames as a genuine cross-region *hedge* (`multi-region-latency-hedge`) triggers purely on p50 latency exceeding a threshold, not price.
- **vLLM Semantic Router** (arXiv 2603.04444v2, a real vLLM-project 2026 paper/repo) names `cost-optimized` alongside `latency-sensitive` and `multi-cloud enterprise` as *static, administrator-configured deployment-scenario types* — Boolean signal-decision configurations set once, not dynamically measured real-time infrastructure cost or queue-latency telemetry.
- **Lodestar** (arXiv 2606.00946v1) is a real, telemetry-driven online-learning router — but it routes only to different *instances of the same already-selected model within one Kubernetes cluster*, is explicitly described in its own text as "orthogonal: it operates after model selection," and its reward function is `−TTFT` (negative time-to-first-token) with zero cost/price/dollar/spot/billing terms anywhere in the paper. It has no cross-region or cross-cloud scope at all.
- **Solyx AI Grid** (arXiv 2606.15050v1) is hardware-telemetry-aware routing across geographically distributed GPU clusters for a single self-operated model (Llama 3.1 70B) — and its own Limitations section states verbatim: "Neither campaign evaluates cost impact or energy efficiency." Its evaluated metrics are exclusively latency/throughput/reliability.

**Read on the research question:** across the most concrete, currently-maintained artifact available (a 128-strategy open-source router) and three current academic systems papers, cost-based cross-region/cross-cloud routing for a single model is not merely absent from what's shipped — it's structurally unrepresentable in the data models these systems use (no region-indexed price field), and absent even from the metrics academic routing research chooses to measure.

**Relevance to Kelvran / relationship to the DR report:** this sharpens rather than repeats the DR report's `not_yet` verdict on multi-region generally — even the field's most comprehensive open-source region-aware router has nothing to copy for a *cost-arbitrage* framing specifically, because the underlying pricing data model doesn't support it. If Kelvran's router ever grows a region dimension, cost arbitrage is not a "add later" feature on top of an existing pattern — it would require inventing per-region price telemetry that no reference implementation surveyed here has.

---

### 2. AWS Bedrock cross-region inference (CRIS) is the one production layer with genuine same-model regional price variance, but its routing decision is real-time/dynamic on capacity only — billing is deliberately pinned to the caller's source region, so no cost signal ever reaches the router
**Confidence: high** (7 merged claims, 4 independently-worded AWS primary sources: two Bedrock user-guide pages, one prescriptive-guidance page, one AWS ML blog — all fetched live, mutually corroborating, no contradictions found)

- AWS's own docs state verbatim: "There's no additional routing cost for using cross-Region inference. The price is calculated based on the Region from which you call an inference profile" — pricing is keyed to the **source** region, never the **destination** region that actually processed the request.
- The *only* cost differential AWS discloses anywhere is a flat, static ~10% discount for choosing the "Global" profile type over "Geographic" — a pre-set rate-card markdown chosen once at call time, not a live per-request cheapest-region calculation. Global CRIS pricing is likewise calculated from the source region, confirmed independently on the Global-CRIS-specific doc page.
- CRIS's actual *routing* mechanism (which destination region serves a given request) is genuinely real-time and dynamic — AWS's own resilience-patterns blog states it "considers multiple factors, including model availability, capacity, and latency" and "the current load and available capacity in each potential destination Region." This is real capacity/availability arbitrage — just never keyed on price.
- Independently, real per-region price variance for the *same on-demand model* does exist on Bedrock's public pricing page (e.g., one model priced lower in US regions than in Mumbai/São Paulo) — so the "no price signal to arbitrage" conclusion is not because regional prices are uniform, but because CRIS's billing mechanism structurally ignores whichever price would apply to the actual serving region.
- CRIS is also explicitly self-described by AWS as "a capacity mechanism, not a failover or disaster recovery mechanism" that "does not protect against model or provider disruptions" — corroborated by three further independent AWS sources (a Ring case-study blog, AWS Prescriptive Guidance's CRIS reference page, and the original CRIS launch blog), none of which frame CRIS as DR/failover.

**Read on the research question:** this is the sharpest, most falsifiable answer the research produced. Bedrock CRIS is exactly the kind of infrastructure the research question hypothesized might make cost arbitrage possible (real regional variance + real cross-region routing) — and AWS's own documentation shows the two are deliberately kept separate: the routing decision is dynamic and capacity-aware, the price is static and source-region-anchored, and the two never intersect.

**Relevance to Kelvran / relationship to the DR report:** the 2026-09-13 DR report refuted a *broader* claim ("CRIS is a real, already-shipping vendor multi-region reference pattern," voted 1-2) without digging into CRIS's billing mechanics specifically. This report's finding is narrower and more skeptical in exactly the way the task asked for: CRIS *is* real, shipping, dynamic cross-region routing — it is just never cost-based, by AWS's own explicit design, which is a stronger and more specific claim than "not a reference pattern." If Kelvran ever adopts Bedrock cross-region inference for availability/throughput reasons, that adoption would deliver zero cost-arbitrage benefit as a side effect — the two would need to be pursued (or not) as fully independent goals.

---

### 3. OpenRouter's provider routing is the closest real-world analogue to genuine cost arbitrage found anywhere — but it is provider-level substitution across companies hosting the same model, not cloud/region-level routing, and its default is probabilistic, not deterministic-cheapest
**Confidence: high** (2 merged claims, 1 primary source — OpenRouter's own current docs, independently fetched twice via two different tools, byte-identical)

- OpenRouter's documented default behavior for a given model request: "load balance requests across providers, prioritizing price" via a 3-step algorithm — (1) exclude providers with recent significant outages, (2) among stable providers, weight selection by the **inverse square of price** toward cheaper providers, (3) use the rest as fallbacks. This is real, live, per-request economic weighting toward cheaper capacity — the closest thing to "cost arbitrage" found in this entire research round.
- Deterministic "always pick literally cheapest" is a real, documented, but explicitly opt-in mode: setting `sort: "price"` (or the `:floor` model-slug shortcut) **disables** the default load-balancing behavior entirely and tries providers strictly in price order.

**Read on the research question:** this is genuine evidence that cost-arbitrage-flavored routing is commercially viable and shipping today — just not at the cloud-region granularity the research question asked about. OpenRouter arbitrages across *inference providers* (distinct companies each hosting the same open-weights model, e.g., on different GPU fleets) rather than across a single vendor's own regions/clouds. This maps onto the research question's "hosted-model API" half more directly than any Bedrock finding does: for genuinely open, multi-provider-hosted models, real price variance across hosting providers exists and is arbitraged today — it's single-vendor hosted APIs (OpenAI, Anthropic, Gemini's own endpoints) and single-vendor cross-region infra (Bedrock CRIS) where no such signal exists or is used, respectively.

**Relevance to Kelvran / relationship to the DR report:** the DR report never examined OpenRouter. This is a genuinely new data point: if Kelvran ever wants a defensible "cost arbitrage" feature, the nearest real precedent to adapt is provider-level (multiple inference providers of the same open model, if/when Kelvran's adapter set grows to include open-weights providers), not region-level — OpenRouter's inverse-square-of-price weighting is a concrete, documented algorithm to borrow from rather than invent.

---

## Overall Assessment

**Does anything route to whichever cloud/region has the cheapest available capacity/lowest queue latency for the same model, distinct from DR failover?** No — not at the cloud/region granularity the question asks about. Three converging lines of evidence:

1. The most detailed available routing implementation (nexus-llm-router, 128 strategies, code-audited) and three current academic papers all key region-awareness on latency/health/carbon, never price — and in at least one case (nexus-llm-router's `ModelCandidate`), the underlying data model cannot represent per-region pricing at all without a schema change (Finding 1).
2. The one real production layer with genuine same-model cross-region price variance (Bedrock CRIS) has real, dynamic, capacity-based routing — but its own billing design deliberately severs any link between that routing decision and price, by AWS's own explicit documentation (Finding 2).
3. The closest real analogue to true cost arbitrage (OpenRouter) exists at a different layer entirely — competing inference providers of the same open model — not cloud/region selection within one vendor's infrastructure (Finding 3).

**Is it even possible, architecturally?** The research question's own framing survives verification cleanly: for single-vendor hosted-model APIs (OpenAI/Anthropic/Gemini's own endpoints), fixed global pricing genuinely removes any price signal to route on — there is nothing to arbitrage. For self-hosted or Bedrock-style cross-region infrastructure, real regional price variance genuinely exists — but no examined system, vendor, or paper routes on it; every one that has both real cross-region infrastructure *and* real price variance (Bedrock) has chosen, by design, not to connect the two.

---

## Caveats

- **Source concentration on one vendor for the strongest finding.** Finding 2's evidence is entirely AWS-authored (four pages/blog posts), which is the correct source type for "what does AWS's own product actually bill for," but means no independent (non-AWS) verification of Bedrock's real-world billing behavior was possible within this round.
- **nexus-llm-router, while real and actively maintained, is a mid-size open-source project (125 stars), not an industry-dominant reference implementation** — its absence of a cost-arbitrage strategy is strong evidence the pattern isn't common practice, not proof no such system exists anywhere.
- **A meaningful number of candidate claims were refuted** (see below) — treat anything not listed in the Findings above as unconfirmed, even if plausible-sounding.
- **Time-sensitivity is moderate.** The AWS Global CRIS feature is tied to newer models (Claude Sonnet 4.5-era) and is under active documentation revision; nexus-llm-router had commits as recently as this research's own date (2026-09-22). Bedrock's flat ~10% Global-vs-Geographic discount and per-token rate cards should be re-verified before being cited in a design doc more than a few months out.
- **This report does not independently re-verify OpenAI/Anthropic/Gemini's own fixed-pricing claim** — it is treated as the research question's own stated premise, not something this round's adversarial verification specifically tested against those three vendors' current pricing pages.

### Refuted claims (for transparency — do not cite these)
- The vLLM Semantic Router performing multi-endpoint, multi-provider/cross-cloud routing via content-based semantic signals (0-3).
- DVARA's built-in routing being scoped to provider-level failover only for the same model, with no cost/latency-based arbitrage (0-3).
- DVARA's vendor FAQ disclaiming automatic cross-provider failover reliability (0-3).
- Solyx AI Grid's routing being self-hosted/cross-site GPU-replica routing supporting the "arbitrage only meaningful for self-hosted infra" hypothesis (0-3).
- Bedrock Region selection being fully automatic/opaque with no documented selection criteria (0-3).
- AWS describing CRIS routing only as "worldwide" distribution with no claim of lowest-price/lowest-latency/capacity-based selection (1-2).
- CRIS automatically routing based on real-time availability/latency/demand signals as a standalone claim (0-3, superseded by the merged, more precise Finding 2 wording).
- AWS positioning Global CRIS as trading residency for aggregate cost/capacity benefits at the profile-configuration level (1-2).
- Bedrock CRIS charging the same per-token price as the source region regardless of destination, framed as directly undercutting cost-arbitrage (1-2, superseded by Finding 2's more precise merged wording).
- CRIS routing decisions being driven by a pre-configured 2-3 region set rather than price comparison, framed as "availability arbitrage, not cost arbitrage" (1-2, superseded by Finding 2).

---

## Open Questions

1. Do any single-vendor hosted-model API providers (OpenAI, Anthropic, Google Gemini) expose *any* region-selectable endpoint with genuinely different per-token pricing by region — as opposed to uniform global pricing — that this round did not specifically search for? The research question's premise assumes uniform pricing for these three; that premise itself was not independently re-verified against their current pricing pages this round.
2. If Kelvran's adapter set ever grows to include open-weights models served by multiple competing inference providers (Together, Fireworks, DeepInfra, Groq, etc. — the OpenRouter-style landscape), would Kelvran build its own price-weighted provider-selection logic (per Finding 3's inverse-square-of-price precedent), or would routing through OpenRouter itself as an upstream provider be the more cost-effective way to get that arbitrage without building it?
3. Does any hyperscaler *other than AWS* (Azure OpenAI, Google Vertex AI) document a cross-region inference mechanism analogous to Bedrock CRIS, and if so, does it share CRIS's same source-region-pinned billing design, or does one of them actually price by destination region? This round examined only AWS's implementation in depth.
4. Given that nexus-llm-router's `ModelCandidate` schema has no region-indexed pricing field, is that a deliberate design choice (the maintainer judged it unneeded) or simply unaddressed scope — and would a feature request/PR proposing one surface any maintainer reasoning that would sharpen this report's "structurally unrepresentable" framing?
