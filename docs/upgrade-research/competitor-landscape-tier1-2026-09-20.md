# 2026 Competitive Re-Scan: LiteLLM, Portkey, Helicone, Kong AI Gateway, Envoy AI Gateway (Agent Router), Cloudflare AI Gateway (2026-09-20)

## Research Question

A fresh re-scan of LiteLLM, Portkey, Helicone, Kong AI Gateway, Envoy AI Gateway, and Cloudflare AI
Gateway's *current* feature sets, checked against each vendor's own real docs/changelogs (not
secondary "best LLM gateway" listicles) — what has each shipped or changed since
`docs/upgrade-research/competitor-feature-parity-2026-09-14.md`'s prior report (2026-09-14)? Cross-
referenced against what Kelvran itself has newly shipped since that same date (real embeddings
endpoint with cross-provider `Dimensions` support, sticky-canary routing, prompt versioning+labels,
push-based config propagation, admin cache-erasure/backup routes, `BillingSubjectID`, per-model SLO
burn-rate rules, Standard-Webhooks-signed alerting) — producing an updated gap list: what
competitors have that Kelvran genuinely still lacks, and what Kelvran now has that most competitors
don't, as a differentiation angle for "next tier" positioning.

**Method.** Direct primary-source fetches (`WebFetch`/`tvly search`) against each vendor's own live
changelog, release-notes, or GitHub releases/commits page, plus one direct code-level verification
pass against Kelvran's own current source (`gateway/internal/router/router.go`,
`gateway/internal/admin/admin.go`, `gateway/internal/alerting/alerting.go`,
`gateway/internal/adapter/{openai,bedrock}/embeddings.go`,
`docs/operations/grafana/prometheus/kelvran-slo-rules.yml`) to confirm every one of the eight
"newly shipped" Kelvran claims in the research prompt is real before using it as a comparison point
— all eight confirmed present in code/config, not just asserted. `mcp__exa__web_search_exa` hit a
hard rate limit on every attempted call this session (see Caveats); `tvly search` (Tavily CLI) and
`WebFetch` were substituted throughout and worked cleanly.

---

## Executive Summary

Five of six vendors shipped real, dated, on-topic material in the Aug–Sep 2026 window; the sixth
(Helicone) is the most consequential finding of this pass precisely because it *didn't*: Helicone's
dedicated AI Gateway repository has had no feature commit since 2025-07-30 (one license-only commit
on 2025-11-21, zero commits in 2026), consistent with the company's own March 2026 announcement of
being acquired by Mintlify and entering "maintenance mode" (Finding 3). The single biggest new
competitive fact in this scan is **Cloudflare's "User Insights"** (shipped 2026-08-05): a real,
org-wide spend dashboard with per-user drill-down *and* anomaly detection that flags sessions
exceeding a user's own p95 baseline — explicitly marketed for catching "a compromised credential or
a misbehaving agent" (Finding 6). This meaningfully sharpens the 09-14 report's Finding 2 (dashboard
gap): Kelvran still has zero web UI of any kind, and now one of the six vendors has gone beyond a
basic spend-visibility dashboard into anomaly detection — a materially different bar than "just show
me a chart."

On the other side, cross-referencing Kelvran's eight newly-shipped capabilities against all six
vendors' current real feature sets, three hold up as genuinely unclaimed differentiators with **no**
qualifying precedent found anywhere in this pass — push-based config propagation, admin
cache-erasure/backup routes (GDPR Article 17-scoped), and per-model SLO burn-rate alerting (Google
SRE multiwindow-multi-burn-rate pattern, scoped to `gen_ai_request_model`) — while two others
(cross-provider embeddings `Dimensions` translation, Standard-Webhooks-signed alerting) hold up with
a *narrowing* caveat: LiteLLM already ships one explicit non-OpenAI `dimensions`→`outputDimensionality`
translation (Gemini only, confirmed in its own docs), and Kong ships a real, live Standard Webhooks
plugin — but confirmed generic Kong Gateway traffic-control infrastructure, not wired into Kong's own
AI-Gateway cost/budget/alert plugins. Sticky-canary routing has no competitor analog anywhere in this
scan; LiteLLM's new shadow-traffic fan-out (silent duplicate-model dispatch) is a different mechanism
(dark-launch testing, not session-sticky live-traffic splitting).

