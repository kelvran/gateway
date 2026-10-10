package dataplane

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ingress/anthropicmsgs"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// fixedSSEReader serves a canned upstream SSE body.
type fixedSSEReader struct{ r *strings.Reader }

func (f *fixedSSEReader) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *fixedSSEReader) Close() error               { return nil }

// newSinkTestPipeline is the runaway test's pipeline with a pluggable
// streaming upstream.
func newSinkTestPipeline(t *testing.T, logBuf *bytes.Buffer, upstream func(ctx context.Context) io.ReadCloser) *Pipeline {
	t.Helper()
	keys := []identity.VirtualKey{{ID: "sink-key", KeyHash: testHashOf("sink-secret"), RateLimitBurst: 100, RateLimitRefill: 100}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatal(err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream must not be called")
			return nil, nil
		},
		UpstreamStream: func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) { return upstream(ctx), nil },
		Logger:         slog.New(slog.NewJSONHandler(logBuf, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func eventNames(stream string) []string {
	var names []string
	for _, line := range strings.Split(stream, "\n") {
		if strings.HasPrefix(line, "event: ") {
			names = append(names, strings.TrimPrefix(line, "event: "))
		}
	}
	return names
}

// TestHandleChatCompletionStreamSinkRendersAnthropicEvents: the real stream
// pipeline (auth, routing, decode loop, accumulator, finish) writes through
// the Anthropic encoder when given one as the sink, and the client sees a
// complete Anthropic event stream -- no [DONE], the upstream's usage in
// message_delta, the deployment's model in message_start.
func TestHandleChatCompletionStreamSinkRendersAnthropicEvents(t *testing.T) {
	frames := strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"lo."},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	var logBuf bytes.Buffer
	p := newSinkTestPipeline(t, &logBuf, func(context.Context) io.ReadCloser { return &fixedSSEReader{r: strings.NewReader(frames)} })
	var out bytes.Buffer
	sink := anthropicmsgs.NewSSEEncoder(&out)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := p.HandleChatCompletionStreamSink(ctx, "Bearer sink-secret", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, sink, "")
	if err != nil {
		t.Fatalf("HandleChatCompletionStreamSink: %v\nlogs: %s", err, logBuf.String())
	}
	s := out.String()
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if got := eventNames(s); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v\n%s", got, want, s)
	}
	for _, needle := range []string{`"text_delta","text":"Hel"`, `"text_delta","text":"lo."`, `"stop_reason":"end_turn"`, `"input_tokens":9,"output_tokens":2`, `"model":"gpt-4o"`} {
		if !strings.Contains(s, needle) {
			t.Errorf("stream lacks %s:\n%s", needle, s)
		}
	}
	if strings.Contains(s, "[DONE]") {
		t.Errorf("an Anthropic stream must not carry the OpenAI [DONE] sentinel:\n%s", s)
	}
}

// TestHandleChatCompletionStreamSinkTruncationEndsWithMaxTokens: when the
// runaway guard cuts the upstream off, an Anthropic client must not see a
// clean close before message_delta (RFC-1 §7 reads that as a dropped
// connection): the open block is closed and the message ends with
// stop_reason max_tokens and message_stop.
func TestHandleChatCompletionStreamSinkTruncationEndsWithMaxTokens(t *testing.T) {
	var logBuf bytes.Buffer
	p := newSinkTestPipeline(t, &logBuf, func(ctx context.Context) io.ReadCloser { return newRunawaySSEReader(ctx, 50, 1_000_000) })
	var out bytes.Buffer
	sink := anthropicmsgs.NewSSEEncoder(&out)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	maxTokens := 5
	err := p.HandleChatCompletionStreamSink(ctx, "Bearer sink-secret", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, MaxTokens: &maxTokens, Messages: []adapter.Message{{Role: "user", Content: "go"}},
	}, sink, "")
	if err != nil {
		t.Fatalf("HandleChatCompletionStreamSink: %v\nlogs: %s", err, logBuf.String())
	}
	s := out.String()
	if !strings.Contains(logBuf.String(), "streaming_runaway_guard_triggered") {
		t.Fatalf("the runaway guard did not trip; logs: %s", logBuf.String())
	}
	names := eventNames(s)
	if len(names) < 3 || strings.Join(names[len(names)-3:], ",") != "content_block_stop,message_delta,message_stop" {
		t.Errorf("stream must end content_block_stop, message_delta, message_stop; got %v", names)
	}
	if !strings.Contains(s, `"stop_reason":"max_tokens"`) {
		t.Errorf("truncated stream must report stop_reason max_tokens:\n%.400s…", s)
	}
}

