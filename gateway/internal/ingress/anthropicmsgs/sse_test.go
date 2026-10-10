package anthropicmsgs

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/streaming"
)

func decodeChunksFixture(t *testing.T, name string) []streaming.ChatCompletionChunk {
	t.Helper()
	var chunks []streaming.ChatCompletionChunk
	if err := json.Unmarshal(mustReadTestdata(t, name), &chunks); err != nil {
		t.Fatalf("decoding %s: %v", name, err)
	}
	return chunks
}

// TestSSEEncoderTextToolThinkingMatchesGolden: canonical chunks become
// Anthropic's event sequence -- message_start once, a thinking block with
// thinking_delta then signature_delta, a text block with text_delta, a
// tool_use block with input_json_delta, each block closed, then
// message_delta carrying the native stop_reason and the usage (output,
// cache and thinking tokens; input_tokens as the inverse of the inclusive
// sum), then message_stop -- as the golden.
func TestSSEEncoderTextToolThinkingMatchesGolden(t *testing.T) {
	var buf bytes.Buffer
	enc := NewSSEEncoder(&buf)
	for _, c := range decodeChunksFixture(t, "chunks_text_tool_thinking.json") {
		if err := enc.WriteChunk(c); err != nil {
			t.Fatalf("WriteChunk: %v", err)
		}
	}
	if err := enc.WriteDone(); err != nil {
		t.Fatalf("WriteDone: %v", err)
	}
	out := buf.String()
	events := eventTypes(out)
	want := "message_start,content_block_start,content_block_delta,content_block_delta,content_block_delta,content_block_stop,content_block_start,content_block_delta,content_block_delta,content_block_stop,content_block_start,content_block_delta,content_block_delta,content_block_stop,message_delta,message_stop"
	if got := strings.Join(events, ","); got != want {
		t.Errorf("event sequence =\n%s\nwant\n%s", got, want)
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) || !strings.Contains(out, `"input_tokens":35`) || !strings.Contains(out, `"thinking_tokens":8`) {
		t.Errorf("message_delta lacks the native stop reason or usage: %s", lastEvent(out, "message_delta"))
	}
	if strings.Contains(out, "Unrepresentable") || strings.Contains(out, "[DONE]") {
		t.Error("the Anthropic stream must carry neither the internal flag nor the OpenAI sentinel")
	}
	checkGolden(t, "sse_text_tool_thinking.golden.txt", []byte(out))
}

// TestSSEEncoderPingAndError: Ping emits Anthropic's ping event; WriteError
// emits an error event (the clean-end rule: a stream that must abort after a
// block started ends with an error event, never silently).
func TestSSEEncoderPingAndError(t *testing.T) {
	var buf bytes.Buffer
	enc := NewSSEEncoder(&buf)
	if err := enc.Ping(); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "event: ping\ndata: {\"type\":\"ping\"}\n\n" {
		t.Errorf("Ping = %q", buf.String())
	}
	buf.Reset()
	if err := enc.WriteError("overloaded_error", "upstream overloaded"); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"upstream overloaded\"}}\n\n" {
		t.Errorf("WriteError = %q", buf.String())
	}
}

