package dataplane

// Cache L3-lite's own load-bearing full-pipeline proofs, per
// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md: a Block-tier
// request is rejected pre-call; a Warn-tier request proceeds; a
// Block-tier response is rejected post-call on the buffered path but
// audit-only (never withheld) on the streaming path; and a guardrail
// policy-version change forces a real cache miss on all three cache
// layers, never a silent, unchecked serve of a stale hit.

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// fakeCreditCardNumber is a real, Luhn-valid test number (the standard
// Visa test card) — never a real, active account.
const fakeCreditCardNumber = "4111111111111111"

func fakeOpenAIResponseWithContent(model, content string) *openai.Response {
	return &openai.Response{
		ID:    "chatcmpl-fake",
		Model: model,
		Choices: []openai.Choice{
			{Index: 0, Message: openai.Message{Role: "assistant", Content: content}, FinishReason: "stop"},
		},
		Usage: openai.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
	}
}

func TestHandleChatCompletionPreCallBlocksBlockTierRequest(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{
		{Role: "user", Content: "my card number is " + fakeCreditCardNumber},
	}}
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req)
	if err == nil {
		t.Fatal("expected ErrGuardrailBlocked, got nil")
	}
	if !errors.Is(err, ErrGuardrailBlocked) {
		t.Errorf("err = %v, want ErrGuardrailBlocked", err)
	}
	if upstreamCalls != 0 {
		t.Errorf("upstreamCalls = %d, want 0 — a Block-tier request must never reach the upstream", upstreamCalls)
	}
}

func TestHandleChatCompletionPreCallDoesNotBlockWarnTierRequest(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{
		{Role: "user", Content: "you can reach me at jane.doe@example.com"},
	}}
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req)
	if err != nil {
		t.Fatalf("expected a Warn-tier finding to proceed normally, got error: %v", err)
	}
	if upstreamCalls != 1 {
		t.Errorf("upstreamCalls = %d, want 1 — a Warn-tier-only finding must not block", upstreamCalls)
	}
}

func TestHandleChatCompletionPostCallBlocksBlockTierResponse(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponseWithContent("gpt-4o", "sure, here it is: "+fakeCreditCardNumber), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "give me a test card number"}}}
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req)
	if err == nil {
		t.Fatal("expected ErrGuardrailBlocked, got nil")
	}
	if !errors.Is(err, ErrGuardrailBlocked) {
		t.Errorf("err = %v, want ErrGuardrailBlocked", err)
	}
	if upstreamCalls != 1 {
		t.Errorf("upstreamCalls = %d, want 1 — post-call means the upstream WAS called; only the response is rejected", upstreamCalls)
	}
}

// TestHandleChatCompletionPostCallBlockedResponseNeverCached proves a
// Block-tier response is never written to any cache layer — a second,
// identical request must still call the upstream, not be served the
// blocked response from a cache that should never have it.
func TestHandleChatCompletionPostCallBlockedResponseNeverCached(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponseWithContent("gpt-4o", "sure, here it is: "+fakeCreditCardNumber), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "give me a test card number"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err == nil {
		t.Fatal("expected the first request to be blocked post-call")
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err == nil {
		t.Fatal("expected the second, identical request to be blocked again")
	}
	if upstreamCalls != 2 {
		t.Errorf("upstreamCalls = %d, want 2 — a blocked response must never populate the cache", upstreamCalls)
	}
}

// sseStreamWithContent builds a minimal, genuine OpenAI SSE stream whose
// single content delta is content — used to control exactly what text
// the guardrail post-call check (streaming, audit-only) scans.
func sseStreamWithContent(content string) string {
	return "" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"` + content + `"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}` + "\n\n" +
		"data: [DONE]\n\n"
}

// TestHandleChatCompletionStreamPostCallBlockTierIsAuditOnlyNeverWithheld
// is the load-bearing proof of the RFC's decisive streaming design: a
// Block-tier response is still delivered to the client in FULL — the
// post-call check on streaming can only log, never withhold what's
// already been flushed.
func TestHandleChatCompletionStreamPostCallBlockTierIsAuditOnlyNeverWithheld(t *testing.T) {
	stream := sseStreamWithContent("sure, here it is: " + fakeCreditCardNumber)
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		return nopCloserReader{strings.NewReader(stream)}, nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, adapter.Registry{"openai": openai.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec)
	if err != nil {
		t.Fatalf("expected the stream to complete successfully (audit-only, never blocked), got: %v", err)
	}
	if !strings.Contains(rec.Body.String(), fakeCreditCardNumber) {
		t.Errorf("body does not contain the full response content — it must be delivered in full, audit-only: %s", rec.Body.String())
	}
}

// TestCacheHitsAreForcedMissesAfterGuardrailPolicyVersionChanges is the
// load-bearing proof for all three cache layers of the RFC's cache-hit
// safety mechanism: an entry written under one guardrail policy version
// is a real miss once the Engine's version changes, never a silent,
// unchecked serve of a hit whose provenance predates the policy change.
func TestCacheHitsAreForcedMissesAfterGuardrailPolicyVersionChanges(t *testing.T) {
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	l1 := inprocess.New(0)
	l2 := inprocess.New(0)
	l3 := inprocess.NewLexicalCache(0)
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}

	var upstreamCalls int
	upstream := func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}

	buildPipeline := func(policyVersion string) *Pipeline {
		p, err := NewPipeline(Config{
			Verifier:       verifier,
			Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
			Budget:         budget.NewTracker(),
			Cache:          l1,
			CacheL2:        l2,
			CacheL3:        l3,
			Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), policyVersion, nil),
			Adapters:       adapter.Registry{"openai": openai.New()},
			Router:         testRouter(deployments),
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

	pV1 := buildPipeline("v1")
	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "cache me please"}}}

	if _, err := pV1.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("first request (policy v1): %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after first request = %d, want 1", upstreamCalls)
	}

	// Same policy version, same request: a real cache hit (upstream call
	// count stays at 1) — proves the shared cache instances genuinely
	// work before testing the version-mismatch behavior.
	if _, err := pV1.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("second request (still policy v1): %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after second, same-policy request = %d, want still 1 (should be a cache hit)", upstreamCalls)
	}

	// A new Pipeline sharing the SAME cache instances, but a bumped
	// guardrail policy version — every L1/L2/L3 entry written under "v1"
	// must now be a real miss.
	pV2 := buildPipeline("v2")
	if _, err := pV2.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("third request (policy v2): %v", err)
	}
	if upstreamCalls != 2 {
		t.Errorf("upstreamCalls after the policy-version bump = %d, want 2 — a policy change must force a real cache miss on all layers, never a silent stale hit", upstreamCalls)
	}
}
