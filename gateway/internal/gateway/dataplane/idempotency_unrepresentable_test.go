package dataplane

import (
	"context"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	idempotencyinprocess "github.com/kelvran/gateway/gateway/internal/idempotency/inprocess"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// A response the canonical shadow cannot represent is never replayed from
// the idempotency store (RFC-1 §8, item 11 slice S11b): like the response
// cache (the S6 flag), the store refuses it and releases the claim, so a
// reused Idempotency-Key re-runs the call rather than returning a copy with a
// block missing. A representable response keeps the documented replay.
func TestHandleChatCompletionUnrepresentableResponseIsNotReplayedByIdempotencyKey(t *testing.T) {
	keys := []identity.VirtualKey{{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	for _, tc := range []struct {
		name      string
		blocks    []anthropic.ContentBlock
		wantCalls int
	}{
		{"unrepresentable response re-runs", []anthropic.ContentBlock{{Type: "text", Text: "ok"}, {Type: "server_tool_use"}}, 2},
		{"representable response replays", []anthropic.ContentBlock{{Type: "text", Text: "ok"}}, 1},
	} {
		calls := 0
		p, err := NewPipeline(Config{
			Verifier:         verifier,
			IdempotencyStore: idempotencyinprocess.New(),
			Limiter:          ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
			Budget:           budget.NewTracker(),
			Cache:            inprocess.New(0),
			CacheL2:          inprocess.New(0),
			CacheL3:          inprocess.NewLexicalCache(0),
			Guardrails:       guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
			Adapters:         adapter.Registry{"anthropic": anthropic.New()},
			Router:           testRouter([]Deployment{{Name: "claude", Model: "claude-sys", Provider: "anthropic", UpstreamModel: "claude-up", BaseURL: "http://unused"}}),
			Deployments:      []Deployment{{Name: "claude", Model: "claude-sys", Provider: "anthropic", UpstreamModel: "claude-up", BaseURL: "http://unused"}},
			CostCalculator:   costaccounting.NewCalculator(costaccounting.PriceTable{}),
			Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
				calls++
				return &anthropic.Response{ID: "msg_1", Model: dep.UpstreamModel, Role: "assistant", Content: tc.blocks, StopReason: "end_turn", Usage: anthropic.Usage{InputTokens: 1, OutputTokens: 1}}, nil
			},
			Logger: discardLogger(),
		})
		if err != nil {
			t.Fatalf("%s: NewPipeline: %v", tc.name, err)
		}
		req := adapter.ChatRequest{Model: "claude-sys", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
		for i := 0; i < 2; i++ {
			if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, "key-"+tc.name); err != nil {
				t.Fatalf("%s: call %d: %v", tc.name, i+1, err)
			}
		}
		if calls != tc.wantCalls {
			t.Errorf("%s: upstream calls = %d, want %d", tc.name, calls, tc.wantCalls)
		}
	}
}
