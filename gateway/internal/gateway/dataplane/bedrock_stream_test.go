package dataplane

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
)

// bedrockWireEvent builds a real eventstream.Message for one Converse
// event, mirroring bedrock/stream_test.go's own newEventMessage helper.
func bedrockWireEvent(eventType, payload string) eventstream.Message {
	return eventstream.Message{
		Headers: eventstream.Headers{
			{Name: ":message-type", Value: eventstream.StringValue("event")},
			{Name: ":event-type", Value: eventstream.StringValue(eventType)},
		},
		Payload: []byte(payload),
	}
}

// encodeBedrockWireFixture uses eventstream.Encoder -- the real encode-side
// counterpart to the eventstream.Decoder streamDeploymentBedrock drives --
// to build a genuinely wire-accurate binary
// application/vnd.amazon.eventstream body, rather than a hand-rolled
// approximation of the framing.
func encodeBedrockWireFixture(t *testing.T, msgs []eventstream.Message) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := eventstream.NewEncoder()
	for _, m := range msgs {
		if err := enc.Encode(&buf, m); err != nil {
			t.Fatalf("encoding fixture event: %v", err)
		}
	}
	return buf.Bytes()
}

// TestHandleChatCompletionStreamBedrockFullSequenceDecodesCorrectly drives
// streamDeploymentBedrock through a real
// messageStart -> contentBlockStart -> contentBlockDelta* ->
// contentBlockStop -> messageStop -> metadata sequence, encoded to genuine
// binary wire bytes, and proves both the client-facing SSE tee and the
// cached final response come out correct.
func TestHandleChatCompletionStreamBedrockFullSequenceDecodesCorrectly(t *testing.T) {
	wire := encodeBedrockWireFixture(t, []eventstream.Message{
		bedrockWireEvent("messageStart", `{"role":"assistant"}`),
		bedrockWireEvent("contentBlockStart", `{"contentBlockIndex":0,"start":{}}`),
		bedrockWireEvent("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hel"}}`),
		bedrockWireEvent("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"lo!"}}`),
		bedrockWireEvent("contentBlockStop", `{"contentBlockIndex":0}`),
		bedrockWireEvent("messageStop", `{"stopReason":"end_turn"}`),
		bedrockWireEvent("metadata", `{"usage":{"inputTokens":9,"outputTokens":4,"totalTokens":13}}`),
	})

	var upstreamCalls int
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		upstreamCalls++
		return io.NopCloser(bytes.NewReader(wire)), nil
	}, []Deployment{{Name: "d1", Model: "claude-bedrock", Provider: "bedrock", UpstreamModel: "anthropic.claude-3-5-sonnet-20241022-v2:0", BaseURL: "http://unused"}},
		adapter.Registry{"bedrock": bedrock.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "claude-bedrock", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, rec)
	if err != nil {
		t.Fatalf("HandleChatCompletionStream: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1", upstreamCalls)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Errorf("body missing role delta: %s", body)
	}
	if !strings.Contains(body, `"content":"Hel"`) || !strings.Contains(body, `"content":"lo!"`) {
		t.Errorf("body missing expected content deltas: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("body missing finish_reason: %s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("body does not end with [DONE] sentinel: %s", body)
	}

	// Bedrock's real "metadata" event carries usage but no content of its
	// own and is never written to the client as a chunk (per
	// streaming.ChatCompletionChunk's own doc comment: a provider that
	// never sends usage mid-stream leaves every chunk's Usage nil) -- so
	// the only way to prove finalUsage was threaded correctly into the
	// cached ChatResponse is via a second, cache-hit request, whose
	// fake-streamed body is built directly from that cached response.
	rec2 := httptest.NewRecorder()
	err = p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "claude-bedrock", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, rec2)
	if err != nil {
		t.Fatalf("second (cache-hit) HandleChatCompletionStream: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after cache-hit call = %d, want still 1", upstreamCalls)
	}
	body2 := rec2.Body.String()
	if !strings.Contains(body2, `"content":"Hello!"`) {
		t.Errorf("fake-streamed cache-hit body missing full accumulated content: %s", body2)
	}
	if !strings.Contains(body2, `"total_tokens":13`) {
		t.Errorf("fake-streamed cache-hit body missing usage from the metadata event: %s", body2)
	}
}

// TestHandleChatCompletionStreamBedrockExceptionFrameSurfacesAsError proves
// an AWS-side exception frame mid-stream fails the request with a real,
// typed error rather than silently truncating the stream.
func TestHandleChatCompletionStreamBedrockExceptionFrameSurfacesAsError(t *testing.T) {
	wire := encodeBedrockWireFixture(t, []eventstream.Message{
		bedrockWireEvent("messageStart", `{"role":"assistant"}`),
		{
			Headers: eventstream.Headers{
				{Name: ":message-type", Value: eventstream.StringValue("exception")},
				{Name: ":exception-type", Value: eventstream.StringValue("throttlingException")},
			},
			Payload: []byte(`{"message":"rate exceeded"}`),
		},
	})

	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(wire)), nil
	}, []Deployment{{Name: "d1", Model: "claude-bedrock", Provider: "bedrock", UpstreamModel: "anthropic.claude-3-5-sonnet-20241022-v2:0", BaseURL: "http://unused"}},
		adapter.Registry{"bedrock": bedrock.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "claude-bedrock", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, rec)
	if err == nil {
		t.Fatal("HandleChatCompletionStream: want error for an exception frame mid-stream, got nil")
	}
	if !strings.Contains(err.Error(), "exception") {
		t.Errorf("error = %v, want it to mention the exception message-type", err)
	}
}
