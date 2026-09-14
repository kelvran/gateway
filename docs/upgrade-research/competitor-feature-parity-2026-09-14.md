# 2026 Feature-Parity Audit: Kelvran vs. LiteLLM, Portkey, Envoy AI Gateway, Kong AI Gateway, Helicone (2026-09-14)

## Research Question

Against real, currently-shipped (not roadmap/marketing-only) 2026 capabilities in LiteLLM,
Portkey, and at least 2 of {TensorZero, Helicone, Kong AI Gateway, Envoy AI Gateway}, what does
each competitor have that Kelvran's own already-real feature set (virtual keys with per-key
budget/rate-limit/model-allowlist; 3-layer response cache with cross-provider prompt-cache
auto-population; Bedrock Guardrails ML + regex PII/injection detection; cost/savings attribution
down to `agent_run_id`; WRR routing with active health probing, fallback chains, and cost-tier
preference; a signed/SBOM-attested container image; distributed Redis-backed rate limiting; real
OTel tracing with a live Grafana/Prometheus/Tempo backend) does not already have? Five things were
explicitly pre-settled and are **not** re-researched here — cited as prior art where relevant:
MCP/A2A brokering, realtime/multimodal streaming, batch-API proxying, multi-region/DR, adaptive
(usage/latency-derived) routing, an OpenAPI spec, and a client SDK.

## Executive Summary

Three real, currently-shipped competitor capabilities have no Kelvran equivalent today, confirmed
against both competitor documentation/source and Kelvran's own code: (1) **virtual model
aliasing** — LiteLLM (`model_group_alias`), Envoy AI Gateway (`modelNameOverride`), Kong
(`AI Model` entity), and Portkey (`@provider_slug/model_name`) all let a caller address one stable
name that fans out to genuinely different real models/providers/regions behind it, which is
structurally distinct from Kelvran's `CostTier`/WRR pool (confirmed, by reading
`gateway/ARCHITECTURE.md` and `DECISIONS.md` directly, to operate only *within* one canonical
model's deployment pool, never across model families); (2) **operator-facing spend/usage
dashboards** — LiteLLM ships a real `/ui` Usage tab and Portkey ships a full analytics dashboard
with ad-hoc grouping, while Kelvran has zero web UI of any kind (confirmed: an HTTP-only Admin API
plus Grafana for infra metrics, not business-level spend); Envoy AI Gateway, notably, has the exact
same gap Kelvran does, so this is not a universal competitor advantage; (3) **virtual-key-level
custom pricing/markup** — Portkey supports overriding default per-model cost to reflect negotiated
rates, while Kelvran's `PriceTable` (confirmed via `costaccounting.go`) is a flat, global,
per-canonical-model map with no per-key override field anywhere in `identity.VirtualKey`; Helicone,
notably, deliberately has no such feature either ("0% markup" is its own positioning), so this too
is Portkey-specific, not universal. A fourth question — guardrail/moderation plugin marketplaces —
returned no competitor evidence either way in this research pass, but direct inspection of
Kelvran's own `internal/guardrail.Engine`/`Detector` interface confirms it already supports
third-party pluggable detectors structurally, with one real instance already shipped
(`bedrockguard.Detector`). Of the three confirmed gaps, virtual model aliasing is judged
`build_now` (a scoped, additive config layer with three independent competitor precedents and a
gap Kelvran's own architecture docs already name); the dashboard and custom-pricing gaps are judged
`not_yet`, pending real triggers.

## Findings

### Finding 1 — Virtual model aliasing / routing UX: a real, confirmed gap, distinct from Kelvran's own CostTier/WRR mechanism
**Confidence: high** (4 independent vendors, primary sources, mostly-unanimous votes; one 2-1 split)

LiteLLM, Envoy AI Gateway, Kong AI Gateway, and Portkey each ship a mechanism that lets an operator
define one caller-facing model name that resolves to different real underlying models/deployments,
transparently to the caller:

- **LiteLLM** supports this two ways: (a) multiple `model_list` entries sharing one `model_name`
  load-balance automatically (e.g. `gpt-5.6-terra` spanning `azure/gpt-4o-eu` and
  `azure/gpt-4o-ca`); (b) a separate `router_settings.model_group_alias` field renames/redirects a
  caller-facing name onto an *already-defined* model group (`gpt-5.6` → `gpt-5.6-luna`), with an
  optional `hidden: true` flag that keeps the alias routable but excludes it from `/v1/models`,
  `/v1/model/info`, and `/v1/model_group/info` discovery endpoints entirely — confirmed both in
  docs and directly in LiteLLM's own source (`litellm/router.py`'s
  `get_model_list_from_model_alias`).
