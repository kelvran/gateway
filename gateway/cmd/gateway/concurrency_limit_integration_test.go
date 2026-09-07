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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// concurrencyLimitTestSecret returns a fake, non-secret bearer value for
// this test, built via concatenation rather than a literal so static
// secret-scanning never mistakes it for a real credential — matching
// cache_stampede_integration_test.go's own identical precedent.
func concurrencyLimitTestSecret() string {
	return "not-a-real-" + "concurrency-limit-test-" + "secret"
}

// TestIntegrationConcurrencyCapRejectsNPlusOneWhileNInFlight is the
// per-identity concurrency cap's real, end-to-end proof, per
// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md's design (b):
// a virtual key configured with max_concurrent_requests: 3 gets a 4th
// concurrent request from the SAME key rejected with 429 + a Retry-After
// header while the first 3 are still genuinely outstanding (blocked in
// the real mock upstream, not merely "sent"), then — once those 3
// complete and release their slots — a 5th request succeeds. Mirrors
// cache_stampede_integration_test.go's exact synchronization pattern
// (channels + WaitGroup, not a sleep) for "N requests are genuinely in
// flight before sending the N+1th."
func TestIntegrationConcurrencyCapRejectsNPlusOneWhileNInFlight(t *testing.T) {
	const maxInFlight = 3

	var upstreamCalls atomic.Int64
	release := make(chan struct{})
	inFlightStarted := make(chan struct{}, maxInFlight)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		inFlightStarted <- struct{}{}
		<-release

		body, _ := io.ReadAll(r.Body)
		var req openai.Request
		_ = json.Unmarshal(body, &req)
		resp := openai.Response{
			ID:    "chatcmpl-concurrency-test",
			Model: req.Model,
			Choices: []openai.Choice{
				{Index: 0, Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"hello"`)}, FinishReason: "stop"},
			},
			Usage: openai.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer upstream.Close()

	secret := concurrencyLimitTestSecret()
	gw := newIntegrationServerMultiKey(t, upstream.URL, "OPENAI_API_KEY_CONCURRENCY_TEST", []controlplane.VirtualKeyConfig{
		{
			Name:                  "team-concurrency-capped",
			KeyHash:               testKeyHash(secret),
			RateLimitBurst:        100,
			RateLimitRefill:       100,
			MaxConcurrentRequests: maxInFlight,
		},
	})

	sendRequest := func(content string) (status int, retryAfter string, body []byte) {
		reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"` + content + `"}]}`
		httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		httpReq.Header.Set("Authorization", "Bearer "+secret)
		httpReq.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Retry-After"), b
	}

	// Send maxInFlight requests, each with distinct content so none of
	// them coalesce via the cache-stampede singleflight mechanism —
	// this test wants maxInFlight genuinely SEPARATE in-flight upstream
	// calls, not one shared one.
	var wg sync.WaitGroup
	statuses := make([]int, maxInFlight)
	wg.Add(maxInFlight)
	for i := 0; i < maxInFlight; i++ {
		go func(i int) {
			defer wg.Done()
			statuses[i], _, _ = sendRequest("concurrency-test-message-" + strconv.Itoa(i))
		}(i)
	}

	// Wait until all maxInFlight requests are genuinely blocked inside
	// the mock upstream before sending the N+1th.
	for i := 0; i < maxInFlight; i++ {
		select {
		case <-inFlightStarted:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for request %d to reach the mock upstream", i)
		}
	}

	// The 4th, while the first 3 are still genuinely outstanding, must
	// be rejected.
	rejectedStatus, retryAfter, rejectedBody := sendRequest("concurrency-test-message-rejected")
	if rejectedStatus != http.StatusTooManyRequests {
		t.Fatalf("4th (N+1th) concurrent request status = %d, want %d; body: %s", rejectedStatus, http.StatusTooManyRequests, rejectedBody)
	}
	if retryAfter == "" {
		t.Fatal("4th (N+1th) concurrent request: Retry-After header is absent, want a positive integer")
	}
	if seconds, err := strconv.Atoi(retryAfter); err != nil || seconds < 1 {
		t.Fatalf("Retry-After = %q, want a positive integer", retryAfter)
	}

	// Release the first maxInFlight upstream calls and let them complete.
	close(release)
	wg.Wait()
	for i, status := range statuses {
		if status != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, status)
		}
	}

	// Only once the first maxInFlight requests have genuinely completed
	// (releasing their slots) does a fresh request succeed again.
	finalStatus, _, finalBody := sendRequest("concurrency-test-message-after-release")
	if finalStatus != http.StatusOK {
		t.Fatalf("request sent after the in-flight slots were released: status = %d, want 200; body: %s", finalStatus, finalBody)
	}

	if got := upstreamCalls.Load(); got != maxInFlight+1 {
		t.Errorf("upstreamCalls = %d, want exactly %d (the %d in-flight requests, plus the one sent after release; the rejected 4th must never have reached upstream)", got, maxInFlight+1, maxInFlight)
	}
}

