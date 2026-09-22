# Kelvran Internal Chargeback / Cost-Allocation — 2026 Upgrade Research

**Date:** 2026-09-22
**Scope:** How a platform team running one shared LLM gateway attributes/reports spend back to internal consuming teams (chargeback/showback) — distinct from external customer billing/invoicing, which `docs/upgrade-research/billing-monetization-integration-2026-09-15.md` already researched. Grounded against Kelvran's real, shipped `internal/budget.Tracker` (Decimal-precision, per-virtual-key, Reserve/Reconcile enforcement) and `internal/costaccounting.Calculator`, plus OTel cost telemetry.
**Method:** Adversarial multi-source research (3-vote verification per claim) against 2026 FinOps-for-AI and LLM-gateway practice: the FinOps Foundation's own Framework, the FOCUS (FinOps Open Cost & Usage Specification) v1.4 standard, LiteLLM, api7.ai's AI-gateway cost-control guidance, and two independent TrueFoundry engineering posts, plus an independent blog (tianpan.co) with a developed chargeback-maturity argument. 14 claims survived adversarial vote (confirmed), 11 refuted. This document merges semantic duplicates from that raw claim set and adds the Kelvran-specific "why it matters here / is this consistent with prior research / effort tier" layer, same convention as the sibling `gateway-2026-09-06.md` and `billing-monetization-integration-2026-09-15.md` reports.

---

## Executive summary

