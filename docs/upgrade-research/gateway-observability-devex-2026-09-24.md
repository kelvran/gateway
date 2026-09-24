# Gateway Observability & DevEx — LiteLLM vs. Portkey vs. Helicone vs. Kelvran (2026-09-24)

**Date:** 2026-09-24
**Scope:** Caller/operator-facing observability and debugging DevEx — what LiteLLM, Portkey, and Helicone ship out-of-the-box vs. what Kelvran's admin-API-only, no-UI/no-SDK design currently offers.
**Method:** Adversarial multi-source research (3-vote verification) against each vendor's own first-party docs, with a deliberate preference for docs pages over GitHub READMEs after several README-sourced claims failed verification (see Caveats).

**Recovery note:** this report's synthesis was completed by the research workflow but the workflow's own file-write step did not persist it to this path — the underlying subagent's own closing caveat states plainly: *"the original research question also asked for full findings to be written to [this path]... per this task's own explicit closing instruction ('Structured output only') I did not write it; a file-writing step (if still required) must happen elsewhere in the pipeline."* That later step silently did not run. This document was reconstructed directly from that already-completed synthesis (recovered from the workflow's own journal), not re-researched.

**Scope-mismatch disclosure (from the synthesis agent's own caveats, preserved here verbatim in substance):** this synthesis processed 18 adversarially-verified claims from **one** deep-research thread — this specific "observability/DevEx" question — not a broader 5-way fan-out. That is expected and correct: this was always meant to be exactly one of five independently-dispatched parallel `/deep-research` topics for this round (the other four are the sibling reports in this directory dated 2026-09-24, plus this round's regulatory-timeline report). The synthesis agent itself couldn't know that at the time it ran and flagged it as an open concern — noted here so the caveat isn't misread as a real gap.

---

## Executive summary

LiteLLM, Portkey, and Helicone each ship a caller/operator-facing observability surface bundled directly into their product with zero extra integration work — a class of DevEx Kelvran's admin-API-only, no-UI/no-SDK design does not yet offer. LiteLLM bundles a full Admin UI into the proxy process itself (`/ui`, no separate deploy), supports live zero-restart config changes (models, spend-log retention), and shows per-request spend/tokens/key/team by default while keeping prompt/response content opt-in. Portkey goes furthest on caller-facing DevEx — a one-click "Replay" button reopens any logged request in a live, editable prompt playground, cost/tokens auto-populate per log entry, and OTel GenAI semantic-convention attributes let externally-instrumented app traces get the same automatic cost tracking as native gateway traffic — though genuine multi-span tracing (vs. logging) still requires explicit SDK/header/callback instrumentation, and Logs Export is Enterprise-gated. Helicone's Sessions feature groups a full multi-step agent flow (LLM calls, tool calls, vector-DB queries) into one trace using only three HTTP headers on its OpenAI-compatible proxy — but claims of a Helicone-side Playground/replay UI comparable to Portkey's did not survive verification and remain genuinely unconfirmed, not disproven.

---

## Findings, ranked

### 1. Portkey's one-click "Replay" — a caller-facing debugging tool with no confirmed equivalent elsewhere (High confidence)

**What:** Portkey's Logs UI provides a one-click "Replay" feature that reopens any previously logged request inside a live prompt playground where the caller can edit and rerun it, directly from the log detail view.

**Why it matters for Kelvran specifically:** this is the single most concrete caller-facing DevEx gap in this report — a genuinely differentiated feature with no equivalent confirmed for LiteLLM or Helicone in this research. Kelvran has no logged-request replay/playground surface of any kind today.

**2026 best-practice grounding:** Unanimous 3-0, quote verified verbatim on live fetch of `portkey.ai/docs/product/observability/logs`. Only three narrow, explicitly-documented exceptions exist (non-chat endpoints, archived providers, Config-target prompts) — normal doc precision, not overreach.

**Concrete next step:** name as a candidate DevEx feature for a future admin-API/dashboard RFC — not urgent, but the single highest-signal "what would meaningfully close the gap" item in this report.

**Effort:** Large — requires both a persisted request-log store queryable by ID and a playground/replay UI, neither of which exist today.

---

### 2. LiteLLM's Admin UI ships bundled into the proxy itself, with live zero-restart config changes (High confidence)

**What:** LiteLLM's Admin UI is served at `<proxy_base_url>/ui` directly from the LiteLLM Proxy process — no separate deployment. It supports live, zero-restart configuration changes (model management, spend-log retention) with no config-file edits or proxy restart, and its Logs UI shows per-request Spend/Token Usage/Key/Team Name by default with zero caller-side SDK integration, while prompt/response content stays opt-in (`store_prompts_in_spend_logs`).