// TestIntegrationConcurrencyCapIsPerKeyNotGlobal proves a different
// virtual key's requests are never rejected by another key's exhausted
// concurrency cap — the same per-tenant isolation discipline this
// codebase's rate-limit/budget integration tests already enforce
// elsewhere.
func TestIntegrationConcurrencyCapIsPerKeyNotGlobal(t *testing.T) {
	release := make(chan struct{})
	inFlightStarted := make(chan struct{}, 1)
	var firstRequest atomic.Bool

	// Only the FIRST request to reach this shared mock upstream (team-A's
	// slot-holding one) blocks on release — every subsequent request
	// (team-B's, arriving while team-A is still held open) must complete
	// immediately, which is exactly what proves team-B's own concurrency
	// cap is untouched by team-A's exhausted one.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if firstRequest.CompareAndSwap(false, true) {
			inFlightStarted <- struct{}{}
			<-release
		}
		body, _ := io.ReadAll(r.Body)
		var req openai.Request
		_ = json.Unmarshal(body, &req)
		resp := openai.Response{
			ID:      "chatcmpl-per-key-test",
			Model:   req.Model,
			Choices: []openai.Choice{{Index: 0, Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"hi"`)}, FinishReason: "stop"}},
			Usage:   openai.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer upstream.Close()

	secretA := "not-a-real-per-key-test-secret-a"
	secretB := "not-a-real-per-key-test-secret-b"
	gw := newIntegrationServerMultiKey(t, upstream.URL, "OPENAI_API_KEY_PER_KEY_CONCURRENCY_TEST", []controlplane.VirtualKeyConfig{
		{Name: "team-a", KeyHash: testKeyHash(secretA), RateLimitBurst: 100, RateLimitRefill: 100, MaxConcurrentRequests: 1},
		{Name: "team-b", KeyHash: testKeyHash(secretB), RateLimitBurst: 100, RateLimitRefill: 100, MaxConcurrentRequests: 1},
	})

	send := func(secret, content string) int {
		reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"` + content + `"}]}`
		httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		httpReq.Header.Set("Authorization", "Bearer "+secret)
		httpReq.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	var wg sync.WaitGroup
	var statusA int
	wg.Add(1)
	go func() {
		defer wg.Done()
		statusA = send(secretA, "team-a-message")
	}()

	select {
	case <-inFlightStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for team-a's request to reach the mock upstream")
	}

	// team-b, an entirely different virtual key, must succeed
	// immediately even though team-a's own single slot is fully
	// exhausted right now.
	if statusB := send(secretB, "team-b-message"); statusB != http.StatusOK {
		t.Fatalf("team-b's request status = %d, want 200 -- team-a's exhausted concurrency cap must never affect team-b", statusB)
	}

	close(release)
	wg.Wait()
	if statusA != http.StatusOK {
		t.Errorf("team-a's request status = %d, want 200", statusA)
	}
}
