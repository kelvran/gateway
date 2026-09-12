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
5. **Phase 5 (not_yet)** — OpenAI/openaicompat: deferred. Research found OpenAI's own replay-strictness for the Responses API's reasoning items is unresolved (hard vs. soft failure — three claims on this were adversarially refuted), and Kelvran's OpenAI/openaicompat adapters target the Chat Completions wire shape, not the Responses API, where reasoning surfaces differently. openaicompat's llama.cpp `reasoning_content` gap (a single opaque string, no interleaving) is a smaller, separate follow-up once a concrete trigger exists (a real user report, or Kelvran adding first-class Responses API / o-series support).

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
- Should `ReasoningBlock` eventually gain a `Model`/`Provider`-scoped variant field, or is the current provider-agnostic shape (`Redacted`/`Text`/`Data`/`Signature`) sufficient for every provider this RFC covers plus OpenAI's `encrypted_content` if Phase 5 is ever picked up? Current design treats OpenAI's `encrypted_content` as mappable onto `Data`+`Redacted=true` without a new field, but this hasn't been implemented or tested.