**Why it matters for Kelvran specifically:** Kelvran's admin surface is API-only today — no bundled UI, no live-editable config, no default per-request cost/token/key breakdown view. This is the lowest-friction "what does a competitor ship that we don't" finding, since it requires no new backend capability Kelvran lacks (Kelvran already tracks per-request cost/tokens/key internally) — only a UI layer on top of data Kelvran already has.

**2026 best-practice grounding:** Bundled-UI claim unanimous 3-0 (corroborated by the `DISABLE_ADMIN_UI` env flag implying on-by-default). Spend-log-settings live-update sub-claim unanimous 3-0, independently corroborated in `BerriAI/litellm` source (`LoggingSettings.tsx`, `_update_general_settings`). Model add/manage sub-claim 2-1. Default-tracked-metadata and opt-in-content claims both unanimous 3-0.

**Concrete next step:** if a future dashboard/admin-UI initiative is scoped, cite LiteLLM's bundled-proxy-process pattern (vs. a separately-deployed dashboard) as the lowest-friction precedent to evaluate first.

**Effort:** Large — a genuinely new UI surface, though it could reuse Kelvran's existing admin-API data model as its backend.

---

### 3. Portkey's OTel interoperability auto-derives cost from externally-instrumented app traces — no extra reporting code needed (High confidence)

**What:** Portkey's tracing is OTel-interoperable at the protocol level (W3C Trace Context `traceparent`/`baggage` headers) and automatically applies the same cost tracking/analytics to OTel-instrumented app traces as to native gateway traffic, by extracting GenAI semantic-convention token attributes (`gen_ai.usage.input_tokens`/`output_tokens`) and deriving cost from them.

**Why it matters for Kelvran specifically:** Kelvran already emits OTel GenAI semantic-convention spans (per this project's own existing telemetry work) — this raises a genuinely open question (see Open Questions) about whether the real gap here is a missing UI/collector integration rather than missing telemetry, since the underlying attribute data may already exist.

**2026 best-practice grounding:** Both sub-claims unanimous 3-0 with verbatim quotes and specific implementation detail (exact header grammar, exact OTel attribute names) reading as engineering docs rather than marketing.

**Concrete next step:** before scoping any new telemetry work, check whether piping Kelvran's existing OTel spans into a collector (rather than building a bespoke UI) would already unlock Portkey-equivalent cost-derivation — a cheaper path than it first appears.

**Effort:** Investigation first (Small); the actual gap, if any, is likely Medium (collector/dashboard integration, not new instrumentation).

---

### 4. Portkey's multi-span tracing is explicitly NOT zero-integration — a nuance worth not overclaiming (High confidence)

**What:** Despite Portkey's automatic logging/cost-derivation, genuine multi-span tracing is not zero-integration — it requires explicit instrumentation via SDK trace params (`traceId`/`spanId`/`spanName`), request headers, or framework callback handlers (Langchain/LlamaIndex) before a request produces a structured trace.

**Why it matters for Kelvran specifically:** this is a guardrail against over-crediting Portkey (and, by extension, over-scoping any Kelvran response) — basic logging/cost-derivation is automatic, but structured multi-span tracing is caller-instrumented everywhere, not a Kelvran-specific shortfall relative to the best competitor.

**2026 best-practice grounding:** Confirmed sub-claim unanimous 3-0. A related, more sweeping claim ("Portkey provides request tracing across the full lifecycle as a built-in capability") was explicitly refuted at 1-2 — reinforcing that basic logging is automatic but structured tracing is caller-instrumented.

**Concrete next step:** none — this is a scoping correction, not an action item.

**Effort:** N/A.

---

### 5. Helicone's Sessions — full agent-flow tracing via three HTTP headers, no dedicated SDK object (High confidence)

**What:** Helicone's Sessions feature groups a full multi-step agent/conversation flow — spanning LLM calls, vector-DB queries, and tool calls (any request logged through Helicone's pipeline) — into one unified trace view, using only three HTTP headers (`Helicone-Session-Id`/`-Path`/`-Name`) added to existing calls through its OpenAI-compatible proxy. No dedicated Session SDK object or backend setup is required for this base capability.

**Why it matters for Kelvran specifically:** a genuinely low-friction pattern worth noting as a model for any future multi-step-agent-flow tracing Kelvran might add — three headers is a much smaller integration surface than a dedicated tracing SDK.

**2026 best-practice grounding:** Both sub-claims unanimous 3-0, verified via direct WebFetch and a raw-HTML curl fetch (bypassing summarization) that found the exact "What Sessions Can Track" bullet list including vector DB queries, tool calls, and "any logged request." **Important caveat:** 7 other Helicone-related claims in this same research round (built-in Playground UI, header-only replay, fetch-edit-resend-compare replay workflow, always-on cost/latency without instrumentation, zero-SDK swap-base-URL as the defining low-friction pattern) were **refuted** (votes 0-3 to 1-2), mostly because they cited a GitHub README rather than Helicone's own docs pages. Helicone's Sessions/proxy-header capability is confirmed; a Portkey-equivalent Replay/Playground feature for Helicone remains genuinely unconfirmed, not proven absent.