---

## Findings

### Finding 1 — LiteLLM: hierarchical budget cascade now shipped (not just Bifrost/Portkey precedent), plus predictive cost estimation and deeper auto-router tiering
**Confidence: high** (primary source: `github.com/BerriAI/litellm/releases`, `docs.litellm.ai`, dated version history v1.100.1 through v1.103.0-dev.2, 2026-09-10 through 2026-09-18)

LiteLLM shipped **team-level `model_max_budget` with key-level overrides** (v1.103.0-dev.2,
2026-09-18) — a real, shipped hierarchical budget cascade. This is directly relevant to
`docs/upgrade-research/gateway-competitor-gaps-2026-09-07.md`'s Finding 1/2 (Bifrost's 4-level
cascade, Portkey's 2-level cascade): LiteLLM is now a **third** independent vendor converging on the
same pattern Kelvran's own `DESIGN.md` sketches and defers, strengthening (not duplicating) that
prior finding's evidence base.

Two capabilities are new since 2026-09-14 and not covered by either prior report: (1) **predictive,
pre-request cost estimation** — LiteLLM's router now "predicts prompt-cache costs across
deployments" (v1.103.0-dev.1, 2026-09-16) as part of the routing decision itself, not merely
recording spend after the fact, which is a materially different capability than Kelvran's own
`costaccounting.PriceTable` (a static, retrospective lookup, confirmed unchanged in this pass — see
Finding 8); (2) **auto-router tiering depth** — complexity-, modality- (image-request-aware), and
TTFT-percentile-based routing tiers, with cooldown/fallback logic when an entire auto-router tier
goes unhealthy, plus a "classification_mode" flag to skip re-classification on continuation turns.

