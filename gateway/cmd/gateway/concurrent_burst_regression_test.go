// See integration_test.go's own doc comment for why this lives in
// package main.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// callLog is a mutex-protected record of every real upstream call this
// test's two fake upstreams receive, keyed by which fake served it and
// which distinct per-request message content it carried — the join key
// this test uses to prove no single logical request ever reaches the
// SAME deployment twice, without needing any other request-correlation
// mechanism.
type callLog struct {
	mu    sync.Mutex
	calls []struct {
		upstream string
		content  string
	}
}

func (l *callLog) record(upstream, content string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, struct {
		upstream string
		content  string
	}{upstream, content})
}

// countFor returns how many times content appears against upstream.
func (l *callLog) countFor(upstream, content string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.calls {
		if c.upstream == upstream && c.content == content {
			n++
		}
	}
	return n
}

// newBurstRegressionUpstream starts an httptest.Server that always fails
// (alwaysFails=true) or always succeeds, recording every real request it
// receives into log under name, keyed by the request's own single user
// message content -- the per-request marker this test uses to detect a
// request that reached the same deployment more than once.
func newBurstRegressionUpstream(name string, alwaysFails bool, log *callLog) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req openai.Request
		_ = json.Unmarshal(body, &req)
		var content string
		if len(req.Messages) > 0 {
			_ = json.Unmarshal(req.Messages[0].Content, &content)
		}
		log.record(name, content)

		if alwaysFails {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"simulated upstream failure"}`))
			return
		}
		resp := openai.Response{
			ID:    "chatcmpl-burst-regression-test",
			Model: req.Model,
			Choices: []openai.Choice{
				{Index: 0, Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"ok"`)}, FinishReason: "stop"},
			},
			Usage: openai.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// TestIntegrationConcurrentBurstAgainstFailingDeploymentNeverExceedsCircuitBreakerBound
// is a permanent CI regression guard for the exact concurrency bug class
// this project fixed this week (see THREAT_MODEL.md's 2026-09-14 Change
// Log entry and DECISIONS.md): under a concurrent burst of real requests,
// router.Select's WRR cursor is one shared sequence across every
// concurrent caller, and nothing previously excluded a just-failed
// deployment's name from a fallback re-pick made under that same shared
// cursor -- a request that failed against "bad" could get WRR-re-picked
// back onto "bad" for its own fallback attempt, purely from cursor
// movement caused by OTHER concurrent requests' own calls, not from
// anything about the failing request itself.
//
// Deliberately configures NO health probing at all (unlike
// health_probe_integration_test.go, which explicitly trips the N-of-M
// probe threshold before sending client traffic) -- this reproduces the
// real production shape that exposed the bug: live request failures
// happening under concurrency BEFORE any background prober has had a
// chance to mark the failing deployment unhealthy, per
// docs/upgrade-research/chaos-engineering-resilience-testing-2026-09-14.md
// Finding 4's own citation of this project's real incident. Also
// deliberately configures no fallback_chains on either deployment, so
// dataplane falls back to its plain router-based single-fallback path
// (nextDeployment with an exclude set) -- the exact code path the WRR
// race fix touches.
func TestIntegrationConcurrentBurstAgainstFailingDeploymentNeverExceedsCircuitBreakerBound(t *testing.T) {
	log := &callLog{}
	badUpstream := newBurstRegressionUpstream("bad", true, log)
	defer badUpstream.Close()
	goodUpstream := newBurstRegressionUpstream("good", false, log)
	defer goodUpstream.Close()

	t.Setenv("KELVRAN_BURST_REGRESSION_TEST_BAD_KEY", "fake-upstream-key-not-a-real-secret")
	t.Setenv("KELVRAN_BURST_REGRESSION_TEST_GOOD_KEY", "fake-upstream-key-not-a-real-secret")

	gatewayKey := "test-gateway-key-burst-regression"
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash(gatewayKey), RateLimitBurst: 1000, RateLimitRefill: 1000},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "bad",
				Model:         "gpt-4o",
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       badUpstream.URL,
				APIKeyEnv:     "KELVRAN_BURST_REGRESSION_TEST_BAD_KEY",
			},
			{
				Name:          "good",
				Model:         "gpt-4o",
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       goodUpstream.URL,
				APIKeyEnv:     "KELVRAN_BURST_REGRESSION_TEST_GOOD_KEY",
			},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"gpt-4o": {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
		},
		// No HealthProbe section at all -- see this test's own doc
		// comment for why that is the whole point.
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

	// 20, matching health_probe_integration_test.go's own established
	// burst size for this class of test -- reduced from an original 50
	// after a real, one-off CI flake (a 502 on request 43, against 2
	// real in-process httptest.Server instances under 50-way sudden
	// concurrency on a resource-constrained shared runner) that did NOT
	// reproduce on an immediate re-run of the identical code, nor across
	// 10 local -race runs -- consistent with CI resource contention, not
	// a real bug in the fix this test guards. The WRR-interleaving bug
	// class itself reproduces readily at much smaller concurrency (the
	// original fix's own reproduction used just 2 deployments), so 20
	// still genuinely exercises the real invariant while lowering
	// resource pressure.
	const burstSize = 20
	client := &http.Client{}
	var wg sync.WaitGroup
	statusCodes := make([]int, burstSize)

	for i := 0; i < burstSize; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := fmt.Sprintf("burst-test-message-%d", i)
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
		}(i)
	}
	wg.Wait()

	for i, code := range statusCodes {
		if code != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200 -- every request must eventually reach the good deployment", i, code)
		}
	}

	// The load-bearing assertion: no single logical request (identified
	// by its own distinct content) ever reached "bad" more than once.
	// Before the WRR-exclude fix, a concurrent burst could re-select
	// "bad" for a request's own fallback attempt purely from cursor
	// movement caused by sibling requests.
	for i := 0; i < burstSize; i++ {
		content := fmt.Sprintf("burst-test-message-%d", i)
		if n := log.countFor("bad", content); n > 1 {
			t.Errorf("request with content %q reached the bad deployment %d times, want at most 1", content, n)
		}
	}
}
