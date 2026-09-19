// See integration_test.go's own doc comment for why this lives in
// package main.
package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// TestIntegrationLoadSmokeOverheadStaysBoundedUnderConcurrentBurst is
// item 8's CI-safe load smoke test, per
// docs/upgrade-research/chaos-load-practice-tier1-2026-09-20.md's own
// finding: real teams (k6/Grafana) run a lightweight smoke test on every
// commit and reserve heavier average-load/stress/spike/soak runs for a
// separately-scheduled job, never commit-triggered, since a full run
// costs 3-15+ minutes and can itself trigger an outage. This test stays
// firmly in the "commit-triggered smoke" tier: a bounded, fast,
// concurrent burst against a real in-process gateway and a real
// (near-instant) fake upstream, asserting the gateway's own added
// latency — X-Kelvran-Overhead-Duration-Ms, per
// docs/rfcs/2026-09-14-gateway-overhead-duration-header.md — stays under
// a generous, CI-noise-tolerant ceiling for every request in the burst.
// Cribs concurrent_burst_regression_test.go's exact
// buildPipeline+httptest.Server+goroutine-burst shape; the difference is
// this test's assertion is about LATENCY, not routing correctness.
func TestIntegrationLoadSmokeOverheadStaysBoundedUnderConcurrentBurst(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-load-smoke-test","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	defer upstream.Close()

	t.Setenv("KELVRAN_LOAD_SMOKE_TEST_UPSTREAM_KEY", "fake-upstream-key-not-a-real-secret")

	gatewayKey := "test-gateway-key-load-smoke"
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash(gatewayKey), RateLimitBurst: 1000, RateLimitRefill: 1000},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "load-smoke",
				Model:         "gpt-4o",
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       upstream.URL,
				APIKeyEnv:     "KELVRAN_LOAD_SMOKE_TEST_UPSTREAM_KEY",
			},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"gpt-4o": {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}
	t.Cleanup(func() { _ = pipeline.Close() })

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler(pipeline))
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)

	// 20, matching concurrent_burst_regression_test.go's own established
	// burst size for this class of in-process test.
	const burstSize = 20
	// Generous on purpose — this is a smoke test asserting the overhead
	// header is BOUNDED, not a strict performance regression benchmark:
	// a shared, resource-constrained CI runner under a 20-way sudden
	// burst can add real scheduling jitter that has nothing to do with
	// gateway-added latency. 2 seconds is far above this pipeline's real
	// added overhead against a near-instant fake upstream (single-digit
	// milliseconds in local runs) while still catching a genuine
	// regression (a stray blocking call, an accidental synchronous
	// network round-trip added to the hot path, etc.).
	const maxOverheadMs = 2000

	client := &http.Client{}
	var wg sync.WaitGroup
	statusCodes := make([]int, burstSize)
	overheadValues := make([]string, burstSize)

	start := time.Now()
	for i := 0; i < burstSize; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := fmt.Sprintf("load-smoke-test-message-%d", i)
			reqBody := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`, content)
			req, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(reqBody))
			if err != nil {
				t.Errorf("building request %d: %v", i, err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+gatewayKey)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, resp.Body)
			statusCodes[i] = resp.StatusCode
			overheadValues[i] = resp.Header.Get(overheadDurationHeader)
		}(i)
	}
	wg.Wait()
	totalBurstDuration := time.Since(start)

	for i, code := range statusCodes {
		if code != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, code)
		}
	}

	for i, headerValue := range overheadValues {
		if headerValue == "" {
			t.Errorf("request %d: %s header is missing", i, overheadDurationHeader)
			continue
		}
		overheadMs, err := strconv.ParseInt(headerValue, 10, 64)
		if err != nil {
			t.Errorf("request %d: parsing header value %q: %v", i, headerValue, err)
			continue
		}
		if overheadMs < 0 {
			t.Errorf("request %d: overhead = %dms, want >= 0", i, overheadMs)
		}
		if overheadMs > maxOverheadMs {
			t.Errorf("request %d: overhead = %dms, want <= %dms -- this smoke test's whole purpose is catching exactly this kind of added-latency regression under concurrent load", i, overheadMs, maxOverheadMs)
		}
	}
	t.Logf("burst_size=%d total_wall_clock=%v", burstSize, totalBurstDuration)
}
