package dataplane

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/router"
)

// TestHandleChatCompletionRoutesTheSameVirtualKeyToTheSameStickySide is
// the real end-to-end wiring proof for Phase 2a: router.SelectSticky's
// own correctness is already proven exhaustively at the router-package
// level (sticky_test.go) — this proves runMissPath's first pick
// (dataplane.go) genuinely threads vk.ID into it, through the real
// HandleChatCompletion path, not just that the router primitive works in
// isolation.
func TestHandleChatCompletionRoutesTheSameVirtualKeyToTheSameStickySide(t *testing.T) {
	depRouter := router.New([]router.Deployment{
		{Name: "stable", Model: "gpt-4o", Weight: 9},
		{Name: "canary", Model: "gpt-4o", Weight: 1, Sticky: true},
	}, router.HealthConfig{})

	// Discover one key ID that lands on each side via the SAME router
	// instance, before it's ever wired into a Pipeline — brute force,
	// mirroring internal/router's own sticky_test.go convention.
	var canaryKeyID, stableKeyID string
	for i := 0; i < 500; i++ {
		id := fmt.Sprintf("tenant-%d", i)
		got, ok := depRouter.SelectSticky("gpt-4o", nil, id)
		if !ok {
			t.Fatalf("SelectSticky(%q): ok=false", id)
		}
		if got == "canary" && canaryKeyID == "" {
			canaryKeyID = id
		}
		if got == "stable" && stableKeyID == "" {
			stableKeyID = id
		}
		if canaryKeyID != "" && stableKeyID != "" {
			break
		}
	}
	if canaryKeyID == "" || stableKeyID == "" {
		t.Fatal("could not find both a canary-side and a stable-side key ID in 500 tries -- test setup is broken")
	}

	deployments := []Deployment{
		{Name: "stable", Model: "gpt-4o", Provider: "openai"},
		{Name: "canary", Model: "gpt-4o", Provider: "openai"},
	}
	keys := []identity.VirtualKey{
		{ID: canaryKeyID, KeyHash: testHashOf("canary-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		{ID: stableKeyID, KeyHash: testHashOf("stable-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	var mu sync.Mutex
	var servedBy []string
	upstream := func(ctx context.Context, dep Deployment, providerReq any) (any, error) {
		mu.Lock()
		servedBy = append(servedBy, dep.Name)
		mu.Unlock()
		return fakeOpenAIResponse("gpt-4o"), nil
	}
	lastServedBy := func() string {
		mu.Lock()
		defer mu.Unlock()
		return servedBy[len(servedBy)-1]
	}

	p, err := NewPipeline(Config{
		Verifier:   verifier,
		Limiter:    ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:     budget.NewTracker(),
		Cache:      inprocess.New(0),
		CacheL2:    inprocess.New(0),
		CacheL3:    inprocess.NewLexicalCache(0),
		Guardrails: guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters: adapter.Registry{
			"openai": openai.New(),
		},
		Router:         depRouter,
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream:       upstream,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}

	for i := 0; i < 5; i++ {
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer canary-secret", "", "", req, ""); err != nil {
			t.Fatalf("call %d (canary key): %v", i, err)
		}
		if got := lastServedBy(); got != "canary" {
			t.Errorf("call %d: canary key's virtual-key ID (%q) served by %q, want canary", i, canaryKeyID, got)
		}
	}

	for i := 0; i < 5; i++ {
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer stable-secret", "", "", req, ""); err != nil {
			t.Fatalf("call %d (stable key): %v", i, err)
		}
		if got := lastServedBy(); got != "stable" {
			t.Errorf("call %d: stable key's virtual-key ID (%q) served by %q, want stable", i, stableKeyID, got)
		}
	}
}
