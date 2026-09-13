# Multi-Region Deployment and Disaster Recovery for the Gateway — Research Report

**Date:** 2026-09-13
**Scope:** `gateway/` deployment topology (`docker-compose.yml`, `docs/operations/DEPLOY.md`), `gateway/internal/telemetry/cachecorrelation`
**Method:** 3-vote adversarial verification against primary vendor/legal sources (Kong, Portkey, LiteLLM, DVARA, EUR-Lex, arXiv, AWS) plus one internal engineering ADR (Basin) and one comparative industry blog; 12 of ~24 candidate claims survived verification.

---

## Ground Truth (Kelvran, confirmed real, not a research finding)

- Kelvran today is architected and deployed as a **single gateway instance**: `docker-compose.yml` runs exactly one gateway container.
- `docs/operations/DEPLOY.md`'s Kubernetes section is explicitly documented as **"intended shape, not built"** — no real K8s manifests exist yet.
- A cross-instance cache-correlation analysis unit exists (`gateway/internal/telemetry/cachecorrelation`) but is explicitly disclosed as **not wired into the live request pipeline** — a standalone analysis tool only, not evidence of multi-instance production operation.
- Kelvran's first real production traffic (a live AWS Bedrock pilot) went live on **2026-09-13 — the same day as this report.**

These facts anchor the "is this a genuine near-term need" assessment below; they are not something this research round needed to re-verify, only to reason from.

---

## Executive Summary

No mainstream LLM gateway — Cloudflare AI Gateway, Kong AI Gateway, Portkey, OpenRouter, or TrueFoundry — ships a vendor-operated, latency-aware, active-active multi-region reference architecture today; the one clear exception is **LiteLLM**, whose proxy ships a real, concrete, self-hosted multi-region topology (shared primary Postgres, per-region Redis, documented env-var setup and a cross-region key-consistency verification procedure) as a paid Enterprise capability that the *customer* operates, not a managed geo-distributed service. What most of the industry actually ships instead is **data-residency architecture**, not geo-latency routing: Portkey's data-plane/control-plane split keeps inference traffic in the customer's chosen VPC while only metadata syncs to Portkey's cloud, and DVARA implements a hard fail-closed data-residency filter in its routing chain. Academic systems research (SkyWalker, UC Berkeley, 2025-2026) shows a real, quantified 25-40% cost benefit from cross-region LLM load balancing — but that benefit is mechanically driven by diurnal demand-variance across multiple, geographically distinct regions/timezones, a precondition Kelvran does not have with one instance and one day of live traffic. Legally, GDPR/AI Act/CLOUD Act constraints mean data residency can outright block an otherwise-available cross-region failover path, so any future multi-region design must treat compliance as a routing constraint, not an afterthought bolted on later. **Verdict: this is squarely a `not_yet` item** — multi-region/DR has no genuine near-term need for a project whose real production traffic is one day old and single-instance by design; the cheapest forward-compatible move worth making now is not infrastructure but ID/key-shape discipline (see Finding 5), mirroring a documented pattern from a comparable-stage real-world engineering team.

---

## Findings

### Finding 1 — Across the named peer gateways, only LiteLLM ships a concrete, documented multi-region reference architecture; the rest ship generic HA or nothing at all
**Confidence: high** (2 primary-source claims + 1 comparative industry claim, one 2-1 split resolved by direct primary-source re-verification)

- LiteLLM's proxy docs (`docs.litellm.ai/docs/proxy/multi_region`) describe a **real, operational** topology: every region's proxy instances point at the same primary-region PostgreSQL database, each region runs its own Redis for rate-limiting/caching, and the setup requires identical `DATABASE_URL`/`LITELLM_MASTER_KEY`/`LITELLM_SALT_KEY` across regions. The doc includes a numbered provisioning procedure (shared DB → cross-region network peering → deploy proxies → optional read replicas → geo-DNS → verify) and an explicit **verification step**: create a virtual key in the primary region's UI, confirm it works in the secondary region's UI, confirm spend attribution reconciles back to the primary. It also documents the active-active vs. active-passive (DR) distinction as the *same* topology differing only in DNS policy (claim 6).
- A six-gateway comparison (LiteLLM, Portkey, OpenRouter, TrueFoundry, Kong AI Gateway, Cloudflare AI Gateway) found only LiteLLM explicitly credited with a "multi-region" feature — and even there it is a **self-hosted Enterprise-tier capability the customer deploys and operates**, not a vendor-run multi-region service. None of the other five gateways use the term "multi-region" as a named capability anywhere in their public materials (claim 9).
- Kong AI Gateway's own architecture docs describe only a generic scaling/HA pattern — "run pools across availability zones or regions for locality and resilience" — with no dedicated geo-routing logic, no cross-region control-plane design, and no data-residency handling beyond that one sentence. This is real infrastructure flexibility, not a reference architecture (claim 0).

