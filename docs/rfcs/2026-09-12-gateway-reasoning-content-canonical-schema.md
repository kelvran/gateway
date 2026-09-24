- **Status**: accepted
- **Date**: 2026-09-12
- **Author(s)**: gateway team (via research + code-scoping pass)

## Summary

Add an ordered, opaque `ReasoningBlocks []ReasoningBlock` field to the canonical `adapter.Message` type, so extended-thinking/reasoning content from Anthropic and Bedrock is captured on the response path (`FromProvider`) and correctly replayed, unmodified and in original order relative to `ToolCalls`, on the request path (`ToProvider`). This closes a confirmed live-breaking bug: Anthropic's (and Bedrock's Anthropic-compatible Claude Messages) API returns a hard `400 invalid_request_error` when a prior turn's `thinking`/`redacted_thinking` blocks aren't echoed back byte-for-byte on any subsequent turn containing a tool result, and some model tiers cannot disable thinking at all.

## Motivation

`docs/upgrade-research/gateway-reasoning-content-canonical-schema-2026-09-12.md` confirms (high confidence, primary Anthropic + AWS docs) that Anthropic's Messages API — and Bedrock's Anthropic-compatible Claude Messages endpoint — is stateless and enforces that every consecutive `thinking`/`redacted_thinking` block from the immediately-preceding assistant turn be echoed back complete and unmodified, in original order, on any subsequent request carrying a tool result. Violation returns a hard 400, explicitly triggered by client code that filters content blocks by type — which is exactly what Kelvran's adapters do today.

A follow-up code-scoping pass (read-only, no fixes applied) confirmed the damage is total, not partial:

- **Anthropic** (`gateway/internal/adapter/anthropic/anthropic.go:425-440`): `FromProvider`'s block loop switches on `case "text"` / `case "tool_use"` only — a `thinking`/`redacted_thinking` block matches neither case and the entire block is silently skipped. No `Thinking`/`Signature`/`Data` field exists on `ContentBlock` (lines 145-171) to have captured it in the first place. Streaming (`anthropic/stream.go:207-238,240-262`) misclassifies a thinking block's `content_block_start` as `blockKindText` (no thinking kind exists in the `blockKind` enum), then emits an empty-content chunk for its deltas since `rawContentBlockDelta` has no field for the real `thinking`/`signature` delta payload keys.
- **Bedrock** (`gateway/internal/adapter/bedrock/bedrock.go:555-570`): identical total drop — `ContentBlock` (lines 54-69) has no `ReasoningContent` field, so a reasoning block matches neither the `ToolUse` nor `Text` case and is fully dropped. Streaming (`bedrock/stream.go:183-221`) treats a reasoning block's start/delta events identically to an unrecognized block, emitting zero/empty chunks.
- **Gemini** (`gateway/internal/adapter/gemini/gemini.go:370-385`): a different, subtler bug — a "thought" part's content still rides on the same JSON key (`text`) that `Part.Text` already captures, so thought content is **silently merged into ordinary answer text**, indistinguishable from a real answer, rather than dropped. Same in streaming (`gemini/stream.go:89-105`).
- **OpenAI / openaicompat**: genuinely flat at the wire level (`content` and `tool_calls` are separate sibling JSON keys, not a unified block union) — no thinking/reasoning surface exists via the Chat Completions API shape these adapters target. `openaicompat.go:23`'s own doc comment already names llama.cpp's `reasoning_content` field as a known, documented gap.

This is real, live damage to Kelvran's own flagship multi-turn agentic tool-calling workload (per `PRD.md`) for any Anthropic/Bedrock model with thinking enabled — including "Always on" tiers that cannot disable it. It was deliberately not fixed inline during the Round 5 backlog audit because it needs a genuine canonical-schema decision, which this RFC makes.

## Detailed Design

**Affected component:** Gateway only (`gateway/internal/adapter/`). No change to the Evals↔Gateway contract or `api/`.

### Canonical schema addition (`gateway/internal/adapter/types.go`)

