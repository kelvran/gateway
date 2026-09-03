package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
)

// newIntegrationServerWithBudgetPersistence builds the same real
// buildPipeline + chatCompletionsHandler wiring as the other integration
// helpers, but returns the underlying *dataplane.Pipeline too, so a test
// can call Close() on it directly to simulate a clean restart before
// building a second, independent server against the same persistPath.
func newIntegrationServerWithBudgetPersistence(t *testing.T, upstreamURL, upstreamKeyEnvVar, persistPath string, keys []controlplane.VirtualKeyConfig) (*httptest.Server, *dataplane.Pipeline) {
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
		Budget: controlplane.BudgetConfig{PersistPath: persistPath},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler(pipeline))

	srv := httptest.NewServer(mux)
	// Deliberately NOT registering t.Cleanup(srv.Close) for the pipeline
	// itself here — the test closes it explicitly, at the exact point it
	// wants to simulate a restart. The httptest.Server IS auto-cleaned,
	// since leaving an HTTP listener open has nothing to do with what this
	// test is proving.
	t.Cleanup(srv.Close)
	return srv, pipeline
}

// TestIntegrationBudgetPersistsAcrossRestart is the load-bearing
// end-to-end proof for docs/rfcs/2026-09-03-budget-persistence.md: a
// virtual key's spend, recorded by one gateway instance, is still
// enforced by a second, independently-built instance pointed at the same
// persist_path — a real close-and-reopen cycle through the full HTTP
// stack, not just the storage-layer unit tests from Task 1 of that RFC's
// plan.
func TestIntegrationBudgetPersistsAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "budget.db")
	upstream, calls := newMockUpstream(t)

	// The mock upstream always returns usage 7 prompt + 4 completion
	// tokens, costing 7*0.0000025 + 4*0.00001 = 0.0000575 USD per request
	// against this test's price_table — the exact same arithmetic
	// TestIntegrationBudgetExceededReturns429DistinctFromRateLimit already
	// verified. A cap of 0.00001 (below the cost of even one request)
	// lets the FIRST request through (spend starts at 0 < 0.00001) but
	// rejects the very next one, on EITHER instance — tight enough that
	// instance #2's first request will already be over budget if (and
	// only if) persistence actually worked.
	keys := []controlplane.VirtualKeyConfig{
		{Name: "team-durable", KeyHash: testKeyHash("durable-secret"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("0.00001")},
	}

	doRequest := func(gw *httptest.Server) (int, string) {
		reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"spend some durable budget"}]}`
		httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		httpReq.Header.Set("Authorization", "Bearer durable-secret")
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	// Instance #1.
	gw1, pipeline1 := newIntegrationServerWithBudgetPersistence(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_M", dbPath, keys)
	status1, body1 := doRequest(gw1)
	if status1 != http.StatusOK {
		t.Fatalf("instance #1 request status = %d, want 200; body: %s", status1, body1)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("mock upstream calls after instance #1's request = %d, want 1", got)
	}

	// Simulate a clean restart: close instance #1's pipeline (releasing
	// the bbolt file's exclusive lock) before instance #2 ever opens it.
	if err := pipeline1.Close(); err != nil {
		t.Fatalf("pipeline1.Close(): %v", err)
	}

	// Instance #2: a brand-new server, built from scratch, pointed at the
	// SAME persist_path — simulating the gateway process restarting.
	gw2, pipeline2 := newIntegrationServerWithBudgetPersistence(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_M2", dbPath, keys)
	t.Cleanup(func() { _ = pipeline2.Close() })

	status2, body2 := doRequest(gw2)
	if status2 != http.StatusTooManyRequests {
		t.Fatalf("instance #2's first request status = %d, want %d (budget already exceeded, proving spend survived the restart); body: %s", status2, http.StatusTooManyRequests, body2)
	}
	if !bytes.Contains([]byte(body2), []byte("budget")) {
		t.Errorf("instance #2's rejection body = %q, want it to mention \"budget\"", body2)
	}
	// The mock upstream must NOT have been called a second time — the
	// budget check rejects before ever reaching a deployment.
	if got := calls.Load(); got != 1 {
		t.Fatalf("mock upstream calls after instance #2's (should be rejected) request = %d, want still 1", got)
	}
}
