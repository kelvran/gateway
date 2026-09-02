package dataplane

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kelvran/gateway/internal/adapter"
	"github.com/kelvran/gateway/internal/adapter/gemini"
	"github.com/kelvran/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/internal/identity"
	"github.com/kelvran/gateway/internal/ratelimit"
)

// realOpenAISSEStream is a minimal but genuine OpenAI streaming response:
// role delta, two content deltas, a finish_reason chunk, a usage-only
// chunk (stream_options.include_usage), and the [DONE] sentinel.
const realOpenAISSEStream = "" +
	`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"lo!"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}` + "\n\n" +
	"data: [DONE]\n\n"

type nopCloserReader struct{ io.Reader }

func (nopCloserReader) Close() error { return nil }

func newStreamingTestPipeline(t *testing.T, upstreamStream UpstreamStreamCaller, deployments []Deployment, adapters adapter.Registry) *Pipeline {
	t.Helper()
	verifier, err := identity.NewVerifier("test-key")
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	p, err := NewPipeline(Config{
		Verifier: verifier,
		Limiter:  ratelimit.NewTokenBucket(100, 100),
		Cache:    inprocess.New(),
		Adapters: adapters,
		Deployments: func() []Deployment {
			if deployments != nil {
				return deployments
			}
			return []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
		}(),
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream should never be called by a streaming test")
			return nil, nil
		},
		UpstreamStream: upstreamStream,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

func TestHandleChatCompletionStreamCacheMissRealStream(t *testing.T) {
	var upstreamCalls int
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		upstreamCalls++
		return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
	}, nil, adapter.Registry{"openai": openai.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, rec)
	if err != nil {
		t.Fatalf("HandleChatCompletionStream: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1", upstreamCalls)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"content":"Hel"`) || !strings.Contains(body, `"content":"lo!"`) {
		t.Errorf("body missing expected content deltas: %s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("body does not end with [DONE] sentinel: %s", body)
	}

	// Second, identical request must be served from cache — upstream call
	// count must stay at 1, and the fake-streamed body must still carry
	// the accumulated content and the correct finish_reason.
	rec2 := httptest.NewRecorder()
	err = p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
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
	if !strings.Contains(body2, `"finish_reason":"stop"`) {
		t.Errorf("fake-streamed cache-hit body missing finish_reason: %s", body2)
	}
}

func TestHandleChatCompletionStreamUnsupportedProviderReturnsTypedError(t *testing.T) {
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		t.Fatal("upstream stream should never be called for a provider that doesn't support streaming")
		return nil, nil
	}, []Deployment{{Name: "d1", Model: "gemini-pro", Provider: "gemini", UpstreamModel: "gemini-pro", BaseURL: "http://unused"}},
		adapter.Registry{"gemini": gemini.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "gemini-pro", Stream: true,
	}, rec)
	if !errors.Is(err, ErrStreamingNotSupported) {
		t.Fatalf("err = %v, want ErrStreamingNotSupported", err)
	}
}

func TestHandleChatCompletionStreamNotConfiguredOnCacheMiss(t *testing.T) {
	p := newStreamingTestPipeline(t, nil, nil, adapter.Registry{"openai": openai.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec)
	if !errors.Is(err, ErrStreamingNotConfigured) {
		t.Fatalf("err = %v, want ErrStreamingNotConfigured", err)
	}
}

func TestHandleChatCompletionStreamFallbackBeforeFirstByte(t *testing.T) {
	var calls []string
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		calls = append(calls, dep.Name)
		if dep.Name == "primary" {
			return nil, errors.New("simulated connection failure before any byte was read")
		}
		return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
	}, []Deployment{
		{Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "secondary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}, adapter.Registry{"openai": openai.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec)
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	if len(calls) != 2 || calls[0] != "primary" || calls[1] != "secondary" {
		t.Fatalf("calls = %v, want [primary secondary]", calls)
	}
	if !strings.Contains(rec.Body.String(), `"content":"Hel"`) {
		t.Errorf("body missing content from the fallback deployment's stream: %s", rec.Body.String())
	}
}

// failAfterNBytesReader wraps a Reader and returns a real I/O error after
// serving its first n bytes — simulating an upstream connection dying
// mid-stream, after some real content has already reached the client.
type failAfterNBytesReader struct {
	r  *strings.Reader
	n  int
	at int
}

func (f *failAfterNBytesReader) Read(p []byte) (int, error) {
	if f.at >= f.n {
		return 0, errors.New("simulated mid-stream connection loss")
	}
	if len(p) > f.n-f.at {
		p = p[:f.n-f.at]
	}
	n, err := f.r.Read(p)
	f.at += n
	return n, err
}

func TestHandleChatCompletionStreamNoFallbackAfterFirstByte(t *testing.T) {
	var calls []string
	// Fail partway through the FIRST real content chunk's own bytes, so at
	// least one full chunk is guaranteed to have already been decoded and
	// written to the client before the read error surfaces.
	firstChunkEnd := strings.Index(realOpenAISSEStream, "\n\n") + len("\n\n")

	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		calls = append(calls, dep.Name)
		return nopCloserReader{&failAfterNBytesReader{r: strings.NewReader(realOpenAISSEStream), n: firstChunkEnd + 5}}, nil
	}, []Deployment{
		{Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "secondary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}, adapter.Registry{"openai": openai.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec)
	if err == nil {
		t.Fatal("expected an error from the mid-stream connection loss, got nil")
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want exactly 1 (no fallback once a chunk reached the client)", calls)
	}
	if !strings.Contains(rec.Body.String(), `"role":"assistant"`) {
		t.Errorf("body should still contain the chunk written before the failure: %s", rec.Body.String())
	}
}