Chargeback (actual money moving between internal cost centers) and showback (visibility/attribution reporting with no money movement) are distinct FinOps practices with different maturity bars, not two names for the same thing — the FinOps Foundation's own Framework treats showback as always required and chargeback as optional and organizational-policy-dependent, and independent industry guidance converges on a strict showback-before-chargeback sequencing because chargeback against attribution data a team disputes is "a political tar pit." Kelvran's prior research already answered the external-billing half of this question (`billing-monetization-integration-2026-09-15.md`: not_yet, no real paying customer exists) and the allocation-*dimension* half (`cost-intelligence-finops-2026-09-14.md` Finding 4: Kelvran's existing `agent_run_id`/`virtual_key_id` granularity already exceeds FOCUS/FinOps Framework's own column taxonomy) — this report's fresh angle is the internal-chargeback-specific gap neither of those touched: Kelvran's `budget.Tracker` is keyed by a single flat virtual-key ID with no team/org rollup, so there is today no way to answer "what did team X spend in aggregate" without manually summing per-key reads, which is exactly the missing piece LiteLLM's and api7.ai's own gateways name as *the* mechanism that makes showback/chargeback possible from a shared instance. This research also surfaces one concrete, currently-actionable correctness risk for Kelvran's own cost telemetry (a cache-token double-counting bug class in the OTel GenAI spec's `input_tokens` field) and one forward-looking design constraint for whenever a real chargeback rate is ever set (bill on outcome-adjacent work units, not raw token volume, since consuming teams don't control what drives token counts). Chargeback itself remains correctly deferred per prior research; this report refines what "getting ready for it" concretely means rather than overturning that deferral.

---

## Findings, ranked

### 1. Showback and chargeback are different FinOps maturity stages with different bars — target showback next; chargeback stays policy-gated and deferred (Small)

**What:** Two prior research passes each answered an adjacent half of this question without addressing the showback-vs-chargeback distinction itself. `billing-monetization-integration-2026-09-15.md` researched *external* customer billing (Stripe, Metronome, Orb, OpenMeter, Lago) and concluded `not_yet`, explicitly naming "a specific finance mandate to convert existing showback numbers into real chargeback" as one of exactly two triggers that would change that verdict (Finding G). `cost-intelligence-finops-2026-09-14.md`'s Finding 4 checked whether Kelvran's cost-data *fields* (agent-run-id + virtual-key-id granularity) meet FOCUS/FinOps Framework's allocation-dimension taxonomy — they already do, verdict "no action needed." Neither pass examined the FinOps definitional distinction between showback and chargeback as *practices*, or the recommended order to introduce them, which is what this report found.

**Why it matters for Kelvran specifically:** the FinOps Framework states showback is "always required in any FinOps practice" while chargeback is "not always required in every organization... dependent on organizational accounting policies" — and explicitly says neither is more mature than the other, rejecting the common misconception that chargeback is a maturity upgrade from showback. Independent industry guidance (a developed engineering blog, cross-referencing the FinOps Foundation's own AI working groups) converges on a concrete three-rung ladder — showback (attribution/visibility, no bills) → budgets (soft/hard enforcement) → chargeback (real inter-cost-center billing) — with chargeback deliberately deferred until showback has made the attribution numbers trustworthy, because "chargeback against attribution data that teams dispute is a political tar pit." Kelvran already has rung 2 (`budget.Tracker`'s synchronous Reserve/Reconcile enforcement, `BudgetAlertBuckets`) and a single-tenant sliver of rung 1 (the `CostViewer`-gated per-key spend endpoint), but has never named multi-team showback as its own deliverable — it has only ever been discussed as a precondition check inside other reports.

**Consistent with settled decisions?** Reinforces, does not contradict, both prior `not_yet` verdicts — it explains *why* chargeback specifically (not showback) has the harder, policy-gated bar, and confirms the trigger `billing-monetization-integration-2026-09-15.md` already named (a real finance mandate to convert showback into chargeback) is the correct one per the wider FinOps literature, not an ad hoc guess.

**2026 best practice grounding:**
- FinOps Framework's Invoicing & Chargeback capability page states chargeback is "not always required in every organization" and is "dependent on organizational accounting policies," while showback "is always required in any FinOps practice," and explicitly: "Neither type of reporting should be considered more mature than the other" [finops.org — confirmed 3-0].
- The same page defines a concrete, checkable KPI for reconciling recharge coverage: General Ledger Recharge Rate = (Total CSP Cloud Spend recorded as a Journal in the General Ledger) / (Total CSP cloud spend for the month), plus a per-cost-center variant [finops.org — confirmed 3-0].
- An independent engineering blog's three-stage progression (showback → budgets → chargeback), explicitly tied to the FinOps Foundation's own AI working groups, states chargeback "only works when rungs one and two have made the numbers trustworthy" and that disputed attribution data makes chargeback "a political tar pit" [tianpan.co/blog/2026/07/02/who-pays-for-the-tokens — confirmed 2-1, high-confidence primary-source read].

**Concrete next step:** explicitly name and scope "team-level spend showback" (multi-consuming-team visibility, zero money movement) as the next chargeback-adjacent milestone/RFC — separate from, and prior to, any future chargeback/invoicing build. This is a scoping decision, not new code.

**Effort:** Small — a naming/sequencing exercise that gives future work (Findings 2–4 below) a clear "not yet, but this is next" home instead of remaining scattered across unrelated reports' caveat sections.

---

### 2. Kelvran's budget.Tracker has no team/org rollup — LiteLLM's and api7.ai's team-scoped budget + grouped spend-report endpoint is the concrete showback precedent it lacks (Medium)

**What:** `gateway/internal/budget/budget.go`'s `Tracker` struct keys every piece of state — `spent`, `periodStart`, `periodEpoch`, `billedCount`, `highestAlertedBucket` — as `map[string]...` on a single flat key ID (the virtual-key ID). There is no team, org, or user level anywhere in the type. LiteLLM's proxy, by contrast, attributes every request's spend simultaneously up an Org → Team → User → Key hierarchy and ships a dedicated `/global/spend/report?group_by=team|customer` endpoint whose own docs state its purpose plainly: "Use this to charge other teams, customers, users" — and separately, LiteLLM's docs describe the same Org→Team→User→Key spend hierarchy as "what makes per-tenant chargeback and showback possible from a shared instance." api7.ai's AI-gateway cost-control guidance independently frames a Team-scoped budget's purpose the same way: "Showback or chargeback."

**Why it matters for Kelvran specifically:** this is the concrete gap `cost-intelligence-finops-2026-09-14.md`'s Finding 4 didn't examine — that finding checked whether Kelvran's cost-data *fields* match external schema-column taxonomies (they do), not whether a *reporting/grouping endpoint* exists to roll per-key spend up to a team level (it doesn't). Kelvran's only spend-read surface today, `GET /admin/virtual_keys/{name}/spend` (per `billing-monetization-integration-2026-09-15.md`'s Finding A), answers "what did this one key spend" — there is no way to answer "what did team X spend in aggregate across its N virtual keys" without a human manually summing N separate API calls, which is precisely the capability two independent gateway vendors name as the enabling mechanism for showback/chargeback.

