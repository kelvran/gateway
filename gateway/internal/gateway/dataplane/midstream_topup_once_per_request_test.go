package dataplane

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// outageBudgetBackend is a budget.RedisBackend whose Redis is down for the
// whole request: the pre-call Reserve and every mid-stream top-up error.
type outageBudgetBackend struct {
	settlementCapturingBudgetBackend
}

func (*outageBudgetBackend) Reserve(context.Context, string, int64, int64) (bool, int64, int64, error) {
	return false, 0, 0, errors.New("simulated redis outage")
}

func (*outageBudgetBackend) ReserveFixed(context.Context, string, int64, int64, int64) (bool, error) {
	return false, errors.New("simulated redis outage")
}

// TestStreamingFailOpenCountsOncePerRequestAcrossPreCallAndTopups pins the
// {request} unit of kelvran.ratelimit.fail_open and kelvran.budget.fail_open
// on the streaming path: when Redis is down for the whole request, the
// pre-call checks fail open and count once, and the mid-stream top-ups that
// then also fail must NOT count the same request again (the once-per-stream
// notes are seeded from the pre-call outcome).
func TestStreamingFailOpenCountsOncePerRequestAcrossPreCallAndTopups(t *testing.T) {
	// Scenario A: the rate-limit and budget backends are both down; the TPM
	// top-up branch runs first and returns, so it is the one exercised.
	// Scenario B: the limiter is in-memory (never errors) and only the
	// budget backend is down, so the budget top-up branch is exercised.
	for _, sc := range []struct {
		name    string
		limiter func(keyID string, keys []identity.VirtualKey) *ratelimit.KeyLimiter
		wantRL  int64
	}{
		{"rate limit and budget down", func(keyID string, _ []identity.VirtualKey) *ratelimit.KeyLimiter {
			return ratelimit.NewRedisKeyLimiter([]ratelimit.KeyConfig{{ID: keyID, Capacity: 100, RefillPerSecond: 100, TPMCapacity: 1000, TPMRefillPerSecond: 100}}, failingRedisBackend{})
		}, 1},
		{"budget down only", func(_ string, keys []identity.VirtualKey) *ratelimit.KeyLimiter {
			return ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys))
		}, 0},
	} {
		t.Run(sc.name, func(t *testing.T) {
			runStreamingFailOpenOnceScenario(t, "team-outage-"+strings.ReplaceAll(sc.name, " ", "-"), sc.limiter, sc.wantRL)
		})
	}
}

func runStreamingFailOpenOnceScenario(t *testing.T, keyID string, newLimiter func(string, []identity.VirtualKey) *ratelimit.KeyLimiter, wantRateLimitDelta int64) {
	t.Helper()
	reader := dataplaneTelemetryMetricsReaderForTest()
	var before metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &before); err != nil {
		t.Fatalf("reader.Collect (before): %v", err)
	}
	beforeSnap := snapshotDataplaneTelemetry(t, before)

	contentStream := "" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"` + strings.Repeat("a", 400) + `"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"` + strings.Repeat("b", 400) + `"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"` + strings.Repeat("c", 400) + `"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":300,"total_tokens":305}}` + "\n\n" +
		"data: [DONE]\n\n"

	keys := []identity.VirtualKey{{ID: keyID, KeyHash: testHashOf(keyID), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("10")}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        newLimiter(keyID, keys),
		Budget:         budget.NewRedisTracker(&outageBudgetBackend{}, logger),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{"gpt-4o": {CompletionPerToken: decimal.NewFromFloat(0.01)}}),
		Upstream: func(context.Context, Deployment, any) (any, error) {
			t.Fatal("non-streaming Upstream should never be called by this test")
			return nil, nil
		},
		UpstreamStream: func(context.Context, Deployment, any) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(contentStream)), nil
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	rec := httptest.NewRecorder()
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer "+keyID, "", "", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, rec, ""); err != nil {
		t.Fatalf("HandleChatCompletionStream: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("stream did not complete; body: %s", rec.Body.String())
	}

	var after metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &after); err != nil {
		t.Fatalf("reader.Collect (after): %v", err)
	}
	afterSnap := snapshotDataplaneTelemetry(t, after)
	if got := afterSnap.failOpenByKeyID[keyID] - beforeSnap.failOpenByKeyID[keyID]; got != wantRateLimitDelta {
		t.Errorf("kelvran.ratelimit.fail_open delta for one streaming request = %d, want %d (pre-call counted once; top-ups must not count again)", got, wantRateLimitDelta)
	}
	if got := afterSnap.budgetFailOpenByKeyID[keyID] - beforeSnap.budgetFailOpenByKeyID[keyID]; got != 1 {
		t.Errorf("kelvran.budget.fail_open delta for one streaming request = %d, want exactly 1", got)
	}
	if got := strings.Count(logs.String(), "msg=budget_backend_unavailable"); got != 1 {
		t.Errorf("budget_backend_unavailable lines = %d, want 1 (the pre-call one); logs: %s", got, logs.String())
	}
	if got := strings.Count(logs.String(), "op=mid_stream_topup"); got != 0 {
		t.Errorf("mid-stream top-up fail-open lines = %d, want 0 when the pre-call check already failed open; logs: %s", got, logs.String())
	}
}
