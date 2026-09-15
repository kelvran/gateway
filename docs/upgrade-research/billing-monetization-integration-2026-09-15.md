# Billing, Metering & Monetization Integration — Deep Research (2026-09-15)

## Question

Kelvran has real, Decimal-precision internal cost/budget tracking per virtual key
(`internal/budget`, `internal/costaccounting`) and a real single-tenant Bedrock pilot in
production. Does Kelvran have ANY path to get that usage data OUT to an actual billing/invoicing
system if Kelvran or its operators ever needed to charge a customer or internal team for usage?
Researched broadly against comparable LLM gateways and API-metering products — LiteLLM Proxy,
Portkey, Helicone, Kong AI Gateway (OpenMeter-powered), OpenMeter, Lago, Stripe's own
usage-based-billing APIs, Orb, and Metronome — to separate what's genuinely applicable to
Kelvran's current single-pilot-tenant stage from what's speculative.

**Already shipped (not re-litigated):** Decimal-precision per-request cost calculation
(`internal/costaccounting.Calculator`) against a static price table, including cache-token
discount pricing; per-virtual-key cumulative-USD budget tracking with rolling-window resets and a
TOCTOU-safe Reserve/Reconcile model (`internal/budget.Tracker`); a fixed percent-of-cap alert
ladder (`BudgetAlertBuckets`); `agent_run_id`/`cost_usd` as additive fields on
`GatewayDecisionEvent` for per-agent-run cost attribution (`docs/rfcs/2026-09-12-gateway-cost-
attribution-aggregation.md`); a minimal `evals cost-report --agent-run-id` CLI that sums cost from
ingested events; and — the single most directly relevant existing primitive to this question — a
real, live `GET /admin/virtual_keys/{name}/spend` endpoint gated by a dedicated `CostViewer`
credential tier explicitly designed, per its own doc comment, to be "a credential an operator can
hand to a billing/finance consumer" (`gateway/internal/admin/admin.go`, shipped per
`docs/upgrade-research/multi-tenancy-access-control-2026-09-14.md` and `admin-operator-experience-
2026-09-14.md`'s Finding 4). **Already deferred (not re-litigated):** hierarchical org→team→user
budget cascades and per-tenant cost dashboards beyond aggregate reporting (three-times-reaffirmed,
per `gateway-hierarchical-budgets-2026-09-07.md` and `multi-tenancy-access-control-2026-09-14.md`);
statistical spend-anomaly detection and budget forecasting (`cost-intelligence-finops-2026-09-14.md`).

## Executive Summary

Kelvran already has the smallest real thing this exact question needs at its current scale — a
human-readable, per-key spend snapshot a finance/billing operator can pull and manually turn into
one line item for the one real tenant that exists (the pilot). What it does **not** have, and what
every vendor surveyed converges on as the actual shape of "getting usage data to a billing system,"
is an **event-based** export: every real metering/billing platform (Stripe's Billing Meters/
Metronome, Orb, OpenMeter, Lago) ingests discrete, idempotent, timestamped usage events keyed to an
external customer identifier — never a cumulative running total. Kelvran's existing telemetry
already contains every piece such an event would need (`trace_id` as an idempotency key,
`occurred_at`, `cost_usd`, `requested_model`, `agent_run_id`) *except* a customer-identity mapping,
and the one durable event-export pipe that already exists (`GatewayDecisionEvent`→Vector→S3/GCS,
per `docs/operations/vector-gatewayevents-s3-compose.yaml`) is itself explicitly disclosed as
blocked pending a real S3 bucket/IAM credential — and even once unblocked, it feeds `evals`
ingestion today, not any billing platform. No vendor surveyed accepts a usage event for an unknown
customer ID at all (Metronome, Orb, and Stripe's Billing Meters all define and use a specific
customer-not-found error), which is a structural confirmation, not just an assumption, that
integrating any of these now would be wiring machinery to a subject that doesn't yet exist:
Kelvran's one real virtual key (`pilot-team`) is an internal design-partner pilot with a $5 hard
cap, not an invoiced customer. The verdict below is therefore `not_yet` across the board for any
actual vendor integration, with the exact, low-effort implementation shape to copy — reuse the
existing (currently-blocked) Vector pipe, add one additive customer-id field mirroring
`agent_run_id`'s own precedent, point one more Vector sink at whichever platform is chosen when a
real trigger fires — recorded here so it doesn't need re-deriving later.

## Findings

