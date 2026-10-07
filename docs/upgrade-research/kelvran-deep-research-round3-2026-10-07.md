# Kelvran Deep-Research Round 3 — Reasoning Tokens, Streaming Usage, Bedrock Throttling, OWASP 2026, Content-Aware Routing

Date: 2026-10-07 · five parallel `/deep-research` runs (Claude Code's built-in dynamic workflow;
~530 agents, ~9,900 tool uses; each extracted claim adversarially verified by 3 independent agents,
≥2/3 refute-votes kill it), then every claim that would drive a code change was independently
re-fetched from its primary source by the main session before being written here. Follows the
2026-09-29 round (`DECISIONS.md` `[2026-09-29]`), whose only actionable hit was the OTel
token-metrics migration (shipped `94da263a`); these five topics were chosen to cover ground that
round did not. Unlike the prior round, all five Synthesize phases returned real content (the
recurring placeholder-JSON bug did not recur).

**Verification legend** — `[V]` primary source fetched and grepped directly by the main session on
2026-10-07 (AWS pages carried `Last-Modified: 2026-10-07`); `[R]` 3-0 adversarially verified by the
research run, not independently re-fetched here; `[C]` confirmed against Kelvran's own code by
direct read; `[O]` open — no claim survived verification.

## Ground truth (Kelvran code, re-read 2026-10-07) — all `[C]`

- `dataplane.finalize` reconciles `float64(resp.Usage.TotalTokens)` into `KeyLimiter.ReconcileTPM`
  (`dataplane.go` ~L4283-4287); `RecordTokens` debits the raw count 1:1.
- Bedrock adapter: `Usage.TotalTokens = native totalTokens + cacheReadInputTokens +
  cacheWriteInputTokens` (`bedrock.go:1016`, `stream.go:354`) — cache **reads** are folded into the
  figure the TPM limiter debits.
- Bedrock adapter never sends `additionalModelResponseFieldPaths` (repo grep: 0 hits).
- `bedrock.go` ~L635: `inferenceConfig.MaxTokens` is forwarded only when the client set
  `req.MaxTokens`; otherwise `inferenceConfig` is omitted entirely.
- No field for reasoning/thinking tokens anywhere in `adapter.Usage` or any adapter (grep for
  `output_tokens_details|thinking_tokens|reasoning_tokens|thoughtsTokenCount`: 0 hits).
- OpenAI adapter always sets `stream_options.include_usage: true` (`openai.go:314`).
- `estimateOrRealUsage` (`streaming.go:925`) divides serialized length by
  `streamRunawayCharsPerToken` only when no provider usage frame arrived (`finalUsage == nil`).
- Fail-open metrics exist for rate-limit and budget (`kelvran.ratelimit.fail_open`,
  `kelvran.budget.fail_open`); a guardrail detector error is a `slog.Warn` line only.
- `hiddenUnicodeRanges` tag block is `{0xE0020, 0xE007F}` (`promptinjection.go:142`).
- No Bedrock exception is classified by name (`ThrottlingException`, `ModelNotReadyException`,
  `modelStreamErrorException`, `serviceUnavailableException`: 0 hits in `internal/adapter/bedrock/`
  and `dataplane/fallback.go`); no `Retry-After` handling anywhere in those files.

---

## 1. Reasoning / thinking-token accounting (run `wsfow9ori` — 106 agents, 18 confirmed / 7 refuted)

- `[V]` **Anthropic Messages API** reports reasoning at `usage.output_tokens_details.thinking_tokens`
  (`OutputTokensDetails { thinking_tokens: number }`, "Always ≤ output_tokens; output_tokens −
  thinking_tokens approximates the non-reasoning output", default 0). The string `reasoning_tokens`
  appears 0 times on the API reference. Release note, May 27 2026: "The Messages API response now
  includes usage.output_tokens_details.thinking_tokens … When streaming, the breakdown appears only
  on the final message_delta event. No beta header is required." —
  `platform.claude.com/docs/en/api/messages`, `/docs/en/release-notes/api`.
- `[V]` Streaming: "The token counts shown in the usage field of the message_delta event are
  cumulative" — merge-overwrite from the final delta, never sum. — `/docs/en/build-with-claude/streaming`.
- `[R]` Pricing: thinking tokens bill at the model's ordinary output rate (five pricing columns, no
  thinking rate); the billed count is the full raw thinking, not the summarized text returned.