**Consistent with settled decisions?** Additive, not a reopening — `budget.Tracker`'s synchronous Reserve/Reconcile enforcement path and the `CostViewer` credential tier stay untouched; this proposes a new read-side aggregation only, following the same "narrow, billing-safe credential" precedent `CostViewer` already established.

**2026 best practice grounding:**
- LiteLLM's multi-tenant architecture docs: spend flows up Key→User→Team→Organization simultaneously, and "this is what makes per-tenant chargeback and showback possible from a shared instance" [docs.litellm.ai/docs/proxy/multi_tenant_architecture — confirmed 2-1].
- LiteLLM's `/global/spend/report` (Enterprise) endpoint supports `group_by=team` or `group_by=customer`, stated purpose "charge other teams, customers, users" [docs.litellm.ai/docs/proxy/cost_tracking — confirmed 3-0].
- api7.ai's layered budget-scope table lists a Team scope whose purpose is "Showback or chargeback," as part of a broader org/environment/team/key/provider/member hierarchy [api7.ai/blog/ai-gateway-cost-control — confirmed 3-0].

**Concrete next step:** if/when a second real internal-team virtual key exists (the same trigger `billing-monetization-integration-2026-09-15.md`'s Finding G already named), add a lightweight `team` tag to `VirtualKeyConfig` (an additive string field, same precedent as `agent_run_id`) and one new admin-API aggregate endpoint that sums `budget.Tracker` spend across every key sharing a team tag — not a full Org→Team→User→Key hierarchy rebuild, just enough grouping to answer the showback question.

**Effort:** Medium — one additive config field plus one new read-only aggregation endpoint over existing `Tracker` state; zero changes to the enforcement path.

---

### 3. If/when a real chargeback rate is ever set, bill on outcome-adjacent work units, not raw token volume (Small — design constraint, not code)

**What:** An independent engineering blog argues chargeback should not bill on raw token volume because consuming teams don't control the variables that actually drive token counts — RAG context-stuffing, model-routing choices, response verbosity are all decisions the *platform*, not the consumer, makes. It recommends billing on "outcome-adjacent work units" instead (per resolved conversation, per document processed, per completed task) with a rate set per unit, informed by but deliberately smoothed relative to measured token costs, revised quarterly.

**Why it matters for Kelvran specifically:** Kelvran's `costaccounting.Calculator` computes `cost_usd` directly from raw provider token counts against a static price table. Today this only feeds showback-style reporting (a dollar figure a human reads), which sidesteps the controllability problem entirely — but the moment any real chargeback rate is ever derived directly from that same raw per-token figure, teams would be billed for gateway-side routing decisions they never made, which is exactly the kind of attribution dispute Finding 1's sources warn turns chargeback into "a political tar pit." This is a forward-looking design constraint to record now, not a code change today, since chargeback rate-setting hasn't been designed at all yet (correctly, per Finding 1's own deferral).

**Consistent with settled decisions?** New consideration; does not conflict with anything settled, since no chargeback-rate design exists yet to revise.

**2026 best practice grounding:**
- "Billing on token volume violates the first rule of chargeback: charge people for what they can change" — consuming teams don't control RAG context size, retry structure, or which model the gateway routes them to. Recommended alternative: charge per completed task/resolved conversation/document processed, with a smoothed per-unit rate revised quarterly as the platform optimizes [tianpan.co/blog/2026/07/02/who-pays-for-the-tokens — confirmed 3-0].

