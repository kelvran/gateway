package dataplane

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
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

// reportingBackend is a budget.RedisBackend whose Reserve and Adjust report
// spend, and whose plain spend read returns a sentinel no request-path code
// may use any more (round-5 item 10 phase 2a).
type reportingBackend struct {
	mu              sync.Mutex
	spentBeforeNano int64
	spentAfterNano  int64
	adjustErr       error
	spentReads      int
	adjustCalls     int
}

// 777.000777 USD: a value with a decimal point no log timestamp fraction can
// spell, so its absence from a log buffer is a safe assertion.
const reportingSentinelNano = int64(777_000_777_000)

func (b *reportingBackend) Reserve(_ context.Context, _ string, capNanoUSD, _ int64) (bool, int64, int64, int64, error) {
	if capNanoUSD <= 0 {
		return true, 0, 11, b.spentBeforeNano, nil
	}
	return true, capNanoUSD - b.spentBeforeNano, 11, b.spentBeforeNano, nil
}
func (b *reportingBackend) ReserveFixed(context.Context, string, int64, int64, int64) (bool, error) {
	return true, nil
}
func (b *reportingBackend) Adjust(context.Context, string, int64, int64, bool) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.adjustCalls++
	return b.spentAfterNano, b.adjustErr
}
func (b *reportingBackend) SpentNanoUSD(context.Context, string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spentReads++
	return reportingSentinelNano, nil
}
func (b *reportingBackend) MarkAlertBucket(context.Context, string, float64, int64) (bool, error) {
	return true, nil
}
func (b *reportingBackend) MarkWarnAlerted(context.Context, string, int64) (bool, error) {
	return true, nil
}
func (b *reportingBackend) Delete(context.Context, string) error { return nil }
func (b *reportingBackend) Close() error                         { return nil }

func foldedReadsPipeline(t *testing.T, vk identity.VirtualKey, backend budget.RedisBackend, logBuf *bytes.Buffer) *Pipeline {
	t.Helper()
	verifier, err := identity.NewVerifier([]identity.VirtualKey{vk})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := NewPipeline(Config{
		Verifier:    verifier,
		Limiter:     ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys([]identity.VirtualKey{vk})),
		Budget:      budget.NewRedisTracker(backend, logger),
		Cache:       inprocess.New(0),
		CacheL2:     inprocess.New(0),
		CacheL3:     inprocess.NewLexicalCache(0),
		Guardrails:  guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:    adapter.Registry{"openai": openai.New()},
		Router:      testRouter(deployments),
		Deployments: deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{
			"gpt-4o": {PromptPerToken: decimal.NewFromFloat(0.001), CompletionPerToken: decimal.NewFromFloat(0.002)},
		}),
		Upstream: func(_ context.Context, dep Deployment, _ any) (any, error) {
			return fakeOpenAIResponse(dep.UpstreamModel), nil
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBudgetSpendAtDecisionComesFromTheReservationNotASeparateRead(t *testing.T) {
	var logBuf bytes.Buffer
	backend := &reportingBackend{spentBeforeNano: 300_000_000} // 0.3 USD
	vk := identity.VirtualKey{ID: "fold-key", KeyHash: testHashOf("fold-key"), BudgetUSD: decimal.NewFromInt(1), RateLimitBurst: 100, RateLimitRefill: 100}
	p := foldedReadsPipeline(t, vk, backend, &logBuf)

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer fold-key", "", "", req, ""); err != nil {
		t.Fatal(err)
	}
	event := decodeLoggedGatewayEvent(t, &logBuf)
	if got := event.GetBudgetSpentUsd(); got != "0.3" {
		t.Fatalf("budget_spent_usd = %q, want \"0.3\": the spend the reservation reported, not a separate read", got)
	}
	if backend.spentReads != 0 {
		t.Fatalf("SpentNanoUSD was read %d times on the request path, want 0: the reserve script reports it", backend.spentReads)
	}
}

func TestBudgetWarnAndLadderReadTheSettledSpendNotASecondRead(t *testing.T) {
	var logBuf bytes.Buffer
	backend := &reportingBackend{spentAfterNano: 9_000_000} // 0.009 USD of a 0.01 USD cap
	vk := identity.VirtualKey{ID: "fold-warn", KeyHash: testHashOf("fold-warn"), BudgetUSD: decimal.NewFromFloat(0.01), BudgetWarnPercent: 0.5, RateLimitBurst: 100, RateLimitRefill: 100}
	p := foldedReadsPipeline(t, vk, backend, &logBuf)

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer fold-warn", "", "", req, ""); err != nil {
		t.Fatal(err)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "budget_warn_threshold_crossed") || !strings.Contains(logs, "budget_threshold_crossed") {
		t.Fatalf("the warn threshold and the 90 %% ladder bucket must fire on the settled 0.009 USD, got: %s", logs)
	}
	if !strings.Contains(logs, `"spent_usd":"0.009"`) || strings.Contains(logs, "777.000777") {
		t.Fatalf("the checks must log the spend Adjust reported (0.009), never the plain read's 777.000777: %s", logs)
	}
	if backend.spentReads != 0 || backend.adjustCalls != 1 {
		t.Fatalf("spend reads = %d, adjusts = %d; want 0 and 1", backend.spentReads, backend.adjustCalls)
	}
}

