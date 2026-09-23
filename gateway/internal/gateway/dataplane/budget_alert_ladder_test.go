package dataplane

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

// TestFinalizeLogsBudgetThresholdCrossed proves the new, separate ladder
// mechanism (alongside checkBudgetWarnThreshold's existing, unchanged
// single-percent warning) logs a budget_threshold_crossed line the first
// time a request pushes spend past one of budget.BudgetAlertBuckets'
// fixed rungs.
func TestFinalizeLogsBudgetThresholdCrossed(t *testing.T) {
	var logBuf bytes.Buffer
	vk := identity.VirtualKey{
		ID:              "ladder-key",
		KeyHash:         testHashOf("ladder-key"),
		BudgetUSD:       decimal.NewFromFloat(0.02), // 50% bucket = $0.01; the real call below costs $0.011
		RateLimitBurst:  100,
		RateLimitRefill: 100,
	}
	p := warnThresholdTestPipeline(t, vk, &logBuf)

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer ladder-key", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	if !strings.Contains(logBuf.String(), "budget_threshold_crossed") {
		t.Errorf("log output missing budget_threshold_crossed, got: %s", logBuf.String())
	}
}

// TestFinalizeDoesNotReLogBudgetThresholdCrossedWithinSameBucket proves the
// dedup: a second request that stays within an already-alerted bucket must
// not re-log the ladder crossing, unlike checkBudgetWarnThreshold's own
// deliberately-every-request log line.
func TestFinalizeDoesNotReLogBudgetThresholdCrossedWithinSameBucket(t *testing.T) {
	var logBuf bytes.Buffer
	vk := identity.VirtualKey{
		ID:              "ladder-key-2",
		KeyHash:         testHashOf("ladder-key-2"),
		BudgetUSD:       decimal.NewFromFloat(1), // 50% bucket = $0.50; two $0.011 calls stay far below it
		RateLimitBurst:  100,
		RateLimitRefill: 100,
	}
	p := warnThresholdTestPipeline(t, vk, &logBuf)

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer ladder-key-2", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion (1st): %v", err)
	}
	if strings.Contains(logBuf.String(), "budget_threshold_crossed") {
		t.Fatalf("log output unexpectedly contains budget_threshold_crossed while spend is far below the lowest bucket, got: %s", logBuf.String())
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer ladder-key-2", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion (2nd): %v", err)
	}
	if strings.Contains(logBuf.String(), "budget_threshold_crossed") {
		t.Errorf("log output unexpectedly contains budget_threshold_crossed after a second request still below the lowest bucket, got: %s", logBuf.String())
	}
}
