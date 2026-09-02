package anthropic

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/kelvran/gateway/internal/adapter"
	"github.com/kelvran/gateway/internal/streaming"
)

// decodeFixture feeds every raw SSE event in the file at path through a
// real streaming.Reader and a fresh Anthropic StreamDecoder — exercising
// the whole reader+decoder pipeline exactly as the dataplane would, not
// just the decoder in isolation. It returns every canonical chunk emitted
// (in arrival order), whether Decode ever reported done=true, the last
// non-nil finalUsage observed, and the number of raw events consumed.
func decodeFixture(t *testing.T, path string) (chunks []streaming.ChatCompletionChunk, done bool, finalUsage *adapter.Usage, eventCount int) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening fixture %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	decoder := New().NewStreamDecoder()
	reader := streaming.NewReader(f)

	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reader.Next(): %v", err)
		}
		eventCount++

		if done {
			t.Fatalf("Decode called again after done=true (event #%d: %+v) — callers must stop calling Decode once the stream is logically complete", eventCount, ev)
		}

		got, isDone, usage, decErr := decoder.Decode(ev)
		if decErr != nil {
			t.Fatalf("Decode(%+v) error = %v", ev, decErr)
		}
		chunks = append(chunks, got...)
		if usage != nil {
			finalUsage = usage
		}
		if isDone {
			done = true
		}
	}

	return chunks, done, finalUsage, eventCount
}

// TestStreamDecoder_TextOnly proves the basic text-only path: message_start
// captures id/model, the first content_block_start stamps Role exactly
// once, content_block_delta text_delta fragments accumulate into the full
// text, content_block_stop is silent, and message_delta/message_stop
// supply finalUsage/done respectively. It also proves the ping keep-alive
// event is silently ignored rather than erroring or emitting a chunk.
func TestStreamDecoder_TextOnly(t *testing.T) {
	chunks, done, usage, eventCount := decodeFixture(t, "testdata/stream_text_only.txt")

	const wantEventCount = 9 // message_start, ping, start, 3x delta, stop, message_delta, message_stop
	if eventCount != wantEventCount {
		t.Fatalf("eventCount = %d, want %d", eventCount, wantEventCount)
	}
	if !done {
		t.Fatal("done = false, want true after message_stop")
	}

	// content_block_start + 3 content_block_delta events + message_delta
	// (which carries the final FinishReason, per the "message_delta is the
	// only event that ever carries stop_reason" regression test below)
	// each produce exactly one chunk; message_start, ping,
	// content_block_stop, message_stop must not.
	if len(chunks) != 5 {
		t.Fatalf("len(chunks) = %d, want 5; chunks = %+v", len(chunks), chunks)
	}

	first := chunks[0]
	if first.ID != "msg_01Text0nly000000000000" {
		t.Errorf("chunks[0].ID = %q, want the id captured from message_start", first.ID)
	}
	if first.Model != "claude-opus-4-20250514" {
		t.Errorf("chunks[0].Model = %q, want the model captured from message_start", first.Model)
	}
	if first.Choices[0].Delta.Role != "assistant" {
		t.Errorf("chunks[0].Delta.Role = %q, want %q (first chunk of the whole stream)", first.Choices[0].Delta.Role, "assistant")
	}
	if first.Choices[0].Delta.Content != "" {
		t.Errorf("chunks[0].Delta.Content = %q, want empty (content_block_start for text carries no text itself)", first.Choices[0].Delta.Content)
	}

	var text string
	for i, c := range chunks[1:] {
		if c.Choices[0].Delta.Role != "" {
			t.Errorf("chunks[%d].Delta.Role = %q, want empty — Role must be set only once, on the very first chunk of the stream", i+1, c.Choices[0].Delta.Role)
		}
		text += c.Choices[0].Delta.Content
	}
	const wantText = "The sky is blue today."
	if text != wantText {
		t.Errorf("accumulated text = %q, want %q", text, wantText)
	}

	if usage == nil {
		t.Fatal("finalUsage = nil, want non-nil after message_delta")
	}
	want := adapter.Usage{PromptTokens: 12, CompletionTokens: 9, TotalTokens: 21}
	if *usage != want {
		t.Errorf("finalUsage = %+v, want %+v", *usage, want)
	}
}

