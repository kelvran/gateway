package dataplane

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// warnThresholdTestPipeline builds a Pipeline with a real, nonzero
// PriceTable and a logger writing to logBuf, for the two tests below —
// neither the shared newTestPipeline* helpers (empty price table) nor
// discardLogger (discards output) can prove anything about a specific
// dollar amount crossing a specific warn threshold.
func warnThresholdTestPipeline(t *testing.T, vk identity.VirtualKey, logBuf *bytes.Buffer) *Pipeline {
	t.Helper()
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
		Logger: slog.New(slog.NewJSONHandler(logBuf, nil)),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestFinalizeLogsBudgetWarnThresholdCrossed is the load-bearing proof
// for the "log only" decision (via AskUserQuestion) on
// docs/rfcs/2026-09-05-gateway-budget-warn-threshold.md: a request that
// pushes spend at or above BudgetWarnPercent of BudgetUSD must log a
// warning, but must still succeed — no new API surface, nothing
// rejected.
func TestFinalizeLogsBudgetWarnThresholdCrossed(t *testing.T) {
	var logBuf bytes.Buffer
	vk := identity.VirtualKey{
		ID:                "warn-key",
		KeyHash:           testHashOf("warn-key"),
		BudgetUSD:         decimal.NewFromFloat(0.01),
		BudgetWarnPercent: 0.5, // warn at $0.005; the real call below costs $0.011
		RateLimitBurst:    100,
		RateLimitRefill:   100,
	}
	p := warnThresholdTestPipeline(t, vk, &logBuf)

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer warn-key", req); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	if !strings.Contains(logBuf.String(), "budget_warn_threshold_crossed") {
		t.Errorf("log output missing budget_warn_threshold_crossed warning, got: %s", logBuf.String())
	}
}

// TestFinalizeDoesNotLogBudgetWarnBelowThreshold proves the negative
// case: spend that stays below BudgetWarnPercent of BudgetUSD must never
// log the warning.
func TestFinalizeDoesNotLogBudgetWarnBelowThreshold(t *testing.T) {
	var logBuf bytes.Buffer
	vk := identity.VirtualKey{
		ID:                "under-key",
		KeyHash:           testHashOf("under-key"),
		BudgetUSD:         decimal.NewFromFloat(100),
		BudgetWarnPercent: 0.9, // warn at $90; the real call below costs $0.011
		RateLimitBurst:    100,
		RateLimitRefill:   100,
	}
	p := warnThresholdTestPipeline(t, vk, &logBuf)

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer under-key", req); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	if strings.Contains(logBuf.String(), "budget_warn_threshold_crossed") {
		t.Errorf("log output unexpectedly contains budget_warn_threshold_crossed while spend is far below threshold, got: %s", logBuf.String())
	}
}
