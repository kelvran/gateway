package dataplane

import (
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

func strPtr(s string) *string { return &s }

func TestStreamAccumulatorTextOnly(t *testing.T) {
	acc := newStreamAccumulator()
	acc.add(streaming.ChatCompletionChunk{ID: "resp-1", Model: "gpt-4o", Choices: []streaming.ChunkChoice{
		{Index: 0, Delta: streaming.MessageDelta{Role: "assistant"}},
	}})
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{
		{Index: 0, Delta: streaming.MessageDelta{Content: "Hel"}},
	}})
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{
		{Index: 0, Delta: streaming.MessageDelta{Content: "lo!"}},
	}})
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{
		{Index: 0, Delta: streaming.MessageDelta{}, FinishReason: strPtr("stop")},
	}})

	got := acc.build(adapter.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8})

	if got.ID != "resp-1" || got.Model != "gpt-4o" {
		t.Errorf("ID/Model = %q/%q, want resp-1/gpt-4o", got.ID, got.Model)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("len(Choices) = %d, want 1", len(got.Choices))
	}
	c := got.Choices[0]
	if c.Message.Content != "Hello!" {
		t.Errorf("Content = %q, want %q", c.Message.Content, "Hello!")
	}
	if c.Message.Role != "assistant" {
		t.Errorf("Role = %q, want assistant", c.Message.Role)
	}
	if c.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", c.FinishReason)
	}
	if got.Usage.TotalTokens != 8 {
		t.Errorf("Usage.TotalTokens = %d, want 8", got.Usage.TotalTokens)
	}
}

func TestStreamAccumulatorToolCallArgumentsConcatenateInOrder(t *testing.T) {
	acc := newStreamAccumulator()
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{{
		Index: 0,
		Delta: streaming.MessageDelta{ToolCalls: []streaming.ToolCallDelta{
			{Index: 0, ID: "call_1", Name: "get_weather"},
		}},
	}}})
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{{
		Index: 0,
		Delta: streaming.MessageDelta{ToolCalls: []streaming.ToolCallDelta{
			{Index: 0, ArgumentsJSON: `{"city":`},
		}},
	}}})
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{{
		Index: 0,
		Delta: streaming.MessageDelta{ToolCalls: []streaming.ToolCallDelta{
			{Index: 0, ArgumentsJSON: `"Boston"}`},
		}},
	}}})

	got := acc.build(adapter.Usage{})
	if len(got.Choices) != 1 || len(got.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("got = %+v, want exactly 1 choice with 1 tool call", got)
	}
	tc := got.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "get_weather" {
		t.Errorf("ID/Name = %q/%q, want call_1/get_weather", tc.ID, tc.Name)
	}
	if tc.ArgumentsJSON != `{"city":"Boston"}` {
		t.Errorf("ArgumentsJSON = %q, want %q", tc.ArgumentsJSON, `{"city":"Boston"}`)
	}
}

// TestStreamAccumulatorTwoToolCallsDoNotCrossContaminate is the
// accumulator-level analog of the Anthropic decoder's interleaving test —
// two tool calls at different indices, with fragments arriving interleaved
// across chunks, must never bleed into each other's ArgumentsJSON.
func TestStreamAccumulatorTwoToolCallsDoNotCrossContaminate(t *testing.T) {
	acc := newStreamAccumulator()
	events := []streaming.ChatCompletionChunk{
		{Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{
			ToolCalls: []streaming.ToolCallDelta{{Index: 0, ID: "call_a", Name: "get_weather"}},
		}}}},
		{Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{
			ToolCalls: []streaming.ToolCallDelta{{Index: 1, ID: "call_b", Name: "get_time"}},
		}}}},
		{Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{
			ToolCalls: []streaming.ToolCallDelta{{Index: 0, ArgumentsJSON: `{"city":"Boston"}`}},
		}}}},
		{Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{
			ToolCalls: []streaming.ToolCallDelta{{Index: 1, ArgumentsJSON: `{"timezone":"EST"}`}},
		}}}},
	}
	for _, ev := range events {
		acc.add(ev)
	}

	got := acc.build(adapter.Usage{})
	if len(got.Choices[0].Message.ToolCalls) != 2 {
		t.Fatalf("len(ToolCalls) = %d, want 2", len(got.Choices[0].Message.ToolCalls))
	}
	// build() sorts by index, so ToolCalls[0] is index 0 (call_a),
	// ToolCalls[1] is index 1 (call_b).
	a, b := got.Choices[0].Message.ToolCalls[0], got.Choices[0].Message.ToolCalls[1]
	if a.ID != "call_a" || a.ArgumentsJSON != `{"city":"Boston"}` {
		t.Errorf("first tool call = %+v, want call_a with city args", a)
	}
	if b.ID != "call_b" || b.ArgumentsJSON != `{"timezone":"EST"}` {
		t.Errorf("second tool call = %+v, want call_b with timezone args", b)
	}
}