// TestStreamDecoder_SingleToolCallMultiChunkArgs proves the decoder
// correctly accumulates a single tool call's input_json_delta fragments,
// arriving across multiple Decode calls, into one reconstructable JSON
// string keyed by the tool call's content-block index.
func TestStreamDecoder_SingleToolCallMultiChunkArgs(t *testing.T) {
	chunks, done, usage, _ := decodeFixture(t, "testdata/stream_single_tool_call.txt")

	if !done {
		t.Fatal("done = false, want true after message_stop")
	}
	// content_block_start + 3 content_block_delta events + message_delta
	// (the final FinishReason-carrying chunk — see the dedicated
	// stop_reason regression test below).
	if len(chunks) != 5 {
		t.Fatalf("len(chunks) = %d, want 5; chunks = %+v", len(chunks), chunks)
	}

	start := chunks[0].Choices[0].Delta
	if start.Role != "assistant" {
		t.Errorf("chunks[0].Delta.Role = %q, want %q", start.Role, "assistant")
	}
	if len(start.ToolCalls) != 1 {
		t.Fatalf("chunks[0].Delta.ToolCalls = %+v, want exactly one entry", start.ToolCalls)
	}
	tc := start.ToolCalls[0]
	if tc.Index != 0 {
		t.Errorf("chunks[0].Delta.ToolCalls[0].Index = %d, want 0", tc.Index)
	}
	if tc.ID != "toolu_01WeatherAAA00000000" {
		t.Errorf("chunks[0].Delta.ToolCalls[0].ID = %q, want the id from content_block_start", tc.ID)
	}
	if tc.Name != "get_weather" {
		t.Errorf("chunks[0].Delta.ToolCalls[0].Name = %q, want %q", tc.Name, "get_weather")
	}

	// The final chunk is message_delta's FinishReason-carrying chunk, not
	// an argument fragment — excluded from this loop and asserted on
	// separately below.
	argFragmentChunks := chunks[1 : len(chunks)-1]
	var argsJSON string
	for i, c := range argFragmentChunks {
		toolCalls := c.Choices[0].Delta.ToolCalls
		if len(toolCalls) != 1 {
			t.Fatalf("chunks[%d].Delta.ToolCalls = %+v, want exactly one fragment", i+1, toolCalls)
		}
		if toolCalls[0].Index != 0 {
			t.Errorf("chunks[%d].Delta.ToolCalls[0].Index = %d, want 0", i+1, toolCalls[0].Index)
		}
		if toolCalls[0].ID != "" || toolCalls[0].Name != "" {
			t.Errorf("chunks[%d].Delta.ToolCalls[0] carries ID/Name on a delta-only chunk: %+v", i+1, toolCalls[0])
		}
		argsJSON += toolCalls[0].ArgumentsJSON
	}

	last := chunks[len(chunks)-1]
	if len(last.Choices[0].Delta.ToolCalls) != 0 {
		t.Errorf("final chunk carries ToolCalls %+v, want none (it's message_delta's FinishReason chunk)", last.Choices[0].Delta.ToolCalls)
	}
	if fr := last.Choices[0].FinishReason; fr == nil || *fr != "tool_calls" {
		t.Errorf("final chunk FinishReason = %v, want %q (mapped from stop_reason=tool_use)", fr, "tool_calls")
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &parsed); err != nil {
		t.Fatalf("accumulated ArgumentsJSON %q did not parse as JSON: %v", argsJSON, err)
	}
	if parsed["location"] != "Boston" {
		t.Errorf("accumulated arguments = %v, want location=Boston", parsed)
	}

	if usage == nil {
		t.Fatal("finalUsage = nil, want non-nil")
	}
	want := adapter.Usage{PromptTokens: 34, CompletionTokens: 31, TotalTokens: 65}
	if *usage != want {
		t.Errorf("finalUsage = %+v, want %+v", *usage, want)
	}
}

