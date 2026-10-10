// Package streaming provides the transport-level SSE framing (reading raw
// provider event streams, writing canonical chunks to the client) plus the
// StreamDecoder seam each streaming-capable provider adapter implements.
//
// This is deliberately a separate package from internal/adapter: streaming
// decoding is inherently stateful (per gateway/ARCHITECTURE.md's documented
// hazard — Anthropic's typed event sequence requires tracking which content
// block is open across calls), which would break the existing Adapter
// interface's pure-function guarantee if bolted on there. See
// docs/rfcs/2026-09-02-streaming-support.md for the full design.
package streaming

import "github.com/kelvran/gateway/gateway/internal/adapter"

// SSEEvent is one already-framed Server-Sent Event read from a provider's
// raw response body — the "event:" and "data:" fields, nothing more (no
// provider sends "id:"/"retry:" fields Kelvran needs to act on).
type SSEEvent struct {
	Event string
	Data  string
}

// ChatCompletionChunk is one incremental fragment of a streaming chat
// completion, in Kelvran's canonical (OpenAI-compatible) shape — the
// client-facing wire format for every streaming response, regardless of
// which upstream provider actually served it.
type ChatCompletionChunk struct {
	ID string `json:"id"`
	// Object and Created mirror adapter.ChatResponse's envelope for the
	// streaming shape ("chat.completion.chunk" and the Unix creation time
	// of the whole stream). No StreamDecoder sets them; the dataplane
	// stamps every chunk from one per-stream envelope before it is
	// accumulated and written (completion_envelope.go), filling ID and
	// Model too when the provider's chunks carry none (Bedrock). omitempty
	// keeps this package's byte-exact SSE goldens unchanged.
	Object  string        `json:"object,omitempty"`
	Created int64         `json:"created,omitempty"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	// Usage is non-nil only on the chunk (typically the final one) where
	// the upstream provider actually supplied usage data. A provider that
	// never sends usage during streaming leaves every chunk's Usage nil —
	// callers must not assume the last chunk always carries it.
	Usage *adapter.Usage `json:"usage,omitempty"`
	// Unrepresentable is set by a StreamDecoder on the chunk it emits for a
	// provider block the canonical schema cannot represent (an Anthropic
	// content-block or delta type newer than the decoder); the dataplane's
	// accumulator folds it into ChatResponse.Unrepresentable so the
	// assembled response is never cached (item 11 slice S6). json:"-": a
	// gateway-internal signal, never on the SSE wire.
	Unrepresentable bool `json:"-"`
}

// ChunkChoice is a single candidate's incremental delta within one chunk.
type ChunkChoice struct {
	Index        int          `json:"index"`
	Delta        MessageDelta `json:"delta"`
	FinishReason *string      `json:"finish_reason"`
	// StopReason and StopSequence mirror adapter.ChatResponse's fields on
	// the finish chunk only: the provider's native stop reason and matched
	// stop sequence, set by the anthropic and bedrock decoders and by the
	// cache replay, empty everywhere else (omitempty keeps the OpenAI wire
	// bytes of every other provider unchanged). The accumulator folds them
	// back into the canonical response.
	StopReason   string `json:"stop_reason,omitempty"`
	StopSequence string `json:"stop_sequence,omitempty"`
}

// MessageDelta is the incremental fragment of a message within one chunk.
// Every field is optional/partial by design — a real streaming response
// spreads a single logical message across many deltas.
type MessageDelta struct {
	// Role is present only on the first chunk of a message.
	Role string `json:"role,omitempty"`
	// Content is an incremental text fragment, possibly empty.
	Content string `json:"content,omitempty"`
	// ToolCalls holds incremental tool-call fragments, keyed by Index so a
	// caller can accumulate a single logical tool call across many chunks.
	ToolCalls []ToolCallDelta `json:"tool_calls,omitempty"`
	// ReasoningBlocks holds incremental reasoning/thinking-block fragments,
	// keyed by Index exactly like ToolCalls above, per
	// docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md's
	// Phase 2 (streaming). Additive/omitempty, matching this type's own
	// convention: a provider adapter that never streams reasoning content
	// leaves every chunk's ReasoningBlocks nil, byte-identical to before
	// this field existed.
	ReasoningBlocks []ReasoningDelta `json:"reasoning_blocks,omitempty"`
	// Refusal is an incremental fragment of the model's own refusal
	// message, streamed the same way Content is -- OpenAI's real
	// ChatCompletionStreamResponseDelta schema carries this as a field
	// SIBLING to Content/Role/ToolCalls (confirmed against the official
	// openai-openapi spec), mirroring adapter.Message.Refusal's own
	// buffered-path shape and doc comment. Only openai/openaicompat ever
	// populate this; every other adapter leaves it permanently empty,
	// matching that field's own established convention.
	Refusal string `json:"refusal,omitempty"`
}

// ToolCallDelta is one incremental fragment of a single tool call within a
// MessageDelta. ID and Name are present only on the chunk that first
// introduces this tool call (Index identifies which one); every chunk that
// contributes to its arguments carries a fragment of ArgumentsJSON, which
// callers concatenate in order to reconstruct the full JSON string.
type ToolCallDelta struct {
	Index         int    `json:"index"`
	ID            string `json:"id,omitempty"`
	Name          string `json:"name,omitempty"`
	ArgumentsJSON string `json:"arguments_json,omitempty"`
}

// ReasoningDelta is one incremental fragment of a single reasoning/thinking
// block within a MessageDelta, keyed by Index exactly like ToolCallDelta so
// a caller can accumulate multiple (possibly interleaved) reasoning blocks
// correctly across many chunks, per
// docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md. Text
// fragments concatenate in arrival order to reconstruct a plaintext block's
// full content; Signature and Data each arrive whole, never fragmented,
// on the single delta (or content-block-start event) that carries them.
type ReasoningDelta struct {
	Index int `json:"index"`
	// Text carries an incremental plaintext thinking fragment (Anthropic's
	// thinking_delta.thinking), concatenated in order to reconstruct the
	// block's full text.
	Text string `json:"text,omitempty"`
	// Signature carries a plaintext thinking block's complete cryptographic
	// signature (Anthropic's signature_delta.signature) -- delivered whole
	// on a single delta event just before the block's content_block_stop,
	// never fragmented across multiple deltas.
	Signature string `json:"signature,omitempty"`
	// Redacted marks this reasoning block as provider-encrypted/opaque,
	// mirroring adapter.ReasoningBlock.Redacted.
	Redacted bool `json:"redacted,omitempty"`
	// Data carries a redacted block's complete opaque ciphertext payload --
	// delivered whole (Anthropic sends it already-complete on the block's
	// content_block_start event; no delta type exists for it).
	Data string `json:"data,omitempty"`
}

// StreamDecoder incrementally translates one upstream provider's raw SSE
// events into canonical ChatCompletionChunks. A StreamDecoder is stateful
// and scoped to exactly one in-flight request — never shared or reused
// across requests, and never called concurrently.
type StreamDecoder interface {
	// Decode consumes one raw provider-native SSE event and returns zero or
	// more canonical chunks ready to forward to the client (a single raw
	// event sometimes maps to zero chunks — e.g. Anthropic's
	// content_block_stop carries no client-visible delta — and, in
	// principle, could map to more than one).
	//
	// done is true once the provider's stream is logically complete
	// (OpenAI's literal "[DONE]" sentinel line; Anthropic's message_stop
	// event) — callers must stop calling Decode after done is true.
	//
	// finalUsage is non-nil the moment usage data is observed in the raw
	// event stream (which provider-specific event carries it varies; see
	// each adapter's stream.go). It may be returned before done becomes
	// true.
	Decode(raw SSEEvent) (chunks []ChatCompletionChunk, done bool, finalUsage *adapter.Usage, err error)
}

// StreamingAdapter is the additive capability a provider adapter opts into
// by also implementing NewStreamDecoder, for the SSE-framed providers:
// OpenAI, Anthropic, Gemini (real per
// docs/rfcs/2026-09-04-gemini-adapter.md), and openaicompat (real per
// docs/rfcs/2026-09-04-openaicompat-adapter.md). Bedrock also streams
// (real per docs/rfcs/2026-09-04-bedrock-converse-stream.md) but does NOT
// implement this interface — its wire format is binary
// (application/vnd.amazon.eventstream), not SSE, so it is decoded by a
// genuinely separate path (bedrock.StreamDecoder, driven directly by
// dataplane's streamDeploymentBedrock) rather than this package's
// SSEEvent-shaped contract. See that RFC's Detailed Design for why a
// shared interface across both framings isn't warranted for a single
// binary-framed implementor.
// Gemini's stream has no terminal sentinel of its own (its StreamDecoder
// always returns done=false) — callers must rely on the transport-level
// io.EOF to end the loop, which this package's own Reader already
// supports. Callers must type-assert to this interface before attempting
// to stream a request and return a clear, typed error if the assertion
// fails — never silently fall back to buffering.
type StreamingAdapter interface {
	adapter.Adapter
	// NewStreamDecoder returns a fresh, request-scoped StreamDecoder. Each
	// call must return an independent decoder with its own internal state.
	NewStreamDecoder() StreamDecoder
}
