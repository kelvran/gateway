package dataplane

import (
	"bytes"
	"context"
	"errors"
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
	}, rec, "")
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
	}, rec2, "")
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
	}, rec, "")
	if err == nil {
		t.Fatal("HandleChatCompletionStream: want error for an exception frame mid-stream, got nil")
	}
	if !errors.Is(err, bedrock.ErrBedrockThrottled) {
		t.Errorf("error = %v, want errors.Is(err, bedrock.ErrBedrockThrottled)", err)
	}
}

// TestHandleChatCompletionStreamBedrockMidFrameTruncationSurfacesAsError is
// the regression proof for the real bug fixed in ErrBedrockStreamTruncated's
// own doc comment: aws-sdk-go-v2's eventstream.Decoder.Decode reports bare
// io.EOF for a connection cut off mid-frame (its own decodePayload uses
// io.Copy, which swallows an EOF from a short payload read as success; the
// very next read -- the frame's own trailing CRC -- then hits the closed
// connection with zero bytes for THAT read and surfaces as a second, plain
// io.EOF) -- indistinguishable from a genuinely clean end-of-stream at a
// real frame boundary without checking whether messageStop was ever seen.
// Truncates a real, wire-encoded fixture mid-way through its LAST frame
// (a contentBlockDelta, deliberately never followed by messageStop/
// metadata) rather than hand-rolling approximate bytes, so this proves
// against the actual AWS SDK decoder, not a mock of it.
func TestHandleChatCompletionStreamBedrockMidFrameTruncationSurfacesAsError(t *testing.T) {
	wire := encodeBedrockWireFixture(t, []eventstream.Message{
		bedrockWireEvent("messageStart", `{"role":"assistant"}`),
		bedrockWireEvent("contentBlockStart", `{"contentBlockIndex":0,"start":{}}`),
		bedrockWireEvent("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hel"}}`),
	})
	// Cut off the last 8 bytes (well inside the final contentBlockDelta
	// frame's own payload/CRC trailer, never at a clean frame boundary) --
	// a real, if crude, simulation of a connection dropping mid-frame.
	truncated := wire[:len(wire)-8]

	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(truncated)), nil
	}, []Deployment{{Name: "d1", Model: "claude-bedrock", Provider: "bedrock", UpstreamModel: "anthropic.claude-3-5-sonnet-20241022-v2:0", BaseURL: "http://unused"}},
		adapter.Registry{"bedrock": bedrock.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "claude-bedrock", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, rec, "")
	if err == nil {
		t.Fatal("HandleChatCompletionStream: want error for a stream truncated mid-frame with no messageStop ever received, got nil — silently treated as a clean end of stream")
	}
	if !errors.Is(err, ErrBedrockStreamTruncated) {
		t.Errorf("error = %v, want errors.Is(err, ErrBedrockStreamTruncated)", err)
	}
}