- `[V]` **Bedrock `TokenUsage`** has exactly `inputTokens`/`outputTokens`/`totalTokens` (required) and
  `cacheDetails`/`cacheReadInputTokens`/`cacheWriteInputTokens` (optional) — no reasoning member, in
  both Converse and the ConverseStream `metadata` event. — `API_runtime_TokenUsage.html`.
- `[V]` Converse/ConverseStream accept `additionalModelResponseFieldPaths` (JSON Pointer strings, max
  10) and return them under `additionalModelResponseFields` — top-level in the Converse response, and
  as `MessageStopEvent.additionalModelResponseFields` in ConverseStream. — `API_runtime_Converse.html`,
  `API_runtime_MessageStopEvent.html`.
- `[R]` Two independent live probes: requesting `["/usage/output_tokens_details"]` returns the native
  `thinking_tokens`, a strict subset of `outputTokens` (probe: `outputTokens` 333 == native
  `output_tokens` 333, containing `thinking_tokens` 206). LiteLLM's Bedrock adapter does exactly
  this. **Observed behaviour, not documented by AWS for this purpose** — could change.
- `[V]` (from the 2026-09-29 migration) the OTel `gen_ai.client.inference.usage.reasoning.output_tokens`
  counter is defined as a **subset** of `…usage.output_tokens`, matching Anthropic's and Bedrock's shape.
- `[R]` (cross-reference from run 2) OpenAI Responses API `ResponseUsage.output_tokens_details.reasoning_tokens`
  exists in the current openai-node/openapi spec; Gemini `usageMetadata.thoughtsTokenCount` exists and
  `totalTokenCount = prompt + toolUsePrompt + thoughts + candidates` — i.e. for Gemini, thoughts are
  **additional to** `candidatesTokenCount`, not a subset, so the OTel-subset invariant needs
  `output = candidates + thoughts` there.
- `[O]` OpenAI Chat Completions `completion_tokens_details.reasoning_tokens` streaming availability;
  whether any provider bills reasoning at a different rate (LiteLLM's price table shows a distinct
  reasoning rate only for Qwen/dashscope, Perplexity sonar-deep-research, Gemini-at-parity — none for Claude).

## 2. Streaming usage frames and mid-stream error semantics (run `wu5f9s0mu` — 107 agents, 20 / 5)

- `[V]` **OpenAI Chat Completions**: with `include_usage`, exactly one extra chunk with `choices: []`
  and the full `usage` arrives before `[DONE]`; "NOTE: If the stream is interrupted or cancelled, you
  may not receive the final usage chunk." — `developers.openai.com/api/reference/.../streaming-events/`.
  `[R]` Billing still accrues backend-side on a cancelled stream (OpenAI staff, 2026-04-06).