**Concrete next step:** a targeted, docs-only (not README) re-check of whether Helicone ships a Playground/replay feature, before treating Portkey as uniquely differentiated on that specific point (see Open Questions).

**Effort:** N/A for this report; a Small follow-up research pass if the Playground question matters for prioritization.

---

### 6. Portkey's custom metadata and 15-field log filtering — immediate grouping/filtering with no dashboard-building (Medium confidence)

**What:** Portkey allows any caller to attach arbitrary custom metadata key-value pairs to requests (via SDK options, `createHeaders`, or an `x-portkey-metadata` cURL header), immediately usable for grouping/filtering in Portkey's own Analytics Dashboard and Logs UI — available on all plans. Separately, Portkey's Logs page lets any ingested trace be filtered by roughly 15 concrete attributes and drilled into at the individual-request level.

**Why it matters for Kelvran specifically:** both are genuine caller-facing UI capabilities, not raw telemetry an operator would need to build a dashboard against — a further data point for how far ahead of an API-only admin surface Portkey's DevEx sits.

**2026 best-practice grounding:** Custom-metadata claim 2-1 (all four integration channels and immediate-dashboard-usability language confirmed verbatim). 15-field-filter claim 2-1 despite verbatim quote match and corroboration across three separate first-party doc pages.

**Concrete next step:** none immediate — informs the same future dashboard/admin-UI RFC as Findings 1-2 if pursued.

**Effort:** N/A for this report.

---

## Top takeaways

1. **The single clearest, highest-signal gap is Portkey's Replay/Playground feature (Finding 1)** — no equivalent exists in Kelvran today, and none was confirmed for LiteLLM or Helicone either, making it the most differentiated single capability found in this round.
2. **Much of LiteLLM's and Portkey's DevEx advantage is a UI layer over data Kelvran may already compute internally** (per-request cost/tokens/key), not a fundamentally new backend capability — worth checking before assuming a large build is required.
3. **Check whether Kelvran's existing OTel GenAI spans (Finding 3) already contain what a collector/dashboard integration would need**, before scoping new instrumentation work — the real gap may be a missing UI/collector, not missing telemetry.
4. **Do not over-credit Helicone or Portkey past what survived verification** — 7 of 9 investigated Helicone claims were refuted for weak (README, not docs) sourcing, and one sweeping Portkey tracing claim was explicitly refuted; both vendors' actual feature sets are narrower than their marketing/README framing.

---

## Caveats

