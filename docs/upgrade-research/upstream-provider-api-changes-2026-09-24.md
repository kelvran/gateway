# Upstream Provider API Changes (Last 2-4 Weeks) — Adapter-Layer Impact (2026-09-24)

**Date:** 2026-09-24
**Scope:** `gateway/internal/adapter/{openai,openaicompat,anthropic,bedrock,gemini}` and `gateway/internal/adapter/capabilities.go` — whether genuinely new OpenAI/Anthropic/Gemini/Bedrock API surface shipped in the last 2-4 weeks (through 2026-09-24) is already handled, silently broken, or a real gap in Kelvran's own adapter code.
**Method:** Adversarial multi-source research (3-vote verification) against each provider's own live docs/changelogs, cross-checked directly against Kelvran's Go adapter source (`capabilities.go`, `anthropic.go`, `openai.go`, `gemini.go`, `bedrock.go`) via targeted grep/read — not just the provider docs in isolation. Sources include deliberate anti-mocking checks (raw curl bypassing AI-mediated fetch, DNS/TLS/404 controls, Wayback Machine cross-checks) per this project's own established caution about sandboxed-fetch content mocking.

**Recovery note:** this report's synthesis was completed by the research workflow (17 findings, all with live-verified sources and adapter-code cross-checks) but the workflow's own file-write step did not persist it to this path — the underlying subagent's own closing caveat states plainly: *"per this subagent's operating rules (no writing report/findings .md files), I did NOT write the findings to `docs/upgrade-research/upstream-provider-api-changes-2026-09-24.md` as the computed task requested — that file-write step is left to the orchestrating process."* The orchestrating step silently did not do so. This document was reconstructed directly from that already-completed synthesis (recovered from the workflow's own journal), not re-researched — no findings below are new since the original run, only the persistence step was missing.

---

## Executive summary

Four real change clusters landed across the four providers in the last few weeks: (1) reasoning/thinking-block semantics — Anthropic Opus 5.5/Fable 5.1 now reject the `thinking` field and forced `tool_choice` (`any`/`tool`) outright, replacing manual thinking control with a new `effort` parameter, plus a new "preserved thinking" integrity check; Bedrock's Claude Sonnet 5 runs adaptive thinking on by default with no way to set a budget. (2) New tool-calling surfaces — Anthropic's beta inline `tool_addition` block, OpenAI's Responses-API-only tool calling for GPT-6 Astra, plus a structurally separate new OpenAI Agents API. (3) Caching mechanics — Anthropic's `max_tokens:0` cache pre-warming, OpenAI's up-to-90% cached-token discount, Gemini's new Interactions API dropping explicit-cache support. (4) Quota/endpoint changes — AWS's per-model→cross-model Bedrock token-per-day quota, and bedrock-mantle rejecting structured outputs (Converse/InvokeModel on bedrock-runtime unaffected).

Direct inspection of Kelvran's own Go adapter source shows two of these are already non-issues by design — bedrock-mantle and Gemini's Interactions API are both endpoints Kelvran's adapters explicitly, deliberately never target (confirmed via existing code comments, one of which already cites this exact finding as "Confirmed 2026-09-23"). The rest range from **one concrete, one-line, low-risk fix** (Opus 5.5 missing from a client-side capability-rejection allowlist) to structural gaps that need a design decision, not a patch (no canonical `thinking`/`effort` field anywhere in `ChatRequest`; no Responses-API code path in the OpenAI adapter at all).

---

## Findings, ranked

### 1. `AnthropicModelRejectsForcedToolChoice` is missing "claude-opus-5-5" — a real, one-line, low-risk gap (High confidence)