### Finding A — Kelvran's existing `GET /admin/virtual_keys/{name}/spend` + `CostViewer` tier is already the correct minimum-viable answer for exactly one tenant, and it needs no new code today
**Confidence: high** (direct code read: `gateway/internal/admin/admin.go` lines 46-62, 518-563;
corroborated by two prior research passes)

`CostViewer` is a real, already-shipped third credential tier — narrower than `Viewer`, authenticating
*only* `GET /admin/virtual_keys/{name}/spend`, returning `spent_usd`/`budget_usd`/`percent_used` and
nothing else (no key hash, no rate-limit config, no allowed-models list) — built specifically, per
its own doc comment, so it can be "hand[ed] to a billing/finance consumer without granting it any
visibility into deployment topology, prompt content, or model/rate-limit configuration." This is
the direct, load-bearing answer to half of this research's own question: yes, Kelvran already has a
path for a billing/finance consumer to read what a tenant owes. What it is **not**, and cannot be
without a redesign, is an automatable feed into an invoicing system: it is a pull-based, single-key,
cumulative-total snapshot of the *current* budget period — not a discrete, per-request, timestamped
usage record. A human can read this number and type it into an invoice; no billing platform's
ingestion API can consume it directly, because every one of them (Finding B) expects a stream of
individually-timestamped, idempotent events, not a running total.

**Verdict: build_now (already shipped) for the human-in-the-loop case; structurally insufficient for
an automated pipe, by design, not by gap.**

### Finding B — Every real metering/billing platform surveyed converges on the identical integration shape: discrete idempotent usage events keyed to an external customer ID, decoupled from pricing
**Confidence: high** (7 independent primary-source vendor docs, near-identical event shape: Stripe
Billing Meters/Metronome, Orb, OpenMeter, Lago, Kong Konnect Metering & Billing)