**Confirmed absent in this window**: no canary/sticky-routing terminology or mechanism (the closest
analog, shadow-traffic fan-out to a `silent_model`, is a dark-launch/duplicate-dispatch pattern, not
session-consistent live-traffic splitting — see Finding 7); no admin cache-erasure/backup route; no
Standard Webhooks signing scheme on its webhook alerts (only a "fixed emit internal user budget
webhook alerts" bug fix, no signing-spec mention); no SLO/burn-rate alerting terminology anywhere in
the fetched release history.

**Embeddings `dimensions` cross-provider translation — confirmed partial precedent.** LiteLLM's own
docs (`docs.litellm.ai/docs/embedding/supported_embedding`) state `dimensions` is "only supported in
OpenAI/Azure text-embedding-3 and later models" as a pass-through, **except** one documented,
explicit exception: for Gemini's `gemini-embedding-2-preview`, "`dimensions` maps to Gemini's
`outputDimensionality`" — a real, shipped, non-OpenAI translation. No equivalent translation is
documented for Bedrock, Cohere, or Vertex AI (those get raw provider-specific params passed through
instead). This directly narrows, but does not eliminate, Kelvran's own claim to this capability —
see Finding 8's framing.

---

### Finding 2 — Portkey (now marketed as "PRISMA AIRS AI Gateway"): active, dated development continues under Palo Alto Networks branding; MCP governance and protocol-translation routing deepen materially
**Confidence: high** (primary source: `portkey.ai/docs/changelog/enterprise`, dated version history v2.14.0 through v2.23.0, 2026-07-09 through 2026-09-18)

Every fetched Portkey docs page in this pass carries a "Portkey is now PRISMA AIRS AI Gateway" banner
— confirming, with a second, more current data point, the 09-14 report's Caveats note that Portkey
was absorbed into Palo Alto Networks' PRISMA AIRS product. Critically, **the underlying docs/product
are still live, still versioned, and still shipping dated releases under the Portkey name/URL as of
2026-09-18** — this is an active-and-shipping rebrand, not an abandoned or frozen product (contrast
directly with Helicone, Finding 3).

New since 2026-09-14, not covered by either prior report:

- **MCP-scoped governance, deepened.** MCP guardrails, workload identity federation (cross-cloud
  AWS↔GCP, credential-less auth to MCP backends), and a unified single-port gateway+MCP mode (v2.20.0,
  2026-09-01); rate limits extended to apply specifically to MCP tool calls, not just top-level
  requests (v2.18.0, 2026-08-10). The original research task's five pre-settled items name
  "MCP/A2A brokering" as out of scope — but these are governance/rate-limiting features layered on
  top of MCP brokering, not the brokering mechanism itself, so they are treated here as new,
  in-scope findings rather than re-litigating a settled exclusion.
- **Protocol-translation routing.** Cross-protocol routing between Anthropic's Messages format and
  OpenAI's Responses format (v2.16.0, 2026-07-27) — request bodies written in one vendor's native
  wire format can now target a model that expects the other's format. This is a materially more
  sophisticated routing-transparency mechanism than the simple model-name aliasing already credited
  to Portkey in the 09-14 report's Finding 1.
- **Billing/cost attribution.** Billing now prefers provider-reported cost over Portkey's own
  internal estimate when available (v2.19.0, 2026-08-21); AWS STS session tagging lets an operator
  "attribute cost and audit trails per application" (same release) — a real, shipped
  billing-subject-*like* mechanism, but narrower in scope than a generic per-request field: it is
  specifically an AWS-credential-session tagging trick, not a first-class `BillingSubjectID`-style
  attribute on every request regardless of cloud/credential type (see Finding 8).
- **Model catalog refinement.** Per-model pricing overrides can now be set per model on an
  Integration (v2.22.0, 2026-09-11) — this is a direct, dated resolution of the 09-14 report's own
  Finding 3 open question about the exact scope boundary of Portkey's custom-pricing override: it is
  confirmed model-entry-scoped-within-an-Integration, at least as of this release.

**Confirmed absent in this window**: no canary/sticky-routing mechanism; no new prompt-versioning/
labels feature (Portkey's pre-existing prompt-template capability is unchanged, not newly extended);
no admin cache-erasure/backup route; no SLO/burn-rate alerting; no Standard Webhooks signing
mentioned on its webhook guardrail (which gained only an `executeOnProxy` opt-in flag, v2.20.0).

---

### Finding 3 — Helicone: the dedicated AI Gateway has gone dormant; the parent company was acquired by Mintlify and entered "maintenance mode"; one real cross-tenant security incident surfaced instead of new features
**Confidence: high** (primary source: `github.com/Helicone/ai-gateway` commits/releases,
`github.com/Helicone/helicone` commits, `www.helicone.ai/blog/joining-mintlify`,
`www.helicone.ai/changelog`)

This is the most consequential finding in this re-scan precisely because it is a negative result
with a concrete, dated explanation, not an absence of evidence:

- **Helicone's dedicated AI Gateway repository (`Helicone/ai-gateway`, the Rust-based standalone
  gateway product) has zero 2026 commits of any kind.** Its most recent commit is a license-only
  change (Apache 2.0 → GPL v3.0) on 2025-11-21; the last substantive *feature* commit is
  2025-07-30 ("return error on invalid config"). No routing, caching, embeddings, MCP, or prompt-
  management change has landed in this repository in over ten months as of this scan's date.
- **The parent company was acquired by Mintlify**, announced via Helicone's own blog on
  2026-03-03 ("Helicone is joining Mintlify"), with the explicit framing: "Helicone's services will
  remain live for the foreseeable future in maintenance mode. This means security updates, new
  models, bug & performance fixes all keep shipping" — i.e., the vendor's own stated post-acquisition
  posture is maintenance, not new-feature development, for the gateway specifically.
- **Helicone's public marketing changelog (`www.helicone.ai/changelog`) has had no new entry since
  2025-11-26** ("Claude Sonnet 4 and Sonnet 4.5 now support 1M context window") — consistent with,
  and corroborating, the maintenance-mode framing.
- **The main `Helicone/helicone` platform monorepo is still actively committed** (most recent commit
  2026-09-16, the day before this research), but the September 2026 commit activity is dominated by
  security remediation, not features: a 2026-09-16 commit explicitly titled "close platform-admin
  takeover and HQL cross-tenant bypass," referencing an incident tagged INC-657 in an earlier
  2026-08-31 commit. **This is a real, dated, primary-source-confirmed cross-tenant isolation
  vulnerability in a production LLM observability/gateway platform** — the exact threat class
  Kelvran's own `THREAT_MODEL.md` names as its highest-priority concern for `internal/identity` and
  `internal/cache`. This is presented here as a relevant external data point validating that threat
  model's priority, not as an invitation to relax vigilance on Kelvran's own side.

**Net assessment**: Helicone is not a source of new competitive pressure in this window — it produced
zero new AI-Gateway-specific capabilities to compare against any of Kelvran's eight new features. Its
own maintenance-mode status is itself the finding.

---

### Finding 4 — Kong AI Gateway: live, upstream-derived dynamic pricing and a new usage-metering/entitlement plugin; Standard Webhooks exists but is confirmed unrelated to AI-Gateway alerting
**Confidence: high** (primary source: `developer.konghq.com/gateway/changelog`, versions 3.15.0.3
through 3.16.0.0, 2026-08-10 through 2026-09-16; `developer.konghq.com/plugins/standard-webhooks`)

- **Dynamic, upstream-derived pricing.** `ai-proxy-advanced` gained `cache_read_cost`/
  `cache_write_cost`/`cache_write_cost_list`/`context_window_factor`/`service_tier_factor` fields,
  with pricing signals ("Anthropic cache TTL + service tier, OpenAI, and Gemini service tiers")
  pulled directly from live upstream provider responses rather than a static table. This is a
  materially more dynamic cost-accounting mechanism than Kelvran's own `costaccounting.PriceTable`
  (confirmed via prior-report code review and unchanged in this pass — flat, global, per-canonical-
  model, no per-request upstream-signal ingestion) — a second, more concrete precedent beyond
  Portkey's model-level override (09-14 report Finding 3) for the same underlying gap.
- **Semantic caching hardening**, not new capability: fixed orphaned vector-index/cached-embedding
  records left behind when a plugin instance is deleted (across `ai-semantic-prompt-guard`,
  `ai-semantic-response-guard`, `ai-proxy-advanced`, `ai-semantic-cache`); fixed a Redis vector-index
  bug that could spuriously drop-and-recreate a compatible index, silently emptying semantic
  cache/RAG results.
- **New entitlement-enforcement plugin** (3.16.0.0, 2026-09-15), outside the AI-plugin family
  specifically but directly billing-adjacent: enforces per-customer access policies (credit balance
  and feature entitlements) via OpenMeter integration — a real usage-metering-backend integration
  pattern. This is narrower than, but points in the same direction as, Kelvran's own
  `BillingSubjectID` primitive (see Finding 8): Kong's version is scoped to an OpenMeter "customer"
  entity and gates access/entitlements, where Kelvran's is a generic per-request attribution field
  with no external metering-backend integration yet.
- **Standard Webhooks — confirmed real, but confirmed unrelated to AI Gateway.** Kong does ship a
  live "Standard Webhooks" plugin (`developer.konghq.com/plugins/standard-webhooks`, HMAC-SHA256
  signature validation per the open spec) — but on direct verification, it is tagged
  `traffic-control`/`validation`, documented as general-purpose Kong Gateway infrastructure for any
  webhook traffic, with **no** mention anywhere of Kong's AI-Gateway cost/budget/alert plugins using
  or integrating with it. This narrows, but does not refute, Kelvran's differentiation claim on this
  point — see Finding 8.

**Confirmed absent in this window**: no model catalog/aliasing change (the "AI Model" virtual entity
credited in the 09-14 report is unchanged, not newly extended); no canary/rollout routing; no prompt-
management feature.

---

### Finding 5 — Envoy AI Gateway is now formally "Agent Router" under the Agentic AI Foundation (AAIF); the vendor's own current-state description (2026-09-09) reconfirms every gap the 09-14 report found, unchanged, while MCP governance deepens
**Confidence: high** (primary source: `theagentrouter.ai/release-notes`, `aaif.io/blog/agent-router-powerful-traffic-handling-for-agent-builders` dated 2026-09-09, `prnewswire.com` v1.0 GA announcement)

Continuity is clean and further formalized beyond the 09-14 report's finding: the same project (same
maintainers/code) reached v1.0 GA on 2026-06-23 (PRNewswire), shipped v1.1.0 on 2026-08-21 (still the
latest tagged release as of this scan), and on 2026-09-09 completed a full rebrand to **Agent Router**
under a newly formed **Agentic AI Foundation** — a step beyond the 09-14 report's "rebrand confirmed,
same maintainers" note, which had not yet captured the foundation-governance step.