```go
// ReasoningBlocks holds this assistant turn's ordered chain-of-thought/
// extended-thinking blocks, per this RFC. Each block is OPAQUE -- Kelvran
// never interprets, restructures, or generates its content -- and
// callers MUST echo the exact slice (unmodified, in order) back on any
// subsequent ChatRequest.Messages entry carrying this same assistant
// turn, or Anthropic/Bedrock's Claude Messages API returns a hard 400.
// Position relative to ToolCalls is preserved via each block's own
// Sequence field, since Claude 4's interleaved-thinking mode can place
// multiple reasoning blocks between multiple tool calls within one turn.
// Empty (the default, and every Message built before this field
// existed) is a silent no-op for every adapter, matching Parts/
// CacheControl's own convention.
ReasoningBlocks []ReasoningBlock `json:"reasoning_blocks,omitempty"`
```

```go
// ReasoningBlock is one opaque reasoning/thinking block, per this RFC.
type ReasoningBlock struct {
	// Sequence is this block's position within the turn, using the same
	// index space as ToolCalls: a value of N means "immediately before
	// ToolCalls[N]" (N >= len(ToolCalls) means "after all tool calls").
	// Required to reconstruct exact original block order for Claude 4's
	// interleaved-thinking mode.
	Sequence int `json:"sequence"`
	// Redacted marks this block as provider-encrypted/opaque ciphertext
	// (Anthropic redacted_thinking, Bedrock redactedContent) -- Kelvran
	// MUST NOT attempt to interpret, scan, log, or fingerprint its
	// contents when true; treat Data as an inert byte string.
	Redacted bool `json:"redacted,omitempty"`
	// Text is the plaintext reasoning content when Redacted is false
	// (Anthropic thinking.thinking, Bedrock reasoningText.text). Empty
	// when Redacted is true.
	Text string `json:"text,omitempty"`
	// Data is the opaque ciphertext payload when Redacted is true
	// (Anthropic redacted_thinking.data, Bedrock redactedContent).
	Data string `json:"data,omitempty"`
	// Signature is the provider-issued cryptographic signature
	// authenticating Text's plaintext (Anthropic thinking.signature,
	// Bedrock reasoningText.signature) -- opaque, must be replayed
	// verbatim alongside Text.
	Signature string `json:"signature,omitempty"`
}
```

This follows the codebase's own established additive-schema convention (`Parts`, `CacheControl`, `Refusal` — see their doc comments in `types.go`) rather than the more invasive "replace `Content`/`ToolCalls` with a single ordered `Blocks []ContentBlock` union" pattern the research found as the general industry precedent (AWS Bedrock's own `ContentBlock` union; Vercel AI SDK's `UIMessage.parts`). The union-type rewrite is a bigger, breaking change to every adapter and every call site that reads `Message.Content`/`Message.ToolCalls` directly (guardrail scanning, cache-key fingerprinting, cost accounting, telemetry); the additive `ReasoningBlocks` field with a `Sequence` index achieves the same losslessness — exact block order is fully recoverable by merging `ReasoningBlocks` (by `Sequence`) with `ToolCalls` (by index) — at zero risk to every existing call site, matching this project's own stated preference for additive fields over restructuring existing ones (`types.go:29-31`'s own `Parts` doc comment states this explicitly as the reason `Parts` was added rather than changing `Content`'s type).

### Phasing

This RFC scopes the full cross-adapter design; implementation ships in phases, one provider/concern per commit, matching this project's established rhythm:

