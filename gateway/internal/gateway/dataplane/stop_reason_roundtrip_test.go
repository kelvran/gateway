package dataplane

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// TestWriteFakeStreamCarriesStopReasonAndStopSequence: a cached response's
// native stop reason and sequence ride on the replayed finish chunk, so a
// streamed cache hit carries what a live stream would (item 11 S3).
func TestWriteFakeStreamCarriesStopReasonAndStopSequence(t *testing.T) {
	resp := adapter.ChatResponse{
		Model:        "claude-sonnet-5-5",
		Choices:      []adapter.Choice{{Index: 0, Message: adapter.Message{Role: "assistant", Content: "one two "}, FinishReason: "stop"}},
		Usage:        adapter.Usage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15},
		StopReason:   "stop_sequence",
		StopSequence: "###",
	}
	rec := httptest.NewRecorder()
	sw, err := streaming.NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := writeFakeStream(sw, resp, pinnedCreated); err != nil {
		t.Fatalf("writeFakeStream: %v", err)
	}
	var found bool
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.HasSuffix(line, "[DONE]") {
			continue
		}
		var chunk streaming.ChatCompletionChunk
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("decoding frame %q: %v", line, err)
		}
		for _, c := range chunk.Choices {
			if c.FinishReason != nil {
				found = true
				if c.StopReason != "stop_sequence" || c.StopSequence != "###" {
					t.Errorf("finish chunk StopReason/StopSequence = %q/%q, want stop_sequence/###", c.StopReason, c.StopSequence)
				}
			}
		}
	}
	if !found {
		t.Fatal("no replayed chunk carried a finish_reason")
	}
}

// TestStreamAccumulatorRecordsStopReasonAndStopSequence: the accumulator
// folds the finish chunk's native stop reason and sequence back into the
// canonical response it writes to the cache (item 11 S3).
func TestStreamAccumulatorRecordsStopReasonAndStopSequence(t *testing.T) {
	acc := newStreamAccumulator()
	acc.add(streaming.ChatCompletionChunk{ID: "msg_1", Model: "claude-sonnet-5-5", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Role: "assistant", Content: "one two "}}}})
	acc.add(streaming.ChatCompletionChunk{Choices: []streaming.ChunkChoice{{Index: 0, FinishReason: strPtr("stop"), StopReason: "stop_sequence", StopSequence: "###"}}})
	got := acc.build(adapter.Usage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15})
	if got.StopReason != "stop_sequence" || got.StopSequence != "###" {
		t.Errorf("StopReason/StopSequence = %q/%q, want stop_sequence/###", got.StopReason, got.StopSequence)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", got.Choices[0].FinishReason)
	}
}
