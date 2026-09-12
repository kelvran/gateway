# Research: Canonical-schema design for reasoning/thinking content across providers

**Date:** 2026-09-12
**Scope:** Fix Kelvran's confirmed silent-drop bug — Anthropic's and Bedrock's adapters currently drop `thinking`/`redacted_thinking` (Anthropic) and `reasoningContent` (Bedrock Converse) content blocks in `FromProvider`, which is real, live-breaking damage for Kelvran's flagship multi-turn agentic tool-calling workload. `adapter.Message` currently has no way to represent an ORDERED, INTERLEAVED sequence of reasoning/tool_use/text blocks, only flat fields. Already shipped and not re-litigated: `adapter.Message`'s existing flat `Content`/`ToolCalls`/`Refusal` fields; structured-output/`ResponseFormat` normalization; cache-token cost accounting.

**Note on recovery:** this file was reconstructed directly from the deep-research workflow's full structured JSON result (verified findings, refuted claims, sources, caveats). The subagent explicitly declined to author the report file itself under this session's operating constraints; the underlying research (112 agents, 29 sources, 131 claims extracted, 25 adversarially verified) completed normally.

## Executive Summary

Every reasoning-capable provider treats reasoning content as an OPAQUE, ORDER-SENSITIVE block that must be replayed byte-for-byte on subsequent turns. Anthropic/Bedrock's Claude Messages endpoint enforce this with a hard 400 — confirmed live-breaking for Kelvran today. OpenAI offers the same pattern voluntarily via an `encrypted_content` field for stateless/ZDR continuity. No production gateway (LiteLLM, Portkey, OpenRouter) abstracts this away — they document the requirement and/or normalize onto an extended, non-OpenAI-standard schema, but still make the caller responsible for verbatim replay. There IS a precedented "ordered heterogeneous block list" pattern Kelvran can adopt directly rather than invent: AWS Bedrock's own `ContentBlock` union already places `reasoningContent` as a peer to `text`/`toolUse`/`toolResult` inside an ordered array, and Vercel AI SDK's `UIMessage.parts` independently validates the same "ordered discriminated-union array" shape at the message layer. No evidence was found addressing guardrail-scanning or cache-key-fingerprinting interactions with reasoning content — that remains a genuine open design gap requiring Kelvran's own decision, not an industry precedent to copy.

## Findings

### Finding 1 — Adopt an ordered, opaque reasoning block as the canonical unit

