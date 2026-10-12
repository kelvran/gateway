package streaming

import (
	"encoding/json"
	"strings"
	"testing"
)

// The Anthropic and Bedrock decoders number tool-call deltas by the
// provider's content-block index, so with a thinking or text block first the
// first call arrives as Index 1. OpenAI's `index` is the call's position in
// the final tool_calls (0 first) and the official accumulators trust it:
// openai-python's stream helper raises IndexError and openai-go pads a
// phantom call (both probed, 2026-10-12). Writer renumbers by order of first
// appearance, per choice, on the wire only.

func toolIndices(t *testing.T, body string) [][2]int {
	t.Helper()
	var out [][2]int
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var c struct {
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					ToolCalls []struct {
						Index int `json:"index"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &c); err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		for _, ch := range c.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				out = append(out, [2]int{ch.Index, tc.Index})
			}
		}
	}
	return out
}

func TestWriterRenumbersToolCallIndicesToPositionsByFirstAppearance(t *testing.T) {
	rec := newFlushCountingRecorder()
	sw, err := NewWriter(rec)
	if err != nil {
		t.Fatal(err)
	}
	chunks := []ChatCompletionChunk{
		// a thinking block at provider index 0 precedes everything
		{Choices: []ChunkChoice{{Delta: MessageDelta{ReasoningBlocks: []ReasoningDelta{{Index: 0, Text: "hmm"}}}}}},
		// first call at provider index 1, then its fragment
		{Choices: []ChunkChoice{{Delta: MessageDelta{ToolCalls: []ToolCallDelta{{Index: 1, ID: "call_1", Name: "get_weather"}}}}}},
		{Choices: []ChunkChoice{{Delta: MessageDelta{ToolCalls: []ToolCallDelta{{Index: 1, ArgumentsJSON: `{"city":"Boston"}`}}}}}},
		// second call at provider index 3 (a text block sat at 2)
		{Choices: []ChunkChoice{{Delta: MessageDelta{ToolCalls: []ToolCallDelta{{Index: 3, ID: "call_2", Name: "get_time"}}}}}},
		// a late fragment for the first call keeps its position
		{Choices: []ChunkChoice{{Delta: MessageDelta{ToolCalls: []ToolCallDelta{{Index: 1, ArgumentsJSON: ``}, {Index: 3, ArgumentsJSON: `{}`}}}}}},
	}
	for _, c := range chunks {
		if err := sw.WriteChunk(c); err != nil {
			t.Fatal(err)
		}
	}
	got := toolIndices(t, rec.Body.String())
	want := [][2]int{{0, 0}, {0, 0}, {0, 1}, {0, 0}, {0, 1}}
	if len(got) != len(want) {
		t.Fatalf("tool indices on the wire = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("delta %d: (choice, index) = %v, want %v (all: %v)", i, got[i], want[i], got)
		}
	}
	// The caller's chunks are never mutated: the accumulator and the audit
	// hold the same slices and must keep the provider's index.
	if chunks[1].Choices[0].Delta.ToolCalls[0].Index != 1 || chunks[3].Choices[0].Delta.ToolCalls[0].Index != 3 {
		t.Errorf("WriteChunk mutated the caller's deltas: %+v / %+v", chunks[1].Choices[0].Delta.ToolCalls, chunks[3].Choices[0].Delta.ToolCalls)
	}
}

func TestWriterRenumbersToolCallIndicesPerChoice(t *testing.T) {
	rec := newFlushCountingRecorder()
	sw, err := NewWriter(rec)
	if err != nil {
		t.Fatal(err)
	}
	// Two candidates, each with its own first call at a non-zero provider
	// index: positions start at 0 inside each choice.
	if err := sw.WriteChunk(ChatCompletionChunk{Choices: []ChunkChoice{
		{Index: 0, Delta: MessageDelta{ToolCalls: []ToolCallDelta{{Index: 2, ID: "a"}}}},
		{Index: 1, Delta: MessageDelta{ToolCalls: []ToolCallDelta{{Index: 5, ID: "b"}}}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := sw.WriteChunk(ChatCompletionChunk{Choices: []ChunkChoice{
		{Index: 1, Delta: MessageDelta{ToolCalls: []ToolCallDelta{{Index: 7, ID: "c"}}}},
	}}); err != nil {
		t.Fatal(err)
	}
	got := toolIndices(t, rec.Body.String())
	want := [][2]int{{0, 0}, {1, 0}, {1, 1}}
	if len(got) != len(want) {
		t.Fatalf("tool indices on the wire = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("delta %d: (choice, index) = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestWriterLeavesPositionIndicesAlone(t *testing.T) {
	// openai/openaicompat upstreams and cache replays already send
	// positions; the renumbering is the identity for them.
	rec := newFlushCountingRecorder()
	sw, err := NewWriter(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []ChatCompletionChunk{
		{Choices: []ChunkChoice{{Delta: MessageDelta{ToolCalls: []ToolCallDelta{{Index: 0, ID: "a"}, {Index: 1, ID: "b"}}}}}},
		{Choices: []ChunkChoice{{Delta: MessageDelta{ToolCalls: []ToolCallDelta{{Index: 1, ArgumentsJSON: "{}"}, {Index: 0, ArgumentsJSON: "{}"}}}}}},
	} {
		if err := sw.WriteChunk(c); err != nil {
			t.Fatal(err)
		}
	}
	got := toolIndices(t, rec.Body.String())
	want := [][2]int{{0, 0}, {0, 1}, {0, 1}, {0, 0}}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("tool indices on the wire = %v, want %v", got, want)
		}
	}
}
