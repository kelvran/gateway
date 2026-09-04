package gemini

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// TestNewStreamDecoderSatisfiesStreamingAdapter proves *Adapter satisfies
// streaming.StreamingAdapter — a compile-time check made explicit at
// runtime so a future accidental signature drift fails a test, not just a
// downstream build.
func TestNewStreamDecoderSatisfiesStreamingAdapter(t *testing.T) {
	var _ streaming.StreamingAdapter = New()
}

// decodeResult collects every canonical chunk/done/finalUsage observation
// from driving a fixture through the REAL streaming.Reader + a fresh
// decoder's Decode. Unlike openai's/anthropic's equivalent helper, this
// one never breaks early on done=true — Gemini's decoder always reports
// done=false (see stream.go's package doc), so the loop runs until the
// reader itself reports io.EOF, exactly mirroring how the real dataplane
// streaming loop consumes it.
type decodeResult struct {
	chunks     []streaming.ChatCompletionChunk
	finalUsage *adapter.Usage
	sawDone    bool
}

func decodeFixture(t *testing.T, name string) decodeResult {
	t.Helper()

	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("opening testdata/%s: %v", name, err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			t.Errorf("closing testdata/%s: %v", name, closeErr)
		}
	}()

	r := streaming.NewReader(f)
	dec := New().NewStreamDecoder()

	var result decodeResult

	for i := 0; ; i++ {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading SSE event %d: %v", i, err)
		}

		chunks, done, usage, err := dec.Decode(ev)
		if err != nil {
			t.Fatalf("Decode event %d (%q): %v", i, ev.Data, err)
		}
		if done {
			result.sawDone = true
		}
		result.chunks = append(result.chunks, chunks...)
		if usage != nil {
			result.finalUsage = usage
		}
	}

	return result
}

// TestDecodeNeverSignalsDone is the load-bearing proof for this decoder's
// central, real divergence from OpenAI's/Anthropic's contract: Gemini has
// no terminal sentinel, so Decode must always report done=false, even on
// the chunk carrying a terminal finishReason — the dataplane's own
// io.EOF-driven loop (confirmed by direct read) is what ends the stream.
func TestDecodeNeverSignalsDone(t *testing.T) {
	result := decodeFixture(t, "stream_text.txt")
	if result.sawDone {
		t.Fatal("Decode reported done=true; Gemini's decoder must never do this")
	}
}

// TestDecodeTextOnlyCompletion feeds a real multi-chunk Gemini text stream
// through the real reader+decoder pipeline and asserts the exact canonical
// chunks produced, including the role-once-only contract.
func TestDecodeTextOnlyCompletion(t *testing.T) {
	result := decodeFixture(t, "stream_text.txt")

	if len(result.chunks) != 3 {
		t.Fatalf("len(chunks) = %d, want 3", len(result.chunks))
	}

	wantContent := []string{"Hello", ", world!", ""}
	for i, want := range wantContent {
		c := result.chunks[i]
		if len(c.Choices) != 1 {
			t.Fatalf("chunk %d: len(Choices) = %d, want 1", i, len(c.Choices))
		}
		if got := c.Choices[0].Delta.Content; got != want {
			t.Errorf("chunk %d: Delta.Content = %q, want %q", i, got, want)
		}
	}

	// Role is set once, on the first chunk only — mirroring anthropic's
	// contract even though Gemini's own wire format carries no role field
	// on stream events at all.
	if got := result.chunks[0].Choices[0].Delta.Role; got != "assistant" {
		t.Errorf("chunk 0: Delta.Role = %q, want %q", got, "assistant")
	}
	for i := 1; i < len(result.chunks); i++ {
		if got := result.chunks[i].Choices[0].Delta.Role; got != "" {
			t.Errorf("chunk %d: Delta.Role = %q, want empty", i, got)
		}
	}

	last := result.chunks[len(result.chunks)-1]
	if last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != "stop" {
		t.Errorf("last chunk FinishReason = %v, want %q", last.Choices[0].FinishReason, "stop")
	}

	if result.finalUsage == nil {
		t.Fatal("finalUsage = nil, want non-nil (final chunk carries usageMetadata)")
	}
	want := adapter.Usage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13}
	if *result.finalUsage != want {
		t.Errorf("finalUsage = %+v, want %+v", *result.finalUsage, want)
	}
}

// TestDecodeFunctionCallChunk proves the real, documented divergence from
// OpenAI's fragment-accumulation contract: Gemini's functionCall.args
// arrives as a complete object in one chunk, so ArgumentsJSON must already
// be the full, valid JSON string on the single chunk that carries it —
// never a partial fragment a caller needs to accumulate further.
func TestDecodeFunctionCallChunk(t *testing.T) {
	result := decodeFixture(t, "stream_tool_call.txt")

	if len(result.chunks) != 1 {
		t.Fatalf("len(chunks) = %d, want 1", len(result.chunks))
	}
	tcs := result.chunks[0].Choices[0].Delta.ToolCalls
	if len(tcs) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(tcs))
	}
	if tcs[0].ID != "call_1" || tcs[0].Name != "get_weather" {
		t.Errorf("ToolCalls[0] = %+v, want ID=call_1 Name=get_weather", tcs[0])
	}
	if !json.Valid([]byte(tcs[0].ArgumentsJSON)) {
		t.Fatalf("ArgumentsJSON is not valid JSON: %q", tcs[0].ArgumentsJSON)
	}

	if result.chunks[0].Choices[0].FinishReason == nil || *result.chunks[0].Choices[0].FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %v, want %q (STOP+functionCall)", result.chunks[0].Choices[0].FinishReason, "tool_calls")
	}
}

// TestDecodeFinalUsageChunk proves usage arriving on a chunk with no
// candidate text content (only finishReason) is still correctly extracted.
func TestDecodeFinalUsageChunk(t *testing.T) {
	result := decodeFixture(t, "stream_usage.txt")

	if result.finalUsage == nil {
		t.Fatal("finalUsage = nil, want non-nil")
	}
	want := adapter.Usage{PromptTokens: 19, CompletionTokens: 2, TotalTokens: 21}
	if *result.finalUsage != want {
		t.Errorf("finalUsage = %+v, want %+v", *result.finalUsage, want)
	}
}