// TestHandleChatCompletionStreamOpenAIBytesOnTruncationUnchanged pins
// today's OpenAI-route behaviour on a guard trip: no synthesised finish
// chunk, the stream ends with [DONE] alone. The Anthropic synthesis above
// must not leak onto this route.
func TestHandleChatCompletionStreamOpenAIBytesOnTruncationUnchanged(t *testing.T) {
	var logBuf bytes.Buffer
	p := newSinkTestPipeline(t, &logBuf, func(ctx context.Context) io.ReadCloser { return newRunawaySSEReader(ctx, 50, 1_000_000) })
	rec := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	maxTokens := 5
	err := p.HandleChatCompletionStream(ctx, "Bearer sink-secret", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, MaxTokens: &maxTokens, Messages: []adapter.Message{{Role: "user", Content: "go"}},
	}, rec, "")
	if err != nil {
		t.Fatalf("HandleChatCompletionStream: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, `"finish_reason":"length"`) || strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("OpenAI route must not synthesise a finish chunk on a guard trip:\n%.400s…", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("OpenAI route must still end with the [DONE] sentinel alone")
	}
}

// flakySink is a ChunkSink whose first WriteChunk fails and whose later
// writes succeed, capturing what it was given.
type flakySink struct {
	calls  int
	chunks []streaming.ChatCompletionChunk
}

func (f *flakySink) WriteChunk(c streaming.ChatCompletionChunk) error {
	f.calls++
	if f.calls == 1 {
		return errors.New("transient client write error")
	}
	f.chunks = append(f.chunks, c)
	return nil
}
func (f *flakySink) WriteDone() error { return nil }

