// See integration_test.go's own doc comment for why this lives in
// package main.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// newHealthProbeCountingUpstream starts an httptest.Server that always
// responds with either a genuine 200 (healthy) or a 500 (failing)
// OpenAI-shaped response, per the healthy flag — and counts every real
// request it receives.
func newHealthProbeCountingUpstream(healthy bool) (*httptest.Server, *atomic.Int64) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var req openai.Request
		_ = json.Unmarshal(body, &req)
		if !healthy {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"simulated upstream failure"}`))
			return
		}
		resp := openai.Response{
			ID:    "chatcmpl-health-probe-test",
			Model: req.Model,
			Choices: []openai.Choice{
				{Index: 0, Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"hello"`)}, FinishReason: "stop"},
			},
			Usage: openai.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return srv, &calls
}

// TestIntegrationHealthProbeRoutesAroundConsistentlyFailingDeployment is
// the plan's own required end-to-end proof: two real deployments for one
// model, each backed by its own real httptest upstream — one always
// fails, one always succeeds. Confirms the router correctly routes every
// real client request around the failing one, over real HTTP on both the
// client and upstream sides, once (and only once) the N-of-M threshold
// trips — mirroring cache_stampede_integration_test.go's own "prove it
// end-to-end, over real HTTP on both sides" precedent.
func TestIntegrationHealthProbeRoutesAroundConsistentlyFailingDeployment(t *testing.T) {
	goodUpstream, goodCalls := newHealthProbeCountingUpstream(true)
	defer goodUpstream.Close()
	badUpstream, badCalls := newHealthProbeCountingUpstream(false)
	defer badUpstream.Close()

	t.Setenv("KELVRAN_HEALTH_PROBE_TEST_GOOD_KEY", "fake-upstream-key-not-a-real-secret")
	t.Setenv("KELVRAN_HEALTH_PROBE_TEST_BAD_KEY", "fake-upstream-key-not-a-real-secret")

	gatewayKey := "test-gateway-key-health-probe"
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash(gatewayKey), RateLimitBurst: 1000, RateLimitRefill: 1000},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "good",
				Model:         "gpt-4o",
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       goodUpstream.URL,
				APIKeyEnv:     "KELVRAN_HEALTH_PROBE_TEST_GOOD_KEY",
			},
			{
				Name:          "bad",
				Model:         "gpt-4o",
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       badUpstream.URL,
				APIKeyEnv:     "KELVRAN_HEALTH_PROBE_TEST_BAD_KEY",
			},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"gpt-4o": {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
		},
		HealthProbe: controlplane.HealthProbeConfig{
			// IntervalSeconds is deliberately left at 0 here: this test
			// drives ProbeDeployments directly (deterministic, no real
			// elapsed time) rather than waiting on RunHealthProbeLoop's
			// real ticker — IntervalSeconds only controls whether
			// cmd/gateway's production run() starts that ticker, which
			// this test never calls at all.
			UnhealthyThreshold: 3,
			HealthyThreshold:   2,
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

	ctx := context.Background()

	// Below the 3-failure threshold: "bad" must still be probed as
	// eligible (no client requests sent yet, so goodCalls/badCalls are
	// solely from probes at this point).
	pipeline.ProbeDeployments(ctx)
	pipeline.ProbeDeployments(ctx)
	if got := badCalls.Load(); got != 2 {
		t.Fatalf("badCalls after 2 probe passes = %d, want 2", got)
	}

	// The 3rd probe pass trips the threshold.
	pipeline.ProbeDeployments(ctx)

	badCallsBeforeClientTraffic := badCalls.Load()
	goodCallsBeforeClientTraffic := goodCalls.Load()

	// Drive 20 real HTTP client requests, each with distinct content so
	// none is served from cache. Every one must reach only goodUpstream.
	client := &http.Client{}
	for i := 0; i < 20; i++ {
		reqBody := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":"unique probe-routing question #%d"}]}`, i)
		req, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(reqBody))
		if err != nil {
			t.Fatalf("building request %d: %v", i, err)
		}
		req.Header.Set("Authorization", "Bearer "+gatewayKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, body = %s", i, resp.StatusCode, body)
		}
	}

	if got := badCalls.Load() - badCallsBeforeClientTraffic; got != 0 {
		t.Errorf("real client requests reaching the excluded \"bad\" upstream = %d, want 0", got)
	}
	if got := goodCalls.Load() - goodCallsBeforeClientTraffic; got != 20 {
		t.Errorf("real client requests reaching \"good\" upstream = %d, want 20", got)
	}
}
