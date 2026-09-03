package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/kelvran/gateway/internal/gateway/controlplane"
)

// newIntegrationServerWithRedisRateLimit builds the same real
// buildPipeline + chatCompletionsHandler wiring as the other integration
// helpers, pointed at a Redis-backed rate limiter.
func newIntegrationServerWithRedisRateLimit(t *testing.T, upstreamURL, upstreamKeyEnvVar, redisAddr string, keys []controlplane.VirtualKeyConfig) *httptest.Server {
	t.Helper()
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")

	cfg := &controlplane.Config{
		ListenAddr:  ":0",
		VirtualKeys: keys,
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "gpt4o-primary",
				Model:         "gpt-4o",
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       upstreamURL,
				APIKeyEnv:     upstreamKeyEnvVar,
			},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"gpt-4o": {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
		},
		RateLimit: controlplane.RateLimitConfig{RedisAddr: redisAddr},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}
	t.Cleanup(func() { _ = pipeline.Close() })

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler(pipeline))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestIntegrationTwoGatewayInstancesShareOneRedisRateLimit is the
// load-bearing end-to-end proof for
// docs/rfcs/2026-09-03-distributed-rate-limiting.md: two independently
// built *dataplane.Pipeline instances (simulating two separate gateway
// processes) pointed at the same Redis address and the same virtual key
// share exactly one burst budget between them — the multi-instance
// correctness property the in-memory ratelimit.TokenBucket cannot
// provide, and the entire reason this RFC exists. This is proven again
// here at the full HTTP-stack level, on top of
// internal/ratelimit/redislimiter's own backend-level proof of the same
// property.
func TestIntegrationTwoGatewayInstancesShareOneRedisRateLimit(t *testing.T) {
	ctx := context.Background()
	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Fatalf("starting test Redis container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	connStr, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("getting test Redis connection string: %v", err)
	}
	redisAddr := strings.TrimPrefix(connStr, "redis://")

	upstream, calls := newMockUpstream(t)

	// A burst of 3 and a negligible refill rate isolate this test from
	// timing flakiness — within the few milliseconds this test runs,
	// refill contributes nothing measurable, so exactly 3 of the first 6
	// requests must succeed, regardless of which of the two "instances"
	// each one lands on.
	keys := []controlplane.VirtualKeyConfig{
		{Name: "team-shared", KeyHash: testKeyHash("shared-secret"), RateLimitBurst: 3, RateLimitRefill: 0.001},
	}

	gw1 := newIntegrationServerWithRedisRateLimit(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_N1", redisAddr, keys)
	gw2 := newIntegrationServerWithRedisRateLimit(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_N2", redisAddr, keys)

	// Each call uses distinct message content (index i) so no two requests
	// can ever share a cache entry — cache lookup happens AFTER the
	// rate-limit check in the Request Lifecycle
	// (gateway/ARCHITECTURE.md), so an identical repeated request would
	// be correctly rate-limited but then possibly served from cache
	// rather than hitting the mock upstream, conflating this test's two
	// separate concerns (rate-limit correctness vs. cache behavior).
	// Distinct content guarantees every rate-limit-allowed request is a
	// genuine cache miss.
	doRequest := func(gw *httptest.Server, i int) int {
		reqBody := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":"consume shared rate limit #%d"}]}`, i)
		httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		httpReq.Header.Set("Authorization", "Bearer shared-secret")
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.ReadAll(resp.Body)
		return resp.StatusCode
	}

	succeeded := 0
	for i := 0; i < 6; i++ {
		gw := gw1
		if i%2 == 1 {
			gw = gw2
		}
		if status := doRequest(gw, i); status == http.StatusOK {
			succeeded++
		}
	}

	if succeeded != 3 {
		t.Fatalf("succeeded = %d across both instances combined, want exactly 3 (the shared burst capacity) — a per-instance-only cap would allow up to 6", succeeded)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("mock upstream calls = %d, want 3 (one per successfully-allowed request, each with distinct content so none could be cache-served)", got)
	}
}

// TestIntegrationRedisRateLimitFailsOpenWhenRedisUnreachable is the
// end-to-end proof of this RFC's fail-open policy: pointed at a Redis
// address nothing is listening on, requests still succeed (never
// rejected), because internal/gateway/dataplane.Pipeline.checkRateLimit
// logs the backend error and allows the request through rather than
// failing closed.
func TestIntegrationRedisRateLimitFailsOpenWhenRedisUnreachable(t *testing.T) {
	upstream, calls := newMockUpstream(t)

	keys := []controlplane.VirtualKeyConfig{
		{Name: "team-failopen", KeyHash: testKeyHash("failopen-secret"), RateLimitBurst: 1, RateLimitRefill: 1},
	}

	// A port nothing is listening on — Redis is unreachable for the
	// entire lifetime of this test.
	gw := newIntegrationServerWithRedisRateLimit(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_N3", "127.0.0.1:1", keys)

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"should succeed despite unreachable redis"}]}`
	httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer failopen-secret")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open: an unreachable rate-limit backend must not reject the request); body: %s", resp.StatusCode, body)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("mock upstream calls = %d, want 1 (the request must have actually proceeded to the upstream, not just returned 200 by coincidence)", got)
	}
}
