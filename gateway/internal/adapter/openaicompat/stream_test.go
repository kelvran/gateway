package openaicompat

import (
	"errors"
	"io"
	"os"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// TestNewStreamDecoderSatisfiesStreamingAdapter proves *Adapter satisfies
// streaming.StreamingAdapter, which is the whole point of this file — a
// compile-time check made explicit at runtime so a future accidental
// signature drift fails a test, not just a downstream build.
func TestNewStreamDecoderSatisfiesStreamingAdapter(t *testing.T) {
	var _ streaming.StreamingAdapter = New()
}

// decodeFixture drives a fixture file through the REAL streaming.Reader,
// feeding every resulting SSEEvent to a fresh decoder's Decode, and
// collects every canonical chunk/done/finalUsage observation along the
// way. This proves the whole pipeline (SSE framing + decoding), not just
// the decoder in isolation.
type decodeResult struct {
	chunks     []streaming.ChatCompletionChunk
	done       bool
	doneAtCall int // index (0-based) of the Decode call that returned done=true, or -1
	finalUsage *adapter.Usage
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
	result.doneAtCall = -1

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
		result.chunks = append(result.chunks, chunks...)
		if usage != nil {
			result.finalUsage = usage
		}
		if done {
			result.done = true
			result.doneAtCall = i
			break // per the StreamDecoder contract: never call Decode after done.
		}
	}

	return result
}

// TestDecodeTextOnlyCompletion feeds a real multi-chunk text-only stream
// (role delta, several content deltas, a finish_reason chunk, then
// [DONE]) through the real reader+decoder pipeline and asserts the exact
// canonical chunks produced.
func TestDecodeTextOnlyCompletion(t *testing.T) {
	result := decodeFixture(t, "stream_text.txt")

	if !result.done {
		t.Fatal("done = false, want true (stream ends with [DONE])")
	}
	if result.finalUsage != nil {
		t.Errorf("finalUsage = %+v, want nil (this fixture never sends usage)", result.finalUsage)
	}

	// 5 chunk-bearing events before [DONE]: role, "Howdy", ", partner", "!",
	// finish_reason:"stop".
	if len(result.chunks) != 5 {
		t.Fatalf("len(chunks) = %d, want 5", len(result.chunks))
	}

	wantContent := []string{"", "Howdy", ", partner", "!", ""}
	for i, want := range wantContent {
		c := result.chunks[i]
		if len(c.Choices) != 1 {
			t.Fatalf("chunk %d: len(Choices) = %d, want 1", i, len(c.Choices))
		}
		if got := c.Choices[0].Delta.Content; got != want {
			t.Errorf("chunk %d: Delta.Content = %q, want %q", i, got, want)
		}
	}

	// First chunk carries the role; later ones don't.
	if got := result.chunks[0].Choices[0].Delta.Role; got != "assistant" {
		t.Errorf("chunk 0: Delta.Role = %q, want %q", got, "assistant")
	}
	for i := 1; i < 4; i++ {
		if got := result.chunks[i].Choices[0].Delta.Role; got != "" {
			t.Errorf("chunk %d: Delta.Role = %q, want empty", i, got)
		}
	}

	// Every chunk shares the same ID/Model.
	for i, c := range result.chunks {
		if c.ID != "chatcmpl-local-stream1" {
			t.Errorf("chunk %d: ID = %q, want the shared completion ID", i, c.ID)
		}
		if c.Model != "llama-3.1-70b-instruct" {
			t.Errorf("chunk %d: Model = %q, want %q", i, c.Model, "llama-3.1-70b-instruct")
		}
	}

	// Only the last chunk carries a finish_reason, and it's "stop".
	last := result.chunks[len(result.chunks)-1]
	if last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != "stop" {
		t.Errorf("last chunk FinishReason = %v, want \"stop\"", last.Choices[0].FinishReason)
	}
	for i := 0; i < len(result.chunks)-1; i++ {
		if result.chunks[i].Choices[0].FinishReason != nil {
			t.Errorf("chunk %d: FinishReason = %v, want nil", i, *result.chunks[i].Choices[0].FinishReason)
		}
	}
}

