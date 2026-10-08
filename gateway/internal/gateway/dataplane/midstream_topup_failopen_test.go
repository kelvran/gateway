package dataplane

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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

// topupFailingBudgetBackend admits the initial reservation but fails every
// mid-stream top-up (ReserveFixed), the way a Redis outage that began after
// the stream started would.
type topupFailingBudgetBackend struct {
	settlementCapturingBudgetBackend
}

func (*topupFailingBudgetBackend) ReserveFixed(context.Context, string, int64, int64, int64) (bool, error) {
	return false, errors.New("simulated redis outage")
}

// newTopupFailOpenTestPipeline builds a pipeline whose mid-stream top-ups hit the given
// limiter and budget backend; gpt-4o is priced so a 1000-token estimate costs
// $1 (far above the tiny reservations the tests seed).
func newTopupFailOpenTestPipeline(t *testing.T, keys []identity.VirtualKey, limiter *ratelimit.KeyLimiter, backend budget.RedisBackend, logger *slog.Logger) (*Pipeline, Deployment) {
	t.Helper()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        limiter,
		Budget:         budget.NewRedisTracker(backend, logger),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{"gpt-4o": {CompletionPerToken: decimal.RequireFromString("0.001")}}),
		Upstream: func(context.Context, Deployment, any) (any, error) {
			return fakeOpenAIResponse("gpt-4o"), nil
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p, deployments[0]
}

// TestMidStreamTopupBackendErrorsFailOpenOnceWithLogAndMetric pins the
// observability of the two mid-stream reservation top-ups: a Redis error on
// either keeps the stream going (fail-open, as before) AND is logged and
// counted like the pre-call Reserve/ReserveTPM sites already were -- before
// 2026-10-08 both branches were silent. Exactly once per stream: the top-up is
// retried on every chunk while the backend is down, and a counter whose unit is
// {request} must not grow per chunk. The TPM branch returns before the budget
// branch runs, so each dimension is driven by its own pipeline.
func TestMidStreamTopupBackendErrorsFailOpenOnceWithLogAndMetric(t *testing.T) {
	reader := dataplaneTelemetryMetricsReaderForTest()
	var before metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &before); err != nil {
		t.Fatalf("reader.Collect (before): %v", err)
	}
	beforeSnap := snapshotDataplaneTelemetry(t, before)

	const tpmKey, budgetKey = "team-topup-tpm", "team-topup-budget"
	key := func(id string) identity.VirtualKey {
		return identity.VirtualKey{ID: id, KeyHash: testHashOf(id), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("10")}
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	// Scenario A: the Redis TPM backend errors; the budget backend is healthy.
	tpmKeys := []identity.VirtualKey{key(tpmKey)}
	tpmLimiter := ratelimit.NewRedisKeyLimiter([]ratelimit.KeyConfig{{ID: tpmKey, Capacity: 100, RefillPerSecond: 100, TPMCapacity: 1000, TPMRefillPerSecond: 100}}, failingRedisBackend{})
	pTPM, depTPM := newTopupFailOpenTestPipeline(t, tpmKeys, tpmLimiter, &settlementCapturingBudgetBackend{}, logger)
	// Scenario B: no TPM configured (in-memory limiter); the budget top-up fails.
	budgetKeys := []identity.VirtualKey{key(budgetKey)}
	pBudget, depBudget := newTopupFailOpenTestPipeline(t, budgetKeys, ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(budgetKeys)), &topupFailingBudgetBackend{}, logger)

	req := adapter.ChatRequest{Model: "gpt-4o"}
	for _, sc := range []struct {
		name string
		p    *Pipeline
		dep  Deployment
		vk   *identity.VirtualKey
	}{{"tpm", pTPM, depTPM, &tpmKeys[0]}, {"budget", pBudget, depBudget, &budgetKeys[0]}} {
		reservedUSD := decimal.RequireFromString("0.0001")
		var budgetEpoch, tpmEpoch int64
		tpmTokens := 1.0
		var budgetNoted, tpmNoted bool
		msr := midStreamReservation{
			vk: sc.vk, budgetReservedUSD: &reservedUSD, budgetReservationEpoch: &budgetEpoch,
			tpmReservedTokens: &tpmTokens, tpmReservationEpoch: &tpmEpoch,
			budgetTopupFailOpenNoted: &budgetNoted, tpmTopupFailOpenNoted: &tpmNoted,
		}
		for i := 0; i < 3; i++ {
			// 4000 chars ~ 1000 estimated tokens: above both the 1-token TPM
			// reservation and the $0.0001 budget reservation at $0.001/token.
			if !sc.p.checkMidStreamReservationTopup(context.Background(), sc.dep, req, 4000, msr) {
				t.Fatalf("%s call %d: a top-up backend error must fail open and let the stream continue", sc.name, i)
			}
		}
	}

	var after metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &after); err != nil {
		t.Fatalf("reader.Collect (after): %v", err)
	}
	afterSnap := snapshotDataplaneTelemetry(t, after)
	if got := afterSnap.failOpenByKeyID[tpmKey] - beforeSnap.failOpenByKeyID[tpmKey]; got != 1 {
		t.Errorf("kelvran.ratelimit.fail_open delta over three failed TPM top-ups = %d, want exactly 1 (once per stream)", got)
	}
	if got := afterSnap.budgetFailOpenByKeyID[budgetKey] - beforeSnap.budgetFailOpenByKeyID[budgetKey]; got != 1 {
		t.Errorf("kelvran.budget.fail_open delta over three failed budget top-ups = %d, want exactly 1 (once per stream)", got)
	}
	if got := strings.Count(logs.String(), "msg=ratelimit_tpm_backend_unavailable"); got != 1 {
		t.Errorf("ratelimit_tpm_backend_unavailable lines = %d, want 1; logs: %s", got, logs.String())
	}
	if got := strings.Count(logs.String(), "msg=budget_backend_unavailable"); got != 1 {
		t.Errorf("budget_backend_unavailable lines = %d, want 1; logs: %s", got, logs.String())
	}
	if got := strings.Count(logs.String(), "op=mid_stream_topup"); got != 2 {
		t.Errorf("op=mid_stream_topup fields = %d, want 2 (one per dimension); logs: %s", got, logs.String())
	}
}