**Concrete next step:** record this as a named constraint for whenever a chargeback-rate RFC is eventually written (per Finding 1's own deferred sequencing) — do not propagate `costaccounting.Calculator`'s raw per-token `cost_usd` directly into a future chargeback invoice/journal line without first considering an outcome-based unit rate instead.

**Effort:** Small now (a documentation/design-constraint note); real effort deferred to the not-yet-triggered chargeback RFC itself.

---

### 4. FOCUS 1.4's Split Cost Allocation columns are a candidate standardized export vocabulary for a future showback report — complementary to, not a replacement for, the event shape already scoped for external billing (Small, later)

**What:** `billing-monetization-integration-2026-09-15.md`'s Finding C already scoped the *event* shape a future external-billing export would need (`trace_id` as idempotency key, `occurred_at`, `cost_usd`, `virtual_key_id`, `agent_run_id`). FOCUS (the FinOps Open Cost & Usage Specification) is a different, complementary artifact: a vendor-neutral, standards-body-defined *column* vocabulary specifically for describing how a shared/pooled cost was split across internal consumers — `AllocatedResourceId`, `AllocatedResourceName`, `AllocatedMethodId`, `AllocatedMethodDetails` — which is a closer published analog to "how do you attribute one shared gateway's pooled spend across many internal teams" (Kelvran's own chargeback/showback question) than the external-invoicing event shape the prior report answered. FOCUS's own documentation explicitly names "Internal teams who do chargebacks for data center usage" as one of its adopting personas, and multiple hyperscalers/platforms already emit FOCUS-conformant exports at various spec versions (AWS, Microsoft/Azure, Google Cloud at v1.2; Snowflake, Databricks, MongoDB, Vercel at v1.3), so mapping a future Kelvran showback export onto FOCUS's column names would make it legible to any tool that already ingests FOCUS, rather than requiring a bespoke adapter.

**Why it matters for Kelvran specifically:** `cost-intelligence-finops-2026-09-14.md`'s Finding 4 already checked that Kelvran's existing granularity is *at least as fine as* FOCUS's dimensions (yes, no action needed) — this finding goes one step further, proposing FOCUS's actual column *names* as a future export vocabulary, not just a granularity bar Kelvran already clears.

**Consistent with settled decisions?** A new option, not a conflict — extends rather than revisits Finding 4 of the prior FinOps report.

**2026 best practice grounding:**
- FOCUS 1.4's "Data Generator-Calculated Split Cost Allocation" feature defines `AllocatedResourceId`/`AllocatedResourceName`/`AllocatedMethodId`/`AllocatedMethodDetails` as the columns recording how a shared resource's Billed/Effective Cost was split across consumers [focus.finops.org/docs/specification/v1-3/features/data-generator-calculated-split-cost-allocation/ — confirmed 3-0; carried forward unchanged into the current v1.4 spec].
- FOCUS is described by its own site as normalizing billing datasets "across AI, cloud, SaaS, data center, and other technology vendors," and explicitly lists "Internal teams who do chargebacks for data center usage" as an adopting persona [focus.finops.org — confirmed 2-1].
- Current published version is FOCUS 1.4, shipping a defined Column Library including Allocation-category fields (`AllocatedTags`, `AllocationMethodId`, `AllocationMethodDetails`, `AllocatedResourceId`, `AllocatedResourceName`) [focus.finops.org — confirmed 2-1].
- AWS, Microsoft/Azure, and Google Cloud ship FOCUS-conformant exports at v1.2; Snowflake, Databricks, MongoDB, and Vercel at v1.3 — confirming real, current multi-vendor adoption at varying versions [focus.finops.org — confirmed 2-1].

**Concrete next step:** no code today — this stays gated behind Finding 1's own showback-first sequencing. When/if the team-level aggregation endpoint (Finding 2) is ever exposed as an export (CSV/API), prefer mapping its columns onto FOCUS's `AllocatedResourceId`/`AllocatedMethodId`/`ChargeCategory` vocabulary over inventing bespoke names.

**Effort:** N/A now; Small later — a naming/mapping choice made at export-design time, not new logic.

---

### 5. A concrete, currently-relevant cost-computation bug class for Kelvran's own OTel cost telemetry: cache-token double-counting (Small — verification, not new build)

**What:** `gateway-2026-09-06.md`'s own Finding 3 recommends adding `gen_ai.client.token.usage` and `gen_ai.client.operation.duration` OTel histogram instruments at Kelvran's existing `finalize` call site, and `costaccounting.Calculator` already has cache-token discount pricing (per `billing-monetization-integration-2026-09-15.md`'s "already shipped" list). The OTel GenAI semantic-conventions spec defines `gen_ai.usage.input_tokens` as *including* both `cache_read` and `cache_write` token subtotals — they are subsets of the total, not additive to it. A pricing/attribution function that subtracts `cache_read` tokens from `input_tokens` to isolate "fresh" tokens, but forgets to also subtract `cache_write` tokens, double-counts the cache-write portion at both the fresh-input rate and the cache-write rate — silently inflating both the per-request `cost_usd` and any future chargeback figure derived from it.

**Why it matters for Kelvran specifically:** this is exactly the kind of bug that would make a chargeback number "dispute-worthy" per Finding 1's own warning — attribution data a team doesn't trust is the one thing every source in this research agrees chargeback cannot survive. It is directly relevant to two pieces of work already in flight or recommended elsewhere: `gateway-2026-09-06.md`'s new OTel instruments, and `costaccounting.Calculator`'s existing (already-shipped) cache-discount logic.

**Consistent with settled decisions?** A verification/regression-test item against already-planned or already-shipped work — not a new build.

**2026 best practice grounding:**
- `gen_ai.usage.input_tokens` is defined as the total including both cache lines; subtracting once for `cache_read` but not `cache_write` "double-counts the cache-write portion" — independently corroborated directly against the OpenTelemetry GenAI semantic-conventions spec itself, which documents both `cache_read.input_tokens` and `cache_write.input_tokens` as values that SHOULD be included in `input_tokens` [truefoundry.com/blog/llm-cost-attribution-team-budgets, cross-checked against open-telemetry/semantic-conventions-genai — confirmed 3-0].

**Concrete next step:** before or immediately after adding the new OTel histogram instruments (`gateway-2026-09-06.md` Finding 3), add a unit test to `costaccounting.Calculator`'s cache-discount path asserting `cache_read` and `cache_write` are each subtracted from `input_tokens` exactly once, never double-subtracted or omitted.

**Effort:** Small — one targeted unit test / review pass on existing cache-pricing logic.

---

### 6. Two of Kelvran's already-shipped mechanisms already match 2026 industry pattern — no action needed, one threshold difference noted (None)

**What:** Two confirmed claims describe industry-standard mechanisms Kelvran already has real analogs of. First: `costaccounting.Calculator` computes `cost_usd` synchronously per request, not retrospectively from a monthly provider invoice — matching the documented industry pattern of computing cost "at span close, not at invoice time." Second: `budget.Tracker`'s `BudgetAlertBuckets = []float64{0.5, 0.75, 0.9, 1.0}` (50/75/90/100%) already implements a soft-then-hard percent-of-cap alert ladder, structurally the same pattern the cited source documents (an 80% soft PagerDuty/Slack notification before a 100% hard HTTP-429 block) — just with a different rung count and thresholds (Kelvran: 4 rungs at 50/75/90/100%; cited source: 2 rungs at 80/100%).

**Why it matters for Kelvran specifically:** confirms two already-shipped mechanisms are not gaps relative to 2026 practice, closing out rather than re-litigating them.

**Consistent with settled decisions?** Validates `docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md`'s design and `cost-intelligence-finops-2026-09-14.md`'s alert-ladder Phase 1 as already sound.

**2026 best practice grounding:**
- "Cost is computed at span close, not at invoice time" — cost derived synchronously from usage tokens at the response's final chunk, not from a later provider billing statement [truefoundry.io/blog/llm-cost-attribution-team-budgets — confirmed 3-0].
- "Soft limit (80% of budget)" triggers a notification with no block; "Hard limit (100% of budget)" returns HTTP 429 and refuses the call [truefoundry.io/blog/llm-cost-attribution-team-budgets — confirmed 2-1].

**Concrete next step:** none required. Kelvran's existing 4-rung ladder is arguably more informative than the cited 2-rung pattern, not a downgrade — no change recommended.

**Effort:** None.

---

## Top 3 do next

1. **Name "team-level spend showback" as the next concrete chargeback-adjacent milestone** (Finding 1, Small). Gives Findings 2–4's currently-`not_yet` work a clear, sequenced home instead of remaining scattered across unrelated reports' caveat sections.
2. **Add the cache-token double-counting regression guard to `costaccounting.Calculator`** (Finding 5, Small). Directly protects the trust of any cost figure — showback today, chargeback later — and is cheap, time-relevant given `gateway-2026-09-06.md`'s own OTel-instrument work is already recommended elsewhere.
3. **When a second real internal-team virtual key exists, add the team tag + aggregation endpoint** (Finding 2, Medium). The concrete, externally-precedented gap-closer for multi-team showback — not urgent today since no second real team exists yet, but fully scoped so it doesn't need re-deriving later.

**Worth tracking, not building yet:** the outcome-based-billing-unit design constraint (Finding 3) and the FOCUS-column export vocabulary (Finding 4) are both real but gated behind chargeback (or even a showback export) actually being triggered — recorded here so neither needs re-deriving from scratch when that trigger fires.

---

## Caveats

- Chargeback itself remains explicitly `not_yet` per two independent prior research passes (`billing-monetization-integration-2026-09-15.md`, `cost-intelligence-finops-2026-09-14.md`) — this report does not overturn that; it only refines what "getting ready for it" concretely means (showback-first sequencing, a named attribution-trust bar, and one cost-correctness bug to guard against).
- A claim asserting LiteLLM enforces a budget check at all four hierarchy levels simultaneously, blocking on any level being over budget, was refuted (1-2) during adversarial verification and is deliberately excluded from Finding 2 — only the spend-attribution/reporting half of LiteLLM's hierarchy is cited as confirmed, not its enforcement semantics.
- Several FOCUS-related claims asserting v1.4 makes tag-based/split-cost allocation a *normative, mandatory* requirement of the spec were refuted (0-3, 1-2) — Finding 4 treats FOCUS's allocation columns as an optional, documented convenience vocabulary Kelvran could choose to adopt, not a compliance requirement it is currently failing to meet.
- A claim that native cloud-provider billing consoles use structurally incompatible per-provider granularities (the stated reason a gateway needs its own internal attribution layer) was refuted (0-3) — this report does not rely on that reasoning; Kelvran's own internal-attribution need stands on the showback/chargeback distinction in Finding 1 alone.
- TrueFoundry, api7.ai, and tianpan.co are vendor or independent engineering blogs, not standards bodies — individually rated medium confidence, but corroborate each other and the FinOps Foundation's own Framework page on the core showback-before-chargeback sequencing claim, which is why Finding 1 is rated on the stronger primary-source (finops.org) leg of that corroboration.
- Time-sensitivity: FOCUS 1.4 is the current published spec as of this research date (a v1.5 working draft exists but is unpublished); per-vendor FOCUS-version adoption (v1.0–v1.3 across different hyperscalers) is a moving target and should be re-checked before any real export-format decision is made.

---

## Open questions

- Does Kelvran ever get a second real internal-team virtual key — the concrete trigger both Finding 2 here and `billing-monetization-integration-2026-09-15.md`'s Finding G independently name? Until then, neither the team-hierarchy aggregation endpoint nor the FOCUS-column export vocabulary has a real consumer.
- Should a future team tag live directly on `VirtualKeyConfig`, or as a separate lookup table? Not resolved here — mirrors `billing-monetization-integration-2026-09-15.md`'s own open question about the `external_customer_id` field's shape and timing, and for the same reason (guessing the shape now risks a second migration for no present benefit).
- Once a real chargeback rate is ever set (if ever), should it be per-token, per-outcome-unit (Finding 3), or a blend? Genuinely unanswered — needs its own design pass with real usage-pattern data Kelvran does not have yet.
- Is FOCUS's flagship "Data Generator-Calculated Split Cost Allocation" example (splitting shared infrastructure cost via consumption metrics) actually analogous enough to LLM-gateway pooled spend to model a future Kelvran export after closely, or does it only transfer at the column-naming level (Finding 4's narrower claim)? A related, more specific claim about this feature's Kubernetes-cost-splitting framing was refuted during adversarial verification, so this needs its own fresh, narrower look rather than assuming the flagship example transfers cleanly.
