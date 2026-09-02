package dataplane

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/kelvran/gateway/internal/adapter"
	"github.com/kelvran/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/internal/cache"
	"github.com/kelvran/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/internal/identity"
	"github.com/kelvran/gateway/internal/ratelimit"
)

// discardLogger silences log output during tests.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func newTestPipeline(t *testing.T, upstream UpstreamCaller, deployments []Deployment) *Pipeline {
	t.Helper()

	verifier, err := identity.NewVerifier("test-key")
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	p, err := NewPipeline(Config{
		Verifier: verifier,
		Limiter:  ratelimit.NewTokenBucket(100, 100), // effectively unlimited for these tests
		Cache:    inprocess.New(),
		Adapters: adapter.Registry{
			"openai": openai.New(),
		},
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream:       upstream,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

func fakeOpenAIResponse(model string) *openai.Response {
	return &openai.Response{
		ID:    "chatcmpl-fake",
		Model: model,
		Choices: []openai.Choice{
			{Index: 0, Message: openai.Message{Role: "assistant", Content: "hello"}, FinishReason: "stop"},
		},
		Usage: openai.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
	}
}

func TestHandleChatCompletionRejectsMissingAuth(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		t.Fatal("upstream should never be called when auth fails")
		return nil, nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	_, err := p.HandleChatCompletion(context.Background(), "", adapter.ChatRequest{Model: "gpt-4o"})
	if err == nil {
		t.Fatal("expected an error for missing Authorization header")
	}
}

func TestHandleChatCompletionRejectsRateLimited(t *testing.T) {
	verifier, err := identity.NewVerifier("test-key")
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	p, err := NewPipeline(Config{
		Verifier: verifier,
		Limiter:  ratelimit.NewTokenBucket(0, 0), // zero burst, zero refill: always empty
		Cache:    inprocess.New(),
		Adapters: adapter.Registry{"openai": openai.New()},
		Deployments: []Deployment{
			{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		},
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("upstream should never be called when rate-limited")
			return nil, nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	_, err = p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestHandleChatCompletionMissThenCacheHit(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	req := adapter.ChatRequest{
		Model:    "gpt-4o",
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}

	resp1, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if resp1.Model != "gpt-4o" {
		t.Errorf("resp1.Model = %q, want %q", resp1.Model, "gpt-4o")
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after first call = %d, want 1", upstreamCalls)
	}

	resp2, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after second (cache-hit) call = %d, want still 1", upstreamCalls)
	}
	if resp2.Choices[0].Message.Content != resp1.Choices[0].Message.Content {
		t.Errorf("cached response content mismatch: %q vs %q", resp2.Choices[0].Message.Content, resp1.Choices[0].Message.Content)
	}
}

func TestHandleChatCompletionFallsBackOnUpstreamError(t *testing.T) {
	var calls []string
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		calls = append(calls, dep.Name)
		if dep.Name == "primary" {
			return nil, errors.New("simulated upstream failure")
		}
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{
		{Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "secondary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	})

	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("upstream calls = %v, want exactly 2 (primary then secondary)", calls)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("resp.Choices[0].FinishReason = %q, want %q", resp.Choices[0].FinishReason, "stop")
	}
}

func TestHandleChatCompletionNoDeploymentForModel(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		t.Fatal("upstream should never be called for an unconfigured model")
		return nil, nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "unknown-model"})
	if err == nil {
		t.Fatal("expected an error for an unconfigured model")
	}
}

// compile-time sanity: cache.Cache is used, not redefined, inside this
// test file's imports.
var _ cache.Cache = (*inprocess.Cache)(nil)
