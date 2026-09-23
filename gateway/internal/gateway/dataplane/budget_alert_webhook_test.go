package dataplane

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestBudgetThresholdCrossedDeliversRealWebhook is the end-to-end proof
// for docs/upgrade-research/operator-alerting-integrations-2026-09-15.md
// Finding 4: checkBudgetAlertLadder's existing OTel-counter/log-line
// signal is now also DELIVERED via a real HTTP POST to a real
// httptest.Server, through the exact same real request path
// (HandleChatCompletion) TestFinalizeLogsBudgetThresholdCrossed already
// proves logs the signal -- not a direct, synthetic call to
// checkBudgetAlertLadder itself.
func TestBudgetThresholdCrossedDeliversRealWebhook(t *testing.T) {
	received := make(chan *http.Request, 1)
	var bodyBytes []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyBytes = b
		received <- r
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	vk := identity.VirtualKey{
		ID:              "hook-key1",
		KeyHash:         testHashOf("hook-key1"),
		BudgetUSD:       decimal.NewFromFloat(0.02), // 50% bucket = $0.01; the real call below costs $0.011
		RateLimitBurst:  100,
		RateLimitRefill: 100,
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

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer hook-key1", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	select {
	case <-received:
		if !strings.Contains(string(bodyBytes), "budget_threshold_crossed") ||
			!strings.Contains(string(bodyBytes), "hook-key1") {
			t.Errorf("webhook body = %s, want it to contain the event type and key_id", bodyBytes)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the webhook delivery")
	}
}

// TestBudgetThresholdCrossedNeverDeliversWebhookWithNilNotifier proves
// the nil-AlertNotifier default (every config file written before this
// feature existed) is a complete, harmless no-op -- the exact same
// request path that delivers a webhook above must still succeed with
// zero webhook activity when AlertNotifier is unset.
func TestBudgetThresholdCrossedNeverDeliversWebhookWithNilNotifier(t *testing.T) {
	vk := identity.VirtualKey{
		ID:              "hook-key2",
		KeyHash:         testHashOf("hook-key2"),
		BudgetUSD:       decimal.NewFromFloat(0.02),
		RateLimitBurst:  100,
		RateLimitRefill: 100,
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
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer hook-key2", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
}