// TestDecodeToolCallAccumulatesArgumentFragments is the load-bearing test
// for this decoder: it proves a single tool call's "arguments" string,
// split across several chunks and keyed by delta.tool_calls[].index,
// comes through Decode as fragments that concatenate (in order) to the
// exact original JSON — real cross-call correctness, not just a single-
// event happy path.
func TestDecodeToolCallAccumulatesArgumentFragments(t *testing.T) {
	result := decodeFixture(t, "stream_tool_call.txt")

	if !result.done {
		t.Fatal("done = false, want true")
	}

	// 6 chunk-bearing events: role, tool-call-intro, 3 argument fragments,
	// finish_reason:"tool_calls".
	if len(result.chunks) != 6 {
		t.Fatalf("len(chunks) = %d, want 6", len(result.chunks))
	}

	// Chunk 0: role only, no tool calls yet.
	if len(result.chunks[0].Choices[0].Delta.ToolCalls) != 0 {
		t.Fatalf("chunk 0: expected no tool calls yet, got %+v", result.chunks[0].Choices[0].Delta.ToolCalls)
	}

	// Chunk 1 introduces the tool call: ID + Name present, first (empty)
	// arguments fragment.
	intro := result.chunks[1].Choices[0].Delta.ToolCalls
	if len(intro) != 1 {
		t.Fatalf("chunk 1: len(ToolCalls) = %d, want 1", len(intro))
	}
	if intro[0].Index != 0 {
		t.Errorf("chunk 1: ToolCalls[0].Index = %d, want 0", intro[0].Index)
	}
	if intro[0].ID != "call_lVzR9tKpQwErAlLm5Nx0Qb1c" {
		t.Errorf("chunk 1: ToolCalls[0].ID = %q, want the native call ID", intro[0].ID)
	}
	if intro[0].Name != "get_weather" {
		t.Errorf("chunk 1: ToolCalls[0].Name = %q, want %q", intro[0].Name, "get_weather")
	}

	// Chunks 2-4 carry the argument fragments, and must NOT re-send
	// ID/Name (only the intro chunk carries them).
	var accumulated string
	for i := 1; i <= 4; i++ {
		tcs := result.chunks[i].Choices[0].Delta.ToolCalls
		if len(tcs) != 1 {
			t.Fatalf("chunk %d: len(ToolCalls) = %d, want 1", i, len(tcs))
		}
		if i > 1 {
			if tcs[0].ID != "" {
				t.Errorf("chunk %d: ToolCalls[0].ID = %q, want empty (only the intro chunk carries it)", i, tcs[0].ID)
			}
			if tcs[0].Name != "" {
				t.Errorf("chunk %d: ToolCalls[0].Name = %q, want empty", i, tcs[0].Name)
			}
		}
		if tcs[0].Index != 0 {
			t.Errorf("chunk %d: ToolCalls[0].Index = %d, want 0", i, tcs[0].Index)
		}
		accumulated += tcs[0].ArgumentsJSON
	}

	const wantArgs = `{"city": "Austin"}`
	if accumulated != wantArgs {
		t.Errorf("accumulated ArgumentsJSON = %q, want %q", accumulated, wantArgs)
	}

	// Final chunk carries finish_reason "tool_calls" and no tool-call delta.
	last := result.chunks[5]
	if len(last.Choices[0].Delta.ToolCalls) != 0 {
		t.Errorf("final chunk: expected no tool calls, got %+v", last.Choices[0].Delta.ToolCalls)
	}
	if last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("final chunk FinishReason = %v, want \"tool_calls\"", last.Choices[0].FinishReason)
	}
}

// TestDecodeFinalUsageChunk proves usage is extracted the moment it's
// observed in the raw event stream — the final, empty-choices chunk sent
// when stream_options.include_usage is set, per the ASSUMPTION documented
// on streamDecoder in stream.go.
func TestDecodeFinalUsageChunk(t *testing.T) {
	result := decodeFixture(t, "stream_usage.txt")

	if !result.done {
		t.Fatal("done = false, want true")
	}
	if result.finalUsage == nil {
		t.Fatal("finalUsage = nil, want non-nil")
	}
	want := adapter.Usage{PromptTokens: 14, CompletionTokens: 1, TotalTokens: 15}
	if *result.finalUsage != want {
		t.Errorf("finalUsage = %+v, want %+v", *result.finalUsage, want)
	}

	// 4 chunk-bearing events: role, content "7", finish_reason:"stop",
	// then the usage-only chunk (empty Choices).
	if len(result.chunks) != 4 {
		t.Fatalf("len(chunks) = %d, want 4", len(result.chunks))
	}
	usageChunk := result.chunks[3]
	if len(usageChunk.Choices) != 0 {
		t.Errorf("usage chunk: len(Choices) = %d, want 0", len(usageChunk.Choices))
	}
	if usageChunk.Usage == nil || *usageChunk.Usage != want {
		t.Errorf("usage chunk: Usage = %v, want %+v", usageChunk.Usage, want)
	}
}

// TestDecodeDoneSentinel explicitly exercises the "[DONE]" sentinel path
// in isolation: a stream that is nothing but the sentinel must decode to
// zero chunks, done=true, nil usage, nil error — and the literal string
// must be recognized as-is, never handed to json.Unmarshal (it isn't
// valid JSON).
func TestDecodeDoneSentinel(t *testing.T) {
	result := decodeFixture(t, "stream_done_only.txt")

	if !result.done {
		t.Fatal("done = false, want true")
	}
	if result.doneAtCall != 0 {
		t.Errorf("doneAtCall = %d, want 0 (the only event is [DONE])", result.doneAtCall)
	}
	if len(result.chunks) != 0 {
		t.Errorf("len(chunks) = %d, want 0", len(result.chunks))
	}
	if result.finalUsage != nil {
		t.Errorf("finalUsage = %+v, want nil", result.finalUsage)
	}
}

// TestDecodeAfterDoneReturnsError proves the decoder's cross-call done
// state is enforced: calling Decode again after it has already reported
// done=true must fail loudly rather than silently mis-parsing.
func TestDecodeAfterDoneReturnsError(t *testing.T) {
	dec := New().NewStreamDecoder()

	_, done, _, err := dec.Decode(streaming.SSEEvent{Data: doneSentinel})
	if err != nil {
		t.Fatalf("first Decode([DONE]) error = %v, want nil", err)
	}
	if !done {
		t.Fatal("first Decode([DONE]) done = false, want true")
	}

	_, _, _, err = dec.Decode(streaming.SSEEvent{Data: `{"id":"x","model":"llama-3.1-70b-instruct","choices":[]}`})
	if err == nil {
		t.Fatal("second Decode call after [DONE] returned nil error, want an error")
	}
}