// TestHandleChatCompletionStreamSinkFirstWriteFailureStillFallsBack:
// firstChunkSent is set only after a successful sink write, so a stream
// whose very first write fails has delivered nothing and may still fall
// back to the next deployment (the no-fallback-after-first-byte rule,
// RFC-1 §7, keyed on the flag); the retry then streams to the same sink. A
// sink abstraction that set the flag before the write would turn this into
// a dead request with nothing delivered.
func TestHandleChatCompletionStreamSinkFirstWriteFailureStillFallsBack(t *testing.T) {
	frames := strings.Join([]string{
		`data: {"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	var logBuf bytes.Buffer
	keys := []identity.VirtualKey{{ID: "sink-key", KeyHash: testHashOf("sink-secret"), RateLimitBurst: 100, RateLimitRefill: 100}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatal(err)
	}
	deployments := []Deployment{
		{Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "secondary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream must not be called")
			return nil, nil
		},
		UpstreamStream: func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
			return &fixedSSEReader{r: strings.NewReader(frames)}, nil
		},
		Logger: slog.New(slog.NewJSONHandler(&logBuf, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := &flakySink{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = p.HandleChatCompletionStreamSink(ctx, "Bearer sink-secret", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, sink, "")
	if err != nil {
		t.Fatalf("want the fallback to rescue a stream whose first write failed before any byte was delivered; got %v", err)
	}
	if sink.calls < 2 || len(sink.chunks) == 0 || sink.chunks[0].Choices[0].Delta.Content != "Hi" {
		t.Errorf("calls=%d chunks=%d; want the retry to stream the content through the same sink", sink.calls, len(sink.chunks))
	}
	if event := decodeLoggedGatewayEvent(t, &logBuf); !event.GetFallbackHappened() {
		t.Error("FallbackHappened = false; a failed first write delivered nothing, so the fallback must have run")
	}
}

// noFlushWriter is an http.ResponseWriter without http.Flusher -- the one
// input streaming.NewWriter refuses.
type noFlushWriter struct {
	hdr    http.Header
	buf    bytes.Buffer
	status int
}

func (n *noFlushWriter) Header() http.Header         { return n.hdr }
func (n *noFlushWriter) Write(b []byte) (int, error) { return n.buf.Write(b) }
func (n *noFlushWriter) WriteHeader(s int)           { n.status = s }

// TestHandleChatCompletionStreamNonFlushingWriterIsStillFinalized: a writer
// constructor failure is an early return like any other, and every early
// return of the stream pipeline is finalized (gateway event, telemetry,
// cost accounting). The sink abstraction must not move the constructor out
// of the deferred finalize's scope.
func TestHandleChatCompletionStreamNonFlushingWriterIsStillFinalized(t *testing.T) {
	var logBuf bytes.Buffer
	p := newSinkTestPipeline(t, &logBuf, func(context.Context) io.ReadCloser {
		t.Fatal("the upstream must never be called when the client writer cannot stream")
		return nil
	})
	err := p.HandleChatCompletionStream(context.Background(), "Bearer sink-secret", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, &noFlushWriter{hdr: http.Header{}}, "")
	if err == nil || !strings.Contains(err.Error(), "dataplane: stream") {
		t.Fatalf("err = %v, want the dataplane: stream constructor error", err)
	}
	if !strings.Contains(logBuf.String(), `"msg":"chat_completion"`) {
		t.Fatalf("the request was not finalized: no chat_completion log line\n%s", logBuf.String())
	}
	event := decodeLoggedGatewayEvent(t, &logBuf)
	if event.GetVirtualKeyId() != "sink-key" {
		t.Errorf("gateway event virtual key = %q, want sink-key (the request reached auth before the writer failed)", event.GetVirtualKeyId())
	}
}

// TestHandleChatCompletionStreamSinkTruncatedResponseIsNotCached: a stream
// the gateway cut off is recorded as finish_reason length / stop_reason
// max_tokens on the canonical response, so the cache guard excludes it and
// an identical request reaches the upstream again instead of being served a
// replay that would claim end_turn (the S8 security review's medium).
func TestHandleChatCompletionStreamSinkTruncatedResponseIsNotCached(t *testing.T) {
	var logBuf bytes.Buffer
	upstreamCalls := 0
	p := newSinkTestPipeline(t, &logBuf, func(ctx context.Context) io.ReadCloser {
		upstreamCalls++
		return newRunawaySSEReader(ctx, 50, 1_000_000)
	})
	maxTokens := 5
	req := adapter.ChatRequest{Model: "gpt-4o", Stream: true, MaxTokens: &maxTokens, Messages: []adapter.Message{{Role: "user", Content: "go"}}}
	for i := 1; i <= 2; i++ {
		var out bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := p.HandleChatCompletionStreamSink(ctx, "Bearer sink-secret", "", "", req, anthropicmsgs.NewSSEEncoder(&out), "")
		cancel()
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if !strings.Contains(out.String(), `"stop_reason":"max_tokens"`) {
			t.Errorf("request %d: want stop_reason max_tokens (never a replayed end_turn):\n%.300s…", i, out.String())
		}
	}
	if upstreamCalls != 2 {
		t.Errorf("upstream calls = %d, want 2: a truncated response must not be served from cache", upstreamCalls)
	}
}

// TestHandleChatCompletionStreamSinkCeilingCrossedOnTheFinishChunkIsNotTruncated:
// a single upstream event can both carry the finish chunk and push the
// accumulated text past the runaway ceiling. The client already received the
// provider's real finish, so the stream is complete, not cut off: the
// Anthropic encoder must report the recorded stop reason, and the response
// is cacheable like any other complete answer.
func TestHandleChatCompletionStreamSinkCeilingCrossedOnTheFinishChunkIsNotTruncated(t *testing.T) {
	// max_tokens 5 → ceiling 200 chars; one frame with 250 chars AND finish_reason stop.
	frames := `data: {"id":"chatcmpl-3","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"` + strings.Repeat("x", 250) + `"},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n"
	var logBuf bytes.Buffer
	upstreamCalls := 0
	p := newSinkTestPipeline(t, &logBuf, func(context.Context) io.ReadCloser {
		upstreamCalls++
		return &fixedSSEReader{r: strings.NewReader(frames)}
	})
	maxTokens := 5
	req := adapter.ChatRequest{Model: "gpt-4o", Stream: true, MaxTokens: &maxTokens, Messages: []adapter.Message{{Role: "user", Content: "go"}}}
	for i := 1; i <= 2; i++ {
		var out bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := p.HandleChatCompletionStreamSink(ctx, "Bearer sink-secret", "", "", req, anthropicmsgs.NewSSEEncoder(&out), "")
		cancel()
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if !strings.Contains(out.String(), `"stop_reason":"end_turn"`) || strings.Contains(out.String(), `"stop_reason":"max_tokens"`) {
			t.Errorf("request %d: the finish chunk arrived, so the stream is complete; want end_turn, not max_tokens:\n%.300s…", i, out.String())
		}
	}
	if !strings.Contains(logBuf.String(), "streaming_runaway_guard_triggered") {
		t.Fatalf("the ceiling was not crossed; the test is vacuous. logs: %s", logBuf.String())
	}
	if upstreamCalls != 1 {
		t.Errorf("upstream calls = %d, want 1: a complete response that happened to cross the ceiling is cacheable", upstreamCalls)
	}
}