Stripe's Billing Meters API: `POST /v1/billing/meter_events` with `event_name`, `payload.value`,
`payload.stripe_customer_id`, and an `identifier` idempotency key (auto-generated if omitted,
enforced unique for at least 24 hours). Metronome's ingest endpoint: `transaction_id` (idempotency
key, 34-day dedup window, supports backdating up to 34 days), `customer_id` (or an ingest alias),
`event_type`, `timestamp`, free-form `properties`. Orb's ingest endpoint: `idempotency_key`
(required), `external_customer_id`, `event_name`, `timestamp`, free-form `properties` — Orb stores
every event immutably and computes invoices as a deterministic query over raw events rather than
incrementing counters at ingest time, specifically so late-arriving events, pricing changes, and
metric redefinitions never require a data migration. OpenMeter's events are CNCF CloudEvents
(`specversion`, `id`, `source`, `type`, `subject`, `data`) — `subject` is the customer-attribution
field. Lago's events: `transaction_id` (idempotency), `code` (billable-metric reference),
`external_subscription_id`, free-form `properties`. Kong's `metering-and-billing` plugin emits one
event per input/output token count, tagged by a `subject` resolved from the request's authenticated
Consumer. **None of the five platforms accepts, or has any concept of, a cumulative running total as
an ingestion input** — the closest analog (Stripe's `Last` aggregation formula) still requires a
discrete event per observation, just changes how multiple events combine.

**Verdict: n/a — this is the fixed shape any real integration must take, not itself a build/defer
decision.**

### Finding C — Kelvran's existing telemetry already contains every field such an event would need except a customer-identity mapping, and the one durable export pipe that exists is blocked on the same real prerequisite already blocking the S3/GCS analytics path
**Confidence: high** (direct code read: `api/gatewayevents/v1/gatewayevents.proto`,
`gateway/internal/identity/identity.go`, `gateway/internal/gateway/controlplane/config.go`;
`DECISIONS.md`'s 2026-09-13 entry)

`GatewayDecisionEvent` already carries `trace_id` (a ready-made idempotency key, mirroring Stripe/
Orb/Metronome/Lago's own required field), `occurred_at` (the required event timestamp), `cost_usd`
and `virtual_key_id` (the exact numeric-value/subject pairing every platform's event schema wants),
`requested_model`, and `agent_run_id`. What is genuinely absent, on both `VirtualKeyConfig` (the
static config type) and `identity.VirtualKey` (the runtime type), is any field mapping a virtual key
to an external biller's customer identifier — no `external_customer_id`, no `stripe_customer_id`, no
billing-subject of any kind. This is the literal first onboarding step of all five platforms
surveyed (Finding B) and Kelvran has no equivalent today. Separately, the one event-export pipe that
already exists end-to-end in design — `GatewayDecisionEvent`→Vector→S3/GCS, real Vector config
committed at `docs/operations/vector-gatewayevents-s3-compose.yaml` — is explicitly disclosed as
"blocked pending external action: no real S3 bucket exists, no IAM credential exists for either
Vector's write side or evals' read side" (`DECISIONS.md`, 2026-09-13), and even once unblocked its
one real consumer is `evals ingest`/`cost-report`, not any billing platform. A billing-vendor sink
would face the identical real-world blocker (external credentials/infrastructure that doesn't exist
yet) that the already-attempted analytics sink has been sitting behind since 2026-09-13.

**Verdict: not_yet.** The gap is real and precisely locatable (one missing config field, one blocked
external credential), not vague or structural — which is exactly why it's cheap to close later and
not worth speculatively closing now.

### Finding D — LiteLLM's closest-to-Kelvran-shaped real precedent validates that no new cost-calculation logic would be needed, only a new sink
**Confidence: high** (LiteLLM's own current docs, `docs.litellm.ai/docs/proxy/billing`)

LiteLLM Proxy — a self-hosted gateway, architecturally the closest peer to Kelvran among everything
surveyed — integrates with Lago (an open-source billing platform, see Finding E) via a single opt-in
callback: `litellm_settings: { callbacks: ["lago"] }`, plus a `LAGO_API_CHARGE_BY` env var selecting
whether the billing subject is `team_id`, `user_id`, or `end_user_id`. The event LiteLLM posts to
Lago carries `transaction_id`, `external_customer_id` (whichever charge-by field was configured),
`code` (a fixed billing event code), and `properties.response_cost` — **LiteLLM's own already-
calculated cost**, not a value Lago re-derives. This is architecturally the smallest possible
integration shape and confirms Kelvran's own already-shipped `cost_usd`/`virtual_key_id` pairing
(Finding C) is sufficient raw material for an equivalent integration — the entire remaining gap is a
sink and a customer-id mapping, never a pricing/cost-calculation rebuild. LiteLLM separately ships a
CloudZero AnyCost-format export (hourly batch, `CLOUDZERO_EXPORT_INTERVAL_MINUTES`) — but that is a
FinOps/cost-observability integration (CloudZero is a cost-intelligence platform, not an invoicing
system), the same internal-vs-external distinction `cost-intelligence-finops-2026-09-14.md`'s
Finding 4 already drew for Kelvran's own chargeback/showback question, and it does not change this
finding.

**Verdict: n/a — precedent supporting Finding C, not a new decision point.**

### Finding E — If a self-hosted billing platform is ever adopted, OpenMeter or Lago's OSS tier fits Kelvran's existing Compose-profile-sidecar posture; Stripe itself now steers new integrations away from its own Billing Meters API toward a SaaS-only successor
**Confidence: medium-high** (primary vendor docs and current pricing/licensing pages; the "no
self-host tier found" half of this claim is an absence-of-evidence result for Orb/Metronome, not a
verified negative)

OpenMeter ships a real, runnable open-source stack (Apache-2.0; API + workers + Kafka + ClickHouse +
Postgres, with a documented Docker Compose evaluation setup) and was itself acquired by Kong in
September 2025, now also powering Kong Konnect Metering & Billing (Finding F) as the SaaS packaging
of the same OSS core. Lago ships a self-hostable core (AGPLv3) with a documented Docker/Helm
self-hosted deployment guide, explicitly markets itself as "the best alternative to Chargebee,
Recurly or Stripe Billing for companies that need to handle complex billing logic," and is used in
production by at least one named AI company (Mistral AI, per Lago's own customer story) for exactly
this LLM-usage-billing use case — Lago even ships a dedicated Python/JS "Agent SDK" that wraps
LLM-client calls to emit token/cost events directly. Both would slot into Kelvran's already-
established "optional Compose-profile sidecar" pattern (`redis`/`vector`/`observability`, all
gated behind their own profile, none started by a bare `docker compose up gateway`) without
introducing a mandatory SaaS-account dependency. By contrast, Stripe's own current documentation for
its Billing Meters API states plainly: "Not Recommended... Unless you maintain an existing Billing
Meters integration, use Metronome, Stripe's primary usage-based billing platform, instead" — and
Metronome (acquired by Stripe, effective January 2026) is a cloud-hosted enterprise platform with no
self-hosted tier found anywhere in this research. Orb is likewise SaaS-only in everything checked.

**Verdict: not_yet, preference recorded for if/when triggered.** OpenMeter/Lago's self-hosted OSS
tier is the architecturally-consistent default over Stripe/Orb/Metronome specifically *because* of
Kelvran's own existing self-hosted-sidecar deployment posture — not a generic "open source is
better" claim.

### Finding F — Kong's AI Gateway bundles metering+billing natively via its OpenMeter acquisition — the single most direct "gateway with integrated monetization" precedent found, and it confirms billing/metering should stay fully decoupled from real-time enforcement
**Confidence: high** (Kong's own current product/press docs, GA'd Q4 2025/December 2025)

Kong Konnect Metering & Billing, "powered by OpenMeter," is the most directly comparable precedent
in this entire survey: a gateway-native plugin (`metering-and-billing`) that meters AI Gateway LLM
token traffic by emitting one event per input/output token count, resolves a billing `subject` from
the request's authenticated Consumer, and feeds a separate product-catalog/pricing/invoice engine
that "at the culmination of each billing cycle... push[es] [invoices] directly to Stripe or their
preferred payment gateway." Critically, Kong's own docs explicitly disclose that "the AI Gateway does
not automatically block traffic when a customer's entitlement is exhausted" — real-time enforcement,
if wanted at all, requires a separate webhook-notification rule wired into the operator's own
infrastructure. This is a materially *weaker* real-time enforcement story than Kelvran's own already-
shipped, synchronous, TOCTOU-safe `Reserve`/`Reconcile` budget model — confirming that even the
single most gateway-native metering-and-billing product in this survey treats billing/metering as a
downstream, eventually-consistent concern, never something that should sit in the request's own
synchronous enforcement path. Adopting any billing-export pattern at Kelvran should therefore extend
the existing async telemetry pipe (Finding C), never touch `budget.Tracker`'s enforcement path.

**Verdict: n/a — informs design direction (decouple, never touch enforcement path), not a build
decision itself.**

### Finding G — No platform surveyed documents an "unbilled pilot/design-partner" integration tier; every one assumes a real, identified paying-or-chargeback customer relationship from the very first event, which Kelvran's single real virtual key does not have
**Confidence: medium** (inferred from documented rejection/error behavior across 3 of 5 platforms,
not directly tested against a live Kelvran-to-vendor integration)

Metronome's ingest endpoint matches events to customers "using customer IDs or customer ingest
aliases" with no documented fallback for an unrecognized one; Orb's ingestion model requires either
an Orb-internal `customer_id` or a pre-registered `external_customer_id` alias; Stripe's Billing
Meters API has a dedicated, named error event
(`v1.billing.meter.error_report_triggered`/`meter_event_customer_not_found`) specifically for this
case. None of the three documents a "track usage but don't bill yet" mode distinct from having a
real customer record already provisioned. Kelvran's own real virtual key, `pilot-team` (per
`docs/agents/LOGS.md`'s pilot dry-run entries: a $5 hard budget cap, real Bedrock traffic, an
internal design-partner arrangement — not, per any session log read, an invoiced external
relationship), is exactly the kind of subject none of these platforms' own onboarding flows assume
exists. This is a structural confirmation that there is no real subject to point any of these
integrations at today, not merely an assertion that Kelvran hasn't gotten around to it.

**Verdict: not_yet, with the clearest possible named trigger** — a second, real (paying or
internal-chargeback) tenant/customer actually needing an invoice, or a specific finance mandate to
convert existing showback numbers into real chargeback. Until then, every one of the five surveyed
platforms' own onboarding assumptions are simply unmet.

## Caveats

- A tertiary source (`theneuralbase.com`'s "Export to accounting" Helicone tutorial, describing a
  scheduled webhook-based accounting-sync feature with a specific JSON config schema) could not be
  corroborated anywhere in Helicone's own official docs (`docs.helicone.ai`), which document only a
  CLI/REST log-export tool and a Customer Portal — not a scheduled accounting push. Treated as an
  unverified, possibly-fabricated secondary source and excluded from the findings above; Helicone's
  real, confirmed capability is the CLI/REST export plus its Enterprise-gated Customer Portal, cited
  in Finding B's surrounding research but not load-bearing to any verdict here.
- Kong Konnect Metering & Billing is very new (GA announced October 2025, expanded December 2025) —
  specific plugin config fields and entitlement-enforcement mechanics may still change; treat Finding
  F's specifics as a dated snapshot, consistent with every other vendor-pricing/feature caveat this
  project's prior research already logs for fast-moving commercial products.
- This research did not verify Kelvran's own actual business/contractual relationship with the pilot
  tenant (whether "pilot-team" carries any real invoicing obligation today) — that is a business fact
  outside this research's scope. Every verdict above assumes it remains an uninvoiced internal/
  design-partner arrangement, per every session log read (`docs/agents/LOGS.md`'s pilot entries never
  mention an invoice, payment, or external billing relationship).
- Multi-currency, tax calculation, and dunning/collections — real, load-bearing components of every
  platform surveyed (Stripe/OpenMeter both document tax integration; Lago documents dunning/retries)
  — were not researched in depth here, since they sit several steps downstream of the actual current
  gap (no export path exists at all, Finding C). A future pass, if the Finding G trigger ever fires,
  needs its own dedicated look at these before implementation, not just this report's event-shape
  findings.

## Open Questions

1. If/when Finding G's trigger fires, should Kelvran add an `external_customer_id`-style field to
   `VirtualKeyConfig`/`GatewayDecisionEvent` (mirroring `agent_run_id`/`cost_usd`'s own additive-field
   precedent) *before* or *after* choosing a specific vendor? This report leans toward after — every
   vendor names this field slightly differently (`stripe_customer_id`, `external_customer_id`,
   `customer_id`, `subject`) and guessing the shape now risks a second migration later for zero
   present benefit.
2. Would Kelvran's own real measured pilot spend (`$0.0009504` disclosed in `docs/agents/LOGS.md` for
   an entire session's worth of real Bedrock calls) ever plausibly grow large or frequent enough to
   justify anything beyond a human reading `GET /admin/virtual_keys/{name}/spend` for invoicing
   purposes? No evidence either way in this pass — genuinely unanswered until real usage volume
   exists.
3. Is there a genuine future need for real-time entitlement *enforcement* tied to a billing platform
   (mirroring Kong's webhook-driven credit-exhaustion model), or does Kelvran's own already-stricter,
   already-synchronous `Reserve`/`Reconcile` budget enforcement make that entirely moot regardless of
   which billing platform (if any) is ever layered on top for invoicing? Finding F suggests moot, but
   this pass did not conclusively settle it.
4. When the S3/GCS Vector sink (Finding C) is eventually unblocked by real infrastructure, should a
   billing-platform sink be added to the *same* Vector pipeline as a second, parallel sink (cheapest,
   reuses the exact same source/remap), or does a billing destination's correctness requirements
   (guaranteed delivery, no silent drops, real idempotency enforcement on the receiving end) warrant a
   dedicated, non-Vector path instead? Not researched here — worth its own focused look once Finding
   G's trigger actually fires.

## Synthesis: build_now vs not_yet

| Item | Verdict | Why |
|---|---|---|
| Rely on existing `GET /admin/virtual_keys/{name}/spend` + `CostViewer` as the pilot's own manual-invoicing primitive | **build_now (already shipped)** | Already live, already sufficient for exactly one uninvoiced tenant; zero new code needed |
| `external_customer_id`/billing-subject field on `VirtualKeyConfig`/`GatewayDecisionEvent` | **not_yet** | Zero real consumer today; every vendor names this field differently — guessing the shape now risks a second migration for no present benefit |
| Any concrete vendor pipe (Stripe Billing Meters/Metronome, Orb, OpenMeter, Lago, or a Kong-style bundle) | **not_yet** | No real second/paying/chargeback-mandated tenant exists; all 5 platforms' own onboarding flows assume a real billed-customer relationship from event #1, which Kelvran's pilot key doesn't have |
| Design reference to copy WHEN triggered: extend the existing (currently blocked-on-infra) `GatewayDecisionEvent`→Vector pipe with one more sink + one additive customer-id field, using `trace_id`+`occurred_at` as the idempotency key | **not_yet, but fully scoped** | Genuinely a small, well-precedented addition once triggered — recorded here so it doesn't need re-deriving from scratch later |
| Preference for a self-hosted OSS platform (OpenMeter or Lago) over Stripe Billing Meters/Metronome/Orb, if a platform is ever adopted | **not_yet (preference recorded)** | Fits Kelvran's existing Compose-profile-sidecar self-hosted posture; Stripe's own docs steer new integrations away from Billing Meters toward SaaS-only Metronome |
| Wiring any billing/metering export into the real-time `Reserve`/`Reconcile` enforcement path | **disqualified as a design direction** | Every platform surveyed, including the most gateway-native one (Kong/OpenMeter), treats metering/billing as downstream and eventually-consistent from enforcement; Kelvran's existing synchronous enforcement is already stricter and must stay decoupled |

## Sources

- `gateway/internal/budget/budget.go`, `gateway/internal/costaccounting/costaccounting.go`,
  `gateway/internal/admin/admin.go`, `gateway/internal/identity/identity.go`,
  `gateway/internal/gateway/controlplane/config.go`, `api/gatewayevents/v1/gatewayevents.proto`
  (direct repo code reads, 2026-09-15)
- `docs/rfcs/2026-09-02-virtual-keys-budgets.md`, `docs/rfcs/2026-09-03-budget-persistence.md`,
  `docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md`,
  `docs/rfcs/2026-09-05-gateway-admin-api.md`, `docs/rfcs/2026-09-09-gateway-admin-viewer-role.md`,
  `docs/rfcs/2026-09-12-gateway-cost-attribution-aggregation.md`
- `docs/upgrade-research/multi-tenancy-access-control-2026-09-14.md`,
  `docs/upgrade-research/admin-operator-experience-2026-09-14.md`,
  `docs/upgrade-research/cost-intelligence-finops-2026-09-14.md`,
  `docs/upgrade-research/gateway-cost-finops-round4-2026-09-11.md`,
  `docs/upgrade-research/llm-cost-optimization-finops-2026-09-14.md`
- `DECISIONS.md` (2026-09-02, 2026-09-07, 2026-09-12, 2026-09-13 entries), `docs/agents/LOGS.md`
  (pilot dry-run entries, 2026-09-13/14)
- LiteLLM: https://docs.litellm.ai/docs/proxy/billing ,
  https://docs.litellm.ai/docs/proxy/cost_tracking , https://docs.litellm.ai/docs/proxy/alerting ,
  https://docs.litellm.ai/docs/observability/cloudzero ,
  https://github.com/BerriAI/litellm/pull/35931
- Portkey: https://portkey.ai/docs/product/observability/logs-export ,
  https://portkey.ai/docs/product/observability/cost-management ,
  https://portkey.ai/docs/guides/use-cases/multi-tenant-ai-feature
- Helicone: https://docs.helicone.ai/features/customer-portal ,
  https://docs.helicone.ai/guides/cookbooks/etl.md ,
  https://docs.helicone.ai/guides/cookbooks/cost-tracking
- Stripe: https://docs.stripe.com/billing/subscriptions/usage-based/implementation-guide ,
  https://docs.stripe.com/billing/subscriptions/usage-based/meters/configure ,
  https://docs.stripe.com/api/billing/meter-event/create ,
  https://docs.stripe.com/billing/subscriptions/usage-based/recording-usage-api ,
  https://docs.stripe.com/invoicing/integration
- Orb: https://docs.withorb.com/how-orb-works , https://docs.withorb.com/events-and-metrics/event-ingestion ,
  https://docs.withorb.com/self-serve/agent-pricing , https://docs.withorb.com/core-concepts ,
  https://orb-9bba378a.mintlify.app/architecture/query-based-billing
- Metronome: https://docs.metronome.com/api-reference/usage/ingest-events
- OpenMeter: https://github.com/openmeterio/openmeter , https://openmeter.io/use-cases/ai ,
  https://openmeter.io/docs/integrations/stripe/invoicing , https://openmeter.io/docs/billing/quickstart ,
  https://openmeter.io/docs/collectors/langchain
- Lago: https://github.com/getlago/lago , https://getlago.com/ ,
  https://docs.getlago.com/guide/ai-agents/agent-sdk/billing ,
  https://docs.getlago.com/templates/per-token/openai
- Kong: https://konghq.com/blog/product-releases/konnect-metering-and-billing ,
  https://konghq.com/blog/product-releases/metering-and-billing-kong-konnect ,
  https://developer.konghq.com/how-to/meter-llm-traffic/ ,
  https://developer.konghq.com/plugins/metering-and-billing/examples/meter-ai-tokens/ ,
  https://developer.konghq.com/metering-and-billing/
- Comparative overview: https://nevermined.ai/blog/stripe-vs-openmeter-vs-lago
