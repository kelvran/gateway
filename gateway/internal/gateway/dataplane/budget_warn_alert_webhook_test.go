package dataplane

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/alerting"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// TestBudgetWarnThresholdCrossedWebhookFiresOnlyOncePerEpoch is the
// end-to-end proof for checkBudgetWarnThreshold's new webhook-delivery
// dedup: crossing vk.BudgetWarnPercent on TWO separate billable
// completions within the same rolling-window epoch (BudgetResetInterval
// left at its zero-value lifetime-cap default here, so the epoch never
// advances between the two calls) must deliver the
// "budget_warn_threshold_crossed" webhook exactly ONCE — not zero (the
// dedup must still let the FIRST crossing through) and not twice (the
// dedup must actually suppress the second). The two requests use
// different message content specifically so neither response-cache layer
// serves the second one as a (non-billable) cache hit, which would skip
// checkBudgetWarnThreshold entirely and prove nothing.
//
// BudgetUSD is set high enough ($1) that budget.BudgetAlertBuckets'
// separate 50/75/90/100% ladder (checkBudgetAlertLadder's own,
// independent webhook mechanism) never crosses across these two tiny
// (~$0.011 each) requests -- isolating this test to the warn-threshold
// mechanism alone, no bodyBytes-content filtering required.
func TestBudgetWarnThresholdCrossedWebhookFiresOnlyOncePerEpoch(t *testing.T) {
	var warnDeliveries int32
	firstDelivery := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "budget_warn_threshold_crossed") {
			if atomic.AddInt32(&warnDeliveries, 1) == 1 {
				close(firstDelivery)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	vk := identity.VirtualKey{
		ID:                "warn-hook-key1",
		KeyHash:           testHashOf("warn-hook-key1"),
		BudgetUSD:         decimal.NewFromFloat(1.0),
		BudgetWarnPercent: 0.01, // warnAt = $0.01, well below each ~$0.011 real request's cost
		RateLimitBurst:    100,
		RateLimitRefill:   100,
	}
	verifier, err := identity.NewVerifier([]identity.VirtualKey{vk})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := NewPipeline(Config{
		Verifier:    verifier,
		Limiter:     ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys([]identity.VirtualKey{vk})),
		Budget:      budget.NewTracker(),
		Cache:       inprocess.New(0),
		CacheL2:     inprocess.New(0),
		CacheL3:     inprocess.NewLexicalCache(0),
		Guardrails:  guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:    adapter.Registry{"openai": openai.New()},
		Router:      testRouter(deployments),
		Deployments: deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{
			"gpt-4o": {
				PromptPerToken:     decimal.NewFromFloat(0.001),
				CompletionPerToken: decimal.NewFromFloat(0.002),
			},
		}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			return fakeOpenAIResponse(dep.UpstreamModel), nil
		},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		AlertNotifier: alerting.NewWebhookNotifier(server.URL, "", slog.New(slog.NewTextHandler(io.Discard, nil))),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	req1 := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer warn-hook-key1", "", "", req1, ""); err != nil {
		t.Fatalf("HandleChatCompletion #1: %v", err)
	}

	select {
	case <-firstDelivery:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first warn webhook delivery")
	}

	// Second billable completion, still within the same epoch (no
	// BudgetResetInterval configured) and still above warnAt -- the slog
	// line re-fires every time (unchanged, not asserted here), but the
	// webhook must not deliver a second time. Different message content
	// so this cannot be served as a cache hit.
	req2 := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hello there"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer warn-hook-key1", "", "", req2, ""); err != nil {
		t.Fatalf("HandleChatCompletion #2: %v", err)
	}

	// Give a wrongly-duplicated second delivery a chance to arrive before
	// asserting its absence.
	time.Sleep(300 * time.Millisecond)

	if got := atomic.LoadInt32(&warnDeliveries); got != 1 {
		t.Fatalf("budget_warn_threshold_crossed webhook deliveries after crossing the threshold twice in the same epoch = %d, want exactly 1", got)
	}
}

// TestBudgetWarnThresholdCrossedNeverDeliversWebhookWithNilNotifier mirrors
// TestBudgetThresholdCrossedNeverDeliversWebhookWithNilNotifier's identical
// nil-AlertNotifier no-op proof, for the warn-threshold mechanism
// specifically: a config written before this feature existed must keep
// working exactly as before, with zero webhook activity and no panic on
// the nil check.
func TestBudgetWarnThresholdCrossedNeverDeliversWebhookWithNilNotifier(t *testing.T) {
	vk := identity.VirtualKey{
		ID:                "warn-hook-key2",
		KeyHash:           testHashOf("warn-hook-key2"),
		BudgetUSD:         decimal.NewFromFloat(1.0),
		BudgetWarnPercent: 0.01,
		RateLimitBurst:    100,
		RateLimitRefill:   100,
	}
	verifier, err := identity.NewVerifier([]identity.VirtualKey{vk})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := NewPipeline(Config{
		Verifier:    verifier,
		Limiter:     ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys([]identity.VirtualKey{vk})),
		Budget:      budget.NewTracker(),
		Cache:       inprocess.New(0),
		CacheL2:     inprocess.New(0),
		CacheL3:     inprocess.NewLexicalCache(0),
		Guardrails:  guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:    adapter.Registry{"openai": openai.New()},
		Router:      testRouter(deployments),
		Deployments: deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{
			"gpt-4o": {
				PromptPerToken:     decimal.NewFromFloat(0.001),
				CompletionPerToken: decimal.NewFromFloat(0.002),
			},
		}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			return fakeOpenAIResponse(dep.UpstreamModel), nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// AlertNotifier deliberately left nil.
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer warn-hook-key2", "", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
}