**What:** Claude Opus 5.5 (inheriting Fable 5.1's restrictions) can no longer have extended thinking disabled/enabled via the `thinking` field (400 error; use the new `effort` parameter instead), and forced `tool_choice` values `any`/`tool` also now return a 400, leaving only `auto`/`none`.

**Why it matters for Kelvran specifically:** `capabilities.go`'s `AnthropicModelRejectsForcedToolChoice` already client-side-rejects forced `tool_choice` for model-name substrings `"claude-fable-5-1"` and `"claude-mythos-5-1"` — but the substring list does **not** include `"claude-opus-5-5"`. A request with forced `tool_choice` routed to Opus 5.5 today gets forwarded and hits a raw, upstream 400 instead of Kelvran's own clean, client-side validation error. This is the single most concrete, cheapest, lowest-risk actionable item in this whole report.

**2026 best-practice grounding:**
- Anthropic's own release notes (2026-09-22, verified via raw curl against `docs.anthropic.com/en/release-notes/api` and `platform.claude.com/docs/en/release-notes/overview`) confirm both restrictions for Opus 5.5 [confirmed 3-0].
- Separately, Kelvran's canonical `ChatRequest` (`types.go`) has no request-level `thinking`/`effort`/`budget_tokens` field at all — Kelvran never sends `thinking` today, so it can't trigger the new 400 for *setting* it, but it also can't yet expose the new `effort` control to callers. This is a pre-existing gap, not a new break (see Finding 6).

**Concrete next step:** add `"claude-opus-5-5"` to the substring list in `AnthropicModelRejectsForcedToolChoice` (`capabilities.go`) — mirrors the exact pattern already used for Fable 5.1/Mythos 5.1.

**Effort:** Trivial — a one-line allowlist addition, no new tests beyond a table-driven case for the new substring.

---

### 2. Bedrock's structured-output rejection on `bedrock-mantle` is already a confirmed non-issue — Kelvran never targets that endpoint (High confidence)

**What:** On the new `bedrock-mantle` endpoint, the Messages API rejects `output_config.format` (structured outputs) with a 400; the same request succeeds via Converse/InvokeModel on `bedrock-runtime`.

**Why it matters for Kelvran specifically:** `bedrock.go`'s own code comments already reference this exact finding ("structured-output-2026-09-14.md Finding 5. Confirmed 2026-09-23") and the adapter only ever targets the Converse API (`bedrock-runtime`) — it never constructs a `bedrock-mantle` request. Already documented in-repo; no action needed.

**2026 best-practice grounding:** Confirmed against two independent AWS doc pages with matching, exact wording [confirmed 3-0].

**Concrete next step:** none — already correctly scoped and documented.

**Effort:** N/A (no action).

---

### 3. Gemini's Interactions API drops explicit-cache support — already a non-issue by design (High confidence)

**What:** Google's new Interactions API surface (separate from `generateContent`) supports only implicit caching; explicit cache-object creation/management still requires routing through `generateContent`.

**Why it matters for Kelvran specifically:** `gemini.go`'s own code comment (near the tool_choice mapping) explicitly states its wire-format target is `generateContent` and *not* "a newer Interactions-API mode" — the adapter deliberately never targets the Interactions API. Non-issue by design, no code change needed.

**2026 best-practice grounding:** Verified verbatim via direct curl of the live primary doc, cross-checked against the companion Interactions-API page [confirmed 3-0].

**Concrete next step:** none.

**Effort:** N/A (no action).

---

### 4. "Preserved thinking" integrity check — a real, currently-unmediated 400 risk for edited conversations (Medium confidence)

**What:** Anthropic added a "preserved thinking" integrity/binding check: replaying a thinking block after the preceding system prompt/tools/messages changed now returns a 400 by default (for accounts created on/after Aug 31, 2026), with an opt-in non-strict mode that instead drops the mismatched block and flags which blocks were dropped.

**Why it matters for Kelvran specifically:** `reasoningBlockToProvider`/`ReasoningBlock` in `anthropic.go` already replays thinking/redacted_thinking blocks verbatim with their original `Signature` on later turns — exactly the pattern this new check validates against — but there is no `block_binding`/`prefix_mismatch_behavior` field or `thinking-binding-controls-2026-08-01` beta header anywhere in the adapter. If a Kelvran-mediated conversation is edited mid-stream (retries, prompt templating) and a thinking block is replayed, new-Anthropic-account callers get an unmediated 400 that Kelvran neither pre-empts, explains, nor can opt out of via non-strict mode.

**2026 best-practice grounding:** Anthropic's own support article and release notes/product page independently corroborate the strict-default + non-strict-opt-in mechanism [confirmed 2-1 / 2-1 / 3-0 across three merged sub-facets].

**Concrete next step:** scope a follow-up design pass (not a blind patch) for whether to plumb the `thinking-binding-controls-2026-08-01` beta header and a `block_binding`/`prefix_mismatch_behavior` field through — relevant to `docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md`.

**Effort:** Small-Medium — needs a design decision on default strict/non-strict behavior before coding.

---

### 5. Anthropic's beta inline `tool_addition` block — additive gap, cache-cost-relevant (Medium confidence)

**What:** A new Anthropic beta lets tools be added/changed mid-conversation via a `tool_addition` content block (`inline-tools-2026-09-15` header) instead of editing the top-level `tools` array, specifically to avoid invalidating the prompt cache.

**Why it matters for Kelvran specifically:** the anthropic adapter only ever emits the top-level `tools` array (`ChatRequest.Tools`) with no `tool_addition` content-block type and no plumbing for the beta header. Real, additive feature gap (not a break): Kelvran cannot yet let a caller add/change a tool mid-conversation without a full cache-busting `tools` re-send.

**2026 best-practice grounding:** Verified via raw curl of the live docs page plus independent corroboration from the `anthropic-sdk-python` release notes [confirmed 2-1].

**Concrete next step:** name as a future RFC candidate if/when cache-cost from tool-array churn becomes a real, measured problem — not urgent today.

**Effort:** Medium — new content-block type plus beta-header plumbing.

---

### 6. GPT-6 Astra requires the Responses API for tool calling — needs a runtime check, not yet a confirmed break (Medium confidence)

**What:** For GPT-6 Astra, tool/function calling is only supported via the Responses API — Chat Completions tool users must migrate.

**Why it matters for Kelvran specifically:** the `openai` adapter's wire types are entirely Chat-Completions-shaped, with no Responses-API request/response mapping (no `input`/`output` item types) anywhere in the package, and no gpt-6/astra-specific capability gating exists in adapter code at all. **Caveat, disclosed by the research itself:** the actual HTTP path selection may live in the router/transport layer outside this package, which was *not* checked — this should be verified before treating it as a confirmed break, not assumed.

**2026 best-practice grounding:** Verified verbatim via two independent fetch tools plus a Wayback Machine archive snapshot [confirmed 2-1].

**Concrete next step:** a targeted follow-up read of the router/transport/dataplane layer to confirm whether gpt-6-astra tool-calls are already routed to `/v1/responses`, before deciding whether this is a real end-to-end break.

**Effort:** Investigation first (Small); the fix itself, if confirmed needed, is Large (a new Responses-API code path).

---

### 7. OpenAI's new 429/503 split needs router-layer verification, not just adapter-layer (High confidence on the API change itself)

**What:** OpenAI split its generic 429 into a 429/`slow_down` (traffic ramping too fast) and a new 503/`server_is_overloaded` (temporary model overload), both of which may carry a `Retry-After` header that should take precedence over exponential backoff.

**Why it matters for Kelvran specifically:** Kelvran already has generic Retry-After-forwarding infrastructure (`retry_after.go`, `backoff.go`) but no per-status-code (`slow_down` vs `server_is_overloaded`) differentiation was found in `adapter/errors.go`. The two new codes would likely be handled correctly by the existing generic Retry-After path, but Kelvran cannot yet distinguish "back off, you're too fast" from "the model itself is overloaded" for routing/fallback decisions.

**2026 best-practice grounding:** Verified against two independent primary OpenAI pages plus DNS/TLS/404-control checks ruling out a sandboxed mock [confirmed 3-0].

**Concrete next step:** a follow-up code read of the router/fallback layer specifically, to see whether this distinction would change any fallback decision if plumbed through.

**Effort:** Investigation first (Small); differentiation itself likely Small-Medium.

---

### 8. Cost/pricing-only changes — no adapter code change implied

**What:** OpenAI cached input tokens can now receive discounts of up to 90% (vs. the older 50% figure); AWS replaced Bedrock's per-model token-per-day quota with a single cross-model, per-account, per-Region quota.

**Why it matters for Kelvran specifically:** neither is an adapter-shape change. The caching-discount figure is relevant to Kelvran's cost-tracking/FinOps layer's pricing tables, not the adapter package. The Bedrock quota change is an operational/capacity-planning note — no corresponding per-model Bedrock token-per-day tracking was found client-side in the bedrock adapter or the `ratelimit` package, suggesting Kelvran relies on AWS's own server-side enforcement today (likely by design).

**2026 best-practice grounding:** Both confirmed 3-0 / 2-1 respectively against primary OpenAI/AWS sources.

**Concrete next step:** flag the new caching-discount figure to whoever owns Kelvran's pricing tables; flag the Bedrock quota rename to whoever owns per-provider quota dashboards. Neither needs adapter code changes.

**Effort:** N/A for adapter code; a pricing-table/docs update elsewhere.

---

### 9. New, structurally-separate surfaces explicitly out of scope for the current 5-adapter architecture (Medium confidence, no action implied)

**What:** OpenAI shipped a new, additive stateful "Agents API" (`POST /v1/agents/sessions`, `OpenAI-Beta: agents=v1`) sitting alongside Chat Completions/Responses. OpenAI's Responses API also gained async tool calling and mid-turn WebSocket steering for GPT-6 Astra. Google's Antigravity Agent changed built-in tool parameter casing/naming (breaking for local function-call parsers, not for remote-sandbox output readers). Gemini 3.8 Live/Live Extended Thinking made async function calling the Live-API (audio-to-audio) default.

**Why it matters for Kelvran specifically:** none of these touch anything Kelvran's current adapters target — no agent/session objects, no Live API (audio-to-audio streaming), no Antigravity Managed-Agent surface exist anywhere in `gateway/internal/adapter`. All are new, optional surfaces requiring a wholly new adapter/endpoint mapping if Kelvran ever decides to support them — not patches to existing code.

**2026 best-practice grounding:** Each confirmed 2-1 or 3-0 against primary/changelog sources with Wayback Machine cross-checks.

**Concrete next step:** none now — name as out-of-scope-until-a-real-need in a future architecture note if this recurs.

**Effort:** N/A (no action).

---

## Top takeaways

1. **Ship the one-line Opus 5.5 fix.** Add `"claude-opus-5-5"` to `AnthropicModelRejectsForcedToolChoice`'s substring list — trivial, zero design risk, closes a real gap today.
2. **Two suspected gaps are already confirmed non-issues** — bedrock-mantle and Gemini's Interactions API are both already correctly out of scope by design, and already documented in-repo for the Bedrock case.
3. **The "preserved thinking" 400 risk (Finding 4) is the most consequential real gap** — it can surface as an unmediated upstream 400 for real Kelvran traffic (mid-stream-edited conversations), and deserves a deliberate design decision (strict vs. non-strict default) rather than a blind patch.
4. **Two findings need a runtime/router-layer check before being treated as confirmed** (GPT-6 Astra Responses-API requirement; the OpenAI 429/503 split's actual routing impact) — this report's adapter-only inspection cannot resolve either on its own.

---

## Caveats

- This report's Kelvran-adapter-impact analysis is based on targeted `grep`/`Read` inspection of the Go adapter source files only (`capabilities.go`, `anthropic.go`, `openai.go`, `gemini.go`, `bedrock.go`) — it did not run the test suite, build the gateway, or inspect the router/transport/dataplane layer that actually selects HTTP paths. "Would break" characterizations for Findings 6 and 7 should be treated as strong hypotheses pending a runtime check, not proven facts.
- Six related claims were explicitly refuted by 3-vote adversarial review (0-3 or 1-2) and are excluded above: a 1-hour Anthropic cache TTL, OpenAI's 30-minute cache-eligibility TTL, an OpenAI cross-response reasoning-effort `configuration_update` field, Gemini implicit caching being on by default for 2.5+, a second Bedrock inference endpoint requiring OpenAI/Anthropic-native async calls, and a Bedrock Converse `serviceTier` field.
- Several sources are stale/redirecting URL aliases (e.g. `docs.anthropic.com/en/release-notes/api` redirects to `platform.claude.com`) rather than the final canonical URL — content was verified, but citations should be updated to the resolved URL if this document is revised.
- Model names cited (Claude Opus 5.5, Claude Fable/Mythos 5.1, GPT-6 Astra, Gemini 3.8 Live, Antigravity Agent 09-2026) are all past this project's Jan-2026 knowledge cutoff and could not be corroborated from training data — confidence rests entirely on the research agents' live-fetch verification (including deliberate anti-mocking checks), not independent knowledge.
- The Claude Sonnet 5 adaptive-thinking-on-by-default finding is dated Aug 24, 2026 — slightly outside a strict "last 2-4 weeks" window relative to 2026-09-24, though it describes still-current, live model-card behavior with no superseding entry found.
- No original 5-parallel-research fan-out was run by the synthesis step itself — it operated on an already-completed, already-3-vote-verified claim set handed down by the orchestrating workflow.

## Open questions

- Does Kelvran's router/transport/dataplane layer already have model-aware logic that would route `gpt-6-astra` tool-calls to `/v1/responses` instead of `/v1/chat/completions`, or is this a real, currently-unaddressed breaking change end-to-end?
- Should Kelvran add a canonical `thinking`/`effort` field to `ChatRequest` so callers can control Anthropic's new effort-based reasoning (Opus 5.5) and Bedrock Claude Sonnet 5's default-on adaptive thinking, rather than leaving extended thinking entirely unconfigurable through the gateway?
- Is the AWS Cross-Model Max Tokens Per Day quota change something Kelvran's own quota/budget-tracking layer needs to mirror, or is it purely an AWS-side enforcement detail with no client-visible shape change?
- Given the one-line, concretely-identified Opus 5.5 gap (Finding 1), should it be fixed immediately as a small, low-risk patch ahead of any larger reasoning/effort-field redesign (Finding 4/Open Question 2)?