- `[V]` **Anthropic**: `message_start` carries input/cache usage; the final `message_delta` carries
  cumulative totals; in-band `event: error` with `error.type` (e.g. `overloaded_error`, "would
  normally correspond to an HTTP 529").
- `[V]` **Bedrock ConverseStream**: the `metadata` event is the only usage carrier (`usage` and
  `metrics` Required: Yes inside it) but the `metadata` member of the output union is itself
  `Required: No` — a stream terminated by an in-band exception ends with **no usage at all**. In-band
  exception events and their statuses: `internalServerException` 500, `modelStreamErrorException` 424,
  `serviceUnavailableException` 503, `throttlingException` 429, `validationException` 400. —
  `API_runtime_ConverseStreamOutput.html`, `API_runtime_ConverseStreamMetadataEvent.html`.
- `[R]` Bedrock cache fields are optional/omitted on the wire; when caching is active `inputTokens` is
  the non-cached portion.
- `[R]` **Gemini** `streamGenerateContent`: `usageMetadata` is optional per chunk; Google's reference
  does not say whether it is per-chunk or last-only, and third-party sources conflict — safe rule:
  "latest non-empty `usageMetadata` wins, never sum".
- `[R]` (2-1) OpenAI Responses API has two in-band error shapes (`type: error` and `response.failed`).
- `[O]` Client-disconnect billing for Anthropic/Bedrock/Gemini; how LiteLLM/Portkey/Envoy/Kong
  propagate cancellation; any quantified accuracy of character-based estimation.
- **Verdict**: estimation can be eliminated for **completed** streams on all five surfaces — and
  Kelvran already does this (the estimator is `finalUsage == nil` fallback only). It must remain, flagged,
  for aborted / in-band-error streams on every provider. The actionable delta is observability
  (record *why* a stream fell back: abort vs in-band exception vs missing frame) and the Bedrock
  exception-name → status contract, not removal of the estimator.

## 3. Bedrock throttling, quotas, adaptive concurrency (run `w8paq7nup` — 107 agents, 15 / 10)

- `[V]` **Burndown**: "The burndown rate for Anthropic Claude models version 4.8 is 15x for output
  tokens … Claude Opus 5.5, Claude Sonnet 5, Claude Opus 5, and Claude Fable 5.1 is 10x … For all
  other Anthropic models version 4.7 and below, the burndown is 5x for output tokens." Input quota
  consumption = "InputTokenCount + CacheWriteInputTokenCount" — **cache reads are excluded**. —
  `quotas-token-burndown.html`.
- `[V]` Quotas are shared across every inference API ("Although the quota names refer to InvokeModel,
  they aren't per-API"); "RPM quotas on the bedrock-runtime endpoint are model-specific. Some models
  … do not have an RPM quota". — `quotas-runtime.html`.
- `[V]` **Quota table for Kelvran's exact models** (`general/latest/gr/bedrock.html`): Sonnet 5 has
  **only** TPM rows (on-demand 3,000,000 not adjustable; cross-region and global 6,000,000
  adjustable) and **no requests-per-minute row**; Sonnet 4.6: RPM 5,000 on-demand (not adjustable) /
  10,000 cross-region and global, TPM 3M / 6M; Haiku 4.5: RPM 10,000 cross-region and global, TPM 5M.
  A per-account, per-Region "Cross-Model Max Tokens Per Day" quota also exists.
- `[V]` **AWS guidance**: "A quota is an upper bound, not a guarantee"; "Stop the ramp and return to the
  last stable request rate and concurrency level"; "Use token-aware client-side rate limiting, bounded
  concurrency, and bounded queues. An RPM-only limiter does not protect against changes in request
  size"; honor `Retry-After` when present, otherwise jittered exponential backoff from ~1 s inside a
  bounded retry budget (example: six total attempts). — `scaling-throughput-best-practices.html`.
- `[R]` Error surface: HTTP 429 carries `ThrottlingException` or `ModelNotReadyException`; 503
  `ServiceUnavailableException` is a shared-capacity condition, not quota; 529 `overloaded_error`
  (Messages-API path) may carry `Retry-After`; no `x-ratelimit-*`/`anthropic-ratelimit-*` headers are
  documented for Bedrock, and none for 429. A 429 can occur while within quota.
- `[R]` Admission reserves `input + max_tokens` against the quota and settles at
  `input + cache_write + output × burndown` (AWS blog); consistent with the burndown page's own
  "Understanding the impact of the max_tokens parameter" section but not re-fetched word-for-word.
- `[R]` Priority/Flex service tiers are not selectable for Sonnet 5 / Sonnet 4.6 / Haiku 4.5; the
  Reserved tier's exact Claude model coverage conflicted between claims — treat as `[O]`.
- `[O]` How LiteLLM/Portkey/Envoy/Kong handle Bedrock throttling specifically; any 2026 postmortem
  quantifying 429 reduction from adaptive concurrency.
- **Kelvran gap (confirmed `[C]` + `[V]`)**: the TPM limiter debits `TotalTokens` 1:1 with cache
  reads folded in. Against Bedrock's real quota that **under-counts output burn 5–10×** (Sonnet 5:
  10×; Sonnet 4.6/Haiku 4.5: 5×) and **over-counts cached input**. Per-key RPM buckets model a limit
  Bedrock does not apply to Sonnet 5 (harmless, but misleading). No `max_tokens` clamp is sent when the
  client omits it. Errors are classified by HTTP status, never by exception name; `Retry-After` is
  never read.

## 4. OWASP Top 10 for LLM Applications 2026 + agentic guardrails (run `w9zgkvzls` — 106 agents, 20 / 5)

- `[V]` The 2026 edition exists as authoritative markdown at
  `github.com/GenAI-Security-Project/GenAI-LLM-Top10/2026/final/` — LLM01 Prompt Injection, LLM02
  Sensitive Information Disclosure, LLM03 Excessive Agency, LLM04 Supply Chain, LLM05 Data and Model
  Poisoning, LLM06 Unbounded Consumption, LLM07 Misinformation, LLM08 Hidden Context Exposure, LLM09
  Vector and Embedding Weaknesses, LLM10 Improper Output Handling. The `genai.owasp.org/download/56857/`
  URL serves an HTML landing page to a plain client, so the PDF itself was **not** fetched here.
- `[R]` Published 2026-08-03/04 (the 2026-09-01/02 press release re-announced it); 122 pages; ranking
  weighted a ~29-respondent vote 3:1 against a 7,714-incident corpus; Excessive Agency rose 6→3.
- `[V]` **LLM01:2026** text contains: "no reliable prevention mechanism", "architectural rather than
  interceptive", three citations of Nasr et al. (adaptive attacks >90% success against 12 defenses),
  "deterministic policy engine", "Rule of Two", and the invisible-Unicode strip ranges including
  **U+E0000**–E007F and U+2060. **LLM03:2026** contains "Complete mediation" (×2), "policy decision
  point", "Rate limiting", "Monitor tool use".
- `[V]` The **Agent Control Standard (ACS)** repo exists (`GenAI-Security-Project/agent-control-standard`,
  created 2026-04-10, last push 2026-10-05). `[R]` It is a wire spec: pre-execution Intent Gate / PDP
  validating tool intent and arguments, per-tool least-privilege, a distinct `toolCallResult` hook,
  audited fail-open ("Every step that proceeds without a decision MUST be recorded as an audit
  event"), and a liveness `system/ping`. Its Core baseline changed on 2026-10-05.
- `[R]` **LLM02:2026** Tier 1 #5: "Sanitize with classifiers, not regex alone: pattern matching plus
  NER plus trained classifiers"; reasoning traces and tool arguments are first-class disclosure
  surfaces (Kelvran already scans both post-call).
- `[O]` AWS Bedrock Guardrails' 2026 agentic features (tool-call evaluation, `InvokeGuardrailChecks`);
  what LiteLLM/Portkey/Kong/OpenRouter/Cloudflare/Envoy shipped for tool-call guardrails in 2026.
- **Kelvran gaps relative to the real 2026 text**: (a) tag-block range starts at U+E0020, OWASP
  prescribes U+E0000 (trivial); (b) hidden Unicode is detected, not stripped at ingest/render (design
  choice; OWASP says strip); (c) guardrail detector fail-open is a log line, not a metric/audit event —
  inconsistent with the gateway's own rate-limit/budget fail-open counters and with ACS; (d) no
  per-tool policy decision point / allowlist / rate ceiling — content classification only (LLM03
  "complete mediation" and ACS Intent Gate are architectural, RFC-scale); (e) tool results re-enter
  the prompt as plain text with no provenance-aware policy (RFC-scale); (f) PII detection is
  regex-only (LLM02 asks for NER + classifiers — a real feature, not a fix).

## 5. Content-aware routing and cost cascades (run `w6id3k9q7` — 106 agents, 17 / 8)

- `[V]` **LiteLLM's own benchmark**: the rule-based complexity router's "area under the ROC curve (AUC)
  was: 0.524 across the full training set, which is close to random. 0.420 on chat prompts, which
  indicates mildly inverted predictions"; a cost-matched shuffled control was used; the whole
  auto-router is still labelled `[Beta]` and the embedding "Semantic Auto Router" is `(deprecated)`. —
  `docs.litellm.ai/docs/proxy/auto_routing_benchmark`.
- `[R]` OpenRouter's Auto Router (live for all users 2026-08-10) routes on a ~30-task-type classifier
  plus 7-day community spend share — a signal a self-hosted gateway cannot reproduce; its self-reported
  cost effect ranges from −64% to **+6.45×** by suite, measured against its own previous router; zero
  third-party reproductions found.
- `[R]` RouteLLM dormant since 2024-08 on an obsolete GPT-4/Mixtral pair; Kong semantic load-balancing
  and Arch-Router/Plano do domain/intent matching, explicitly not difficulty; Portkey conditional
  routing cannot inspect message content.
- `[R]` Per-request model switching rebuilds provider-side prompt caches (OpenRouter mitigates with
  session stickiness; one client measured 17.3% of prompt tokens billed uncached without it). Kelvran
  has no session→model pin today (`sticky.go` is a stable/canary split within one canonical model) —
  only relevant if model switching is ever introduced.
- `[O]` FrugalGPT-style cascades, provider logprob/confidence support, Not Diamond/Martian/Unify/vLLM
  Semantic Router status (three vLLM-SR claims refuted 0-3).
- **Verdict**: do **not** write a content-aware-routing RFC. No independently reproduced production
  result exists, and the one approach Kelvran could implement cheaply is disproven by its vendor's own
  benchmark. Revisit only with a shadow-mode measurement against an all-cheap baseline and a
  cost-matched random control on Kelvran's own evals harness.

---

## Ranked actionable items

1. **Bedrock-quota-aware TPM accounting** (correctness of an existing limiter against the real
   provider quota): exclude `CacheReadTokens` from the reconciled TPM figure; apply a per-model output
   burndown multiplier (configurable table — Sonnet 5 = 10, Sonnet 4.6 = 5, Haiku 4.5 = 5, Claude 4.8 =
   15, default 1 for non-Bedrock); optionally send an explicit per-deployment `max_tokens` clamp so
   unset requests stop reserving the model maximum `[R]`. Config guidance: no RPM bucket for Sonnet 5.
2. **Reasoning-token tracking** (the follow-up `94da263a` deferred): `adapter.Usage.ReasoningTokens`
   as a subset of `CompletionTokens`; Anthropic from `output_tokens_details.thinking_tokens`
   (non-stream and final `message_delta`); Bedrock via `additionalModelResponseFieldPaths:
   ["/usage/output_tokens_details"]` read from `additionalModelResponseFields` (Converse) and
   `messageStop.additionalModelResponseFields` (stream); emit
   `gen_ai.client.inference.usage.reasoning.output_tokens`; no separate price needed for Claude.
   OpenAI/Gemini only after their field paths are verified (Gemini is additional-not-subset `[R]`).
3. **Bedrock error classification by exception name + `Retry-After` capture**: map
   `ThrottlingException`/`ModelNotReadyException` (429), `ServiceUnavailableException` (503),
   `modelStreamErrorException` (424) and the 529 `overloaded_error` for fallback-chain error classes
   instead of bare status codes; log any `Retry-After`/`X-Amz-Retry-After` on 429/503/529 so the open
   question "does Bedrock ever send one on 429" gets answered by field data.
4. **Guardrail fail-open metric + Unicode range fix**: `kelvran.guardrail.fail_open` mirroring the
   rate-limit/budget counters (also closes the ACS "audited fail-open" expectation); widen the tag
   block to `{0xE0000, 0xE007F}`.
5. **Streaming fallback reason** in telemetry: distinguish abort / in-band exception / missing frame
   when `costEstimated=true` (observability only; the estimator itself stays as the fallback).
6. **Design-only RFC candidates (not scheduled)**: adaptive concurrency (AIMD) driven by 429/503
   signals per AWS's "stop the ramp" guidance; a tool-call policy decision point / per-tool allowlist
   (OWASP LLM03 complete mediation, ACS Intent Gate); a classifier/NER PII layer (LLM02).

## Explicitly not actionable

- Content/complexity-aware routing or cost cascades — negative verdict (section 5).
- Removing the streaming usage estimator — it is already fallback-only; every provider documents
  cases where no usage frame arrives.
- Bedrock Priority/Flex service tiers — not selectable for any of Kelvran's three models.

## Open questions carried forward

- Does Bedrock ever emit `Retry-After`/`X-Amz-Retry-After` on a 429 `ThrottlingException`? (Item 3's
  logging answers this.)
- Exact OpenAI Chat Completions reasoning-token path and Gemini per-chunk `usageMetadata` semantics —
  need empirical probes before any adapter change.
- Current Bedrock Guardrails agentic coverage and competitor tool-call guardrails (two research
  sub-questions produced no surviving claims).
- Reserved-tier Claude model coverage (conflicting claims).

## Method notes

- Web-search tooling was degraded for most verifier lanes (Exa rate-limited, DuckDuckGo captcha,
  Bing/Brave blocked), so contradiction sweeps leaned on GitHub source, SDK code, live API probes and
  official docs rather than broad web coverage — third-party critiques could exist unseen.
- Kelvran's pilot IAM user is `AccessDenied` for the Service Quotas API, so the quota numbers above are
  from the AWS documentation table, not the account's applied values.
