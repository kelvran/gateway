package dataplane

import (
	"context"
	"errors"
	"testing"

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

// TestCountTokensRateLimitFailOpenIncrementsMetricCounter is
// TestRateLimitFailOpenIncrementsMetricCounter's twin for the count_tokens
// gate (item 11 slice S10b): a rate-limit backend error must let the count
// through to routing (fail open, like checkRateLimit), record exactly one
// kelvran.ratelimit.fail_open point for the key, and touch no cache layer.
// Same shared ManualReader and before/after snapshot pair as the sibling
// tests in gatewayevents_test.go, for the reasons documented there.
func TestCountTokensRateLimitFailOpenIncrementsMetricCounter(t *testing.T) {
	reader := dataplaneTelemetryMetricsReaderForTest()
	var before metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &before); err != nil {
		t.Fatalf("reader.Collect (before): %v", err)
	}
	beforeSnap := snapshotDataplaneTelemetry(t, before)

	const keyID = "team-count-tokens-failopen"
	keys := []identity.VirtualKey{
		{ID: keyID, KeyHash: testHashOf(keyID), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewRedisKeyLimiter(keyConfigsFromVirtualKeys(keys), failingRedisBackend{}),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("count_tokens must never call an upstream before slice S11")
			return nil, nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	body, err := p.HandleCountTokens(context.Background(), "Bearer "+keyID, "", "gpt-4o", []byte(`{"model":"gpt-4o","messages":[]}`), nil)
	if body != nil {
		t.Fatalf("HandleCountTokens body = %q, want nil before slice S11", body)
	}
	if !errors.Is(err, ErrCountTokensUnavailable) {
		t.Fatalf("HandleCountTokens with a failing rate-limit backend returned %v, want ErrCountTokensUnavailable: the backend error must fail open and fall through to routing", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect (after): %v", err)
	}
	afterSnap := snapshotDataplaneTelemetry(t, rm)
	if got := afterSnap.failOpenByKeyID[keyID] - beforeSnap.failOpenByKeyID[keyID]; got != 1 {
		t.Errorf("kelvran.ratelimit.fail_open[key_id=%s] delta = %d, want 1", keyID, got)
	}
	if got := afterSnap.cacheLookupTotal - beforeSnap.cacheLookupTotal; got != 0 {
		t.Errorf("kelvran.cache.lookup total delta = %d, want 0: count_tokens reads no cache layer", got)
	}
}
