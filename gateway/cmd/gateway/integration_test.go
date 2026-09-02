// This file is deliberately package main (not main_test / an external
// _test package): the real production wiring (buildPipeline,
// chatCompletionsHandler) is unexported, on purpose, since cmd/gateway is
// a binary entrypoint with no external consumers. An external test
// package could not reach either function without either exporting them
// (weakening the "this is a binary, not a library" boundary) or
// reimplementing the wiring inline (which would silently drift from what
// main.go actually does and defeat the point of an end-to-end test). Living
// inside package main lets this test call the exact same functions run()
// calls, so "wired the same way cmd/gateway/main.go wires it" is
// mechanically true, not just asserted in a comment.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/kelvran/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/internal/gateway/controlplane"
)

// newMockUpstream starts an httptest.Server that speaks OpenAI's actual
// Chat Completions wire format (a real JSON response shape, not a
// hand-wavy stub), so the dataplane's adapter + real net/http client code
// all run for real against it. Only the actual upstream LLM provider (the
// network endpoint bytes travel to) is faked, per
// docs/testing/TESTING.md §4's "never hit a real upstream LLM provider
// API in CI" ban and its prescribed fix: "a local mock server that speaks
// each provider's actual wire format".
func newMockUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}
		var req openai.Request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid upstream request body: %v", err), http.StatusBadRequest)
			return
		}

		resp := openai.Response{
			ID:    "chatcmpl-integration-test",
			Model: req.Model,
			Choices: []openai.Choice{
				{
					Index:        0,
					Message:      openai.Message{Role: "assistant", Content: "hello from the mock upstream"},
					FinishReason: "stop",
				},
			},
			Usage: openai.Usage{PromptTokens: 7, CompletionTokens: 4, TotalTokens: 11},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// newIntegrationServer builds the exact same pipeline main.run() builds
// (buildPipeline) and serves it through the exact same handler
// (chatCompletionsHandler) wrapped in a real httptest.Server driven by a
// real net/http client — auth, rate-limit, in-process cache, round-robin
// router, real openai adapter, and real cost accounting all run
// unmodified. Only the deployment's BaseURL points at the mock upstream
// from newMockUpstream instead of a real provider.
func newIntegrationServer(t *testing.T, upstreamURL, gatewayKey, upstreamKeyEnvVar string) *httptest.Server {
	t.Helper()

	const apiKeyEnv = "KELVRAN_INTEGRATION_TEST_GATEWAY_KEY"
	t.Setenv(apiKeyEnv, gatewayKey)
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")

	cfg := &controlplane.Config{
		ListenAddr: ":0",
		APIKeyEnv:  apiKeyEnv,
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
			"gpt-4o": {PromptPerToken: 0.0000025, CompletionPerToken: 0.00001},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler(pipeline))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestIntegrationMissingAuthRejected drives (a): a request with no
// Authorization header must be rejected before ever reaching the mock
// upstream.
func TestIntegrationMissingAuthRejected(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_A")

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", bytes.NewReader([]byte(reqBody)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("mock upstream calls = %d, want 0 (auth must fail before ever reaching upstream)", got)
	}
}

// TestIntegrationWellFormedRequestSucceeds drives (b): a well-formed,
// authenticated, OpenAI-shaped request over a real net/http round trip
// must return 200 with a valid canonical response body.
func TestIntegrationWellFormedRequestSucceeds(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_B")

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"integration test hello"}]}`
	httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer test-gateway-key")
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusOK, body)
	}

	var decoded struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}

	if decoded.Model != "gpt-4o" {
		t.Errorf("response Model = %q, want %q", decoded.Model, "gpt-4o")
	}
	if len(decoded.Choices) != 1 || decoded.Choices[0].Message.Content != "hello from the mock upstream" {
		t.Errorf("response Choices = %+v, want one choice echoing the mock upstream's content", decoded.Choices)
	}
	if decoded.Usage.TotalTokens != 11 {
		t.Errorf("response Usage.TotalTokens = %d, want 11", decoded.Usage.TotalTokens)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("mock upstream calls = %d, want exactly 1", got)
	}
}

// TestIntegrationRepeatedRequestServedFromCache drives (c): the same
// request sent twice must be served from cache the second time — proven
// by the mock-upstream call counter never incrementing past 1, meaning
// the second HTTP round trip through the real gateway never reached the
// fake upstream at all.
func TestIntegrationRepeatedRequestServedFromCache(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_C")

	reqBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"cache me please"}]}`)

	doRequest := func() *http.Response {
		httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader(reqBody))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		httpReq.Header.Set("Authorization", "Bearer test-gateway-key")
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		return resp
	}

	resp1 := doRequest()
	body1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first request status = %d, want 200; body: %s", resp1.StatusCode, body1)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("mock upstream calls after first request = %d, want 1", got)
	}

	resp2 := doRequest()
	body2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request status = %d, want 200; body: %s", resp2.StatusCode, body2)
	}

	// The load-bearing assertion: the mock upstream's call counter must
	// still read 1 after the second, identical request — proving the
	// response was served from the gateway's cache and never reached the
	// fake upstream.
	if got := calls.Load(); got != 1 {
		t.Fatalf("mock upstream calls after second (should be cache-hit) request = %d, want still 1", got)
	}
	if string(body1) != string(body2) {
		t.Errorf("cached response body differs from original:\nfirst:  %s\nsecond: %s", body1, body2)
	}
}

// TestIntegrationUnconfiguredModelReturnsError drives (d): a request for
// a model with no configured deployment must return an error status
// without ever reaching the mock upstream.
func TestIntegrationUnconfiguredModelReturnsError(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_D")

	reqBody := `{"model":"this-model-does-not-exist","messages":[{"role":"user","content":"hi"}]}`
	httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer test-gateway-key")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 400 {
		t.Errorf("status = %d, want an error status (>=400) for an unconfigured model", resp.StatusCode)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("mock upstream calls = %d, want 0 (an unconfigured model must never reach any upstream)", got)
	}
	if len(body) == 0 {
		t.Error("expected a non-empty error body")
	}
}
