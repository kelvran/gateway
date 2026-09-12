package gemini

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
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

// TestDecodeFinalUsageChunkExtractsRealCachedTokens proves the streaming
// path populates CacheReadTokens too, not just the buffered FromProvider
// path -- both share the same native UsageMetadata type and usageFromNative
// helper, but this is the load-bearing proof they actually reach it, not
// two independently-drifting copies.
func TestDecodeFinalUsageChunkExtractsRealCachedTokens(t *testing.T) {
	dec := New().NewStreamDecoder()

	_, _, finalUsage, err := dec.Decode(streaming.SSEEvent{
		Data: `{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1024,"candidatesTokenCount":10,"totalTokenCount":1034,"cachedContentTokenCount":896}}`,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if finalUsage == nil {
		t.Fatal("finalUsage = nil, want non-nil")
	}
	want := adapter.Usage{PromptTokens: 1024, CompletionTokens: 10, TotalTokens: 1034, CacheReadTokens: 896}
	if *finalUsage != want {
		t.Errorf("finalUsage = %+v, want %+v", *finalUsage, want)
	}
}

// TestDecodeThoughtPartRoutesToReasoningBlocksDeltaInsteadOfMerging is the
// streaming-path counterpart of
// gemini_test.go's TestFromProviderSeparatesThoughtPartsFromAnswerTextInsteadOfMerging:
// before this fix, Decode's part-dispatch switch had no case for
// part.Thought, so a thought part's Text fell into the ordinary
// `case part.Text != ""` branch and was silently concatenated into
// Delta.Content alongside the real answer, indistinguishable from it,
// per docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md.
// Delta.Content must contain ONLY the real answer, and Delta.ReasoningBlocks
// must contain ONLY the thought's own Text/Signature.
func TestDecodeThoughtPartRoutesToReasoningBlocksDeltaInsteadOfMerging(t *testing.T) {
	dec := New().NewStreamDecoder()

	chunks, _, _, err := dec.Decode(streaming.SSEEvent{
		Data: `{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"Let me think about this carefully.","thoughtSignature":"sig_thought_1"},{"text":"The answer is 42."}]},"finishReason":"STOP"}]}`,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("len(chunks) = %d, want 1", len(chunks))
	}

	gotContent := chunks[0].Choices[0].Delta.Content
	if gotContent != "The answer is 42." {
		t.Errorf("Delta.Content = %q, want %q -- the thought part's text must NOT be merged into the visible answer delta", gotContent, "The answer is 42.")
	}
	if strings.Contains(gotContent, "think about this") {
		t.Errorf("Delta.Content = %q, contains thought text -- the merge bug is still present in the streaming path", gotContent)
	}

	gotReasoning := chunks[0].Choices[0].Delta.ReasoningBlocks
	if len(gotReasoning) != 1 {
		t.Fatalf("Delta.ReasoningBlocks len = %d, want 1", len(gotReasoning))
	}
	rb := gotReasoning[0]
	if rb.Text != "Let me think about this carefully." {
		t.Errorf("Delta.ReasoningBlocks[0].Text = %q, want the thought part's own text", rb.Text)
	}
	if rb.Signature != "sig_thought_1" {
		t.Errorf("Delta.ReasoningBlocks[0].Signature = %q, want %q", rb.Signature, "sig_thought_1")
	}
	if rb.Redacted || rb.Data != "" {
		t.Errorf("Delta.ReasoningBlocks[0] = %+v, want Redacted=false and empty Data -- Gemini's schema carries no redacted-thought concept", rb)
	}
	if rb.Index != 0 {
		t.Errorf("Delta.ReasoningBlocks[0].Index = %d, want 0 (the thought part's own position in this chunk's Parts array)", rb.Index)
	}
}

// TestDecodeCapturesSignatureFusedOntoFunctionCallPart is the streaming
// counterpart of gemini_test.go's
// TestFromProviderCapturesSignatureFusedOntoFunctionCallPart: per
// ai.google.dev/gemini-api/docs/thinking#signatures, "signatures are
// metadata that can be attached to any part, such as living inside
// functionCall parts" -- a functionCall part carrying its own
// thoughtSignature (no separate thought:true part) must still surface
// that signature via Delta.ReasoningBlocks, not silently drop it.
func TestDecodeCapturesSignatureFusedOntoFunctionCallPart(t *testing.T) {
	dec := New().NewStreamDecoder()

	chunks, _, _, err := dec.Decode(streaming.SSEEvent{
		Data: `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Boston"}},"thoughtSignature":"sig_fused_on_call"}]}}]}`,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("len(chunks) = %d, want 1", len(chunks))
	}

	delta := chunks[0].Choices[0].Delta
	if len(delta.ToolCalls) != 1 || delta.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("Delta.ToolCalls = %+v, want the functionCall captured normally", delta.ToolCalls)
	}
	if len(delta.ReasoningBlocks) != 1 {
		t.Fatalf("Delta.ReasoningBlocks len = %d, want 1 -- the functionCall-fused signature must not be silently dropped", len(delta.ReasoningBlocks))
	}
	rb := delta.ReasoningBlocks[0]
	if rb.Signature != "sig_fused_on_call" {
		t.Errorf("Delta.ReasoningBlocks[0].Signature = %q, want %q", rb.Signature, "sig_fused_on_call")
	}
	if rb.Text != "" {
		t.Errorf("Delta.ReasoningBlocks[0].Text = %q, want empty", rb.Text)
	}
	if rb.Index != 0 {
		t.Errorf("Delta.ReasoningBlocks[0].Index = %d, want 0 (this part's own position)", rb.Index)
	}
}

// TestDecodePromptBlockedBySafetyFilteringReturnsError proves the mid-
// stream counterpart of
// gemini_test.go's TestFromProviderPromptBlockedBySafetyFilteringWrapsContentPolicySentinel:
// a chunk carrying zero candidates PLUS a populated
// promptFeedback.blockReason must be a real, classifiable error, never
// silently treated as an ordinary empty keep-alive/usage-only frame.
// Sanity-checked-by-breaking: before this fix, this exact input produced
// (nil, false, nil, nil) — a silent no-op — which would have let the
// dataplane's streaming loop finish the request as an empty, successful
// response with no error and no chance for attemptFallbackChain to ever
// run.
func TestDecodePromptBlockedBySafetyFilteringReturnsError(t *testing.T) {
	dec := New().NewStreamDecoder()

	chunks, done, usage, err := dec.Decode(streaming.SSEEvent{
		Data: `{"promptFeedback":{"blockReason":"SAFETY"}}`,
	})
	if err == nil {
		t.Fatal("Decode: want error for a prompt-blocked chunk, got nil")
	}
	if !errors.Is(err, adapter.ErrProviderContentPolicyBlocked) {
		t.Errorf("Decode: err = %v, want it to wrap adapter.ErrProviderContentPolicyBlocked", err)
	}
	if chunks != nil || done || usage != nil {
		t.Errorf("Decode: got (chunks=%v, done=%v, usage=%v) alongside a real error, want all zero-valued", chunks, done, usage)
	}
}

// TestDecodeEmptyCandidatesWithoutBlockReasonIsStillABenignKeepAlive
// proves the fix is narrowly scoped: an ordinary chunk with zero
// candidates and NO blockReason (a genuine keep-alive, or a
// usage-metadata-only chunk) must remain a silent no-op, exactly as
// before this fix.
func TestDecodeEmptyCandidatesWithoutBlockReasonIsStillABenignKeepAlive(t *testing.T) {
	dec := New().NewStreamDecoder()

	chunks, done, usage, err := dec.Decode(streaming.SSEEvent{Data: `{}`})
	if err != nil {
		t.Fatalf("Decode: %v, want nil for a benign empty chunk", err)
	}
	if chunks != nil || done || usage != nil {
		t.Errorf("Decode: got (chunks=%v, done=%v, usage=%v), want all zero-valued for a benign empty chunk", chunks, done, usage)
	}
}
