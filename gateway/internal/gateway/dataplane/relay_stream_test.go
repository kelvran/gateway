package dataplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// recordingRelaySink is a ChunkSink, Finisher and RawRelay that records what
// the dataplane hands it.
type recordingRelaySink struct {
	raw     []string
	chunks  int
	headers http.Header
	ends    []streaming.StreamEnd
	done    int
}

func (s *recordingRelaySink) WriteChunk(streaming.ChatCompletionChunk) error { s.chunks++; return nil }
func (s *recordingRelaySink) WriteDone() error                               { s.done++; return nil }
func (s *recordingRelaySink) WriteFinish(end streaming.StreamEnd) error {
	s.ends = append(s.ends, end)
	return nil
}
func (s *recordingRelaySink) RelayHeaders(h http.Header) { s.headers = h.Clone() }
func (s *recordingRelaySink) WriteRaw(frame []byte) error {
	s.raw = append(s.raw, string(frame))
	return nil
}

var anthropicRelayFrames = []string{
	"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_relay\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-up\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n",
	": upstream comment\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n",
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n",
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
}

const openaiRelayFrames = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// newRelayTestPipeline wires an anthropic deployment (fallback_chains to an
// openai one of another canonical model) with a scripted UpstreamStream that
// fills the response carrier the way NewHTTPUpstreamStreamCaller does.
func newRelayTestPipeline(t *testing.T, anthropicStream func(dep Deployment) string, calls map[string]int) *Pipeline {
	t.Helper()
	keys := []identity.VirtualKey{{ID: "relay-key", KeyHash: testHashOf("relay-secret"), RateLimitBurst: 100, RateLimitRefill: 100}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatal(err)
	}
	deployments := []Deployment{
		{Name: "claude-primary", Model: "claude-sys", Provider: "anthropic", UpstreamModel: "claude-up", BaseURL: "http://unused",
			FallbackChains: map[string][]string{FallbackClassGeneric: {"gpt-fallback"}}},
		{Name: "gpt-fallback", Model: "gpt-fb", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"anthropic": anthropic.New(), "openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream must not be called")
			return nil, nil
		},
		UpstreamStream: func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
			calls[dep.Name]++
			if dep.Provider == "anthropic" {
				// What NewHTTPUpstreamStreamCaller records on a passthrough 2xx: the relayable headers, no body.
				recordUpstreamResponseMeta(ctx, "anthropic", &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Should-Retry": {"false"}, "Request-Id": {"req_1"}}}, nil)
				return io.NopCloser(strings.NewReader(anthropicStream(dep))), nil
			}
			return io.NopCloser(strings.NewReader(openaiRelayFrames)), nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func relayPassthroughRequest(model string) adapter.ChatRequest {
	return adapter.ChatRequest{
		Model:    model,
		Stream:   true,
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
		Passthrough: &adapter.Passthrough{
			Format:  adapter.IngressFormatAnthropicMessages,
			RawBody: []byte(`{"model":"` + model + `","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		},
	}
}

// On an anthropic passthrough hop every upstream frame reaches the sink's
// WriteRaw exactly as read -- comment line included -- before it is decoded,
// no canonical chunk is written beside it, the carrier's relayable headers
// are handed over before the first frame, and the sink still ends the stream
// through WriteFinish (RFC-1 §9, item 11 slice S11b2).
func TestHandleChatCompletionStreamSinkRelaysRawFramesOnAnAnthropicPassthroughHop(t *testing.T) {
	calls := map[string]int{}
	p := newRelayTestPipeline(t, func(Deployment) string { return strings.Join(anthropicRelayFrames, "") }, calls)
	ctx, _ := WithUpstreamResponseMeta(context.Background())
	sink := &recordingRelaySink{}
	if err := p.HandleChatCompletionStreamSink(ctx, "Bearer relay-secret", "", "", relayPassthroughRequest("claude-sys"), sink, ""); err != nil {
		t.Fatalf("HandleChatCompletionStreamSink: %v", err)
	}
	if strings.Join(sink.raw, "") != strings.Join(anthropicRelayFrames, "") || len(sink.raw) != len(anthropicRelayFrames) {
		t.Fatalf("relayed frames = %q, want the upstream's six frames unchanged", sink.raw)
	}
	if sink.chunks != 0 {
		t.Errorf("WriteChunk called %d times beside the relay, want 0 (double delivery)", sink.chunks)
	}
	if sink.headers.Get("X-Should-Retry") != "false" || sink.headers.Get("Request-Id") != "" {
		t.Errorf("relay headers = %v, want x-should-retry only", sink.headers)
	}
	if len(sink.ends) != 1 || sink.ends[0].Truncated || sink.done != 0 {
		t.Errorf("ends = %+v done = %d, want one untruncated WriteFinish", sink.ends, sink.done)
	}
	if calls["gpt-fallback"] != 0 {
		t.Errorf("fallback deployment called %d times on a clean relay", calls["gpt-fallback"])
	}
}

// A translate hop never relays: the same request on the openai deployment
// writes canonical chunks and no raw frame, even though the sink could relay.
func TestHandleChatCompletionStreamSinkDoesNotRelayOnATranslateHop(t *testing.T) {
	calls := map[string]int{}
	p := newRelayTestPipeline(t, func(Deployment) string { return "" }, calls)
	ctx, _ := WithUpstreamResponseMeta(context.Background())
	sink := &recordingRelaySink{}
	if err := p.HandleChatCompletionStreamSink(ctx, "Bearer relay-secret", "", "", relayPassthroughRequest("gpt-fb"), sink, ""); err != nil {
		t.Fatalf("HandleChatCompletionStreamSink: %v", err)
	}
	if len(sink.raw) != 0 || sink.chunks == 0 {
		t.Errorf("raw = %d chunks = %d on an openai hop, want 0 raw and canonical chunks", len(sink.raw), sink.chunks)
	}
}

// The first relayed frame counts as the first byte sent: a decode failure
// after it is an error to the client, never a fallback hop that would stream
// a second message_start.
func TestHandleChatCompletionStreamSinkFirstRelayedFrameBlocksFallback(t *testing.T) {
	calls := map[string]int{}
	p := newRelayTestPipeline(t, func(Deployment) string {
		return anthropicRelayFrames[0] + "event: content_block_start\ndata: {not json\n\n"
	}, calls)
	ctx, _ := WithUpstreamResponseMeta(context.Background())
	sink := &recordingRelaySink{}
	err := p.HandleChatCompletionStreamSink(ctx, "Bearer relay-secret", "", "", relayPassthroughRequest("claude-sys"), sink, "")
	if err == nil {
		t.Fatal("want a decode error, got nil")
	}
	if calls["gpt-fallback"] != 0 {
		t.Errorf("fallback hop ran after a frame was already relayed: calls %v", calls)
	}
	if len(sink.raw) != 2 {
		t.Errorf("relayed frames = %d, want both (message_start and the malformed one, relayed before decoding)", len(sink.raw))
	}
}

// An event type the decoder does not know reaches the client through the
// relay but not the shadow, so the response is unrepresentable: never cached,
// and a second identical request calls upstream again.
func TestHandleChatCompletionStreamSinkUnknownTopLevelEventMarksTheResponseUnrepresentable(t *testing.T) {
	for name, frames := range map[string][]string{
		"known events only":  anthropicRelayFrames,
		"with unknown event": append(append([]string{}, anthropicRelayFrames[:3]...), append([]string{"event: new_thing\ndata: {\"type\":\"new_thing\"}\n\n"}, anthropicRelayFrames[3:]...)...),
	} {
		calls := map[string]int{}
		p := newRelayTestPipeline(t, func(Deployment) string { return strings.Join(frames, "") }, calls)
		for i := 0; i < 2; i++ {
			ctx, _ := WithUpstreamResponseMeta(context.Background())
			if err := p.HandleChatCompletionStreamSink(ctx, "Bearer relay-secret", "", "", relayPassthroughRequest("claude-sys"), &recordingRelaySink{}, ""); err != nil {
				t.Fatalf("%s: call %d: %v", name, i+1, err)
			}
		}
		want := 1
		if name == "with unknown event" {
			want = 2
		}
		if calls["claude-primary"] != want {
			t.Errorf("%s: upstream calls = %d, want %d", name, calls["claude-primary"], want)
		}
	}
}

type failingReadCloser struct{}

func (failingReadCloser) Read([]byte) (int, error) {
	return 0, errors.New("connection reset before the first frame")
}
func (failingReadCloser) Close() error { return nil }

// A relay hop that dies before its first frame leaves nothing on the response:
// the carrier's headers travel only with the first relayed frame, so the
// fallback hop that serves the turn is not decorated with a failed hop's
// anthropic headers, and the turn arrives re-encoded.
func TestHandleChatCompletionStreamSinkRelaysHeadersOnlyWithTheFirstFrame(t *testing.T) {
	calls := map[string]int{}
	p := newRelayTestPipeline(t, func(Deployment) string { return "" }, calls)
	p.upstreamStream = func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		calls[dep.Name]++
		if dep.Provider == "anthropic" {
			recordUpstreamResponseMeta(ctx, "anthropic", &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Should-Retry": {"false"}}}, nil)
			return failingReadCloser{}, nil
		}
		return io.NopCloser(strings.NewReader(openaiRelayFrames)), nil
	}
	ctx, _ := WithUpstreamResponseMeta(context.Background())
	sink := &recordingRelaySink{}
	if err := p.HandleChatCompletionStreamSink(ctx, "Bearer relay-secret", "", "", relayPassthroughRequest("claude-sys"), sink, ""); err != nil {
		t.Fatalf("HandleChatCompletionStreamSink: %v", err)
	}
	if calls["claude-primary"] != 1 || calls["gpt-fallback"] != 1 {
		t.Fatalf("calls = %v, want one anthropic attempt and one fallback hop", calls)
	}
	if sink.headers != nil || len(sink.raw) != 0 || sink.chunks == 0 {
		t.Errorf("headers=%v raw=%d chunks=%d, want no relayed header, no raw frame, canonical chunks from the fallback", sink.headers, len(sink.raw), sink.chunks)
	}
}

// An in-band error frame as the FIRST upstream event is not relayed: nothing
// has reached the client, so the decoder's UpstreamStreamError is redacted and
// the hop falls back exactly as before the relay existed; the fallback's turn
// arrives re-encoded and none of the error bytes.
func TestHandleChatCompletionStreamSinkErrorFirstFrameIsNotRelayedAndFallsBack(t *testing.T) {
	calls := map[string]int{}
	p := newRelayTestPipeline(t, func(Deployment) string {
		return "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	}, calls)
	ctx, _ := WithUpstreamResponseMeta(context.Background())
	sink := &recordingRelaySink{}
	if err := p.HandleChatCompletionStreamSink(ctx, "Bearer relay-secret", "", "", relayPassthroughRequest("claude-sys"), sink, ""); err != nil {
		t.Fatalf("HandleChatCompletionStreamSink: %v", err)
	}
	if calls["claude-primary"] != 1 || calls["gpt-fallback"] != 1 {
		t.Fatalf("calls = %v, want the anthropic attempt and one fallback hop", calls)
	}
	if len(sink.raw) != 0 || sink.headers != nil || sink.chunks == 0 {
		t.Errorf("raw=%d headers=%v chunks=%d, want nothing relayed and the fallback's canonical chunks", len(sink.raw), sink.headers, sink.chunks)
	}
}

// Bytes the decoder rejects as the FIRST upstream event are not relayed either:
// the client receives nothing from that hop, the decode error is redacted and
// the fallback serves the turn, exactly as before the relay existed.
func TestHandleChatCompletionStreamSinkUnparsableFirstFrameIsNotRelayedAndFallsBack(t *testing.T) {
	calls := map[string]int{}
	p := newRelayTestPipeline(t, func(Deployment) string { return "event: message_start\ndata: <html>proxy</html>\n\n" }, calls)
	ctx, _ := WithUpstreamResponseMeta(context.Background())
	sink := &recordingRelaySink{}
	if err := p.HandleChatCompletionStreamSink(ctx, "Bearer relay-secret", "", "", relayPassthroughRequest("claude-sys"), sink, ""); err != nil {
		t.Fatalf("HandleChatCompletionStreamSink: %v", err)
	}
	if calls["claude-primary"] != 1 || calls["gpt-fallback"] != 1 {
		t.Fatalf("calls = %v, want the anthropic attempt and one fallback hop", calls)
	}
	if len(sink.raw) != 0 || sink.headers != nil || sink.chunks == 0 {
		t.Errorf("raw=%d headers=%v chunks=%d, want nothing relayed and the fallback's canonical chunks", len(sink.raw), sink.headers, sink.chunks)
	}
}

// A known event whose body the decoder rejects (deep shape, not SSE framing
// or JSON syntax) as the FIRST frame is not relayed either: the first frame is
// decoded before it is relayed, so the decoder alone decides, and the turn
// falls back with the client having received nothing from that hop.
func TestHandleChatCompletionStreamSinkMalformedFirstFrameIsNotRelayedAndFallsBack(t *testing.T) {
	calls := map[string]int{}
	p := newRelayTestPipeline(t, func(Deployment) string {
		return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":5}\n\n"
	}, calls)
	ctx, _ := WithUpstreamResponseMeta(context.Background())
	sink := &recordingRelaySink{}
	if err := p.HandleChatCompletionStreamSink(ctx, "Bearer relay-secret", "", "", relayPassthroughRequest("claude-sys"), sink, ""); err != nil {
		t.Fatalf("HandleChatCompletionStreamSink: %v", err)
	}
	if calls["claude-primary"] != 1 || calls["gpt-fallback"] != 1 {
		t.Fatalf("calls = %v, want the anthropic attempt and one fallback hop", calls)
	}
	if len(sink.raw) != 0 || sink.headers != nil || sink.chunks == 0 {
		t.Errorf("raw=%d headers=%v chunks=%d, want nothing relayed and the fallback's canonical chunks", len(sink.raw), sink.headers, sink.chunks)
	}
}

// Decoding the first frame first must not drop what the decoder tolerates: an
// event type it does not know, arriving first, is relayed like any other.
func TestHandleChatCompletionStreamSinkUnknownFirstFrameIsRelayed(t *testing.T) {
	calls := map[string]int{}
	unknown := "event: new_thing\ndata: {\"type\":\"new_thing\"}\n\n"
	p := newRelayTestPipeline(t, func(Deployment) string { return unknown + strings.Join(anthropicRelayFrames, "") }, calls)
	ctx, _ := WithUpstreamResponseMeta(context.Background())
	sink := &recordingRelaySink{}
	if err := p.HandleChatCompletionStreamSink(ctx, "Bearer relay-secret", "", "", relayPassthroughRequest("claude-sys"), sink, ""); err != nil {
		t.Fatalf("HandleChatCompletionStreamSink: %v", err)
	}
	if len(sink.raw) != len(anthropicRelayFrames)+1 || sink.raw[0] != unknown || sink.headers == nil {
		t.Errorf("raw=%d first=%q headers=%v, want the unknown first frame relayed with the headers", len(sink.raw), sink.raw[:1], sink.headers)
	}
}

// errAfterReadCloser yields its bytes once, then fails every further Read.
type errAfterReadCloser struct {
	data []byte
	err  error
}

func (r *errAfterReadCloser) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}
func (r *errAfterReadCloser) Close() error { return nil }

// The first relayed frame is the first byte sent even when no second frame
// ever arrives: a connection that drops right after it must not fall back,
// or the client would see a second message_start from the fallback hop.
func TestHandleChatCompletionStreamSinkReadErrorAfterFirstRelayedFrameDoesNotFallBack(t *testing.T) {
	calls := map[string]int{}
	p := newRelayTestPipeline(t, func(Deployment) string { return "" }, calls)
	p.upstreamStream = func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		calls[dep.Name]++
		if dep.Provider == "anthropic" {
			recordUpstreamResponseMeta(ctx, "anthropic", &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}, nil)
			return &errAfterReadCloser{data: []byte(anthropicRelayFrames[0]), err: errors.New("connection reset after the first frame")}, nil
		}
		return io.NopCloser(strings.NewReader(openaiRelayFrames)), nil
	}
	ctx, _ := WithUpstreamResponseMeta(context.Background())
	sink := &recordingRelaySink{}
	err := p.HandleChatCompletionStreamSink(ctx, "Bearer relay-secret", "", "", relayPassthroughRequest("claude-sys"), sink, "")
	if err == nil {
		t.Fatal("want the read error, got nil")
	}
	if calls["gpt-fallback"] != 0 {
		t.Errorf("fallback hop ran after a frame was already relayed: calls %v", calls)
	}
	if len(sink.raw) != 1 || sink.raw[0] != anthropicRelayFrames[0] {
		t.Errorf("relayed frames = %q, want exactly the first frame", sink.raw)
	}
}