- **Cloudflare AI Gateway was explicitly in scope for the original research question but produced zero surviving or refuted claims** — it appears to be uninvestigated (or its results weren't included) in this round. This is a real, disclosed gap in coverage, not a "Cloudflare has nothing" finding.
- Helicone coverage is notably thin and skewed toward refutation (only 2 of 9 investigated Helicone claims survived) — this reflects source-quality problems (GitHub README vs. docs site) more than confirmed absence of features. Do not read the refutations as proof Helicone lacks a Playground/replay tool.
- Several verifier passes explicitly noted rate-limited or unavailable independent search tools (Exa/Tavily/DuckDuckGo), so cross-vendor corroboration was sometimes thin — treat claims with 2-1 votes (LiteLLM model-mgmt-no-restart, Portkey OTel-suite-sufficiency, Portkey Logs-page-caller-facing, Portkey custom-metadata) as directionally correct but slightly less certain than the unanimous 3-0 claims.
- All findings are dated to live docs fetched 2026-09-24; enterprise-gating and UI feature sets for all three vendors are fast-moving and could shift with future releases.
- This report did not itself re-verify Kelvran's own current architecture (`ARCHITECTURE.md`/telemetry docs) against these external findings in a fresh pass beyond what's noted above — that comparison should be a first step of any follow-up scoping work, not assumed already done.

## Open questions

- What does Cloudflare AI Gateway's own analytics UI ship out-of-the-box (per-request cost/latency, live request inspection)? Explicitly in scope but uninvestigated this round — needs its own follow-up pass.
- Does Helicone ship any built-in prompt-playground/one-click-replay feature equivalent to Portkey's Replay button? Genuinely open (refuted only for weak sourcing) — needs a targeted re-check against `docs.helicone.ai` pages directly, not the GitHub repo.
- Do LiteLLM or Helicone offer an OpenAPI spec or official SDK generation for their own admin/observability APIs? None of the surviving claims addressed this DevEx dimension for any vendor.
- ~~Given Kelvran already emits OTel GenAI semantic-convention spans, would piping those into an OTel collector unlock Portkey-equivalent automatic cost derivation without Kelvran building its own dashboard — i.e., is the real gap a missing UI/collector integration rather than missing telemetry?~~ **Closed 2026-09-25**: yes, and it's already built and live-verified, not just possible — see the Follow-up scoping section below.

## Follow-up scoping (2026-09-24, same day): neither DevEx feature is ready for an implementation RFC

A dedicated scoping pass (design-only, no code) investigated what building Finding 1 (Portkey-style Replay) and Finding 2 (LiteLLM-style bundled admin UI) would actually require against Kelvran's real current code.

**Shared blocker, confirmed by both investigations independently:** neither feature can be built without a new, persisted, queryable request-log store — one does not exist in any form today. `internal/admin/auditstore` logs admin *mutation* events (key CRUD, config reads), never a proxied chat request/response. The response cache (L1/L2/L3) and idempotency store both hold response blobs keyed by an irreversible SHA-256 hash of the request — neither can reconstruct "show me request #X." `GatewayDecisionEvent` and OTel spans carry only outcome/cost/trace metadata, zero prompt/completion content, by explicit design.

**Replay (Finding 1) — Large, confirmed.** Needs three new subsystems: an opt-in (off-by-default, separately-retained) content store distinct from metadata capture, given this repo's PII posture; a new `GET /admin/requests/{trace_id}` read route; and a genuinely new dry-run/no-rebilling/no-cache-write mode for `HandleChatCompletion`, since naive replay would re-run rate-limit/budget checks and re-bill. **Hardest open question:** who is authorized to replay another tenant's logged request, and under what credential — this is a privilege-escalation decision, not a feature flag.

**Bundled admin UI (Finding 2) — Large, confirmed, but ~60% of the data layer already exists.** Server-rendered Go + htmx is the right stack (confirmed independently, not just repeating `DECISIONS.md`'s prior note): the repo has zero JS/npm toolchain today, and the admin API's stateless-bearer-token auth model has no session/cookie concept a React SPA would need anyway. Bundling into the existing admin `net.Listener` (rather than a new deployable) fits the two-artifact model cleanly. Most pages are ready today (virtual keys, spend, prompts, deployments, audit log) — the two headline LiteLLM-parity features (live config editing, per-request logs) are both backend-gated: `GET /admin/config` has no write route, and the request-log list needs the same store Replay needs. **Hardest open question:** how does a browser authenticate to this surface at all, given the admin API has zero session/cookie concept today — this single decision determines CSRF exposure and XSS blast radius, and `THREAT_MODEL.md` hasn't priced a browser-facing write surface yet.

**Verdict: do not scope an RFC for either yet.** Both features originated from a competitive gap scan, not an operator complaint or incident — no urgency has been demonstrated. A cheaper, still-unexplored step sat ahead of both: this document's own Open Questions above already asked whether piping Kelvran's already-emitted OTel spans into a collector unlocks Portkey-equivalent cost derivation for near-zero cost. **That investigation is now closed (2026-09-25) — it was already done, not merely possible.** `docs/operations/TELEMETRY.md` confirms Kelvran already ships: real `gen_ai.usage.*` semantic-convention span attributes, an always-set `kelvran.cost.usd` span attribute (plus `kelvran.savings.usd` on cache hits), a fully wired OTLP exporter (`otlp_endpoint`, both traces and metrics), and — critically — a **provisioned, live-verified Grafana dashboard** (`docs/operations/grafana/dashboards/kelvran-overview.json`, confirmed loaded and resolving real non-zero values through Grafana's own datasource proxy against real Bedrock traffic, not just a direct Prometheus query) plus an example agent-run-cost-by-`agent_run_id` dashboard (`grafana-agent-run-cost-dashboard.json`) — the exact per-request cost breakdown Portkey's own Logs UI provides by default. The "missing telemetry vs. missing UI/collector integration" question has a definitive answer: **neither is missing** for cost/spend visibility specifically. What remains genuinely absent is Portkey's interactive one-click Replay/editable-playground UX (needs the request-log store either way) and a UI *bundled into the gateway process itself* (LiteLLM's shape) — Kelvran's standards-based OTel+Grafana approach is a real, live-verified, deliberate architectural alternative to a proprietary bundled dashboard, not an unaddressed gap. **If either DevEx feature is ever pursued, revised order:** (1) the shared request-log store, metadata-only by default (now the true first step, since the OTel-collector step is done); (2) the bundled UI, if a same-process/no-separate-Grafana-deploy experience is ever demonstrated to matter to real operators; (3) Replay last, gated on its cross-tenant-authorization question being resolved first.
