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
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	// Usage is non-nil only on the chunk (typically the final one) where
	// the upstream provider actually supplied usage data. A provider that
	// never sends usage during streaming leaves every chunk's Usage nil —
	// callers must not assume the last chunk always carries it.
	Usage *adapter.Usage `json:"usage,omitempty"`
}

// ChunkChoice is a single candidate's incremental delta within one chunk.
type ChunkChoice struct {
	Index        int          `json:"index"`
	Delta        MessageDelta `json:"delta"`
	FinishReason *string      `json:"finish_reason"`
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
// by also implementing NewStreamDecoder. Only OpenAI and Anthropic satisfy
// this in the current scaffolding — Gemini/Bedrock/openaicompat remain
// stubbed for both non-streaming and streaming, per
// docs/rfcs/2026-09-02-streaming-support.md's scope boundary. Callers must
// type-assert to this interface before attempting to stream a request and
// return a clear, typed error if the assertion fails — never silently fall
// back to buffering.
type StreamingAdapter interface {
	adapter.Adapter
	// NewStreamDecoder returns a fresh, request-scoped StreamDecoder. Each
	// call must return an independent decoder with its own internal state.
	NewStreamDecoder() StreamDecoder
}
