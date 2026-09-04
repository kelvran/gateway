package bedrock

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

// newEventMessage builds a real eventstream.Message for one Converse
// event, mirroring the real ":event-type"/":message-type" header shape
// confirmed against aws-sdk-go-v2's eventstreamapi package.
func newEventMessage(eventType string, payload string) eventstream.Message {
	return eventstream.Message{
		Headers: eventstream.Headers{
			{Name: ":message-type", Value: eventstream.StringValue("event")},
			{Name: ":event-type", Value: eventstream.StringValue(eventType)},
		},
		Payload: []byte(payload),
	}
}

func TestDecodeMessageStartProducesRoleChunk(t *testing.T) {
	msg := newEventMessage("messageStart", `{"role":"assistant"}`)

	chunks, usage, err := NewStreamDecoder().Decode(msg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if usage != nil {
		t.Errorf("usage = %+v, want nil", usage)
	}
	if len(chunks) != 1 || chunks[0].Choices[0].Delta.Role != "assistant" {
		t.Fatalf("chunks = %+v, want one chunk with Delta.Role=assistant", chunks)
	}
}

func TestDecodeContentBlockStartToolUseProducesIntroChunk(t *testing.T) {
	msg := newEventMessage("contentBlockStart", `{"contentBlockIndex":0,"start":{"toolUse":{"toolUseId":"tooluse_1","name":"get_weather"}}}`)

	chunks, _, err := NewStreamDecoder().Decode(msg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("len(chunks) = %d, want 1", len(chunks))
	}
	tcs := chunks[0].Choices[0].Delta.ToolCalls
	if len(tcs) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(tcs))
	}
	if tcs[0].Index != 0 || tcs[0].ID != "tooluse_1" || tcs[0].Name != "get_weather" {
		t.Errorf("ToolCalls[0] = %+v, unexpected", tcs[0])
	}
	if tcs[0].ArgumentsJSON != "" {
		t.Errorf("ArgumentsJSON = %q, want empty on the intro chunk", tcs[0].ArgumentsJSON)
	}
}

func TestDecodeContentBlockStartTextProducesNoChunk(t *testing.T) {
	msg := newEventMessage("contentBlockStart", `{"contentBlockIndex":0,"start":{}}`)

	chunks, _, err := NewStreamDecoder().Decode(msg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("chunks = %+v, want none for a text block's own start", chunks)
	}
}

func TestDecodeContentBlockDeltaTextProducesContent(t *testing.T) {
	msg := newEventMessage("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hello"}}`)

	chunks, _, err := NewStreamDecoder().Decode(msg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(chunks) != 1 || chunks[0].Choices[0].Delta.Content != "Hello" {
		t.Fatalf("chunks = %+v, want Content=Hello", chunks)
	}
}

// TestDecodeToolCallArgumentsAccumulateAcrossFragments is the load-bearing
// test for this decoder: proves toolUse.input arrives as an accumulating
// STRING FRAGMENT across multiple contentBlockDelta events -- confirmed
// against the real ToolUseBlockDelta.Input *string struct and the real
// wire deserializer's own string-type assertion -- never a whole object
// per chunk the way Gemini's is.
func TestDecodeToolCallArgumentsAccumulateAcrossFragments(t *testing.T) {
	fragments := []string{`{"city":`, `"Boston"`, `}`}
	decoder := NewStreamDecoder()
	var accumulated strings.Builder

	for _, f := range fragments {
		msg := newEventMessage("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"toolUse":{"input":`+quoteJSON(f)+`}}}`)
		chunks, _, err := decoder.Decode(msg)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if len(chunks) != 1 {
			t.Fatalf("len(chunks) = %d, want 1", len(chunks))
		}
		tcs := chunks[0].Choices[0].Delta.ToolCalls
		if len(tcs) != 1 {
			t.Fatalf("len(ToolCalls) = %d, want 1", len(tcs))
		}
		if tcs[0].ID != "" || tcs[0].Name != "" {
			t.Errorf("ToolCalls[0] = %+v, want no ID/Name on a delta fragment", tcs[0])
		}
		accumulated.WriteString(tcs[0].ArgumentsJSON)
	}

	const want = `{"city":"Boston"}`
	if got := accumulated.String(); got != want {
		t.Errorf("accumulated ArgumentsJSON = %q, want %q", got, want)
	}
}

