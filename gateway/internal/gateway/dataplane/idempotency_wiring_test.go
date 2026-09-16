package dataplane

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
	"github.com/kelvran/gateway/gateway/internal/idempotency"
	idempotencyinprocess "github.com/kelvran/gateway/gateway/internal/idempotency/inprocess"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// newTestPipelineWithIdempotency mirrors newTestPipelineWithKeysAndBudget
// but also wires an idempotency.Store — every OTHER test-pipeline helper
// in this package deliberately leaves Config.IdempotencyStore nil (the
// mechanism's own documented "off" state, per Config.IdempotencyStore's
// doc comment), so this file needs its own dedicated constructor.
func newTestPipelineWithIdempotency(t *testing.T, upstream UpstreamCaller, deployments []Deployment, keys []identity.VirtualKey, store idempotency.Store) *Pipeline {
	t.Helper()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	p, err := NewPipeline(Config{
		Verifier:         verifier,
		IdempotencyStore: store,
		Limiter:          ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:           budget.NewTracker(),
		Cache:            inprocess.New(0),
		CacheL2:          inprocess.New(0),
		CacheL3:          inprocess.NewLexicalCache(0),
		Guardrails:       guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters: adapter.Registry{
			"openai": openai.New(),
		},
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

// TestHandleChatCompletionIdempotencyKeyReplaysTheFirstResponseWithoutASecondUpstreamCall
// proves the replay is genuinely driven by claimIdempotency's own
// StateCompleted branch, not an incidental L1 cache hit (which would be
// indistinguishable from a replay for an identical request body, since
// both would return the first response without a second upstream call):
// the virtual key's rate-limit bucket holds exactly ONE token, so if the
// second call reached checkRateLimit at all (i.e. idempotency did NOT
// short-circuit before it, per claimIdempotency's own "right after auth,
// before rate-limit" placement), it would fail with ErrRateLimited
// instead of succeeding.
func TestHandleChatCompletionIdempotencyKeyReplaysTheFirstResponseWithoutASecondUpstreamCall(t *testing.T) {
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 1, RateLimitRefill: 0},
	}
	var upstreamCalls int
	p := newTestPipelineWithIdempotency(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, keys, idempotencyinprocess.New())

	req := adapter.ChatRequest{
		Model:    "gpt-4o",
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}

	resp1, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req, "replay-key-1")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after first call = %d, want 1", upstreamCalls)
	}

	resp2, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req, "replay-key-1")
	if err != nil {
		t.Fatalf("second (replay) call: %v (a real second call would have failed with ErrRateLimited, since the bucket only had 1 token)", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after replay call = %d, want still 1", upstreamCalls)
	}
	if resp2.ID != resp1.ID {
		t.Errorf("replayed response ID = %q, want %q (the exact first response)", resp2.ID, resp1.ID)
	}
}

// TestHandleChatCompletionNoIdempotencyKeyBehavesExactlyAsBefore is the
// regression guard: merely CONFIGURING an idempotency.Store must not
// change any behavior for a request that sends no Idempotency-Key at
// all. The rate-limit bucket again holds exactly one token, so if an
// empty key accidentally claimed/replayed anything, this would (wrongly)
// succeed instead of hitting ErrRateLimited on the second call.
func TestHandleChatCompletionNoIdempotencyKeyBehavesExactlyAsBefore(t *testing.T) {
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 1, RateLimitRefill: 0},
	}
	var upstreamCalls int
	p := newTestPipelineWithIdempotency(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, keys, idempotencyinprocess.New())

	req := adapter.ChatRequest{
		Model:    "gpt-4o",
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req, ""); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after first call = %d, want 1", upstreamCalls)
	}

	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req, "")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited (an empty Idempotency-Key must be a pure no-op, even with a store configured)", err)
	}
}

// TestHandleChatCompletionIdempotencyKeyMismatchedBodyReturnsFingerprintMismatchError
// is the documented Stripe/OpenAI reuse case: the SAME key, a DIFFERENT
// request body, must be rejected outright, never silently proceed with
// either body.
func TestHandleChatCompletionIdempotencyKeyMismatchedBodyReturnsFingerprintMismatchError(t *testing.T) {
	p := newTestPipelineWithIdempotency(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, defaultTestVirtualKeys(), idempotencyinprocess.New())

	req1 := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	req2 := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "a totally different message"}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req1, "reused-key"); err != nil {
		t.Fatalf("first call: %v", err)
	}

	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req2, "reused-key")
	if !errors.Is(err, idempotency.ErrFingerprintMismatch) {
		t.Fatalf("err = %v, want ErrFingerprintMismatch", err)
	}
}