// TestStreamAccumulatorReasoningBlocksConcatenateTextAndCaptureSignature
// is the accumulator-level analog of anthropic/stream_test.go's
// TestStreamDecoder_Thinking: a thinking block's Text fragments
// (thinking_delta, sent repeatedly) must concatenate in order, and its
// Signature (signature_delta, sent exactly once) must survive into the
// built response — proving the real bug this whole file's own package
// (streamaccumulator.go) previously had: cc.Delta.ReasoningBlocks was
// never read at all, so Message.ReasoningBlocks came back empty on every
// streamed response regardless of what the decoder emitted.
func TestStreamAccumulatorReasoningBlocksConcatenateTextAndCaptureSignature(t *testing.T) {
	acc := newStreamAccumulator()
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{{
		Index: 0,
		Delta: streaming.MessageDelta{ReasoningBlocks: []streaming.ReasoningDelta{
			{Index: 0, Text: "Let me check the weather API first."},
		}},
	}}})
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{{
		Index: 0,
		Delta: streaming.MessageDelta{ReasoningBlocks: []streaming.ReasoningDelta{
			{Index: 0, Text: " I'll call get_weather."},
		}},
	}}})
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{{
		Index: 0,
		Delta: streaming.MessageDelta{ReasoningBlocks: []streaming.ReasoningDelta{
			{Index: 0, Signature: "sig_abc123"},
		}},
	}}})

	got := acc.build(adapter.Usage{})
	if len(got.Choices) != 1 {
		t.Fatalf("len(Choices) = %d, want 1", len(got.Choices))
	}
	rbs := got.Choices[0].Message.ReasoningBlocks
	if len(rbs) != 1 {
		t.Fatalf("len(ReasoningBlocks) = %d, want 1", len(rbs))
	}
	want := "Let me check the weather API first. I'll call get_weather."
	if rbs[0].Text != want {
		t.Errorf("Text = %q, want %q", rbs[0].Text, want)
	}
	if rbs[0].Signature != "sig_abc123" {
		t.Errorf("Signature = %q, want sig_abc123", rbs[0].Signature)
	}
	if rbs[0].Redacted {
		t.Error("Redacted = true, want false — this is a plaintext thinking block")
	}
	if rbs[0].Sequence != 0 {
		t.Errorf("Sequence = %d, want 0 (no tool calls at all)", rbs[0].Sequence)
	}
}

