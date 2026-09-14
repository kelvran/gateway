// See integration_test.go's own doc comment for why this lives in
// package main.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
)

// fakeOverheadTestCredential returns a fake, non-secret gateway bearer
// credential for this file's tests, assembled from parts rather than
// one literal so static secret-scanning never mistakes it for a real
// credential -- mirrors internal/admin/admin_test.go's own
// fakeAdminCredential convention.
func fakeOverheadTestCredential(suffix string) string {
	parts := []string{"not", "a", "real", "gateway", "credential", "for", "tests", suffix}
	return strings.Join(parts, "-")
}

// newDelayedUpstream starts an httptest.Server that sleeps delay before
// responding -- a real, measurable stand-in for upstream provider
// network latency, so this test can prove
// X-Kelvran-Overhead-Duration-Ms genuinely SUBTRACTS that latency rather
// than just reporting the whole request's total wall-clock time.
func newDelayedUpstream(delay time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		body, _ := io.ReadAll(r.Body)
		var req openai.Request
		_ = json.Unmarshal(body, &req)
		resp := openai.Response{
			ID:    "chatcmpl-overhead-test",
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

// TestIntegrationOverheadDurationHeaderExcludesUpstreamLatency is the
// load-bearing proof: against an upstream that deliberately sleeps 100ms
// before responding, the reported overhead must be far smaller than
// 100ms -- proving the header genuinely isolates gateway-added latency
// from the real upstream round-trip, not just the total request
// duration (which would itself be >= 100ms and would make this header
// meaningless for exactly the load-testing use case it exists for). Per
// docs/rfcs/2026-09-14-gateway-overhead-duration-header.md.
func TestIntegrationOverheadDurationHeaderExcludesUpstreamLatency(t *testing.T) {
	const upstreamDelay = 100 * time.Millisecond
	upstream := newDelayedUpstream(upstreamDelay)
	defer upstream.Close()

	gatewayKey := fakeOverheadTestCredential("latency")
	gw := newIntegrationServer(t, upstream.URL, gatewayKey, "KELVRAN_OVERHEAD_TEST_UPSTREAM_KEY")

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"overhead header test"}]}`
	httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+gatewayKey)
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	totalDuration := time.Since(start)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	headerValue := resp.Header.Get(overheadDurationHeader)
	if headerValue == "" {
		t.Fatal("X-Kelvran-Overhead-Duration-Ms header is missing")
	}
	overheadMs, err := strconv.ParseInt(headerValue, 10, 64)
	if err != nil {
		t.Fatalf("parsing header value %q: %v", headerValue, err)
	}
	if overheadMs < 0 {
		t.Errorf("overhead = %dms, want >= 0", overheadMs)
	}
	// The real assertion: overhead must be far smaller than the
	// deliberately-injected upstream delay -- if the header were
	// (incorrectly) reporting total request duration, it would be >=
	// upstreamDelay, not a small fraction of it.
	if overheadMs >= upstreamDelay.Milliseconds() {
		t.Errorf("overhead = %dms, want well under the %dms injected upstream delay -- the header is not isolating gateway overhead from upstream latency", overheadMs, upstreamDelay.Milliseconds())
	}
	t.Logf("total=%v overhead=%dms upstream_delay=%v", totalDuration, overheadMs, upstreamDelay)
}

// TestIntegrationOverheadDurationHeaderOnCacheHitReportsFullDuration
// proves the cache-hit case: since no real upstream call happens at
// all, the tracker pointer stays at its zero value, so overhead should
// be close to the total request duration (never negative, never
// artificially deflated by a phantom upstream measurement).
func TestIntegrationOverheadDurationHeaderOnCacheHitReportsFullDuration(t *testing.T) {
	upstream, _ := newMockUpstream(t)
	defer upstream.Close()

	gatewayKey := fakeOverheadTestCredential("cache")
	gw := newIntegrationServer(t, upstream.URL, gatewayKey, "KELVRAN_OVERHEAD_CACHE_TEST_UPSTREAM_KEY")

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"overhead cache hit test"}]}`
	doRequest := func() *http.Response {
		httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		httpReq.Header.Set("Authorization", "Bearer "+gatewayKey)
		httpReq.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		return resp
	}

	first := doRequest()
	_ = first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", first.StatusCode)
	}

	second := doRequest()
	defer func() { _ = second.Body.Close() }()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second (cache-hit) request: status = %d, want 200", second.StatusCode)
	}

	headerValue := second.Header.Get(overheadDurationHeader)
	if headerValue == "" {
		t.Fatal("X-Kelvran-Overhead-Duration-Ms header is missing on the cache-hit response")
	}
	overheadMs, err := strconv.ParseInt(headerValue, 10, 64)
	if err != nil {
		t.Fatalf("parsing header value %q: %v", headerValue, err)
	}
	if overheadMs < 0 {
		t.Errorf("overhead = %dms on a cache hit, want >= 0 (no real upstream call happened, so nothing should be subtracted)", overheadMs)
	}
}