The AAIF's own 2026-09-09 blog post — a current, first-party description of the project's state, not
a stale doc — explicitly reconfirms, in its own words, exactly the two things the 09-14 report needed
external validation for:

- **Model-name virtualization is shipped**: "changing the model is changing the model name" —
  directly reconfirms Finding 1 of the 09-14 report (virtual model aliasing) is real, current, and
  unchanged in mechanism.
- **No dashboard product exists**: the post describes only OpenTelemetry GenAI tracing and
  Prometheus metrics for observability, with zero mention of a spend/usage UI — directly reconfirms
  Finding 2 of the 09-14 report ("Envoy AI Gateway has the identical no-dashboard gap Kelvran does")
  still holds, from the vendor's own primary source, 11 days before this report's date.
- **No A2A protocol support** and **no canary/percentage-based gradual-rollout routing** are
  mentioned anywhere in the post — only binary provider failover ("retries a failed call at the next
  provider… with that provider's credentials") is described.

New since 2026-09-14, not covered by either prior report: **MCP server multiplexing with
identity-based tool-catalog filtering across multiple MCP servers** — the gateway builds a filtered
tool catalog "from selected tools across many MCP servers" with identity-based filtering at both
discovery and invocation time, a more sophisticated MCP-governance capability than the
"MCP hostname routing + CEL backend selection" credited to v1.1.0 in the 09-14 report's underlying
research. As with Portkey's MCP governance deepening (Finding 2), this is presented as new,
in-scope evidence of the *governance* layer maturing, not a re-litigation of the pre-settled
MCP-brokering exclusion.

---

### Finding 6 — Cloudflare AI Gateway ships the single most consequential new capability in this scan: an org-wide spend dashboard with anomaly detection, plus identity-aware per-user controls
**Confidence: high** (primary source: `developers.cloudflare.com/ai-gateway/changelog`, dated entries
2026-08-05 through 2026-09-14)

- **"User Insights" (2026-08-05).** A real, shipped, org-wide spend-visibility dashboard — cost,
  tokens, requests, adoption — with per-user drill-down, at no extra cost. Critically, it also ships
  **anomaly detection**: flagging sessions that exceed a user's own historical p95 baseline *plus* an
  org-level threshold, explicitly marketed for catching "a compromised credential or a misbehaving
  agent." This is a materially different, and higher, bar than the plain spend-dashboards credited to
  LiteLLM and Portkey in the 09-14 report's Finding 2 — none of that finding's evidence for those two
  vendors described anomaly detection, only ad-hoc grouping/breakdown of spend data. Kelvran still has
  zero web UI of any kind in this pass (confirmed unchanged: no frontend directory exists anywhere in
  the repo) — this sharpens, rather than merely reconfirms, the prior dashboard-gap finding.
- **Identity-aware controls (2026-08-05).** Cloudflare Access integration injects `cf.user_id` into
  request metadata, usable directly for per-identity spend limits and log filtering — an
  IdP-integrated per-user governance hook layered on top of the dashboard above.
- **Cost-tracking hardening.** Cache-token pricing fields (`per_cache_read_token`/
  `per_cache_write_token`) added to stop double-counting across providers (2026-09-09); invoices
  consolidated to a single total-cost line item per model with standardized `provider/model` naming
  (2026-09-01); a new `byok_only` flag / `cf-aig-no-wholesale` header prevents Unified Billing
  fallback for BYOK third-party providers specifically to close a billing-arbitrage failure mode
  (2026-09-14).
- **Platform unification.** Workers AI and AI Gateway now share one binding/REST API, with prepaid
  credits usable across both, and higher rate limits for frontier models (2026-08-07).

**Confirmed absent in this window**: no embeddings-specific update, no semantic-caching update, no
prompt-management feature, no canary/sticky routing — Cloudflare AI Gateway remains, at its core, a
provider-passthrough gateway with no gradual-rollout traffic-splitting mechanism found anywhere in
this research (this pass or the prior ones).

---

### Finding 7 — Sticky-canary routing: no competitor analog found anywhere in this six-vendor scan
**Confidence: high** (absence confirmed across all six primary sources fetched in this pass; positive
claim confirmed directly against Kelvran's own code)

Kelvran's `gateway/internal/router/router.go` implements a real, shipped per-model-group `Sticky`
toggle (`stickyGroups`/`stickyDeployments`, `sticky.go`'s `SelectSticky`/`stickyPick`) explicitly
documented in-code as "a per-model-group toggle an operator turns on for one canary/stable pair" —
i.e., session-consistent routing so a given caller/session keeps landing on the same
canary-or-stable deployment for the duration of a rollout, not a per-request coin-flip.

No vendor in this six-vendor scan ships anything analogous. The closest mechanism found anywhere is
LiteLLM's new shadow-traffic capability (Finding 1: "stream shadow traffic and fan out silent_model
to multiple targets") — but this is a **dark-launch/duplicate-dispatch** pattern (the real response
still comes from the primary target; the shadow target's response is discarded or logged
silently for comparison), which is a different mechanism solving a different problem (safe
pre-production testing) than session-sticky live-traffic canary splitting (safe *production* rollout
with consistent per-session behavior). No canary, percentage-rollout, or session-affinity routing
concept was found in Portkey, Kong, Envoy/Agent Router, Cloudflare, or Helicone's current documented
feature sets either.

---

### Finding 8 — Cross-referencing Kelvran's newly-shipped capabilities against all six vendors: updated gap list
**Confidence: high for each individual claim below (see Findings 1–7 for citations); this finding is
a synthesis, not a new primary-source claim**

All eight of the research prompt's named Kelvran capabilities were directly confirmed present in
code/config before this comparison (`router.go`'s `stickyGroups`; `prompt/prompt.go`,
`admin/admin.go`, `telemetry/result.go` for prompt versioning+labels; `configpropagation/
configpropagation.go` for push-based propagation; `admin/admin.go`'s `backupHandler` and
`eraseCacheEntryHandler` — the latter explicitly commented as "a real GDPR Article 17 erasure
request" — for cache erasure/backup; `identity/identity.go` for `BillingSubjectID`;
`docs/operations/grafana/prometheus/kelvran-slo-rules.yml`'s `KelvranSLOFastBurnByModel`/
`KelvranSLOSlowBurnByModel` alerts, added 2026-09-18, for per-model SLO burn-rate rules; and
`alerting/alerting.go`'s `webhook-id`/`webhook-timestamp`/`webhook-signature` headers with
HMAC-SHA256 over `id.timestamp.body` and `whsec_`-prefixed-secret convention, explicitly commented as
implementing "the Standard Webhooks specification… the identical scheme Svix's own webhook platform
uses," for Standard-Webhooks-signed alerting).

**Updated gap list — what competitors now have that Kelvran still lacks:**

1. **Operator-facing spend dashboard with anomaly detection** (Cloudflare, Finding 6) — the single
   most concrete, prioritizable new gap in this scan. Kelvran has zero web UI of any kind.
2. **Dynamic, live-upstream-derived pricing** (Kong, Finding 4) — a second, more concrete precedent
   (beyond Portkey's model-level override) for moving Kelvran's static `PriceTable` toward ingesting
   real upstream pricing signals per request.
3. **MCP-scoped governance depth** (Portkey's MCP-specific rate limits/guardrails/workload identity
   federation, Finding 2; Agent Router's identity-filtered multi-server tool-catalog multiplexing,
   Finding 5) — governance layered *on top of* MCP brokering (itself out of scope per the original
   task framing), a boundary case worth flagging for a dedicated follow-up rather than folding
   silently into the pre-settled exclusion.
4. **Predictive, pre-request cost estimation** (LiteLLM, Finding 1) — Kelvran's cost accounting
   remains retrospective/post-hoc.
5. **External usage-metering/entitlement-backend integration** (Kong's OpenMeter entitlement plugin,
   Finding 4; Portkey's AWS STS cost/audit session-tagging, Finding 2) — both point toward wiring a
   billing-subject-style primitive to a real external metering backend, which Kelvran's
   `BillingSubjectID` (a request-tagging field with no external metering integration) doesn't yet do.

**Updated differentiation angle — what Kelvran now has that most competitors don't, confirmed
holding after this fresh scan, for "next tier" positioning:**

- **Sticky-canary routing** — no qualifying analog found anywhere in this six-vendor scan (Finding 7).
- **Push-based config propagation** — no vendor's changelog in this pass describes a push (vs.
  poll/reload) config-distribution mechanism.
- **Admin cache-erasure (GDPR Article 17-scoped, per-entry) + backup routes** — no vendor ships a
  documented, operator-facing per-entry cache-erasure or cache-backup API; Kong's cache work this
  window was internal-consistency bug fixes, not an operator-facing capability.
- **Per-model SLO burn-rate alerting** (Google SRE multiwindow-multi-burn-rate pattern) — no vendor
  in this scan documents anything resembling burn-rate/error-budget alerting at all, let alone scoped
  per model.
- **Standard-Webhooks-signed alerting, narrowed but holding** — no vendor's actual LLM-gateway
  alerting/webhook path uses Standard Webhooks signing; Kong ships the building block generically
  (Finding 4) but hasn't wired it into its AI Gateway plugins, so an operator could replicate this
  today only by bolting two separate Kong products together themselves — Kelvran ships it as one
  integrated capability.
- **Cross-provider embeddings `Dimensions` translation, narrowed but holding** — LiteLLM has one
  non-OpenAI precedent (Gemini only, Finding 1); Kelvran's OpenAI-pass-through + Bedrock-Titan-V2-
  translation-with-sensible-default is a different provider pair, not a strictly larger scope. Fair
  framing: "prior art exists for the general idea; the specific provider coverage differs" — not
  "wholly novel."

---

## Caveats

- **`mcp__exa__web_search_exa` failed on every attempted call this session** with a hard MCP
  rate-limit error before any search succeeded. `tvly search` (Tavily CLI) and direct `WebFetch`
  calls against vendor-native URLs were substituted throughout and worked cleanly for all six
  vendors — this is a tooling substitution, not a degradation in source quality; every claim above
  still traces to a vendor-owned primary source (official changelog, release-notes page, or GitHub
  repository), consistent with this project's established citation discipline.
- **WebFetch's own AI-summarization layer produced one internally-inconsistent year read** on the
  Envoy/Agent Router GitHub releases page (misreading v1.0.0/v1.1.0's relative timestamps as 2025
  instead of 2026) — caught and corrected in this pass by cross-checking against the dated
  PRNewswire v1.0 GA announcement (explicitly "June 23, 2026") and the AAIF's own dated blog post
  (explicitly "September 9, 2026"). Findings 5 and 7 rely on the corrected, cross-checked dates, not
  the initial misread.
- **Helicone's public marketing-site changelog and the dedicated `Helicone/ai-gateway` GitHub repo
  were both checked and both show no 2026 feature activity** (Finding 3) — this was verified two
  independent ways (marketing changelog dates, GitHub commit history) specifically because a single
  stale page could otherwise be mistaken for a tooling/caching artifact rather than a real vendor
  state change; both sources agree, and both are consistent with the company's own March 2026
  acquisition announcement.
- **Kong's Standard Webhooks plugin's non-integration with its AI Gateway plugins is confirmed only
  by absence** (no cross-reference found in either product's documentation) **— not by an explicit
  vendor statement that the two are unconnected.** Treat this as "no evidence of integration found,"
  not "vendor has confirmed no integration exists" — a meaningfully weaker claim, flagged accordingly
  in Finding 4 and Finding 8.
- **This report's per-vendor windows are bounded by each vendor's own most-recent dated entry as
  fetched on 2026-09-20** — for LiteLLM and Portkey that reaches 2026-09-18; for Cloudflare and Kong,
  2026-09-14/16; for Envoy/Agent Router, the vendor's own most recent primary-source statement is
  dated 2026-09-09 (11 days before this report) since v1.1.0 (2026-08-21) remains its latest tagged
  release. None of these gaps are treated as evidence of a vendor "having nothing more to say" —
  each is a real, dated snapshot, not a claim about activity between the snapshot date and today.

## Open Questions

- Does LiteLLM's or Portkey's new MCP-scoped rate-limiting/guardrail work (Findings 1, 2) constitute
  a genuine expansion of the original task's "MCP/A2A brokering" pre-settled exclusion, or is it
  cleanly separable as "governance layered on brokering" the way this report has treated it? A
  dedicated follow-up scoped specifically to MCP governance (distinct from MCP brokering) would
  settle this more rigorously than this report's boundary-case framing.
- Kong's entitlement-enforcement plugin (OpenMeter-backed, Finding 4) and Portkey's AWS STS
  session-tagging (Finding 2) both gesture at external usage-metering-backend integration — is there
  a real, current Kelvran demand signal (an operator asking to wire `BillingSubjectID` to an external
  metering/billing system) that would make this `build_now` rather than merely "worth watching"? No
  such signal exists yet in this research; genuinely open.
- If Cloudflare's anomaly-detection angle (p95-baseline-plus-org-threshold flagging, Finding 6)
  is judged worth pursuing for Kelvran, would it sit on top of a future dashboard (as Cloudflare ships
  it), or could a narrower, dashboard-free version (an alert rule over existing OTel spans, similar in
  spirit to the SLO burn-rate rules already shipped) deliver most of the value without requiring the
  much larger dashboard investment the 09-14 report's Finding 2 already scoped as `not_yet`? Worth a
  dedicated follow-up given this report found a genuinely new, concrete competitor capability to
  benchmark against.
- Helicone's "maintenance mode" status (Finding 3) is confirmed as of 2026-09-20 — does this persist,
  or does Mintlify (itself an active acquirer, per this pass's incidental finding of a separate
  Mintlify→Trieve acquisition in RAG infrastructure) re-invest in the AI Gateway specifically at some
  future point? Not answerable from this pass; worth a dated re-check in a future research round
  rather than assuming the current dormancy is permanent.

**Closing note (2026-09-23):** This report's own six-vendor scan (LiteLLM, Portkey, Helicone, Kong,
Envoy/Agent Router, Cloudflare) never covered Martian or RouteLLM — neither name appears anywhere
above (confirmed via grep, zero hits for both terms in this file). The actual open question about
them was raised earlier, in `docs/upgrade-research/gateway-model-routing-intelligence-2026-09-11.md`'s
own Caveats/Open Questions ("no source in this pass discussed Martian or Not Diamond directly...
worth a dedicated follow-up research pass"). `docs/upgrade-research/learned-model-routing-algorithms-2026-09-22.md`
named this document as the natural home to record that follow-up once found (its Recommendation 3),
and its Findings 4–5 now close it: **Martian has abandoned model routing as a product entirely**,
pivoting to interpretability research; its team's new offering ("Ship," via an incubated lab called
Thesean AI) is an inference-time compute-allocation endpoint with a quality SLA, not a multi-model
router — architecturally a different category, and out of scope for anything Kelvran's own gateway
(which proxies to providers directly) could build or emulate. **RouteLLM (LMSYS)** is independently
confirmed dormant — no commits since August 2024 — still `pip`-installable but an unmaintained
upstream dependency if ever adopted; its `not_yet` verdict for Kelvran (blocked on the same
labeled-preference-data precondition every trained router needs) is reconfirmed, not changed by this.
Neither finding requires a Kelvran code change or revisits any settled decision; recorded here so a
future research pass doesn't re-ask an already-answered question against this document.
