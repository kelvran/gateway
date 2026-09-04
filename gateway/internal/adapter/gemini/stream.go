package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// streamDecoder is the request-scoped streaming.StreamDecoder for Gemini's
// streamGenerateContent format. Confirmed against the live discovery
// document that each SSE "data:" frame is a complete, independently-
// parseable GenerateContentResponse — the exact same schema gemini.go's
// Response type already defines for the buffered path, unlike OpenAI's
// distinct chat.completion.chunk shape. This makes the decoder itself
// simpler than either openai's (no separate chunk type) or anthropic's (no
// typed multi-event sequence to track block-kind state across) — its only
// state is whether the role has already been sent, mirroring anthropic's
// sentRole field.
//
// A real divergence from OpenAI/Anthropic's tool-call contract, documented
// here rather than silently handled: Gemini's functionCall.args arrives as
// a complete JSON object in a single chunk, never split across multiple
// deltas the way OpenAI's Arguments string fragments are. This decoder
// therefore emits one ToolCallDelta per functionCall part with its full
// ArgumentsJSON already populated — callers must not assume they need to
// accumulate it across further chunks the way OpenAI's contract requires.
//
// There is no terminal sentinel in Gemini's stream (no "[DONE]", no
// distinguished final event) — Decode always returns done=false. The
// stream's real end is signaled by the transport closing (io.EOF from
// streaming.Reader.Next()), which the dataplane's streaming loop already
// handles regardless of whether Decode ever reports done — confirmed by
// direct read of gateway/internal/gateway/dataplane/streaming.go before
// relying on it, per docs/rfcs/2026-09-04-gemini-adapter.md.
type streamDecoder struct {
	sentRole bool
}

// NewStreamDecoder implements streaming.StreamingAdapter, returning a
// fresh, independent decoder for one in-flight Gemini streaming request.
func (a *Adapter) NewStreamDecoder() streaming.StreamDecoder {
	return &streamDecoder{}
}

// Decode implements streaming.StreamDecoder.
func (d *streamDecoder) Decode(raw streaming.SSEEvent) ([]streaming.ChatCompletionChunk, bool, *adapter.Usage, error) {
	var native Response
	if err := json.Unmarshal([]byte(raw.Data), &native); err != nil {
		return nil, false, nil, fmt.Errorf("gemini: decoding stream chunk: %w", err)
	}

	if len(native.Candidates) == 0 {
		// A chunk carrying only usageMetadata (or an empty keep-alive
		// frame) with no candidate content at all — nothing client-visible
		// to emit, but still a legitimate part of the stream.
		return nil, false, usageFromNative(native), nil
	}

	candidate := native.Candidates[0]

	var delta streaming.MessageDelta
	if !d.sentRole {
		delta.Role = "assistant"
		d.sentRole = true
	}

	var textParts []string
	var toolCallDeltas []streaming.ToolCallDelta
	for i, part := range candidate.Content.Parts {
		switch {
		case part.FunctionCall != nil:
			argsJSON, err := json.Marshal(part.FunctionCall.Args)
			if err != nil {
				return nil, false, nil, fmt.Errorf("gemini: marshaling stream functionCall %q args: %w", part.FunctionCall.Name, err)
			}
			toolCallDeltas = append(toolCallDeltas, streaming.ToolCallDelta{
				Index:         i,
				ID:            part.FunctionCall.ID,
				Name:          part.FunctionCall.Name,
				ArgumentsJSON: string(argsJSON),
			})
		case part.Text != "":
			textParts = append(textParts, part.Text)
		}
	}
	delta.Content = strings.Join(textParts, "")
	delta.ToolCalls = toolCallDeltas

	var finishReason *string
	if candidate.FinishReason != "" {
		mapped, err := finishReasonFromGemini(candidate.FinishReason, len(toolCallDeltas) > 0)
		if err != nil {
			return nil, false, nil, err
		}
		finishReason = &mapped
	}

	chunk := streaming.ChatCompletionChunk{
		Choices: []streaming.ChunkChoice{
			{Index: 0, Delta: delta, FinishReason: finishReason},
		},
	}
	finalUsage := usageFromNative(native)
	if finalUsage != nil {
		chunk.Usage = finalUsage
	}

	// Always false: Gemini's stream has no terminal sentinel — see the
	// package doc above. The dataplane's own reader loop ends the stream
	// on io.EOF regardless.
	return []streaming.ChatCompletionChunk{chunk}, false, finalUsage, nil
}

// usageFromNative returns a non-nil adapter.Usage only if native carries
// real, non-zero usage data — Gemini only populates usageMetadata once the
// model has finished generating, so most chunks legitimately have none.
func usageFromNative(native Response) *adapter.Usage {
	u := native.UsageMetadata
	if u.PromptTokenCount == 0 && u.CandidatesTokenCount == 0 && u.TotalTokenCount == 0 {
		return nil
	}
	return &adapter.Usage{
		PromptTokens:     u.PromptTokenCount,
		CompletionTokens: u.CandidatesTokenCount,
		TotalTokens:      u.TotalTokenCount,
	}
}