// TestStreamAccumulatorRedactedReasoningBlockCapturedWhole is the
// accumulator-level analog of anthropic/stream_test.go's
// TestStreamDecoder_RedactedThinking: a redacted_thinking block's entire
// opaque Data payload arrives on a single delta (content_block_start, per
// stream.go's own doc comment) rather than fragmented — must be captured
// verbatim, with Redacted set, and its ciphertext Data must never be
// confused with plaintext Text.
func TestStreamAccumulatorRedactedReasoningBlockCapturedWhole(t *testing.T) {
	acc := newStreamAccumulator()
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{{
		Index: 0,
		Delta: streaming.MessageDelta{ReasoningBlocks: []streaming.ReasoningDelta{
			{Index: 0, Redacted: true, Data: "opaque-ciphertext-blob"},
		}},
	}}})

	got := acc.build(adapter.Usage{})
	rbs := got.Choices[0].Message.ReasoningBlocks
	if len(rbs) != 1 {
		t.Fatalf("len(ReasoningBlocks) = %d, want 1", len(rbs))
	}
	if !rbs[0].Redacted {
		t.Error("Redacted = false, want true")
	}
	if rbs[0].Data != "opaque-ciphertext-blob" {
		t.Errorf("Data = %q, want opaque-ciphertext-blob", rbs[0].Data)
	}
	if rbs[0].Text != "" {
		t.Errorf("Text = %q, want empty — a redacted block carries no plaintext", rbs[0].Text)
	}
}

// TestStreamAccumulatorReasoningBlockSequenceReflectsToolCallInterleaving
// proves Sequence is reconstructed correctly relative to tool calls —
// required per adapter.ReasoningBlock.Sequence's own doc comment ("N
// means immediately before ToolCalls[N]") so a cached/replayed streamed
// response with interleaved thinking/tool-use blocks doesn't corrupt a
// later conversation turn that echoes it back to Anthropic. Two thinking
// blocks straddle one tool call, at content-block indices 0 (before), 2
// (after) with the tool call itself at index 1 — the same shared
// per-message index space anthropic/stream.go, bedrock/stream.go, and
// gemini/stream.go all document using for both ToolCallDelta.Index and
// ReasoningDelta.Index.
func TestStreamAccumulatorReasoningBlockSequenceReflectsToolCallInterleaving(t *testing.T) {
	acc := newStreamAccumulator()
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{{
		Index: 0,
		Delta: streaming.MessageDelta{ReasoningBlocks: []streaming.ReasoningDelta{
			{Index: 0, Text: "thinking before the tool call"},
		}},
	}}})
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{{
		Index: 0,
		Delta: streaming.MessageDelta{ToolCalls: []streaming.ToolCallDelta{
			{Index: 1, ID: "call_1", Name: "get_weather", ArgumentsJSON: `{}`},
		}},
	}}})
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{{
		Index: 0,
		Delta: streaming.MessageDelta{ReasoningBlocks: []streaming.ReasoningDelta{
			{Index: 2, Text: "thinking after the tool call"},
		}},
	}}})

	got := acc.build(adapter.Usage{})
	rbs := got.Choices[0].Message.ReasoningBlocks
	if len(rbs) != 2 {
		t.Fatalf("len(ReasoningBlocks) = %d, want 2", len(rbs))
	}
	if rbs[0].Text != "thinking before the tool call" || rbs[0].Sequence != 0 {
		t.Errorf("rbs[0] = %+v, want Sequence 0 (before ToolCalls[0])", rbs[0])
	}
	if rbs[1].Text != "thinking after the tool call" || rbs[1].Sequence != 1 {
		t.Errorf("rbs[1] = %+v, want Sequence 1 (after the one tool call)", rbs[1])
	}
}

func TestStreamAccumulatorUsageOnlyChunkAddsNoChoices(t *testing.T) {
	acc := newStreamAccumulator()
	acc.add(streaming.ChatCompletionChunk{ID: "resp-2", Model: "claude-opus-4"}) // no Choices at all
	got := acc.build(adapter.Usage{TotalTokens: 10})
	if len(got.Choices) != 0 {
		t.Errorf("len(Choices) = %d, want 0 for a usage-only chunk sequence", len(got.Choices))
	}
	if got.ID != "resp-2" {
		t.Errorf("ID = %q, want resp-2 (should still be captured from a choice-less chunk)", got.ID)
	}
}