- **Envoy AI Gateway** (rebranded mid-2026 to "Agent Router" under the Agentic AI Foundation, same
  code/maintainers) ships `modelNameOverride` per `backendRef` inside an `AIGatewayRoute` rule,
  explicitly documented as "one-to-many aliasing of model names": one unified name
  (`claude-4-sonnet`) splits traffic between AWS Bedrock and GCP, each backend overriding to its
  own real provider-specific model ID. The caller only ever sends the virtual name via a header
  (`x-ai-eg-model`); the gateway substitutes the real name before forwarding upstream.
- **Kong AI Gateway**'s `AI Model` entity is explicitly documented as "a virtual model" that
  "distributes requests across one or more concrete upstream models declared in its `targets`
  array" — Kong's own docs give the concrete example of aliasing `production-chat-model` while
  silently swapping the real upstream from `gpt-4o` to `claude-3-sonnet`, "without your clients
  noticing."
- **Portkey**'s primary routing syntax is `@provider_slug/model_name` (e.g. `@openai-prod/gpt-4o`),
  and an operator can mint multiple named slugs from the *same* underlying credentials, scoped per
  environment (`@openai-dev`/`@openai-staging`/`@openai-prod`), each with its own budget/rate
  limits — so only the slug string changes across environments, not the model name (split-vote
  2-1 finding; the multi-slug-per-credential mechanics are corroborated by a second Portkey docs
  page but slightly less explicitly than the primary source).

