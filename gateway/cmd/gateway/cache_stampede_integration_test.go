// See integration_test.go's own doc comment for why this lives in
// package main.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
)

// fakeStampedeTestSecret returns a fake, non-secret bearer value for this
// test, built via concatenation rather than a literal so static
// secret-scanning never mistakes it for a real credential — matching
// this project's own established pattern (e.g. graceful_shutdown_
// integration_test.go's gracefulShutdownTestSecret()).
func fakeStampedeTestSecret() string {
	return "not-a-real-" + "stampede-test-" + "secret"
}

// TestIntegrationConcurrentIdenticalRequestsCoalesceIntoOneUpstreamCall
// proves the cache concurrent-miss-stampede fix end-to-end, over real
// HTTP on both sides (client requests AND the mock upstream): N real,
// concurrent HTTP requests for the exact same virtual key/model/messages
// must produce exactly one real call to the upstream provider, per
// THREAT_MODEL.md's Cache Denial-of-Service row.
func TestIntegrationConcurrentIdenticalRequestsCoalesceIntoOneUpstreamCall(t *testing.T) {
	var upstreamCalls atomic.Int64
	release := make(chan struct{})
	started := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upstreamCalls.Add(1) == 1 {
			close(started)
		}
		<-release
		body, _ := io.ReadAll(r.Body)
		var req openai.Request
		_ = json.Unmarshal(body, &req)
		resp := openai.Response{
			ID:    "chatcmpl-stampede-test",
			Model: req.Model,
			Choices: []openai.Choice{
				{Index: 0, Message: openai.Message{Role: "assistant", Content: "hello"}, FinishReason: "stop"},
			},
			Usage: openai.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer upstream.Close()

	secret := fakeStampedeTestSecret()
	srv := newIntegrationServer(t, upstream.URL, secret, "OPENAI_API_KEY_STAMPEDE_TEST")

	const n = 10
	var ready sync.WaitGroup
	ready.Add(n)
	goCh := make(chan struct{})
	var wg sync.WaitGroup
	statuses := make([]int, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-goCh
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
				strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Errorf("request %d: building request: %v", i, err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+secret)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			statuses[i] = resp.StatusCode
		}(i)
	}

	ready.Wait()
	close(goCh)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the real upstream call to start")
	}
	time.Sleep(100 * time.Millisecond)
	close(release)

	wg.Wait()

	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstreamCalls = %d, want exactly 1 -- %d concurrent identical HTTP requests must coalesce into one real upstream call", got, n)
	}
	for i, status := range statuses {
		if status != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, status)
		}
	}
}