// TestSSEEncoderDoneWithoutFinishStillClosesTheMessage: a stream that ends
// without a finish chunk (an upstream that never sent one) still gets
// message_delta (stop_reason end_turn as the forward default) and
// message_stop, so the client never sees a clean end before message_delta.
func TestSSEEncoderDoneWithoutFinishStillClosesTheMessage(t *testing.T) {
	var buf bytes.Buffer
	enc := NewSSEEncoder(&buf)
	if err := enc.WriteChunk(streaming.ChatCompletionChunk{ID: "x", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Role: "assistant", Content: "hi"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteDone(); err != nil {
		t.Fatal(err)
	}
	events := strings.Join(eventTypes(buf.String()), ",")
	if events != "message_start,content_block_start,content_block_delta,content_block_stop,message_delta,message_stop" {
		t.Errorf("events = %s", events)
	}
	if !strings.Contains(buf.String(), `"stop_reason":"end_turn"`) {
		t.Errorf("message_delta without a finish chunk: %s", lastEvent(buf.String(), "message_delta"))
	}
}

func eventTypes(sse string) []string {
	var out []string
	for _, line := range strings.Split(sse, "\n") {
		if strings.HasPrefix(line, "event: ") {
			out = append(out, strings.TrimPrefix(line, "event: "))
		}
	}
	return out
}

func lastEvent(sse, name string) string {
	idx := strings.LastIndex(sse, "event: "+name)
	if idx < 0 {
		return ""
	}
	return sse[idx:]
}

// TestSSEEncoderRedactedThinkingAndTwoTools: a redacted block carries its
// data on content_block_start and emits no delta; a second tool call opens
// a third block; a native stop_sequence reaches message_delta.
func TestSSEEncoderRedactedThinkingAndTwoTools(t *testing.T) {
	var buf bytes.Buffer
	enc := NewSSEEncoder(&buf)
	stop := "stop"
	chunks := []streaming.ChatCompletionChunk{
		{ID: "s2", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{ReasoningBlocks: []streaming.ReasoningDelta{{Index: 0, Redacted: true, Data: "EmQB"}}}}}},
		{ID: "s2", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{ToolCalls: []streaming.ToolCallDelta{{Index: 0, ID: "t1", Name: "A", ArgumentsJSON: `{}`}}}}}},
		{ID: "s2", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{ToolCalls: []streaming.ToolCallDelta{{Index: 1, ID: "t2", Name: "B", ArgumentsJSON: `{"q":1}`}}}}}},
		{ID: "s2", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, FinishReason: &stop, StopReason: "stop_sequence", StopSequence: "END"}}},
	}
	for _, c := range chunks {
		if err := enc.WriteChunk(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.WriteDone(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`"content_block":{"type":"redacted_thinking","data":"EmQB"}`,
		`"index":1,"content_block":{"type":"tool_use","id":"t1","name":"A","input":{}}`,
		`"index":2,"content_block":{"type":"tool_use","id":"t2","name":"B","input":{}}`,
		`"delta":{"stop_reason":"stop_sequence","stop_sequence":"END"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream lacks %s\n%s", want, out)
		}
	}
	if strings.Count(out, "event: content_block_delta") != 2 {
		t.Errorf("want exactly two deltas (the two tool inputs), none for the redacted block:\n%s", out)
	}
	if strings.Count(out, "event: content_block_stop") != 3 {
		t.Errorf("want three content_block_stop events:\n%s", out)
	}
}

// TestSSEEncoderTextDuringOpenToolBlockIsDeferred: Anthropic blocks are
// sequential, so text arriving while a tool_use block is still receiving
// input_json_delta fragments cannot interrupt it (closing the block early
// would strand the remaining fragments). The text is held and emitted as its
// own block once the tool block closes; every fragment stays in one block
// and no tool_use id appears twice.
func TestSSEEncoderTextDuringOpenToolBlockIsDeferred(t *testing.T) {
	var buf bytes.Buffer
	enc := NewSSEEncoder(&buf)
	chunks := []streaming.ChatCompletionChunk{
		{ID: "s3", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{ToolCalls: []streaming.ToolCallDelta{{Index: 0, ID: "t1", Name: "A", ArgumentsJSON: `{"q":`}}}}}},
		{ID: "s3", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Content: "after"}}}},
		{ID: "s3", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{ToolCalls: []streaming.ToolCallDelta{{Index: 0, ArgumentsJSON: `1}`}}}}}},
	}
	for _, c := range chunks {
		if err := enc.WriteChunk(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.WriteDone(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Count(out, `"type":"tool_use"`) != 1 {
		t.Errorf("want exactly one tool_use block:\n%s", out)
	}
	toolStop := strings.Index(out, `{"type":"content_block_stop","index":0}`)
	textStart := strings.Index(out, `"content_block":{"type":"text","text":""}`)
	if toolStop < 0 || textStart < 0 || textStart < toolStop {
		t.Errorf("text block must open after the tool block closed (tool stop at %d, text start at %d):\n%s", toolStop, textStart, out)
	}
	for _, want := range []string{`"partial_json":"{\"q\":"`, `"partial_json":"1}"`, `"type":"text_delta","text":"after"`} {
		if !strings.Contains(out, want) {
			t.Errorf("stream lacks %s\n%s", want, out)
		}
	}
	if !strings.Contains(out, `{"type":"content_block_stop","index":1}`) {
		t.Errorf("the deferred text block must be closed before message_delta:\n%s", out)
	}
}

// TestSSEEncoderToolFragmentAfterItsBlockClosedKeepsIdentity: no provider
// interleaves fragments of two tool calls today; if one did, the fragment
// for an already-closed block opens a continuation block that carries the
// original id and name rather than an anonymous one.
func TestSSEEncoderToolFragmentAfterItsBlockClosedKeepsIdentity(t *testing.T) {
	var buf bytes.Buffer
	enc := NewSSEEncoder(&buf)
	chunks := []streaming.ChatCompletionChunk{
		{ID: "s4", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{ToolCalls: []streaming.ToolCallDelta{{Index: 0, ID: "t1", Name: "A", ArgumentsJSON: `{"a":`}}}}}},
		{ID: "s4", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{ToolCalls: []streaming.ToolCallDelta{{Index: 1, ID: "t2", Name: "B", ArgumentsJSON: `{}`}}}}}},
		{ID: "s4", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{ToolCalls: []streaming.ToolCallDelta{{Index: 0, ArgumentsJSON: `1}`}}}}}},
	}
	for _, c := range chunks {
		if err := enc.WriteChunk(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.WriteDone(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Count(out, `"content_block":{"type":"tool_use","id":"t1","name":"A","input":{}}`) != 2 {
		t.Errorf("the continuation block must carry id t1 and name A:\n%s", out)
	}
	if strings.Contains(out, `"id":"","name":""`) || strings.Contains(out, `{"type":"tool_use","input":{}}`) {
		t.Errorf("an anonymous tool_use block was emitted:\n%s", out)
	}
}

// TestSSEEncoderPendingTextIsCapped: text held behind an open tool_use block
// is bounded; past maxPendingTextBytes the tool block is closed early and
// the text emitted, so a misbehaving upstream cannot grow the encoder's
// memory beyond the dataplane's own accumulator. A later fragment for the
// tool still opens a continuation block carrying the original identity.
func TestSSEEncoderPendingTextIsCapped(t *testing.T) {
	var buf bytes.Buffer
	enc := NewSSEEncoder(&buf)
	open := streaming.ChatCompletionChunk{ID: "s5", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{ToolCalls: []streaming.ToolCallDelta{{Index: 0, ID: "t1", Name: "A", ArgumentsJSON: `{"a":`}}}}}}
	if err := enc.WriteChunk(open); err != nil {
		t.Fatal(err)
	}
	piece := strings.Repeat("x", 64<<10)
	for written := 0; written <= maxPendingTextBytes; written += len(piece) {
		if err := enc.WriteChunk(streaming.ChatCompletionChunk{ID: "s5", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Content: piece}}}}); err != nil {
			t.Fatal(err)
		}
	}
	if enc.pending.Len() > maxPendingTextBytes {
		t.Errorf("pending holds %d bytes, cap is %d", enc.pending.Len(), maxPendingTextBytes)
	}
	midway := buf.String()
	if !strings.Contains(midway, `{"type":"content_block_stop","index":0}`) || !strings.Contains(midway, `"content_block":{"type":"text","text":""}`) {
		t.Errorf("past the cap the tool block must close and the text block open:\n%.600s", midway)
	}
	tail := streaming.ChatCompletionChunk{ID: "s5", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{ToolCalls: []streaming.ToolCallDelta{{Index: 0, ArgumentsJSON: `1}`}}}}}}
	if err := enc.WriteChunk(tail); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteDone(); err != nil {
		t.Fatal(err)
	}
	if strings.Count(buf.String(), `"content_block":{"type":"tool_use","id":"t1","name":"A","input":{}}`) != 2 {
		t.Errorf("the late fragment must open a continuation block with the original identity:\n%.400s…", buf.String())
	}
}