// TestStreamDecoder_TwoToolCallsInterleaved is the decisive test for the
// decoder's statefulness: two tool_use blocks (index 1 and index 2) are
// opened, and their input_json_delta fragments arrive genuinely
// interleaved (index 1, then index 2 opens mid-stream, then a fragment
// for index 2, then back to index 1, then back to index 2 again) around a
// text block at index 0. A decoder that tracks only "the current tool
// call" instead of a real per-index map would misattribute fragments
// across the two tool calls the moment they interleave — this proves that
// does not happen, because each ToolCallDelta.Index and its accumulated
// ArgumentsJSON are asserted independently, per index, and cross-checked
// for the *absence* of the other tool call's content.
func TestStreamDecoder_TwoToolCallsInterleaved(t *testing.T) {
	chunks, done, usage, _ := decodeFixture(t, "testdata/stream_two_tool_calls_interleaved.txt")

	if !done {
		t.Fatal("done = false, want true after message_stop")
	}
	// idx0: start + 2 deltas = 3 chunks.
	// idx1: start + 2 deltas (interleaved with idx2) = 3 chunks.
	// idx2: start + 2 deltas (interleaved with idx1) = 3 chunks.
	// + message_delta's FinishReason-carrying chunk = 1 chunk.
	if len(chunks) != 10 {
		t.Fatalf("len(chunks) = %d, want 10; chunks = %+v", len(chunks), chunks)
	}

	var (
		rolesSent int
		text      string
		args      = map[int]string{}
		ids       = map[int]string{}
		names     = map[int]string{}
	)

	for i, c := range chunks {
		delta := c.Choices[0].Delta
		if delta.Role != "" {
			rolesSent++
			if i != 0 {
				t.Errorf("chunks[%d] carries Delta.Role, want it set only on chunks[0]", i)
			}
		}
		text += delta.Content
		for _, tc := range delta.ToolCalls {
			if tc.ID != "" {
				ids[tc.Index] = tc.ID
			}
			if tc.Name != "" {
				names[tc.Index] = tc.Name
			}
			args[tc.Index] += tc.ArgumentsJSON
		}
	}

	if rolesSent != 1 {
		t.Errorf("rolesSent = %d, want exactly 1 (only the very first chunk of the whole stream)", rolesSent)
	}
	const wantText = "Let me check the weather and time."
	if text != wantText {
		t.Errorf("accumulated text (index 0) = %q, want %q", text, wantText)
	}

	if ids[1] != "toolu_01WeatherBBB00000000" {
		t.Errorf("ids[1] = %q, want the get_weather tool_use id", ids[1])
	}
	if names[1] != "get_weather" {
		t.Errorf("names[1] = %q, want %q", names[1], "get_weather")
	}
	if ids[2] != "toolu_01TimeZoneCCC0000000" {
		t.Errorf("ids[2] = %q, want the get_time tool_use id", ids[2])
	}
	if names[2] != "get_time" {
		t.Errorf("names[2] = %q, want %q", names[2], "get_time")
	}

	const wantArgs1 = `{"location": "Boston"}`
	const wantArgs2 = `{"timezone": "EST"}`
	if args[1] != wantArgs1 {
		t.Errorf("args[1] = %q, want %q", args[1], wantArgs1)
	}
	if args[2] != wantArgs2 {
		t.Errorf("args[2] = %q, want %q", args[2], wantArgs2)
	}

	// Cross-contamination guard: a decoder that mixed up which index an
	// interleaved fragment belonged to would leak one tool call's content
	// into the other's accumulated arguments.
	if strings.Contains(args[1], "timezone") {
		t.Errorf("args[1] = %q unexpectedly contains get_time's data — cross-index contamination", args[1])
	}
	if strings.Contains(args[2], "location") {
		t.Errorf("args[2] = %q unexpectedly contains get_weather's data — cross-index contamination", args[2])
	}

	var parsed1, parsed2 map[string]any
	if err := json.Unmarshal([]byte(args[1]), &parsed1); err != nil {
		t.Fatalf("args[1] %q did not parse as JSON: %v", args[1], err)
	}
	if err := json.Unmarshal([]byte(args[2]), &parsed2); err != nil {
		t.Fatalf("args[2] %q did not parse as JSON: %v", args[2], err)
	}
	if parsed1["location"] != "Boston" {
		t.Errorf("parsed args[1] = %v, want location=Boston", parsed1)
	}
	if parsed2["timezone"] != "EST" {
		t.Errorf("parsed args[2] = %v, want timezone=EST", parsed2)
	}

	if usage == nil {
		t.Fatal("finalUsage = nil, want non-nil")
	}
	want := adapter.Usage{PromptTokens: 48, CompletionTokens: 142, TotalTokens: 190}
	if *usage != want {
		t.Errorf("finalUsage = %+v, want %+v", *usage, want)
	}

	last := chunks[len(chunks)-1]
	if fr := last.Choices[0].FinishReason; fr == nil || *fr != "tool_calls" {
		t.Errorf("final chunk FinishReason = %v, want %q (mapped from stop_reason=tool_use)", fr, "tool_calls")
	}
}

// TestStreamDecoder_UsageAndMessageStop is the explicit case for
// message_delta's usage handling and message_stop's done semantics,
// decoded event-by-event (rather than through the merging decodeFixture
// helper) so the test can assert the exact point at which finalUsage
// becomes non-nil (message_delta) versus the exact point at which done
// becomes true (message_stop, strictly later) — proving the two are
// genuinely distinct signals, not conflated into one.
func TestStreamDecoder_UsageAndMessageStop(t *testing.T) {
	f, err := os.Open("testdata/stream_usage_and_stop.txt")
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	decoder := New().NewStreamDecoder()
	reader := streaming.NewReader(f)

	var (
		sawUsageAt = -1
		sawDoneAt  = -1
		usage      *adapter.Usage
	)
	for i := 0; ; i++ {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reader.Next(): %v", err)
		}

		_, done, gotUsage, decErr := decoder.Decode(ev)
		if decErr != nil {
			t.Fatalf("Decode(%+v) error = %v", ev, decErr)
		}
		if gotUsage != nil {
			if sawUsageAt != -1 {
				t.Fatalf("finalUsage observed more than once (event #%d and #%d)", sawUsageAt, i)
			}
			sawUsageAt = i
			usage = gotUsage
		}
		if done {
			if sawDoneAt != -1 {
				t.Fatalf("done=true observed more than once (event #%d and #%d)", sawDoneAt, i)
			}
			sawDoneAt = i
		}
	}

	if sawUsageAt == -1 {
		t.Fatal("finalUsage was never observed")
	}
	if sawDoneAt == -1 {
		t.Fatal("done=true was never observed")
	}
	if sawDoneAt <= sawUsageAt {
		t.Fatalf("done became true at event #%d, at or before finalUsage arrived at event #%d — message_stop must come strictly after message_delta", sawDoneAt, sawUsageAt)
	}

	if usage == nil {
		t.Fatal("usage = nil")
	}
	want := adapter.Usage{PromptTokens: 50, CompletionTokens: 12, TotalTokens: 62}
	if *usage != want {
		t.Errorf("finalUsage = %+v, want %+v", *usage, want)
	}
}