1. **Phase 1 (this RFC's first implementation commit)** — Anthropic direct, non-streaming: `types.go` schema addition + `anthropic.go`'s `FromProvider` (capture) and `ToProvider` (replay in correct interleaved order). This is the confirmed, hard-400, live-breaking case — highest value, smallest bounded diff.
2. **Phase 2** — Anthropic streaming (`anthropic/stream.go`): add a `blockKindThinking` kind, a `Delta.Thinking`/`Delta.Signature` field on `rawContentBlockDelta`, and correct dispatch in `decodeContentBlockStart`/`decodeContentBlockDelta`.
3. **Phase 3** — Bedrock (`bedrock.go` + `bedrock/stream.go`), both paths: same `ReasoningBlock` capture/replay, using Bedrock's `reasoningContent` block shape (`reasoningText{text,signature}` / `redactedContent`). Bedrock's native Converse API enforcement strictness is unconfirmed by research (see Unresolved Questions) — capture losslessly regardless of whether Converse enforces the same hard 400 as the Claude Messages-compatible endpoint.
4. **Phase 4** — Gemini (`gemini.go` + `gemini/stream.go`): fix the silent-merge bug by adding a `Thought bool` discriminator to the native `Part` type (mirroring Gemini's real `thought`/`thoughtSignature` fields) and routing thought parts into `ReasoningBlocks` instead of `textParts`. Distinct bug class from Anthropic/Bedrock (misclassification, not drop) but same root cause (no canonical field existed).
5. **Phase 5 — OpenAI: still deferred; openaicompat: shipped 2026-09-13.** OpenAI's Chat Completions API was reconfirmed (2026-09-13, against OpenAI's own current API reference) to have no reasoning/thinking field in the response body at all — only `usage.completion_tokens_details.reasoning_tokens` as a plain count, and encrypted reasoning items/summaries exist only under the Responses API, which `openai.go` does not target. Nothing to build there; the deferral stands. openaicompat's own gap turned out larger and more fragmented than this RFC originally assumed: field-name verification against each target runtime's live source (not docs) found llama.cpp emits `reasoning_content`, but vLLM has since **renamed** its own field away from `reasoning_content` to `reasoning` (accepting the old name only as a request-side backward-compat alias, never emitting it), Ollama's OpenAI-compat layer uses `reasoning`, and TGI has no reasoning field of any kind. Shipped: `openaicompat.go`/`stream.go` now capture/replay under both live wire names (`Message.Reasoning`/`Message.ReasoningContent`, and the streaming equivalents), a single flat `ReasoningBlock` at `Sequence: 0` (no interleaving signal exists on this wire shape), Redacted blocks never written to either field (no self-hosted runtime here produces or accepts ciphertext reasoning).

### Interaction with guardrail scanning and cache-key fingerprinting

Research question 4 found zero industry precedent either direction — flagged as a genuine open design gap, not an unresearched oversight. This RFC makes the call explicitly rather than leaving it silently unresolved:

- **Guardrail scanning**: plaintext blocks (`Redacted == false`) are scanned identically to visible text — they are provider-returned plaintext, no different in kind from `Content`. Redacted blocks (`Redacted == true`) are never scanned — `Data` is ciphertext by definition (the provider explicitly withholds raw chain-of-thought), and attempting to scan it would be meaningless at best.
- **Cache-key fingerprinting**: per `AGENTS.md`'s "never weaken the cache hard-gate" rule, two requests differing only in accumulated `ReasoningBlocks` history are NOT cache-equivalent even when visible `Content` matches — replayed reasoning content is causally read by the model and can change output. Kelvran's cache-key fingerprint must include a hash of the full `ReasoningBlocks` sequence, not just `Content`.

## Drawbacks

- Widens `adapter.Message`, Kelvran's own client-facing wire format — every existing integration test/golden fixture that constructs a `Message` literal continues to work unchanged (new field, zero value is a no-op), but any snapshot/golden-JSON fixture asserting an exact marshaled shape for an Anthropic/Bedrock response that legitimately contains thinking blocks will need updating once Phase 1 ships (previously-silently-dropped content will now appear).
- `Sequence`-based interleaving reconstruction is more complex to reason about than a single flat field, and is untested against a real multi-block-interleaved-with-multiple-tool-calls turn until Phase 1's tests are written against Anthropic's real documented shape (no live traffic exists yet to validate against).
- Cache-key fingerprinting change (hashing `ReasoningBlocks`) will reduce cache hit rate for any conversation history that includes reasoning turns, since two otherwise-identical follow-up requests with different upstream-generated reasoning content will no longer collide. This is the correct, safe direction per `AGENTS.md`'s hard-gate rule, but is a real, if small, cost — not costed against real traffic since none exists yet.
- Bedrock native Converse API enforcement strictness is unconfirmed (see Unresolved Questions) — Phase 3 ships the capture/replay regardless, on the reasoning that lossless capture is strictly safer than the current total-drop status quo even if Converse's own enforcement turns out to be softer than Claude Messages'.

## Alternatives Considered

- **Full `Blocks []ContentBlock` union, replacing `Content`/`ToolCalls`.** This is the general industry pattern (AWS Bedrock's own `ContentBlock`, Vercel AI SDK's `UIMessage.parts`) and was seriously considered. Rejected for v1 because it is a breaking change to every adapter and every call site reading `Content`/`ToolCalls` directly, versus the additive `ReasoningBlocks` design which achieves the same losslessness (full order recoverable via `Sequence`) at zero risk to existing code. May be revisited if a future content type also needs ordered interleaving with `ToolCalls` (this RFC doesn't foreclose it — `ReasoningBlocks` and a hypothetical future union type aren't mutually exclusive).
- **Do nothing, treat as `not_yet`.** Rejected: this is a confirmed, live, hard-breaking bug for Kelvran's own flagship multi-turn agentic workload, not a speculative improvement — the "never build ahead of a named trigger" discipline this project otherwise correctly applies to the rest of the backlog does not apply here, because the trigger (real thinking-enabled traffic against Anthropic/Bedrock) has already fired structurally: any "Always on" thinking-tier model returns thinking blocks unconditionally, regardless of whether Kelvran has real production traffic yet.
- **A single opaque `ReasoningPayload string` field with no `Sequence`/ordering.** Rejected: fails Claude 4's interleaved-thinking requirement (multiple reasoning blocks between multiple tool calls in one turn) — a flat single-string field cannot represent that shape at all, and this project's own regression-corpus discipline requires modeling the real, documented provider contract, not a simplified approximation of it.

## Unresolved Questions

- Bedrock's native Converse API enforcement strictness (hard 400 vs. something looser) is unconfirmed — the broader claim that it's identical to the Claude Messages endpoint's contract was adversarially refuted. Phase 3 should include a direct empirical check against a live Bedrock Converse call with a deliberately-dropped reasoning block, if/when Bedrock credentials for live testing are available.
- OpenAI's Responses API reasoning-item replay strictness (hard vs. soft failure on omission) is unresolved by research — revisit if/when Kelvran adds first-class Responses API / o-series support.

## Addendum (2026-09-24): preserved-thinking prefix-binding integrity check

**Status**: implemented (request-side gate + response-side surfacing; see Unresolved Questions below for the one deliberately deferred piece).

### The risk this addendum closes

Anthropic added a "preserved thinking" integrity check, live-verified against `platform.claude.com/docs/en/build-with-claude/preserved-thinking` (2026-09-24): starting with Claude Fable 5.1 (and Claude Opus 5.5), the API checks every `thinking`/`redacted_thinking` block's `signature` for two things — which model produced it, and whether "everything sent before it" (the top-level `system` prompt, `tools`, and every message before the block) is byte-identical to what it was signed under. A mismatch on the second check makes that block and every later thinking block invalid; the API either rejects the request with a 400 or drops the invalid blocks, depending on `thinking.block_binding.prefix_mismatch_behavior`. **The API enforces the prefix check by default (`"error"`) for accounts created on or after 2026-08-31.**

This RFC's own body (the "Motivation" section above) already established that `reasoningBlockToProvider`/`reasoningBlocksToProvider` in `anthropic.go` replay `thinking`/`redacted_thinking` blocks verbatim with their original `Signature` on later turns — exactly the pattern this new check validates against. It was silent on this specific failure mode: it covers the omitted-block 400 (a block dropped entirely), not a block whose *prefix* has changed underneath it.

A dedicated reachability check (same day, per `docs/upgrade-research/upstream-provider-api-changes-2026-09-24.md` Finding 4) confirmed this is real and reachable *today*, through no fault of the caller: Kelvran's own prompt management (`docs/rfcs/2026-09-13-gateway-prompt-management.md`) is global and live-mutable — `resolvePromptIfSet` (`gateway/internal/gateway/dataplane/dataplane.go:1830`) re-resolves `PromptID`/`PromptVersion`/`PromptLabel`+`PromptVariables` and replaces `req.Messages` server-side on **every** call. If an admin live-mutates or version-bumps a prompt template between turn N (where a thinking block gets signed under the old resolved prefix) and turn N+1 (where the caller replays that same thinking block), Kelvran forwards a now-stale-signed thinking block alongside a different, freshly-resolved prefix — triggering Anthropic's strict-default 400 for a caller who did nothing wrong.

### Decision: default to Anthropic's non-strict mode

Kelvran has no mechanism to guarantee prefix stability across turns given its own live-mutable global prompt design, so inheriting Anthropic's strict default would let Kelvran's own admin surface silently break in-flight reasoning conversations with an opaque 400. `ChatRequest.ThinkingBindingMode` (`gateway/internal/adapter/types.go`) therefore defaults (empty string, and every `ChatRequest` built before this field existed) to Anthropic's own **non-strict** mode (`"drop_block"`) — the opposite of Anthropic's account-level default. An explicit `"strict"` value opts a caller into Anthropic's own default (`"error"`) instead, for callers who want a hard reasoning-continuity guarantee.

### Wire shape (confirmed, not guessed)

Live-fetched from Anthropic's current docs (2026-09-24), not assumed from the prior research pass, which had not confirmed the exact field/header names:

- Beta header: `thinking-binding-controls-2026-08-01` (`anthropic-beta`). Required for both the request-side field below and the response-side array to have any effect.
- Request: `thinking.block_binding.prefix_mismatch_behavior` — nests **inside** `thinking`, alongside `thinking.type`. Values `"error"` (Anthropic's default) or `"drop_block"`.
- Response: a top-level `input_transformations` array, present only when the beta header was sent. Each entry: `{"type": "thinking_dropped" | "thinking_mismatch_allowed", "path": "messages.<n>.content.<n>", "reason": "prefix_binding_mismatch" | "model_binding_mismatch"}`.

### A narrower model gate than originally scoped, discovered during implementation

The original "concrete next step" assumed a flat `ThinkingBindingMode` → beta header + field mapping, independent of model. Implementation research surfaced a real constraint that scoping missed: `block_binding` is only ever documented alongside `thinking.type: "adaptive"` or `thinking.type: "enabled"` — Kelvran does not otherwise configure `thinking` at all today (no budget/type control exists anywhere in the adapter). Sending `thinking.type: "adaptive"` unconditionally to every Anthropic model would be a real, live regression: a model that supports only manual "enabled" thinking rejects `"adaptive"` with a 400. Anthropic's own docs name exactly two models running the prefix check at all today: Claude Fable 5.1 and Claude Opus 5.5. `anthropic.go` therefore gates `Thinking` population on a `thinkingBlockBindingModelSubstrings` allowlist (mirroring `internal/adapter/capabilities.go`'s existing `anthropicForcedToolChoiceUnsupportedModelSubstrings` pattern, kept local to the `anthropic` package rather than promoted to `capabilities.go`) — every other model gets a `nil` Thinking, byte-identical to pre-existing behavior. This list carries the same known maintenance burden as its `capabilities.go` sibling (already visible in Finding 1 of the same research doc) and will need updating as Anthropic ships more models with this capability.

### Transport wiring

`setUpstreamAuthHeaders` (`gateway/internal/gateway/dataplane/dataplane.go`) gained a `providerReq any` parameter — the same provider-native value every call site already marshals into `body`, now also passed through unmarshaled so the `"anthropic"` case can type-assert it to `*anthropic.Request` and read `Thinking` directly, via the new exported `anthropic.ThinkingBindingBetaHeaderValue`. This avoided widening the `UpstreamCaller`/`UpstreamStreamCaller` function-type signatures (used across many call sites and test fakes) and avoided a context-value indirection: `providerReq` was already in scope at every real call site inside `NewHTTPUpstreamCaller`, `NewHTTPEmbeddingUpstreamCaller`, and `NewHTTPUpstreamStreamCaller` — including the streaming path, so this fix covers streaming and non-streaming identically despite the implementing session's own file-scope restriction excluding `streaming.go`.

### Response-side surfacing

`ChatResponse.InputTransformations []InputTransformation` (canonical, in `types.go`) surfaces Anthropic's `input_transformations` array, mirroring how `Refusal` is surfaced today — Anthropic-only, nil for every other adapter and for Anthropic responses from a non-gated model. This was NOT scoped as a fallback-to-telemetry-only feature: the beta header/field name and the exact response shape were independently confirmed live against Anthropic's current documentation during implementation (not assumed from the prior research pass, which had explicitly flagged this as unconfirmed), so the full field is wired rather than only an OTel span attribute.

### Cache-key fingerprinting (closed same day, post-review)

An independent review pass caught a real omission in this addendum's own initial implementation: this RFC's own original body, two sections above ("Interaction with guardrail scanning and cache-key fingerprinting"), already establishes the general rule -- "two requests differing only in accumulated `ReasoningBlocks` history are NOT cache-equivalent... Kelvran's cache-key fingerprint must include a hash." `ThinkingBindingMode` governs how a replayed `ReasoningBlocks`/thinking block is handled on a given call, so the identical rule applies to it, but the first implementation pass folded `ThinkingBindingMode` into neither `cache.Key`/`cache.NormalizedKey` (L1/L2, `gateway/internal/cache/key.go`) nor `checkLexicalCache`'s hard-gate set (L3-lite, `gateway/internal/gateway/dataplane/dataplane.go`). A caller who set `"strict"` specifically to get a hard 400 on a stale-signed thinking block could silently be served an earlier, same-content request's response cached under Kelvran's non-strict default (or vice versa) -- defeating that opt-in's entire purpose, and doing so through every cache layer (L1, L2, and L3), not just one.

Fixed: `Key`/`NormalizedKey` now take a `thinkingBindingMode` parameter, folded into the hash exactly like every other conditional field on those functions (`TestKeyDiffersOnThinkingBindingMode`/`TestNormalizedKeyDiffersOnThinkingBindingMode`, `gateway/internal/cache/key_test.go`); `checkLexicalCache` gained an exact-match gate on `LexicalCandidate.ThinkingBindingMode` alongside its existing `ReasoningBlocksFingerprint` gate (`TestCheckLexicalCacheNeverServesAcrossDifferentThinkingBindingMode`); and a full-pipeline regression
(`TestHandleChatCompletionNeverServesAcrossDifferentThinkingBindingMode`) proves two byte-identical requests differing only in `ThinkingBindingMode` now produce two real upstream calls, never a cross-mode cache hit, at every layer.

### Unresolved Questions (addendum)

- **Highest-priority open item**: the entire wire-shape confirmation above (the beta header name, `thinking.block_binding.prefix_mismatch_behavior`'s nesting, and the `input_transformations` response shape) rests on a single live `WebFetch` call made from inside this agent's own sandboxed session, fetched and trusted within the same session that wrote the code depending on it. This workspace has a documented, recurring pattern of sandboxed `WebFetch`/AWS-MCP calls returning byte-identical or fabricated-looking content across otherwise-independent tool paths, including fabricated-looking model names — see this project's own prior session notes on this exact risk class. Model names this feature depends on (Claude Fable 5.1, Claude Opus 5.5, and the `2026-08-31`/`2026-08-01` dates above) are all past this assistant's training cutoff and could not be independently corroborated from training data either. Before this feature carries real production traffic that depends on it (i.e. before any caller sets `ThinkingBindingMode: "strict"` and relies on getting a real 400 instead of a silent drop), an operator should independently re-verify the exact header name and response shape against Anthropic's own current documentation from a non-sandboxed browser or a live test call — not just re-trust this same session's own fetch.
- Whether Bedrock's Anthropic-family Claude models (Converse API) run the same preserved-thinking prefix check is unverified — `thinkingBlockBindingModelSubstrings` intentionally stays local to the `anthropic` package rather than promoted to `capabilities.go`/wired into `bedrock.go` until this is confirmed, per this codebase's own "verify-then-fix, not blind-wire" convention.
- The live-prompt-mutation reachability scenario itself (an admin actually mutating a prompt mid-conversation, observed end-to-end against a real Anthropic account) is proven at the unit/wiring level (the beta header and non-strict field reach the real outgoing request by default) but not against a live Anthropic account with a genuinely new-tier model — no such account/credentials were available during implementation.
- The distinction between `"thinking_dropped"` and `"thinking_mismatch_allowed"` entries in `input_transformations` is surfaced verbatim on `ChatResponse.InputTransformations` but not yet consumed anywhere (e.g. no OTel span attribute or metric keys off `Reason`/`Type` yet) — a natural, small follow-up once real traffic exists to observe.
- Should `ReasoningBlock` eventually gain a `Model`/`Provider`-scoped variant field, or is the current provider-agnostic shape (`Redacted`/`Text`/`Data`/`Signature`) sufficient for every provider this RFC covers plus OpenAI's `encrypted_content` if Phase 5 is ever picked up? Current design treats OpenAI's `encrypted_content` as mappable onto `Data`+`Redacted=true` without a new field, but this hasn't been implemented or tested.