**Read on the research question:** the field is still mostly single-region-per-deployment in practice. The one vendor with a genuine multi-region blueprint frames it explicitly as an optional, customer-operated add-on for teams that already have multi-region *demand* — not a default or an assumed requirement.

**build_now / not_yet:** `not_yet`. If a future decision is made to build multi-region, LiteLLM's shared-primary-DB + per-region-Redis + geo-DNS topology is the closest real precedent to adapt, but nothing here indicates the field considers this table-stakes today.

---

### Finding 2 — Where gateways *do* build "geo-aware" architecture, it is almost always about data residency/sovereignty, not latency-optimized routing
**Confidence: high** (4 claims across 2 independent vendors, primary-sourced, one internally corroborated by two independent fetches)

- Portkey's hybrid deployment splits into a **Data Plane inside the customer's own VPC** (all AI traffic, inference, and provider calls happen here — prompts/completions never transit Portkey's infrastructure) and a **Control Plane hosted by Portkey** (administration, configs, analytics, non-sensitive metadata). Sync between the two is config-pull (routing configs, prompt templates) and metrics-push (anonymized operational metrics) — the customer picks the region for the Data Plane; only metadata leaves it (claim 1).
- Portkey additionally offers a logging mode that keeps request/response logs in the **customer's own Blob Store** rather than Portkey's cloud log store, explicitly marketed against data-residency and regulated-industry requirements (claim 2).
- DVARA implements this even more mechanically: a 5-stage routing fallback chain (health filter → **data-residency filter** → same-region preference → cross-region fallback → last resort) where the data-residency filter runs by reading a workspace's `data-residency.allowed-regions` metadata, resolved **server-side from the API key** — explicitly not trusting client-supplied region claims ("the caller supplies nothing, and cannot"). If the residency filter empties the candidate pool, DVARA raises a hard `403 DATA_RESIDENCY_VIOLATION` rather than silently routing out-of-region (claims 4, 5).

**Read on the research question:** "multi-region" in this space is doing double duty as a term — for LiteLLM it means geo-distributed availability/scale-out; for Portkey and DVARA it means keeping inference traffic *inside a boundary* the customer controls, which is a data-sovereignty concern more than a latency or DR concern. Any future Kelvran design needs to be explicit about which of these two problems it is solving, because the architectures differ (shared-DB scale-out vs. traffic-containment-with-fail-closed-filter).

**build_now / not_yet:** `not_yet` for either variant, but Finding 3 below explains why the residency-filter variant is the one worth remembering if/when a real trigger appears (regulated customer, EU/other jurisdiction requirement).

---

### Finding 3 — Data residency law creates a real availability-vs-compliance conflict that can outright block cross-region failover; US-incorporated providers remain reachable by US compulsion regardless of region
**Confidence: high** (3 claims — 1 vendor blog independently corroborated by AWS's own architecture guidance, 1 primary EU legislative text, 1 blog independently corroborated by the CLOUD Act's own Wikipedia/statutory summary)

- A regional outage cannot always "just" fail over cross-region: if the residency policy prohibits the cross-border path (e.g., EU-West → US-East), the failover logic must choose between violating the policy or violating availability targets — a real per-deployer policy decision, not a solvable-once engineering problem. This is corroborated independently by AWS's own Architecture Blog and Well-Architected guidance on data-residency-constrained recovery planning (claim 10).
- The EU AI Act explicitly does **not** override GDPR, the ePrivacy Directive, or the Law Enforcement Directive (Art. 2(7)) — meaning any actual cross-border-transfer constraint on prompts/completions containing personal data is governed by GDPR's own transfer mechanics (adequacy decisions, SCCs), not by AI-Act-specific rules. Practically: the AI Act itself does not add a *new* data-localization mandate for inference traffic, but it also does nothing to relax the GDPR constraints that already exist (claim 7).
- The US CLOUD Act lets US authorities compel **US-incorporated** cloud providers to produce data stored anywhere in the world — meaning EU-region deployments on AWS Frankfurt, Azure West Europe, or Google Cloud Belgium remain subject to US legal compulsion despite being geographically in-region. This is the well-established legal basis behind EU sovereign-cloud initiatives and is directly relevant to any assumption that "deploy in the EU region" alone satisfies EU data-sovereignty expectations for a US-headquartered gateway provider (claim 11).

**Read on the research question:** this is the concrete "data-residency/sovereignty constraints specific to LLM inference traffic" the research question asked about. The takeaway isn't merely "know your compliance regime" — it's that a multi-region *failover* design, if built naively, can be actively unsafe from a compliance standpoint (routing a request cross-border during an outage) unless the residency check is wired into the routing/failover decision itself, exactly as DVARA does (Finding 2).

**build_now / not_yet:** `not_yet` — there is no current Kelvran customer requirement that has surfaced this constraint. But this is the one piece of this research that should be treated as a **standing design constraint**, not a feature: if multi-region is ever built, the residency check must be part of the failover decision from day one, not retrofitted after a violation.

---

### Finding 4 — Academic research shows a real, quantified cross-region cost benefit, but its precondition (diurnal demand variance across multiple distinct regions/timezones) doesn't exist for a single-instance gateway
**Confidence: high** (1 primary source, UC Berkeley systems group, current 2025-2026 paper)

- SkyWalker (arXiv 2505.24095, Xia/Mao/Kerney/Jackson/Li/Xing/Shenker/Stoica) demonstrates cross-region LLM inference load balancing that reduces total serving cost by **25%** versus existing load balancers, and shows that provisioning for aggregate global peak demand (instead of each region provisioning for its own local peak) cuts cost by **40.5%**. The mechanism is diurnal load-pattern variance across US/Europe/Asia — peak-hour timing differs by timezone, so aggregating demand smooths the peak-to-average ratio from 2.88x–32.64x per-region down to 1.29x in aggregate (claim 3).

**Read on the research question:** this is real, current, credible evidence that cross-region routing has genuine economic value — but only once an operator has multiple regions with materially different peak-demand timing to exploit. A single-instance gateway with one day of production traffic has zero regions to arbitrage between; this benefit is not accessible until there is real multi-region, multi-timezone traffic volume to smooth.

**build_now / not_yet:** `not_yet` — this finding is itself the strongest evidence *for* the "needs production traffic at real geographic/volume scale first" framing in the research question. The benefit is real but conditional on scale Kelvran does not have.

---

### Finding 5 — The cheapest forward-compatible move available now is schema/ID discipline, not infrastructure — a documented pattern from a comparable-stage real engineering team
**Confidence: high** (1 primary source — a dated, current internal engineering ADR from a real, actively maintained open-source project)

- An ADR from Basin (a self-hostable serverless-Postgres project, dated 2026-04-30) documents a deliberate **single-region-only** decision for the same reason Kelvran would make one today — no current multi-region need — while preserving cheap optionality: WAL entries are keyed by `(project_id, partition_key)` so a region tag can be added to the partition key later **without rewriting the WAL**, and project IDs are ULIDs (not region-namespaced) specifically so a region prefix can be prepended later **without an ID migration** (claim 8).

**Read on the research question:** this directly answers "name the cheapest forward-compatible architectural choice worth making now." It is not a Kelvran-specific recommendation invented for this report — it is a real, documented precedent from a team at a comparable stage making the same build_now/not_yet call. Applied to Kelvran, the analogous move (if not already true) would be auditing whether Kelvran's own tenant/request/cache keys are structured so a region dimension could be added to the key/partition scheme later without a data migration — a cheap, low-risk check now vs. an expensive retrofit later.

**build_now / not_yet:** `build_now`, but only in the narrow sense of "verify/adopt this key-shape discipline where it costs nothing" — this is explicitly *not* a recommendation to build any multi-region infrastructure, control plane, or failover logic now.

---

## Overall Assessment

**Is multi-region/DR a genuine near-term need for Kelvran?** No. Three independent lines of evidence converge on `not_yet`:
1. The vendor landscape itself treats multi-region as an opt-in, demand-driven add-on (Finding 1) rather than a baseline expectation — even the one gateway (LiteLLM) that ships a real reference architecture frames it as something you add "when you need lower latency... or DR," not a default.
2. The one academic source with a quantified economic case for cross-region routing (Finding 4) requires a precondition — multiple regions with offsetting diurnal demand — that a single-instance gateway with one day of production traffic categorically does not have.
3. Kelvran's own ground truth (single gateway container, K8s section explicitly "intended shape, not built," cross-instance cache correlation explicitly not wired into the live pipeline) confirms there is no existing multi-instance operational substrate to extend into multi-region in the first place.

**Cheapest forward-compatible choice worth making now:** schema/ID discipline (Finding 5) — confirm tenant, request, and cache keys could absorb a future region dimension without a migration — not any infrastructure, routing, or control-plane build-out. If and when real geographic/volume-scale production traffic materializes, Finding 3's residency-aware-failover constraint should be the first design requirement gathered (before topology), because retrofitting a compliance check into an already-built failover path is exactly the kind of expensive rework the "build now vs. later" framing exists to avoid.

---

## Caveats

- **Source skew toward vendor docs and one comparative blog.** Most confirmed claims are vendor-authored (Kong, Portkey, LiteLLM, DVARA), which is appropriate for descriptive "how does X work" claims but means none of the performance/reliability claims from those vendors were independently stress-tested here.
- **DVARA is not one of the four gateways named in the original research question** (Cloudflare AI Gateway, Portkey, Kong AI Gateway, LiteLLM proxy) and self-describes as competing with LiteLLM/Portkey. Its data-residency filter is evidence about the smaller-vendor landscape, not proof the four named peers behave the same way. Its residency mechanism was also non-functional prior to v1.7.0 (it previously read a client-suppliable key that nothing in the product ever wrote) — a recent fix, not a long-proven design.
- **A meaningful number of candidate claims were refuted** (see below) — several plausible-sounding claims about Kong's deployment topology, AWS Bedrock cross-region inference as an already-shipping vendor pattern, quantified cross-region latency costs, and EDPB on-prem-as-strongest-safeguard did **not** survive 3-vote verification. Treat anything not listed in the Findings above as unconfirmed, even if it sounds plausible.
- **Time-sensitivity is moderate-to-high.** The AI Act consolidated text is current as of 2026-07-27; the SkyWalker paper was revised as recently as 2026-11-06 relative to this report's framing (systems research in this space is moving fast); LiteLLM and DVARA's specific feature sets are under active development. Figures and specific vendor capabilities should be re-verified before being cited in a design doc more than a few months from now.
- **The Portkey/DVARA "data residency" pattern and the LiteLLM "multi-region scale-out" pattern are genuinely different problems** being described with overlapping vocabulary in vendor marketing — conflating them in a future Kelvran design doc would be a real risk this report is trying to flag, not just a pedantic distinction.

### Refuted claims (for transparency — do not cite these)
- Kong AI Gateway having exactly one deployment topology with no self-hosted control-plane option (1-2).
- Kong's node failover being purely reactive with no region-aware routing (0-3).
- DVARA having no dedicated multi-region reference architecture as of its dev-docs snapshot (1-2).
- The EU AI Act's territorial scope being output-location-triggered with no data-localization mandate (1-2).
- A four-gateway comparison finding none of them document region-level failover (0-3).
- Cloudflare AI Gateway's edge network being an application-level multi-region design choice (0-3).
- LiteLLM OSS having no built-in multi-region/DR infrastructure at all (0-3).
- Data residency requiring EU-origin requests be served only by EU-region destinations absent recorded consent (0-3).
- AWS Bedrock cross-Region inference (CRIS) being a real, already-shipping vendor multi-region reference pattern (1-2).
- Named peer gateways being benchmarked purely on single-cluster throughput, not multi-region architecture (0-3).
- Quantified asymmetric cross-region latency costs (US-East↔APAC vs EU↔US-East) (0-3).
- US CLOUD Act jurisdiction creating a structural GDPR-bypass conflict for major LLM/cloud providers (1-2).
- EDPB identifying on-prem inference as the strongest safeguard, implying cloud multi-region routing doesn't fully solve sovereignty exposure (0-3).

---

## Open Questions

1. If a future customer requirement forces multi-region, does Kelvran's problem look more like Finding 1 (geo-distributed scale-out/latency) or Finding 2/3 (data-residency containment with fail-closed routing)? This research could not determine which is more likely to be the actual first trigger, and the two call for materially different architectures.
2. Are Kelvran's current tenant/request/cache key schemes (across Postgres, Redis, and any cache-key hashing in `gateway/internal/cache/`) already region-agnostic in the way Finding 5 recommends, or would adding a region dimension today require a migration? This report did not audit Kelvran's actual key schemas — that audit is the concrete next step if Finding 5's recommendation is adopted.
3. Does Kelvran have (or plan to have) any customers in a regulated jurisdiction (EU, or others with data-localization requirements) whose contracts would require the Finding 2/3 residency-filter pattern specifically, independent of any latency/DR motivation? This is the realistic trigger condition for `build_now` on data-residency routing, separate from the DR/scale-out question.
4. Given that LiteLLM's real multi-region topology is a shared-primary-Postgres design, would that pattern even fit Kelvran's architecture (Go gateway + embedded Cache, per `gateway/ARCHITECTURE.md`'s dependency rules), or does Kelvran's Cache-embedded-in-Gateway decision (`docs/decisions/0002-cache-embedded-in-gateway.md`) change the shared-state calculus enough that a different topology would be needed?