**Confidence: high.** Sources: [OpenAI reasoning items cookbook](https://developers.openai.com/cookbook/examples/responses_api/reasoning_items), [OpenAI reasoning guide](https://developers.openai.com/api/docs/guides/reasoning).

OpenAI's Responses API models reasoning as a distinct `type: reasoning` output item (own id, never mixed into plain text/message content) whose raw chain-of-thought is NEVER exposed — only an optional summary. For stateless/Zero-Data-Retention use, OpenAI exposes an explicit `encrypted_content` field on that reasoning item which the client must pass back verbatim on later requests (as an alternative to server-side continuity via `previous_response_id`); the item remains opaque to the client even when persisted server-side. This is the strongest existing precedent for treating reasoning content as an OPAQUE BLOB with a client-replay contract rather than something the gateway parses/restructures.

**build_now** — a reasoning block should carry an opaque payload (text OR ciphertext, provider-defined) plus provider-issued metadata (id/signature), with NO cross-provider interpretation of its contents.

### Finding 2 — This is the exact live bug, not a future risk

**Confidence: high.** Sources: [Anthropic thinking troubleshooting](https://platform.claude.com/docs/en/build-with-claude/thinking-troubleshooting), [Anthropic thinking docs](https://platform.claude.com/docs/en/build-with-claude/thinking), [AWS Bedrock Claude Messages extended thinking](https://docs.aws.amazon.com/bedrock/latest/userguide/claude-messages-extended-thinking.html).

Anthropic's Messages API (and Bedrock's Anthropic-compatible Claude Messages endpoint) is stateless and enforces that every consecutive `thinking`/`redacted_thinking` block from the immediately-preceding assistant turn be echoed back byte-for-byte, in original order, with no rearranging/dropping/editing, on any subsequent request carrying a tool result — violation returns a hard 400 `invalid_request_error`, explicitly triggered by client code that filters content blocks by type (the exact failure mode `adapter.Message`'s flat-field design causes). Additionally, some model tiers cannot disable thinking at all — `thinking: {type: disabled}` itself is rejected with 400 — so there is no opt-out escape hatch for those models.

**build_now** — `adapter.Message` needs an ORDERED sequence type that preserves the exact consecutive-thinking-block run from `FromProvider`, unmodified, for replay in `ToProvider` on any tool-result-bearing turn.

### Finding 3 — Bedrock's native Converse API enforcement strictness is unsettled

**Confidence: medium.** Source: [AWS Bedrock conversation inference](https://docs.aws.amazon.com/bedrock/latest/userguide/conversation-inference.html).

The analogous requirement on Bedrock's NATIVE Converse API (as opposed to its Anthropic-compatible Claude Messages endpoint) is less settled: a broader claim that Converse imposes an "identical" hard round-trip contract (signature + every prior message replayed unmodified or the API errors) was refuted (0-3). Converse's `ReasoningContentBlock` schema itself is confirmed (see Finding 5), but the exact enforcement strictness on that specific API surface was not verified by this research pass.

**not_yet** — trigger: needs its own follow-up scoping pass, ideally direct empirical testing against a live Bedrock Converse call with a deliberately-dropped reasoning block. Kelvran's Bedrock adapter work should distinguish the two Bedrock API surfaces (Claude Messages API vs. Converse API) rather than assuming identical enforcement.

### Finding 4 — No production gateway abstracts the replay burden away

**Confidence: high.** Sources: [LiteLLM reasoning_content docs](https://docs.litellm.ai/docs/reasoning_content), [Portkey thinking-mode docs](https://docs.portkey.ai/docs/product/ai-gateway/multimodal-capabilities/thinking-mode), [OpenRouter tool-calling docs](https://openrouter.ai/docs/guides/features/tool-calling).

All either document the replay burden as the caller's responsibility or pass it through in an extended schema. LiteLLM's own docs state the Messages API is stateless and require the caller to resend `thinking_blocks`, explicitly warning that generic OpenAI-compatible clients (LibreChat, Open WebUI, Vercel AI SDK) ignore that field and thereby break multi-turn thinking+tool-calling — a real, named, currently-documented compatibility failure in the wild. Portkey does not pass reasoning through transparently either: it normalizes onto an extended, non-standard schema layered on the OpenAI completion shape, and still requires the caller to manually replay the entire prior thinking block — including an opaque provider-issued `signature` — verbatim in message history. OpenRouter documents a named "Interleaved Thinking" feature as a pass-through of the underlying provider capability, not a gateway-invented abstraction.

**build_now (as design guidance, not a code change)** — there is no "clever" existing normalization to copy that hides the replay contract from callers. The state of the art across LiteLLM/Portkey/OpenRouter is "store the opaque block, force the caller/gateway internals to replay it verbatim." Kelvran's canonical schema should follow the same minimal-abstraction philosophy rather than trying to invent a cross-provider-interpretable reasoning representation.

### Finding 5 — A precedented "ordered heterogeneous block list" schema exists

**Confidence: high.** Sources: [AWS Bedrock conversation inference](https://docs.aws.amazon.com/bedrock/latest/userguide/conversation-inference.html), [AWS Bedrock Claude Messages extended thinking](https://docs.aws.amazon.com/bedrock/latest/userguide/claude-messages-extended-thinking.html), [Vercel AI SDK UIMessage](https://ai-sdk.dev/docs/reference/ai-sdk-core/ui-message).

AWS Bedrock's Converse API already defines a canonical `ReasoningContentBlock` (with `reasoningText{text, signature}` for plaintext-with-signature and `redactedContent` for provider-encrypted/base64 reasoning) placed as a PEER content-block type alongside `text`, `toolUse`, and `toolResult` within a `Message.content` array — a real provider-defined union type Kelvran's own Bedrock adapter must already map onto. Separately, Claude 4-class models' "interleaved thinking" feature requires MULTIPLE thinking blocks interspersed between multiple tool calls within a single assistant turn, which structurally rules out a single flat reasoning field. Independently, Vercel AI SDK's `UIMessage.parts` (a message-layer, not wire-format, precedent) is a single ordered array typed as a discriminated union covering text/reasoning/tool-call-result/file/source/data/step-start block types — corroborating that "ordered array of typed blocks, reasoning as one variant among peers" is an established pattern, not a novel invention.

**build_now** — concrete recommendation: replace/extend `adapter.Message`'s flat `Content`/`ToolCalls`/`Refusal` fields with an ordered `Blocks []ContentBlock` (discriminated union: `text` | `tool_use` | `tool_result` | `reasoning{opaque payload, signature, redacted bool}`), directly modeled on Bedrock's own `ContentBlock` union (which Kelvran's Bedrock adapter must handle anyway) and structurally compatible with Anthropic's content-array shape.

### Finding 6 — Guardrail-scanning and cache-key-fingerprinting interaction is a genuine open gap

**Confidence: low.** No sources survived — absence of evidence, not evidence of absence.

No primary source among LiteLLM, Portkey, OpenRouter, OpenAI, Anthropic, or AWS docs addresses whether reasoning content should be guardrail-scanned the same as visible text, or how two requests differing only in accumulated reasoning-block history should be treated for cache-key fingerprinting. Across 24 verified/attempted claims spanning 6 primary-source domains, none touched either sub-question.

**not_yet** — trigger: requires Kelvran's own design/threat-model spike once the block schema is drafted. Recommendation pending that spike: treat opaque/encrypted reasoning blocks as NOT scannable (ciphertext, by definition, since providers explicitly withhold raw CoT) while plaintext `reasoningText`/`thinking` blocks likely should be scanned identically to visible text since they are provider-returned plaintext. For caching, AGENTS.md's "never weaken the cache hard-gate" rule implies two requests with different reasoning-block history are NOT cache-equivalent even if visible text matches, since the replayed reasoning content is causally read by the model and can change output — but this is inference, not a sourced finding.

### Finding 7 — OpenAI's tool-calling-turn reasoning-replay strictness is unresolved

**Confidence: low.** Sources: [OpenAI reasoning items cookbook](https://developers.openai.com/cookbook/examples/responses_api/reasoning_items), [OpenAI reasoning guide](https://developers.openai.com/api/docs/guides/reasoning).

Whether OpenAI's Responses API imposes a HARD requirement (analogous to Anthropic's 400) to replay reasoning items back on tool-calling turns, versus a SOFT degradation (silently discards stale items, costs ~3% quality, breaks cache), is NOT settled by this research: three related claims on this exact question were explicitly refuted at 1-2 or 0-3 votes.

**not_yet** — trigger: revisit once Kelvran adds first-class OpenAI o-series/gpt-5-class reasoning support, or when better primary-source excerpts become available. Kelvran should not assume OpenAI-side severity parity with Anthropic when prioritizing the fix, even though the same opaque-block schema (Finding 1) should still be used for both providers going forward.

## Caveats

1. **File not written by the subagent** — this session's operating constraints direct subagents not to author report/summary/findings markdown files; this file was persisted by the orchestrating agent from the structured research output.
2. Several LiteLLM code-level implementation claims (PR #22448 `encrypted_content` round-trip, PR #28258 `thinking_blocks`→`reasoning_content` concatenation) were refuted (0-3) — do not treat LiteLLM's exact internal code mechanics as verified; only the general documented behavior on docs.litellm.ai is confirmed.
3. The claim that LiteLLM normalizes ALL providers onto a single standardized `reasoning_content` string field was also refuted (1-2) — treat LiteLLM's actual field-naming/normalization scheme as unconfirmed.
4. Bedrock has two distinct API surfaces (the Anthropic-compatible "Claude Messages API" and the native "Converse API") with different confirmed-strength evidence — the hard-400/exact-replay contract is confirmed for the Claude Messages endpoint; a stronger claim that Converse enforces an identical contract was refuted. Don't conflate the two when scoping the Bedrock adapter fix.
5. OpenAI's tool-calling-turn reasoning-replay strictness (hard vs. soft failure) is unresolved by this pass — three specific claims on it were refuted; this is a real gap in the research, not a settled fact in either direction.
6. All sourcing here is provider/gateway official documentation (primary sources) fetched live around 2026-09-12 — no independent third-party or academic corroboration was sought beyond that, and Exa-based cross-checks were repeatedly rate-limited during verification, so some findings rest on primary docs alone.
7. Guardrail-scanning and cache-key-fingerprinting interaction with reasoning content (research question 4) has zero supporting or refuting evidence in this claim set — treat that section as a design gap requiring Kelvran's own decision, not a researched-and-answered question.

## Open Questions

- Should Kelvran's canonical reasoning block distinguish plaintext (Anthropic `thinking`, Bedrock `reasoningText`) from ciphertext (Anthropic `redacted_thinking`, Bedrock `redactedContent`, OpenAI `encrypted_content`) at the type level, or use one variant with an `is_opaque`/`is_redacted` flag — and does that distinction change the guardrail-scanning answer (scan plaintext, never attempt to scan/decrypt ciphertext)?
- What is OpenAI's ACTUAL failure mode (hard error vs. soft quality/cache degradation) when a reasoning item is omitted from a tool-calling turn on the Responses API — this research could not settle it and needs either better primary-source excerpts or direct empirical testing against a live o-series/gpt-5-class model.
- Does Kelvran's cache-key fingerprint need to include a hash of the full accumulated reasoning-block history (not just visible text), and if so, does that materially raise cache-miss rates for agentic tool-calling workloads in a way that conflicts with cost/latency goals in PRD.md?
- For Bedrock specifically: does the native Converse API's `reasoningContent` block carry the SAME hard-400-on-mismatch enforcement as the Claude Messages-compatible endpoint, or something looser — this needs direct empirical verification against a live Bedrock Converse call with a deliberately-dropped reasoning block.

## Refuted Claims (excluded from findings above)

- OpenAI requires round-tripping the reasoning item back via `previous_response_id` or re-inclusion when a tool/function call is involved, unlike optional plain multi-turn chat (1-2).
- OpenAI's failure mode for omitted reasoning items is soft degradation (silent discard, ~3% quality cost, breaks prompt-cache) rather than a hard error (1-2).
- OpenAI's guidance requires passing back all reasoning/function-call/function-call-output items since the last user message, untouched (0-3).
- LiteLLM normalizes all providers onto two standardized fields (`reasoning_content` string + Anthropic-only `thinking_blocks`) (1-2).
- LiteLLM PR #22448's concrete `encrypted_content`↔`thinking_blocks` round-trip mechanism, as a real shipped example (0-3).
- LiteLLM auto-injecting the `include: reasoning.encrypted_content` parameter when encrypted items are detected (0-3).
- PR #22448 closed unmerged, reattempted across 5 follow-up PRs, indicating this remains contested even in the gateway most likely to have solved it (0-3).
- LiteLLM's pre-fix Anthropic-to-OpenAI adapter never propagated `thinking_blocks` to `reasoning_content`, breaking multi-turn replay (0-3).
- PR #28258's fix concatenates all `thinking`-type block text into one `reasoning_content` string, excluding `redacted_thinking` (1-2).
- OpenRouter's interleaved-thinking worked example is Anthropic-specific, not a universal cross-provider mechanic (0-3).
- AWS Bedrock Converse imposes an identical hard round-trip contract to Anthropic's direct API (0-3) — see Finding 3's caveat.

## Sources Consulted

29 sources fetched across 6 search angles (OpenAI reasoning-items round-trip; gateway passthrough practice; Anthropic/Bedrock thinking contract; ordered-block schema precedent; guardrail/cache interaction effects; opaque-reasoning tradeoffs, skeptical angle); 131 claims extracted, 25 adversarially verified (14 confirmed, 11 refuted, 0 unverified).