// quoteJSON produces a JSON-encoded string literal for embedding a raw
// fragment as the "input" field's value in a test fixture.
func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestDecodeContentBlockStopProducesNoChunk(t *testing.T) {
	msg := newEventMessage("contentBlockStop", `{"contentBlockIndex":0}`)

	chunks, _, err := NewStreamDecoder().Decode(msg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("chunks = %+v, want none", chunks)
	}
}

// TestDecodeMessageStopReusesExistingFinishReasonMapping proves the
// streaming decoder reuses bedrock.go's own finishReasonFromBedrock
// unchanged, rather than duplicating the mapping.
func TestDecodeMessageStopReusesExistingFinishReasonMapping(t *testing.T) {
	msg := newEventMessage("messageStop", `{"stopReason":"tool_use"}`)

	chunks, _, err := NewStreamDecoder().Decode(msg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(chunks) != 1 || chunks[0].Choices[0].FinishReason == nil {
		t.Fatalf("chunks = %+v, want one chunk with a non-nil FinishReason", chunks)
	}
	if *chunks[0].Choices[0].FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want %q", *chunks[0].Choices[0].FinishReason, "tool_calls")
	}
}

func TestDecodeMessageStopMalformedToolUseReturnsError(t *testing.T) {
	msg := newEventMessage("messageStop", `{"stopReason":"malformed_tool_use"}`)

	_, _, err := NewStreamDecoder().Decode(msg)
	if err == nil {
		t.Fatal("Decode: want error for malformed_tool_use, got nil")
	}
}

func TestDecodeMetadataProducesUsage(t *testing.T) {
	msg := newEventMessage("metadata", `{"usage":{"inputTokens":58,"outputTokens":12,"totalTokens":70}}`)

	chunks, usage, err := NewStreamDecoder().Decode(msg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("chunks = %+v, want none", chunks)
	}
	if usage == nil {
		t.Fatal("usage = nil, want non-nil")
	}
	if usage.PromptTokens != 58 || usage.CompletionTokens != 12 || usage.TotalTokens != 70 {
		t.Errorf("usage = %+v, want {58 12 70}", usage)
	}
}

// TestDecodeExceptionMessageTypeReturnsError proves an AWS-side
// exception/error frame surfaces as a real, typed error -- never
// silently dropped.
func TestDecodeExceptionMessageTypeReturnsError(t *testing.T) {
	msg := eventstream.Message{
		Headers: eventstream.Headers{
			{Name: ":message-type", Value: eventstream.StringValue("exception")},
			{Name: ":exception-type", Value: eventstream.StringValue("throttlingException")},
		},
		Payload: []byte(`{"message":"rate exceeded"}`),
	}

	_, _, err := NewStreamDecoder().Decode(msg)
	if err == nil {
		t.Fatal("Decode: want error for an exception message-type, got nil")
	}
	if !strings.Contains(err.Error(), "exception") {
		t.Errorf("error = %v, want it to mention the exception message-type", err)
	}
}

// TestDecodeMissingEventTypeHeaderNeverPanics proves a nil
// Headers.Get(...) result (a missing header) is handled safely --
// calling .String() on a nil interface would panic.
func TestDecodeMissingEventTypeHeaderNeverPanics(t *testing.T) {
	msg := eventstream.Message{
		Headers: eventstream.Headers{
			{Name: ":message-type", Value: eventstream.StringValue("event")},
		},
		Payload: []byte(`{}`),
	}

	chunks, usage, err := NewStreamDecoder().Decode(msg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(chunks) != 0 || usage != nil {
		t.Errorf("chunks/usage = %+v/%+v, want none/nil for an unrecognized empty event type", chunks, usage)
	}
}

func TestDecodeUnknownEventTypeIsForwardCompatible(t *testing.T) {
	msg := newEventMessage("someFutureEventType", `{"anything":"goes"}`)

	chunks, usage, err := NewStreamDecoder().Decode(msg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(chunks) != 0 || usage != nil {
		t.Errorf("chunks/usage = %+v/%+v, want none/nil for an unknown event type", chunks, usage)
	}
}
