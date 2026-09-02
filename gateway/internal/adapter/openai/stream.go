package openai

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kelvran/gateway/internal/adapter"
	"github.com/kelvran/gateway/internal/streaming"
)

// doneSentinel is the literal (non-JSON) payload OpenAI sends on the SSE
// "data:" line to mark the end of a stream. streaming.Reader hands this to
// Decode as an ordinary SSEEvent.Data value — Decode must special-case it
// before attempting to json.Unmarshal, per the RFC's explicit "not JSON"
// callout.
const doneSentinel = "[DONE]"

// nativeStreamChunk is OpenAI's native chat.completion.chunk wire shape —
// the streaming counterpart to Response in openai.go. It reuses the
// existing Usage type since the final chunk's usage object has the exact
// same shape as the non-streaming response's.
type nativeStreamChunk struct {
	ID      string               `json:"id"`
	Model   string               `json:"model"`
	Choices []nativeStreamChoice `json:"choices"`
	// Usage is populated only on the final chunk of a stream started with
	// stream_options.include_usage:true — see the ASSUMPTION note on
	// streamDecoder below for why this decoder assumes that flag is always
	// set by the gateway.
	Usage *Usage `json:"usage,omitempty"`
}

// nativeStreamChoice is one candidate's incremental delta within a native
// stream chunk.
type nativeStreamChoice struct {
	Index        int               `json:"index"`
	Delta        nativeStreamDelta `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
}

// nativeStreamDelta is OpenAI's native incremental message fragment.
type nativeStreamDelta struct {
	Role      string                `json:"role,omitempty"`
	Content   string                `json:"content,omitempty"`
	ToolCalls []nativeToolCallDelta `json:"tool_calls,omitempty"`
}

// nativeToolCallDelta is OpenAI's native incremental tool-call fragment.
// Only the chunk that first introduces a given tool call (identified by
// Index) carries ID/Type/Function.Name — every chunk contributing to its
// arguments (including that first one) carries a fragment of
// Function.Arguments, which the caller of Decode is responsible for
// concatenating in Index order to reconstruct the full JSON string, per
// streaming.ToolCallDelta's own documented contract. This decoder's job is
// only to forward each fragment faithfully, not to pre-concatenate them —
// pre-concatenating would mean buffering until the tool call closes, which
// would defeat the whole point of streaming (time-to-first-byte).
type nativeToolCallDelta struct {
	Index    int                 `json:"index"`
	ID       string              `json:"id,omitempty"`
	Type     string              `json:"type,omitempty"`
	Function nativeFunctionDelta `json:"function"`
}

// nativeFunctionDelta is the function-call payload nested inside a native
// tool-call delta.
type nativeFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// streamDecoder is the request-scoped, stateful streaming.StreamDecoder for
// OpenAI's Chat Completions streaming format. OpenAI's stream is close to
// homogeneous chat.completion.chunk objects, so most of Decode is a direct
// field reshape into the canonical shape — but the decoder still carries
// real cross-call state (done) so it can enforce the StreamDecoder
// contract's "never call Decode after done" rule with a clear error
// instead of silently mis-parsing a post-[DONE] call.
//
// ASSUMPTION: the gateway always sets stream_options.include_usage:true on
// upstream OpenAI streaming requests, since usage is required for cost
// accounting (per docs/rfcs/2026-09-02-streaming-support.md's Cost
// Accounting section) and OpenAI only ever sends a usage object at all when
// that flag is set. This decoder does not special-case a stream that never
// sends usage; if that assumption is ever violated for some deployment,
// finalUsage simply never fires here, and the dataplane's documented
// warning + zero/estimated-cost fallback (RFC, Cost Accounting) applies —
// this file doesn't need its own fallback for that case.
type streamDecoder struct {
	// done is true once the [DONE] sentinel has been observed. Real
	// cross-call state: it must persist from the Decode call that saw
	// [DONE] to every subsequent Decode call on this decoder instance.
	done bool
}

// NewStreamDecoder implements streaming.StreamingAdapter, returning a
// fresh, independent decoder for one in-flight OpenAI streaming request.
func (a *Adapter) NewStreamDecoder() streaming.StreamDecoder {
	return &streamDecoder{}
}

// Decode implements streaming.StreamDecoder.
func (d *streamDecoder) Decode(raw streaming.SSEEvent) ([]streaming.ChatCompletionChunk, bool, *adapter.Usage, error) {
	if d.done {
		return nil, true, nil, errors.New("openai: Decode called again after [DONE] was already observed")
	}

	if raw.Data == doneSentinel {
		d.done = true
		return nil, true, nil, nil
	}

	var native nativeStreamChunk
	if err := json.Unmarshal([]byte(raw.Data), &native); err != nil {
		return nil, false, nil, fmt.Errorf("openai: decoding stream chunk: %w", err)
	}

	chunk := streaming.ChatCompletionChunk{
		ID:    native.ID,
		Model: native.Model,
	}

	if len(native.Choices) > 0 {
		chunk.Choices = make([]streaming.ChunkChoice, 0, len(native.Choices))
		for _, c := range native.Choices {
			chunk.Choices = append(chunk.Choices, streaming.ChunkChoice{
				Index:        c.Index,
				Delta:        toCanonicalDelta(c.Delta),
				FinishReason: c.FinishReason,
			})
		}
	}

	var finalUsage *adapter.Usage
	if native.Usage != nil {
		finalUsage = &adapter.Usage{
			PromptTokens:     native.Usage.PromptTokens,
			CompletionTokens: native.Usage.CompletionTokens,
			TotalTokens:      native.Usage.TotalTokens,
		}
		chunk.Usage = finalUsage
	}

	return []streaming.ChatCompletionChunk{chunk}, false, finalUsage, nil
}

// toCanonicalDelta reshapes one native incremental delta into the canonical
// MessageDelta shape, forwarding each tool-call fragment (Index-keyed) as-
// is rather than accumulating — accumulation across calls is the caller's
// responsibility per streaming.ToolCallDelta's documented contract.
func toCanonicalDelta(d nativeStreamDelta) streaming.MessageDelta {
	delta := streaming.MessageDelta{
		Role:    d.Role,
		Content: d.Content,
	}
	if len(d.ToolCalls) == 0 {
		return delta
	}
	delta.ToolCalls = make([]streaming.ToolCallDelta, 0, len(d.ToolCalls))
	for _, tc := range d.ToolCalls {
		delta.ToolCalls = append(delta.ToolCalls, streaming.ToolCallDelta{
			Index:         tc.Index,
			ID:            tc.ID,
			Name:          tc.Function.Name,
			ArgumentsJSON: tc.Function.Arguments,
		})
	}
	return delta
}