func TestBudgetThresholdChecksAreSkippedWhenTheSettlementFails(t *testing.T) {
	var logBuf bytes.Buffer
	backend := &reportingBackend{spentAfterNano: 9_000_000, adjustErr: errors.New("redis down")}
	vk := identity.VirtualKey{ID: "fold-fail", KeyHash: testHashOf("fold-fail"), BudgetUSD: decimal.NewFromFloat(0.01), BudgetWarnPercent: 0.5, RateLimitBurst: 100, RateLimitRefill: 100}
	p := foldedReadsPipeline(t, vk, backend, &logBuf)

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer fold-fail", "", "", req, ""); err != nil {
		t.Fatal(err)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, `"op":"reconcile"`) {
		t.Fatalf("the lost settlement must still be logged under op=reconcile: %s", logs)
	}
	if strings.Contains(logs, "budget_warn_threshold_crossed") || strings.Contains(logs, "budget_threshold_crossed") {
		t.Fatalf("with the settled spend unknown the threshold checks are skipped for this request, got: %s", logs)
	}
	if !strings.Contains(logs, `"msg":"budget_threshold_checks_skipped"`) || !strings.Contains(logs, `"reason":"settlement_lost"`) {
		t.Fatalf("the skip must be logged as budget_threshold_checks_skipped reason=settlement_lost (R7's greppable twin), got: %s", logs)
	}
	if backend.spentReads != 0 {
		t.Fatalf("spend reads = %d, want 0: no fallback read on the request path", backend.spentReads)
	}
}

// The streaming handler runs the same gate block: one ReserveWithSpend whose
// spend reaches the decision event, no plain spend read, one reporting
// Adjust at the end of the stream.
func TestStreamingBudgetSpendAtDecisionComesFromTheReservationNotASeparateRead(t *testing.T) {
	backend := &reportingBackend{spentBeforeNano: 300_000_000}
	keys := []identity.VirtualKey{{ID: "fold-stream", KeyHash: testHashOf("fold-stream"), BudgetUSD: decimal.NewFromInt(1), RateLimitBurst: 100, RateLimitRefill: 100}}
	const sse = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n" +
		"data: [DONE]\n\n"
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	p := newStreamingTestPipelineWithKeysBudgetAndLogger(t, func(context.Context, Deployment, any) (io.ReadCloser, error) {
		return nopCloserReader{strings.NewReader(sse)}, nil
	}, nil, adapter.Registry{"openai": openai.New()}, keys, budget.NewRedisTracker(backend, logger), logger)

	rec := httptest.NewRecorder()
	req := adapter.ChatRequest{Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer fold-stream", "", "", req, rec, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("the stream did not complete: %s", rec.Body.String())
	}
	if backend.spentReads != 0 || backend.adjustCalls != 1 {
		t.Fatalf("spend reads = %d, adjusts = %d after a stream; want 0 and 1", backend.spentReads, backend.adjustCalls)
	}
	if got := decodeLoggedGatewayEvent(t, &logBuf).GetBudgetSpentUsd(); got != "0.3" {
		t.Fatalf("streaming budget_spent_usd = %q, want \"0.3\" from the reservation", got)
	}
}