// TestStreamDecoder_MessageDeltaCarriesFinishReason is a regression test:
// message_delta is the ONLY Anthropic event that ever carries stop_reason,
// and an earlier version of this decoder read message_delta purely for
// usage, silently discarding stop_reason — every reconstructed canonical
// response would come back with an empty FinishReason regardless of how
// the model actually stopped. This asserts the real fixtures' stop_reason
// values map to the correct canonical (OpenAI-shaped) finish_reason.
func TestStreamDecoder_MessageDeltaCarriesFinishReason(t *testing.T) {
	tests := []struct {
		fixture  string
		wantStop string
	}{
		{"testdata/stream_text_only.txt", "stop"},              // stop_reason: end_turn
		{"testdata/stream_single_tool_call.txt", "tool_calls"}, // stop_reason: tool_use
		{"testdata/stream_two_tool_calls_interleaved.txt", "tool_calls"},
		{"testdata/stream_usage_and_stop.txt", "stop"},
	}

	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			f, err := os.Open(tc.fixture)
			if err != nil {
				t.Fatalf("opening fixture: %v", err)
			}
			defer func() { _ = f.Close() }()

			decoder := New().NewStreamDecoder()
			reader := streaming.NewReader(f)

			var gotFinishReason *string
			for {
				ev, err := reader.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("reader.Next(): %v", err)
				}
				chunks, _, _, decErr := decoder.Decode(ev)
				if decErr != nil {
					t.Fatalf("Decode(%+v) error = %v", ev, decErr)
				}
				for _, c := range chunks {
					if fr := c.Choices[0].FinishReason; fr != nil && *fr != "" {
						if gotFinishReason != nil {
							t.Fatalf("FinishReason set more than once: %q and %q", *gotFinishReason, *fr)
						}
						gotFinishReason = fr
					}
				}
			}

			if gotFinishReason == nil {
				t.Fatal("no chunk ever carried a non-empty FinishReason — message_delta's stop_reason was not surfaced")
			}
			if *gotFinishReason != tc.wantStop {
				t.Errorf("FinishReason = %q, want %q", *gotFinishReason, tc.wantStop)
			}
		})
	}
}

// TestStreamDecoder_ContentBlockStopEmitsNoChunks proves content_block_stop
// specifically returns an empty (non-nil-or-not, but definitely
// zero-length) chunks slice, never a client-visible delta, per the RFC's
// explicit statement that this event carries no client-visible content.
func TestStreamDecoder_ContentBlockStopEmitsNoChunks(t *testing.T) {
	decoder := New().NewStreamDecoder()

	// A minimal, valid sequence: open a text block, then close it.
	if _, _, _, err := decoder.Decode(streaming.SSEEvent{
		Event: "content_block_start",
		Data:  `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	}); err != nil {
		t.Fatalf("content_block_start: %v", err)
	}

	chunks, done, usage, err := decoder.Decode(streaming.SSEEvent{
		Event: "content_block_stop",
		Data:  `{"type":"content_block_stop","index":0}`,
	})
	if err != nil {
		t.Fatalf("content_block_stop: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("content_block_stop chunks = %+v, want empty", chunks)
	}
	if done {
		t.Error("content_block_stop must not itself report done=true")
	}
	if usage != nil {
		t.Errorf("content_block_stop must not itself report usage, got %+v", usage)
	}
}

// TestStreamDecoder_ErrorEvent proves a raw "error" SSE event (Anthropic's
// documented shape for a mid-stream upstream failure) surfaces as a Go
// error from Decode rather than being silently swallowed or crashing.
func TestStreamDecoder_ErrorEvent(t *testing.T) {
	decoder := New().NewStreamDecoder()

	_, _, _, err := decoder.Decode(streaming.SSEEvent{
		Event: "error",
		Data:  `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
	})
	if err == nil {
		t.Fatal("Decode on an error event returned nil error, want non-nil")
	}
}