// TestHandleChatCompletionIdempotencyKeyIsScopedPerTenant guards against a
// real cross-tenant leakage bug caught while writing this feature: an
// idempotency.Store key derived from the bare Idempotency-Key header
// value, with no tenant scoping, would let two different virtual keys
// that happen to send the identical header string AND an identical body
// collide onto the same stored response — tenant B would receive tenant
// A's real completion content it never itself requested. Two DIFFERENT
// tenants sending the SAME idempotency key and the SAME body must each
// get their OWN real upstream call, not a cross-tenant replay.
func TestHandleChatCompletionIdempotencyKeyIsScopedPerTenant(t *testing.T) {
	keys := []identity.VirtualKey{
		{ID: "tenant-a", KeyHash: testHashOf("tenant-a"), RateLimitBurst: 100, RateLimitRefill: 100},
		{ID: "tenant-b", KeyHash: testHashOf("tenant-b"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	var upstreamCalls int
	p := newTestPipelineWithIdempotency(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, keys, idempotencyinprocess.New())

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer tenant-a", req, "shared-literal-key"); err != nil {
		t.Fatalf("tenant-a call: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after tenant-a's call = %d, want 1", upstreamCalls)
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer tenant-b", req, "shared-literal-key"); err != nil {
		t.Fatalf("tenant-b call: %v (must get its OWN upstream call, not tenant-a's replayed response, an error, or a fingerprint mismatch)", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after tenant-b's call = %d, want 2 — cross-tenant idempotency-key collision", upstreamCalls)
	}
}

// newStreamingTestPipelineWithIdempotency mirrors
// newStreamingTestPipelineWithKeysAndBudget but also wires an
// idempotency.Store — see newTestPipelineWithIdempotency's own comment
// for why this file needs its own dedicated constructors.
func newStreamingTestPipelineWithIdempotency(t *testing.T, upstreamStream UpstreamStreamCaller, deployments []Deployment, keys []identity.VirtualKey, store idempotency.Store) *Pipeline {
	t.Helper()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	p, err := NewPipeline(Config{
		Verifier:         verifier,
		IdempotencyStore: store,
		Limiter:          ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:           budget.NewTracker(),
		Cache:            inprocess.New(0),
		CacheL2:          inprocess.New(0),
		CacheL3:          inprocess.NewLexicalCache(0),
		Guardrails:       guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters: adapter.Registry{
			"openai": openai.New(),
		},
		Router:         testRouter(deployments),
		Deployments:    deployments,
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

// TestHandleChatCompletionStreamIdempotencyKeyReplaysWithoutASecondUpstreamCall
// is HandleChatCompletionStream's own analog of the buffered-path replay
// test above — same rate-limit-bucket-of-one proof that idempotency, not
// caching, is what serves the second call, plus the streaming-specific
// assertion that the replay is synthesized via writeFakeStream (a real
// SSE body reaches the client) rather than a second real upstream call.
func TestHandleChatCompletionStreamIdempotencyKeyReplaysWithoutASecondUpstreamCall(t *testing.T) {
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 1, RateLimitRefill: 0},
	}
	var upstreamCalls int
	p := newStreamingTestPipelineWithIdempotency(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		upstreamCalls++
		return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, keys, idempotencyinprocess.New())

	req := adapter.ChatRequest{
		Model:    "gpt-4o",
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	}

	rec1 := httptest.NewRecorder()
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", req, rec1, "stream-replay-key"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after first call = %d, want 1", upstreamCalls)
	}

	rec2 := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", req, rec2, "stream-replay-key")
	if err != nil {
		t.Fatalf("second (replay) call: %v (a real second call would have failed with ErrRateLimited, since the bucket only had 1 token)", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after replay call = %d, want still 1", upstreamCalls)
	}
	if !strings.Contains(rec2.Body.String(), "Hello!") {
		t.Errorf("replayed stream body = %q, want it to contain the original response's own content", rec2.Body.String())
	}
}
