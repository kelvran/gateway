package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// TestHandleChatCompletionMultiModalPartsVaryTheCacheKey is the
// load-bearing proof for docs/rfcs/2026-09-06-gateway-multimodal-
// content.md's cache-key claim: two requests with identical Content but
// different Parts (e.g. different image data) must never L1-collide —
// serializeMessages's blind json.Marshal of the whole Message,
// including Parts, gives this for free, verified here rather than
// merely argued.
func TestHandleChatCompletionMultiModalPartsVaryTheCacheKey(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	baseReq := adapter.ChatRequest{
		Model: "gpt-4o",
		Messages: []adapter.Message{
			{Role: "user", Content: "what's in this image?", Parts: []adapter.ContentPart{
				{Type: "image", MediaType: "image/png", Data: "aW1hZ2VvbmU="},
			}},
		},
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", baseReq); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after first request = %d, want 1", upstreamCalls)
	}

	differentImageReq := adapter.ChatRequest{
		Model: "gpt-4o",
		Messages: []adapter.Message{
			{Role: "user", Content: "what's in this image?", Parts: []adapter.ContentPart{
				{Type: "image", MediaType: "image/png", Data: "aW1hZ2V0d28="},
			}},
		},
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", differentImageReq); err != nil {
		t.Fatalf("second request (different image): %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after second request (different image, identical Content) = %d, want 2 — must not cache-collide", upstreamCalls)
	}
}

// TestHandleChatCompletionMultiModalPartsCanStillCacheHit is the
// negative-of-the-negative: two requests with genuinely IDENTICAL
// Content AND Parts must still L1 cache-hit — proving Parts doesn't
// accidentally break caching for real repeat requests either.
func TestHandleChatCompletionMultiModalPartsCanStillCacheHit(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	req := adapter.ChatRequest{
		Model: "gpt-4o",
		Messages: []adapter.Message{
			{Role: "user", Content: "what's in this image?", Parts: []adapter.ContentPart{
				{Type: "image", MediaType: "image/png", Data: "aW1hZ2VvbmU="},
			}},
		},
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("second (identical) request: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after two identical multi-modal requests = %d, want 1 (a real cache hit)", upstreamCalls)
	}
}

// TestHandleChatCompletionGuardrailScansTextInMultiModalParts is the
// load-bearing proof for docs/rfcs/2026-09-06-gateway-multimodal-
// content.md's guardrail-coverage claim: PII placed only in
// Parts[].Text (never in Content) must still be caught by the pre-call
// guardrail — the same serializeMessages call the guardrail check
// reuses for the cache key already includes Parts, so this is a real
// no-bypass proof, not merely argued from reading the code.
func TestHandleChatCompletionGuardrailScansTextInMultiModalParts(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	req := adapter.ChatRequest{
		Model: "gpt-4o",
		Messages: []adapter.Message{
			{
				Role: "user",
				// Content is deliberately empty/innocuous — the PII lives
				// only in a Parts text part, the exact bypass this test
				// rules out.
				Content: "please review",
				Parts: []adapter.ContentPart{
					{Type: "text", Text: "my card number is " + fakeCreditCardNumber},
				},
			},
		},
	}

	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req)
	if !errors.Is(err, ErrGuardrailBlocked) {
		t.Fatalf("err = %v, want ErrGuardrailBlocked — PII in Parts[].Text must not bypass the pre-call guardrail", err)
	}
	if upstreamCalls != 0 {
		t.Errorf("upstreamCalls = %d, want 0 — a Block-tier request must never reach upstream", upstreamCalls)
	}
}