// TestStreamAccumulatorAddDetectsDuplicateIndexAfterFinishReason proves
// the new anomaly-detection: a chunk delivering real content to an index
// whose finishReason was ALREADY set is recorded in
// duplicateAfterFinishIndices.
func TestStreamAccumulatorAddDetectsDuplicateIndexAfterFinishReason(t *testing.T) {
	acc := newStreamAccumulator()
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{
		{Index: 0, Delta: streaming.MessageDelta{Content: "Hello"}},
	}})
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{
		{Index: 0, Delta: streaming.MessageDelta{}, FinishReason: strPtr("stop")},
	}})
	if len(acc.duplicateAfterFinishIndices) != 0 {
		t.Fatalf("duplicateAfterFinishIndices = %v after a normal finish, want empty", acc.duplicateAfterFinishIndices)
	}

	// A REPLAYED/duplicate event for the same index, after it already
	// finished -- the real anomaly this test proves gets detected.
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{
		{Index: 0, Delta: streaming.MessageDelta{Content: " again"}},
	}})
	if len(acc.duplicateAfterFinishIndices) != 1 || acc.duplicateAfterFinishIndices[0] != 0 {
		t.Errorf("duplicateAfterFinishIndices = %v, want [0]", acc.duplicateAfterFinishIndices)
	}
}

// TestStreamAccumulatorAddDetectsDuplicateIndexAfterFinishReasonForReasoningBlocksOnly
// is the regression proof for a real gap an independent adversarial
// review found in the ReasoningBlocks-folding fix above: a chunk
// carrying ONLY a ReasoningBlocks delta (no Content/Role/ToolCalls) that
// arrives after its own choice already finished was silently folded
// into the accumulated reasoning text with duplicateAfterFinishIndices
// staying empty -- reintroducing, for reasoning blocks specifically, the
// exact "silent half of the gap" this detector exists to close.
func TestStreamAccumulatorAddDetectsDuplicateIndexAfterFinishReasonForReasoningBlocksOnly(t *testing.T) {
	acc := newStreamAccumulator()
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{
		{Index: 0, Delta: streaming.MessageDelta{Content: "Hello"}},
	}})
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{
		{Index: 0, Delta: streaming.MessageDelta{}, FinishReason: strPtr("stop")},
	}})
	if len(acc.duplicateAfterFinishIndices) != 0 {
		t.Fatalf("duplicateAfterFinishIndices = %v after a normal finish, want empty", acc.duplicateAfterFinishIndices)
	}

	// A REPLAYED/duplicate event for the same index, after it already
	// finished -- carrying ONLY a reasoning-block delta, no
	// Content/Role/ToolCalls -- must still be detected.
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{
		{Index: 0, Delta: streaming.MessageDelta{ReasoningBlocks: []streaming.ReasoningDelta{{Index: 0, Text: "more thinking"}}}},
	}})
	if len(acc.duplicateAfterFinishIndices) != 1 || acc.duplicateAfterFinishIndices[0] != 0 {
		t.Errorf("duplicateAfterFinishIndices = %v, want [0] -- a reasoning-block-only delta after finish must be detected identically to Content/ToolCalls", acc.duplicateAfterFinishIndices)
	}
}

// TestStreamAccumulatorAddStillConcatenatesContentOnDuplicateIndexReuse
// proves no behavior regression: the concatenation itself stays
// unchanged (the safest default for this display-only corruption class)
// -- detection is additive, not a replacement for the existing fold
// logic.
func TestStreamAccumulatorAddStillConcatenatesContentOnDuplicateIndexReuse(t *testing.T) {
	acc := newStreamAccumulator()
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{
		{Index: 0, Delta: streaming.MessageDelta{Content: "Hello"}},
	}})
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{
		{Index: 0, Delta: streaming.MessageDelta{}, FinishReason: strPtr("stop")},
	}})
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{
		{Index: 0, Delta: streaming.MessageDelta{Content: " again"}},
	}})

	got := acc.build(adapter.Usage{})
	if len(got.Choices) != 1 {
		t.Fatalf("len(Choices) = %d, want 1", len(got.Choices))
	}
	if got.Choices[0].Message.Content != "Hello again" {
		t.Errorf("Content = %q, want %q (concatenation unchanged)", got.Choices[0].Message.Content, "Hello again")
	}
}