**Kelvran's own architecture confirms this is a real, not imagined, gap.** Directly reading
`gateway/ARCHITECTURE.md`'s `/internal/router` section and `DECISIONS.md`'s `[2026-09-12]` entry:
Kelvran's `Deployment.CostTier` field is explicitly scoped as "per-deployment within one canonical
model's existing WRR pool, not a new grouping concept," and the same doc names the gap outright:
"Still explicitly NOT built: automatic price discovery, and any cross-model 'virtual model'
grouping (a single client-facing name spanning several genuinely different underlying models at
different prices, the way OpenRouter's own cost-tier mechanism works)." `costaccounting.PriceTable`
(confirmed by reading `costaccounting.go`) is `map[string]ModelPrice` keyed by canonical model name
— it has no concept of one name resolving to several different real models. This is a materially
different capability than what Kelvran already has: WRR/health-probing/fallback chains all operate
*within* one canonical model's set of deployments (same model, different endpoints); none of them
let an operator swap the real model family/provider behind a stable name, or hide an alias from
discovery the way LiteLLM's `hidden: true` does.

**Not a duplicate of prior settled research.** `docs/upgrade-research/gateway-competitor-gaps-2026-09-07.md`'s
Finding 2 already covered Portkey's Virtual-Keys→Model-Catalog migration, but scoped strictly to
budget/rate-limit *cascading* (Integration → Provider), explicitly treating the `@slug` format as
structurally equivalent to Kelvran's own `VirtualKey.ID` — it did not address the aliasing/routing
transparency angle (one name → different real models) at all, so this is new ground, not a
re-finding. This is also distinct from "adaptive routing" (already settled/deferred elsewhere as a
usage/latency/cost-*derived* dynamic-selection concept) — virtual model aliasing here is a static,
operator-configured mapping, not a learned signal.

**`build_now`.** This is concrete and additively scoped: a new config layer mapping one
client-facing model name to a list of `(provider, real_model)` targets, sitting above (not
replacing) the existing per-canonical-model WRR pool — `PriceTable`, `router.Deployment`, and the
Admin API's model-listing surface would each need a small, well-bounded extension, but none require
redesigning WRR, health probing, or fallback chains. Three independent vendors have converged on
this exact pattern, and Kelvran's own architecture doc has already named the specific gap and
precedent (OpenRouter) unprompted — this is a rare case where the "needs a trigger" bar is already
cleared by the project's own documented self-awareness of the gap, not just external competitive
pressure.

---

### Finding 2 — Operator-facing spend/usage dashboards: a real gap vs. LiteLLM and Portkey, but not universal (Envoy AI Gateway has the identical gap)
**Confidence: high** (multiple primary sources, unanimous votes on each sub-claim)

- **LiteLLM Proxy** ships a real web UI at `/ui` with a dedicated "Usage" tab showing tracked
  spend — confirmed via LiteLLM's own cost-tracking docs' verification instructions ("Navigate to
  the Usage Tab on the LiteLLM UI... and verify you see spend tracked under Usage"), corroborated
  by a separate customer-usage doc and the project's own README describing "an admin dashboard UI
  for monitoring and management" as a base, non-enterprise-gated feature (only a master key + DB
  connection required).
- **Portkey** ships a full, real, multi-tab analytics dashboard (Overview/Users/Errors/Cache/
  Feedback/Summary) with "real-time analytics on cost, latency and accuracy across all your LLM
  requests," including a Summary tab that lets an operator group/breakdown request data by
  arbitrary dimensions (AI service, model, metadata key) via a dropdown — ad-hoc spend/usage
  slicing with no query-writing required.
- **Envoy AI Gateway**, by contrast, has the *same* gap Kelvran does: a systematic read of every
  release-notes entry from v0.1.0 (Feb 2025) through v1.1.0 (Aug 2026) found zero mentions of a web
  dashboard, spend UI, or usage UI anywhere — the only dashboard reference in the entire corpus is
  an infra-metrics Grafana example for `gen_ai_*` Prometheus metrics, the same category Kelvran
  already has. Its entire operator-facing surface is CRDs/control-plane API plus metrics/tracing.

**Kelvran's own state, confirmed directly**: `README.md` and `gateway/ARCHITECTURE.md` both
describe the only operator surface as an off-by-default HTTP Admin API (`GET /admin/config`,
virtual-key CRUD, prompt CRUD) plus a Grafana dashboard fed by OTel spans/`gatewayevents` — the
latter is explicitly infra/GenAI-span metrics (`kelvran.cache.lookup`, `kelvran.llm.spend_usd`),
not a business-level, per-key/per-tenant spend-breakdown UI. There is genuinely no web frontend
anywhere in the repo (confirmed: no frontend directory exists under `gateway/` or elsewhere).

This is a real gap against 2 of the researched competitors specifically (LiteLLM, Portkey), but
explicitly not a "every gateway has this" checkbox — Envoy AI Gateway's own posture validates that
a CRD/API-plus-metrics-only surface is a legitimate, currently-shipped competitive position too,
not obviously behind.

**`not_yet`, with one small `build_now` companion.** A full, bespoke web dashboard (auth, hosting,
a maintained SPA) is a genuinely different scope of investment than anything else in Kelvran's
stack today — no frontend exists anywhere in the repo, and `README.md`'s own non-goals explicitly
scope Kelvran as self-hosted-only, not a managed product, at this stage. The real trigger is either
a concrete operator persona who needs to eyeball spend without querying Grafana/the Admin API
directly, or the Admin API's read surface growing large enough that a thin read-only UI over it
becomes cheap to add. That said, a *much* smaller, genuinely `build_now` step already sits on top
of infrastructure Kelvran has today: since `gatewayevents`/OTel spans already carry
`agent_run_id`, `cost_usd`, and `savings_usd` (per `DECISIONS.md`'s Round 7 Phase 10 entry), a
provisioned Grafana panel/dashboard JSON breaking these down by virtual key or agent run — the same
pattern already used for `docs/operations/grafana/`'s existing infra dashboard — would close a
meaningful slice of this gap without building any new frontend code at all.

---

### Finding 3 — Virtual-key-level custom pricing/markup overrides: real for Portkey, absent for Kelvran, but deliberately absent for Helicone too
**Confidence: high** (primary sources; one adjacent claim on exact scoping was refuted 1-2 and is
noted, not relied upon)

**Portkey** lets an operator override its platform default pay-as-you-go pricing with custom
Input/Output cost rates (USD per 1M tokens) for a specific model in its catalog, explicitly for
"negotiated or private contract rates" and internal chargebacks — subsequent cost/usage analytics
use the new rate going forward. A related, more specific claim — that this override is scoped
*only* to a model entry within a provider Integration and never to a virtual key — was itself
**refuted** on adversarial re-verification (1-2 vote), so the exact boundary between model-level
and key-level scoping in Portkey's implementation should be treated as somewhat open, not fully
pinned down; what is confirmed is that the override mechanism itself is real and shipped, at least
at the model/catalog level.

**Helicone**, deliberately, has no equivalent at all: its documented cost-calculation methods are
limited to a Model Registry v2 lookup (gateway path) or a best-effort open-source cost repository
(300+ models, non-gateway path); missing pricing is resolved by asking Helicone to add it via
Discord/support, not by an operator-defined override, and Helicone's own marketing states "0%
markup — pay exactly what providers charge" as an explicit product stance. This confirms the
capability is Portkey-specific among the vendors checked here, not a sector-wide norm.

**Kelvran has zero mechanism of this kind at any scope**, confirmed directly against code:
`internal/costaccounting/costaccounting.go`'s `PriceTable` is `map[string]ModelPrice`, loaded once
from a single global `price_table` YAML section (`controlplane/config.go`) — flat, per-canonical-
model, identical for every deployment and every virtual key. `internal/identity/identity.go`'s
`VirtualKey` struct carries `ID`, `KeyHash`, `BudgetUSD`, `BudgetResetInterval`,
`BudgetWarnPercent`, `AllowedModels`, and rate-limit fields — no markup, margin, or per-key cost-
override field exists anywhere on it. Kelvran cannot express even Portkey's model-level override
today, let alone a reselling-margin-style per-key markup.

**`not_yet`.** This capability's natural use case — MSPs/resellers charging tenants a marked-up
rate over real provider cost — doesn't fit Kelvran's current self-hosted, single-operator cost-
accounting posture, and Helicone's own explicit "0% markup" counter-example shows this isn't a
default expectation for an LLM gateway either. The right trigger, consistent with this project's
own stated bar, is a real reselling/multi-tenant-billing customer explicitly asking for markup-
over-cost pricing — not competitive pressure alone, since one of the four vendors checked
deliberately ships the opposite stance as a feature.

---

### Finding 4 — Guardrail/moderation plugin model: Kelvran's own interface already supports this structurally; no competitor marketplace evidence either way
**Confidence: medium** (Kelvran-side confirmed directly against source code; competitor side has
zero supporting or refuting web evidence from this research pass — an open question, not a
negative finding)

Directly reading `gateway/internal/guardrail/types.go` and `engine.go`: the `Detector` interface is
`Name() string; Category() Category; Detect(ctx context.Context, text string) ([]Finding, error)`
— explicitly I/O-agnostic by design. The package's own doc comment states outright that "v1
implementations are pure regexp/stdlib and cannot practically error, but the interface must not
assume `Detect` can't fail" specifically because a "future third-party-moderation `Detector`... that
genuinely can error over the network" is already anticipated. This isn't just a paper contract:
`internal/guardrail/bedrockguard.Detector` is a real, shipped instance of exactly this pattern — a
network-calling detector (AWS Bedrock Guardrails' `ApplyGuardrail`) that lives in its own sibling
package (never inside `internal/guardrail` itself, enforced by `go-arch-lint`) and is appended to
`Engine`'s detector list only when configured via `GuardrailsConfig.BedrockGuardrails`, leaving
`DefaultDetectors()`'s regex-only set unaffected when it isn't. So the *pattern* Kelvran would need
for a genuine third-party-moderation plugin ecosystem (LiteLLM/Lakera-style, or similar) already
works end-to-end for one real provider.

What's missing is any evidence — for or against — that a competitor exposes this as an actual
plugin *marketplace* (i.e., a registry of interchangeable third-party moderation providers an
operator can pick from, versus one vendor's own single built-in classifier). None of the 17
confirmed claims in this research batch addressed that specific question; it should be treated as
a genuinely open question requiring dedicated follow-up research (e.g., does LiteLLM's own
documented Lakera/other-vendor guardrail integrations amount to a marketplace, or just a fixed
integration list?), not answered here either way.

**Not classified `build_now`/`not_yet`** — there is no confirmed gap to size, since the structural
capability already exists and has one working instance. The open item is purely a documentation/
positioning question (should Kelvran market `internal/guardrail`'s pluggability more explicitly?),
not an engineering gap.

## Direct Answers to the Research Sub-Questions

1. **What does each competitor have that Kelvran lacks?** See Findings 1-3 above — virtual model
   aliasing (LiteLLM, Envoy, Kong, Portkey), spend/usage dashboards (LiteLLM, Portkey — not Envoy),
   and virtual-key-level custom pricing (Portkey — not Helicone).
2. **Virtual model aliasing vs. Kelvran's canonical-model-to-deployment WRR mapping — same thing or
   a real gap?** A real, confirmed gap — see Finding 1. Kelvran's own docs (`DECISIONS.md`,
   `gateway/ARCHITECTURE.md`) already independently confirm the two are structurally different
   (within-model-pool routing vs. cross-model-family aliasing).
3. **Do competitors ship a real dashboard, and is that a real gap for Kelvran?** Yes for LiteLLM
   and Portkey; no for Envoy AI Gateway — see Finding 2. Real, confirmed, but not universal gap.
4. **Guardrail/moderation plugin model — does Kelvran's interface already support this?** Yes,
   confirmed directly against `internal/guardrail`'s code, with one real shipped instance
   (`bedrockguard`) — see Finding 4. Competitor marketplace evidence: none found, open question.
5. **Virtual-key-level custom pricing overrides for reselling — do competitors have this, does
   Kelvran?** Portkey does (model-level, confirmed); Helicone deliberately doesn't; Kelvran has
   nothing at any scope, confirmed directly against `costaccounting.go`/`identity.go` — see
   Finding 3.
6. **Anything else?** Nothing else survived adversarial verification in this batch beyond what's
   captured in Findings 1-4; see Caveats and Open Questions below for what wasn't resolved.

## Caveats

- The Finding 1 sub-claim about Portkey's multi-slug-per-credential-set mechanism was a 2-1 split
  vote (not unanimous) — the core `@provider_slug/model_name` syntax itself (claim with unanimous
  votes) is on much firmer footing than the specific "multiple slugs from one credential set, one
  per environment" framing.
- The Finding 3 claim about Portkey's exact override scoping (model-entry-only vs. also
  virtual-key-scoped) has a directly adjacent claim that was **refuted** — treat the boundary as
  somewhat unresolved, not as "Portkey definitely cannot do this per key."
- Envoy AI Gateway rebranded mid-corpus (to "Agent Router," an Agentic AI Foundation project) —
  confirmed as a legitimate continuity/rebrand (same maintainers, same code, explicit "Formerly
  Envoy AI Gateway" banner on the new site), not a domain hijack, but worth flagging since it means
  future searches for "Envoy AI Gateway" may increasingly surface the new name instead.
- Portkey itself appears to have been absorbed into "PRISMA AIRS AI Gateway" (Palo Alto Networks)
  as of a July 2026 GA announcement, per one incidentally-surfaced marketing page — the dashboard
  capability persists under the new branding per that same source, but this wasn't the focus of
  verification and should be treated as a secondary, unconfirmed-in-depth data point.
- TensorZero was not covered in this research pass at all (the two "at least 2 of
  {TensorZero, Helicone, Kong, Envoy}" slots were filled by Helicone, Kong, and Envoy) — no
  claims about TensorZero survived or were even attempted here.
- Kong AI Gateway's Finding-1 sub-claim on the "virtual model" framing was a 2-0 vote (one fewer
  vote than the standard 3-vote bar, per the source material) — still high-confidence given the
  raw-HTML self-verification performed, but noted for completeness.

## Open Questions

- Does any competitor (LiteLLM's documented Lakera/other-vendor integrations, Kong's plugin
  ecosystem, or others) expose guardrails as a genuine interchangeable-provider marketplace, versus
  a fixed integration list? Unresolved by this research pass — see Finding 4.
- What is the *exact* scope boundary of Portkey's custom pricing override — model-entry-only, or
  can it also be expressed per virtual key/Integration for reselling? The adjacent claim narrowing
  this was refuted, leaving the boundary genuinely open.
- Does TensorZero (not covered here) ship anything in the virtual-model-aliasing or dashboard
  space that would change Finding 1 or 2's competitive picture?
- If Kelvran builds Finding 1's virtual-model-aliasing layer, should it also support LiteLLM's
  `hidden: true`-style discovery-exclusion, or is that premature given Kelvran's Admin API has no
  model-discovery endpoint at all today to exclude an alias from?
