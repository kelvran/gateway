package streaming

import (
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// TestWriteChunkGoldenWireFormat pins the exact byte-for-byte SSE output for
// a representative chunk carrying every field kind (role+content, a
// tool-call delta, a set finish_reason, and usage) — the client-facing wire
// format every SSE consumer parses. This is this package's equivalent of
// internal/adapter/{openai,anthropic}/regression_test.go: those pin OUR
// wire format against each PROVIDER's; this pins OUR wire format against
// ITSELF, so an accidental JSON tag rename or field reorder in types.go is
// caught here rather than silently breaking every downstream SSE client.
func TestWriteChunkGoldenWireFormat(t *testing.T) {
	rec := newFlushCountingRecorder()
	sw, err := NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}

	finishReason := "tool_calls"
	chunk := ChatCompletionChunk{
		ID:    "chunk-1",
		Model: "gpt-4o",
		Choices: []ChunkChoice{
			{
				Index: 0,
				Delta: MessageDelta{
					Role:    "assistant",
					Content: "hi",
					ToolCalls: []ToolCallDelta{
						{Index: 0, ID: "call_1", Name: "get_weather", ArgumentsJSON: `{"city":"Boston"}`},
					},
				},
				FinishReason: &finishReason,
			},
		},
		Usage: &adapter.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}

	if err := sw.WriteChunk(chunk); err != nil {
		t.Fatalf("WriteChunk() error = %v", err)
	}

	const want = "data: {\"id\":\"chunk-1\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"name\":\"get_weather\",\"arguments_json\":\"{\\\"city\\\":\\\"Boston\\\"}\"}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("golden SSE byte output mismatch:\n got:  %q\n want: %q", got, want)
	}
}

// TestWriteChunkGoldenNilFinishReasonAndUsage pins two absence/null
// distinctions that are real client-compatibility requirements, not
// incidental: FinishReason has no `omitempty` tag (see types.go) so a nil
// value must serialize as a literal JSON null — OpenAI's own wire format
// always sends "finish_reason" on every chunk, and a client parser may
// assume the key exists. Usage DOES have `omitempty`, so a nil Usage must
// be dropped entirely, not sent as null — a provider that never supplies
// usage during streaming must not fabricate a zero-usage lie on every
// chunk.
func TestWriteChunkGoldenNilFinishReasonAndUsage(t *testing.T) {
	rec := newFlushCountingRecorder()
	sw, err := NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter() error = %v", err)
	}

	chunk := ChatCompletionChunk{
		ID:    "chunk-2",
		Model: "gpt-4o",
		Choices: []ChunkChoice{
			{Index: 0, Delta: MessageDelta{Content: "partial"}, FinishReason: nil},
		},
	}
	if err := sw.WriteChunk(chunk); err != nil {
		t.Fatalf("WriteChunk() error = %v", err)
	}

	const want = "data: {\"id\":\"chunk-2\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("golden SSE byte output mismatch:\n got:  %q\n want: %q", got, want)
	}
}
