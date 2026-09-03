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
