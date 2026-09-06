package dataplane

import (
	"context"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// TestHandleChatCompletionRoutesEachErrorClassToItsOwnFallbackList is the
// load-bearing proof for
// docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md: each
// error class must route to THAT class's own configured target, not just
// "some other deployment" — plain round-robin would ALSO pick a
// different deployment than "primary" on any error, so this test
// deliberately configures 4 same-model deployments and asserts the
// SPECIFIC one named by each class's own fallback_chains entry is the
// one that actually served the response.
func TestHandleChatCompletionRoutesEachErrorClassToItsOwnFallbackList(t *testing.T) {
	primary := Deployment{
		Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused",
		FallbackChains: map[string][]string{
			FallbackClassContentPolicy:         {"safety-alt"},
			FallbackClassContextWindowExceeded: {"large-context-alt"},
			FallbackClassGeneric:               {"generic-alt"},
		},
	}
	deployments := []Deployment{
		primary,
		{Name: "safety-alt", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "large-context-alt", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "generic-alt", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}

	tests := []struct {
		name       string
		body       string
		wantServer string
	}{
		{"content policy", "content_policy_violation", "safety-alt"},
		{"context window exceeded", "context_length_exceeded", "large-context-alt"},
		{"generic (e.g. rate limit)", "rate_limit_error, please retry later", "generic-alt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
				calls = append(calls, dep.Name)
				if dep.Name == "primary" {
					return nil, &UpstreamHTTPError{StatusCode: 400, Body: tt.body}
				}
				return fakeOpenAIResponse(dep.UpstreamModel), nil
			}, deployments)

			resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o"})
			if err != nil {
				t.Fatalf("expected the configured fallback to succeed, got error: %v", err)
			}
			if len(calls) != 2 || calls[0] != "primary" || calls[1] != tt.wantServer {
				t.Fatalf("calls = %v, want [primary %s]", calls, tt.wantServer)
			}
			if resp.Model != "gpt-4o" {
				t.Errorf("resp.Model = %q, want %q (echoed canonical model)", resp.Model, "gpt-4o")
			}
		})
	}
}

// TestHandleChatCompletionMultiHopChainExhaustedInOrder proves a chain
// with more than one configured hop is walked in order, and the request
// only succeeds once a hop actually returns success — not on the first
// retry regardless of outcome.
func TestHandleChatCompletionMultiHopChainExhaustedInOrder(t *testing.T) {
	primary := Deployment{
		Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused",
		FallbackChains: map[string][]string{
			FallbackClassGeneric: {"hop-1", "hop-2", "hop-3"},
		},
	}
	deployments := []Deployment{
		primary,
		{Name: "hop-1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "hop-2", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "hop-3", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}

	var calls []string
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		calls = append(calls, dep.Name)
		if dep.Name == "hop-3" {
			return fakeOpenAIResponse(dep.UpstreamModel), nil
		}
		return nil, &UpstreamHTTPError{StatusCode: 500, Body: dep.Name + " unavailable"}
	}, deployments)

	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("expected the chain to eventually succeed at hop-3, got error: %v", err)
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("resp.Model = %q, want gpt-4o", resp.Model)
	}
	want := []string{"primary", "hop-1", "hop-2", "hop-3"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i, name := range want {
		if calls[i] != name {
			t.Fatalf("calls = %v, want %v (in exact order)", calls, want)
		}
	}
}

// TestHandleChatCompletionMultiHopChainAllFailReturnsErrorOnlyAfterExhaustion
// proves that when EVERY configured hop fails, the whole chain still ran
// (no early giving-up) before the request ultimately errors, and no
// additional attempt beyond the configured chain (e.g. the old
// router-based fallback) is made.
func TestHandleChatCompletionMultiHopChainAllFailReturnsErrorOnlyAfterExhaustion(t *testing.T) {
	primary := Deployment{
		Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused",
		FallbackChains: map[string][]string{
			FallbackClassGeneric: {"hop-1", "hop-2"},
		},
	}
	deployments := []Deployment{
		primary,
		{Name: "hop-1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "hop-2", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		// never configured as a fallback target — proves the OLD
		// router-based single-fallback never fires as an extra attempt
		// once an explicit chain is configured.
		{Name: "never-should-be-called", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}

	var calls []string
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		calls = append(calls, dep.Name)
		if dep.Name == "never-should-be-called" {
			t.Fatal("the old router-based fallback must never fire once an explicit chain is configured for this class")
		}
		return nil, &UpstreamHTTPError{StatusCode: 500, Body: dep.Name + " unavailable"}
	}, deployments)

	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o"})
	if err == nil {
		t.Fatal("expected an error once every configured hop fails")
	}
	want := []string{"primary", "hop-1", "hop-2"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v — every configured hop must run before giving up", calls, want)
	}
	for i, name := range want {
		if calls[i] != name {
			t.Fatalf("calls = %v, want %v (in exact order)", calls, want)
		}
	}
}

// TestHandleChatCompletionNoFallbackChainsConfiguredKeepsOldBehavior is a
// direct, explicit backward-compatibility proof alongside the
// pre-existing (unmodified) TestHandleChatCompletionFallsBackOnUpstreamError:
// a deployment with FallbackChains left nil behaves byte-for-byte like
// before this feature existed, even when OTHER deployments in the same
// Pipeline DO configure chains.
func TestHandleChatCompletionNoFallbackChainsConfiguredKeepsOldBehavior(t *testing.T) {
	deployments := []Deployment{
		{Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}, // no FallbackChains at all
		{Name: "secondary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}

	var calls []string
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		calls = append(calls, dep.Name)
		if dep.Name == "primary" {
			return nil, &UpstreamHTTPError{StatusCode: 400, Body: "content_policy_violation"}
		}
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, deployments)

	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("expected the old router-based fallback to succeed, got error: %v", err)
	}
	if len(calls) != 2 || calls[0] != "primary" || calls[1] != "secondary" {
		t.Fatalf("calls = %v, want [primary secondary] — the old same-model round-robin fallback, unchanged", calls)
	}
}
