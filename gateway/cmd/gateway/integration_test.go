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
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/adapter/gemini"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/adapter/openaicompat"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// testKeyHash is the test-only equivalent of what an operator does once
// with `openssl rand -hex 32 | sha256sum` per
// docs/rfcs/2026-09-02-virtual-keys-budgets.md: config holds the hash of
// the secret, never the secret itself. Tests still send the raw secret in
// the Authorization header, exactly like a real client would.
func testKeyHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

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
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")

	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash(gatewayKey), RateLimitBurst: 100, RateLimitRefill: 100},
		},
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

// newIntegrationServerMultiKey is a general-purpose variant of
// newIntegrationServer for tests that need full control over multiple
// virtual keys' configuration (allowed_models, budget_usd, rate limits) —
// used by the model-not-allowed, budget-exceeded, and cross-tenant
// cache-isolation integration tests below.
func newIntegrationServerMultiKey(t *testing.T, upstreamURL, upstreamKeyEnvVar string, keys []controlplane.VirtualKeyConfig) *httptest.Server {
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

// newMockStreamingUpstream starts an httptest.Server that speaks OpenAI's
// actual Server-Sent-Events streaming wire format — real "data: {...}\n\n"
// frames, flushed incrementally, ending in a "data: [DONE]\n\n" sentinel —
// rather than a single buffered JSON response. This is what lets the
// streaming integration tests below drive a genuine end-to-end SSE round
// trip: a real net/http client through the real gateway HTTP server through
// the real streaming decoder, with only the network endpoint faked.
func newMockStreamingUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
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
		if !req.Stream {
			http.Error(w, "expected a streaming request", http.StatusBadRequest)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "ResponseWriter does not support flushing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		events := []string{
			fmt.Sprintf(`{"id":"chatcmpl-streaming-integration-test","model":%q,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`, req.Model),
			fmt.Sprintf(`{"id":"chatcmpl-streaming-integration-test","model":%q,"choices":[{"index":0,"delta":{"content":"hello "},"finish_reason":null}]}`, req.Model),
			fmt.Sprintf(`{"id":"chatcmpl-streaming-integration-test","model":%q,"choices":[{"index":0,"delta":{"content":"stream"},"finish_reason":null}]}`, req.Model),
			fmt.Sprintf(`{"id":"chatcmpl-streaming-integration-test","model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, req.Model),
			fmt.Sprintf(`{"id":"chatcmpl-streaming-integration-test","model":%q,"choices":[],"usage":{"prompt_tokens":6,"completion_tokens":2,"total_tokens":8}}`, req.Model),
		}
		for _, e := range events {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", e); err != nil {
				return
			}
			flusher.Flush()
		}
		if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
			return
		}
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// newMockAnthropicStreamingUpstream starts an httptest.Server that speaks
// Anthropic's actual Messages API streaming wire format — a typed
// "event: <type>\ndata: {...}\n\n" sequence ending in message_stop, with no
// [DONE] sentinel (that's an OpenAI-only convention; the gateway's own
// outbound SSE to the client still ends in [DONE] regardless of which
// upstream served the request). This drives the plan's requirement to
// prove real end-to-end streaming against BOTH real adapters, not just
// OpenAI's simpler homogeneous-chunk shape.
func newMockAnthropicStreamingUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}
		var req anthropic.Request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid upstream request body: %v", err), http.StatusBadRequest)
			return
		}
		if !req.Stream {
			http.Error(w, "expected a streaming request", http.StatusBadRequest)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "ResponseWriter does not support flushing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		frames := []string{
			fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01AnthropicIntegrationTest\",\"model\":%q,\"usage\":{\"input_tokens\":9}}}\n\n", req.Model),
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hola \"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"stream\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
		for _, f := range frames {
			if _, err := fmt.Fprint(w, f); err != nil {
				return
			}
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// newIntegrationServerWithProvider builds the same real buildPipeline +
// chatCompletionsHandler wiring as newIntegrationServer, but for a single
// deployment of an arbitrary provider — used to drive the real HTTP error
// path for a provider that does not implement streaming (gemini, in the
// current scaffolding). BaseURL is never dialed: an unsupported-streaming
// provider must fail before any upstream call is attempted.
func newIntegrationServerWithProvider(t *testing.T, gatewayKey, upstreamKeyEnvVar, provider, model string) *httptest.Server {
	t.Helper()
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")

	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash(gatewayKey), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "provider-primary",
				Model:         model,
				Provider:      provider,
				UpstreamModel: model,
				BaseURL:       "http://unused",
				APIKeyEnv:     upstreamKeyEnvVar,
			},
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

// newIntegrationServerAnthropic builds the same real buildPipeline +
// chatCompletionsHandler wiring as newIntegrationServer, but for a single
// "anthropic" deployment instead of "openai" — used to drive a real,
// upstream-reaching streaming request through the Anthropic adapter and
// its stateful StreamDecoder, not just OpenAI's.
func newIntegrationServerAnthropic(t *testing.T, upstreamURL, gatewayKey, upstreamKeyEnvVar string) *httptest.Server {
	t.Helper()
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")

	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash(gatewayKey), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "claude-primary",
				Model:         "claude-3-5-sonnet-20241022",
				Provider:      "anthropic",
				UpstreamModel: "claude-3-5-sonnet-20241022",
				BaseURL:       upstreamURL,
				APIKeyEnv:     upstreamKeyEnvVar,
			},
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

// TestIntegrationStreamingRequestSucceedsAnthropic drives the same real
// end-to-end SSE round trip as TestIntegrationStreamingRequestSucceeds, but
// against the Anthropic adapter's stateful typed-event StreamDecoder
// instead of OpenAI's near-passthrough one — proving the dataplane's
// streaming wiring is genuinely provider-agnostic, not accidentally
// OpenAI-shaped.
func TestIntegrationStreamingRequestSucceedsAnthropic(t *testing.T) {
	upstream, calls := newMockAnthropicStreamingUpstream(t)
	gw := newIntegrationServerAnthropic(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_H")

	reqBody := `{"model":"claude-3-5-sonnet-20241022","stream":true,"messages":[{"role":"user","content":"integration streaming hello, anthropic"}]}`
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
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/event-stream")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	bodyStr := string(body)

	if !strings.Contains(bodyStr, `"content":"hola "`) || !strings.Contains(bodyStr, `"content":"stream"`) {
		t.Errorf("SSE body missing expected content deltas: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `"finish_reason":"stop"`) {
		t.Errorf("SSE body missing finish_reason mapped from Anthropic's end_turn: %s", bodyStr)
	}
	if !strings.HasSuffix(strings.TrimSpace(bodyStr), "data: [DONE]") {
		t.Errorf("SSE body does not end with the gateway's own [DONE] sentinel: %s", bodyStr)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("mock Anthropic streaming upstream calls = %d, want exactly 1", got)
	}
}

// TestIntegrationStreamingRequestSucceeds drives a real end-to-end SSE round
// trip: a real net/http client sends stream:true through the real gateway
// HTTP server, which streams real SSE bytes back from a real mock upstream.
// The response body is read incrementally line-by-line via bufio.Scanner,
// the way a real SSE client would, rather than decoded as one JSON blob.
func TestIntegrationStreamingRequestSucceeds(t *testing.T) {
	upstream, calls := newMockStreamingUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_E")

	reqBody := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"integration streaming hello"}]}`
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
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/event-stream")
	}

	var lines []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning SSE body: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("received no SSE lines at all")
	}

	body := strings.Join(lines, "\n")
	if !strings.Contains(body, `"content":"hello "`) || !strings.Contains(body, `"content":"stream"`) {
		t.Errorf("SSE body missing expected content deltas: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("SSE body missing finish_reason: %s", body)
	}
	if lines[len(lines)-1] != "data: [DONE]" {
		t.Errorf("last SSE line = %q, want %q", lines[len(lines)-1], "data: [DONE]")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("mock streaming upstream calls = %d, want exactly 1", got)
	}
}

// TestIntegrationStreamingCacheHitServesFakeStreamWithoutSecondUpstreamCall
// drives the real end-to-end pair: a first streaming request reaches the
// mock upstream and populates the cache from the accumulated stream, and an
// identical second streaming request must be served entirely from cache —
// synthesized ("fake") SSE chunks carrying the FULL accumulated content in
// one shot — without a second call ever reaching the mock upstream.
func TestIntegrationStreamingCacheHitServesFakeStreamWithoutSecondUpstreamCall(t *testing.T) {
	upstream, calls := newMockStreamingUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_G")

	reqBody := []byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"cache me while streaming"}]}`)

	doStreamingRequest := func() (int, string) {
		httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader(reqBody))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		httpReq.Header.Set("Authorization", "Bearer test-gateway-key")
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading response body: %v", err)
		}
		return resp.StatusCode, string(body)
	}

	status1, body1 := doStreamingRequest()
	if status1 != http.StatusOK {
		t.Fatalf("first request status = %d, want 200; body: %s", status1, body1)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("mock upstream calls after first request = %d, want 1", got)
	}

	status2, body2 := doStreamingRequest()
	if status2 != http.StatusOK {
		t.Fatalf("second request status = %d, want 200; body: %s", status2, body2)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("mock upstream calls after second (cache-hit) request = %d, want still 1", got)
	}
	if !strings.Contains(body2, `"content":"hello stream"`) {
		t.Errorf("fake-streamed cache-hit body missing full accumulated content: %s", body2)
	}
	if !strings.Contains(body2, `"finish_reason":"stop"`) {
		t.Errorf("fake-streamed cache-hit body missing finish_reason: %s", body2)
	}
	if !strings.HasSuffix(strings.TrimSpace(body2), "data: [DONE]") {
		t.Errorf("fake-streamed cache-hit body does not end with [DONE] sentinel: %s", body2)
	}
}

// TestIntegrationStreamingUnsupportedProviderReturnsBadRequest drives the
// real HTTP error path for a provider whose adapter does not implement
// streaming.StreamingAdapter: the request must fail with 400 before any
// upstream call is attempted, exactly like dataplane's own
// TestHandleChatCompletionStreamUnsupportedProviderReturnsTypedError proves
// at the package level — this proves the same thing through the real HTTP
// server and its writeErrorResponse status-code mapping. Uses "bedrock" —
// per docs/rfcs/2026-09-04-gemini-adapter.md, gemini is now a real
// streaming adapter and is deliberately no longer this test's example.
func TestIntegrationStreamingUnsupportedProviderReturnsBadRequest(t *testing.T) {
	gw := newIntegrationServerWithProvider(t, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_F", "bedrock", "claude-bedrock")

	reqBody := `{"model":"claude-bedrock","stream":true,"messages":[{"role":"user","content":"hi"}]}`
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

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusBadRequest, body)
	}
}

// TestIntegrationModelNotAllowedReturnsForbidden drives the real HTTP
// error path for a virtual key configured with a non-empty
// allowed_models list that doesn't include the requested model: 403,
// never reaching the mock upstream.
func TestIntegrationModelNotAllowedReturnsForbidden(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newIntegrationServerMultiKey(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_I", []controlplane.VirtualKeyConfig{
		{Name: "team-restricted", KeyHash: testKeyHash("restricted-secret"), RateLimitBurst: 100, RateLimitRefill: 100, AllowedModels: []string{"gpt-4o-mini"}},
	})

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer restricted-secret")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusForbidden, body)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("mock upstream calls = %d, want 0 (a model-not-allowed request must never reach upstream)", got)
	}
}

// TestIntegrationBudgetExceededReturns429DistinctFromRateLimit drives the
// real HTTP path for a virtual key that spends past its configured
// BudgetUSD cap: the second request (which pushes cumulative spend over
// the tiny configured cap) must fail with 429, distinguishable from a
// rate-limit 429 by its error message body, and never reach the mock
// upstream a second time.
func TestIntegrationBudgetExceededReturns429DistinctFromRateLimit(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newIntegrationServerMultiKey(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_J", []controlplane.VirtualKeyConfig{
		// The mock upstream always returns usage 7 prompt + 4 completion
		// tokens, costing 7*0.0000025 + 4*0.00001 = 0.0000575 USD per
		// request against this test's price_table. A cap of 0.00001
		// lets the FIRST request through (spend starts at 0) but rejects
		// the second (0.0000575 already spent > the 0.00001 cap).
		{Name: "team-tiny-budget", KeyHash: testKeyHash("tiny-budget-secret"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("0.00001")},
	})

	doRequest := func() (int, string) {
		reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"spend some budget"}]}`
		httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		httpReq.Header.Set("Authorization", "Bearer tiny-budget-secret")
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	status1, body1 := doRequest()
	if status1 != http.StatusOK {
		t.Fatalf("first request status = %d, want 200; body: %s", status1, body1)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("mock upstream calls after first request = %d, want 1", got)
	}

	status2, body2 := doRequest()
	if status2 != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want %d (budget exceeded); body: %s", status2, http.StatusTooManyRequests, body2)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("mock upstream calls after second (budget-exceeded) request = %d, want still 1", got)
	}
	if !strings.Contains(body2, "budget") {
		t.Errorf("budget-exceeded error body = %q, want it to mention \"budget\" so it's distinguishable from a rate-limit 429", body2)
	}
}

// TestIntegrationTwoVirtualKeysDoNotShareCacheEntries is the real-HTTP
// mirror of dataplane_test.go's load-bearing
// TestHandleChatCompletionCacheIsolatedAcrossVirtualKeys: two different
// valid keys sending a byte-identical request each independently get a
// 200, and the mock upstream's call counter must read 2 (not 1) after
// both keys' first request — proving no accidental cross-tenant cache
// sharing through the real HTTP path, not just the dataplane unit test.
func TestIntegrationTwoVirtualKeysDoNotShareCacheEntries(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newIntegrationServerMultiKey(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_K", []controlplane.VirtualKeyConfig{
		{Name: "team-alpha", KeyHash: testKeyHash("alpha-http-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		{Name: "team-beta", KeyHash: testKeyHash("beta-http-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	})

	reqBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"identical across tenants"}]}`)
	doRequest := func(bearer string) int {
		httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader(reqBody))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.ReadAll(resp.Body)
		return resp.StatusCode
	}

	if status := doRequest("alpha-http-secret"); status != http.StatusOK {
		t.Fatalf("team-alpha first request status = %d, want 200", status)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls after team-alpha's first request = %d, want 1", got)
	}

	// team-beta's identical request must be a real MISS (a second real
	// upstream call), not served from team-alpha's cache entry.
	if status := doRequest("beta-http-secret"); status != http.StatusOK {
		t.Fatalf("team-beta first request status = %d, want 200", status)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls after team-beta's first (should be a MISS) request = %d, want 2 — cross-tenant cache leakage", got)
	}

	// Each key's own second identical request is now a cache hit.
	if status := doRequest("alpha-http-secret"); status != http.StatusOK {
		t.Fatalf("team-alpha second request status = %d, want 200", status)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls after team-alpha's second (should be a HIT) request = %d, want still 2", got)
	}
}

// TestIntegrationL2CacheHitOnNormalizedButNotExactRepeat is the real-HTTP
// proof for docs/rfcs/2026-09-03-cache-l2-normalized-match.md: a second
// request that's byte-different from the first (added outer whitespace
// and a trailing "?") but normalization-equivalent must be served from
// L2, never reaching the mock upstream a second time. A third request
// with genuinely different content must still be a real miss, proving no
// false-positive over-matching.
func TestIntegrationL2CacheHitOnNormalizedButNotExactRepeat(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "l2-http-secret", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_L2")

	doRequest := func(content string) int {
		reqBody, err := json.Marshal(map[string]any{
			"model":    "gpt-4o",
			"messages": []map[string]string{{"role": "user", "content": content}},
		})
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader(reqBody))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		httpReq.Header.Set("Authorization", "Bearer l2-http-secret")
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.ReadAll(resp.Body)
		return resp.StatusCode
	}

	if status := doRequest("what is the weather in paris"); status != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", status)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls after first request = %d, want 1", got)
	}

	// Byte-different (outer whitespace + trailing "?"), normalization-
	// equivalent — must be an L2 hit, not a second real upstream call.
	if status := doRequest("  what is the weather in paris?  "); status != http.StatusOK {
		t.Fatalf("second (normalized-equivalent) request status = %d, want 200", status)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls after second (should be an L2 HIT) request = %d, want still 1", got)
	}

	// Genuinely different content must still be a real miss.
	if status := doRequest("what is the weather in london"); status != http.StatusOK {
		t.Fatalf("third (genuinely different) request status = %d, want 200", status)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls after third (should be a real MISS) request = %d, want 2 — L2 over-matched on genuinely different content", got)
	}
}

// TestIntegrationL3LexicalNearDuplicateHitAndEntityMismatchMiss is the
// real-HTTP, real-production-wiring (buildPipeline, exactly as
// cmd/gateway/main.go wires it) proof for
// docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md: a second
// request whose only difference from the first is internal whitespace —
// untouched by L2's narrow allowlist, so L1 and L2 both genuinely miss —
// is nonetheless served from L3, never reaching the mock upstream a second
// time. A third request that changes a dollar amount (an entity/number
// hard-gate mismatch) must still be a real miss, proving L3 never serves a
// cached response for a different amount just because the surrounding
// wording is nearly identical.
func TestIntegrationL3LexicalNearDuplicateHitAndEntityMismatchMiss(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "l3-http-secret", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_L3")

	doRequest := func(content string) int {
		reqBody, err := json.Marshal(map[string]any{
			"model":    "gpt-4o",
			"messages": []map[string]string{{"role": "user", "content": content}},
		})
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader(reqBody))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		httpReq.Header.Set("Authorization", "Bearer l3-http-secret")
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.ReadAll(resp.Body)
		return resp.StatusCode
	}

	if status := doRequest("explain how binary search works in a sorted array of $92 items"); status != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", status)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls after first request = %d, want 1", got)
	}

	// Byte-different (extra internal whitespace, untouched by L2's
	// allowlist) but lexically identical after word-boundary shingling —
	// must be an L3 hit, not a second real upstream call.
	if status := doRequest("explain how binary search   works in a sorted array of $92 items"); status != http.StatusOK {
		t.Fatalf("second (lexically-near-duplicate) request status = %d, want 200", status)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls after second (should be an L3 HIT) request = %d, want still 1", got)
	}

	// A different dollar amount is an entity/number hard-gate mismatch —
	// must still be a real miss, even though the wording is otherwise
	// near-identical.
	if status := doRequest("explain how binary search works in a sorted array of $93 items"); status != http.StatusOK {
		t.Fatalf("third (entity-mismatched) request status = %d, want 200", status)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls after third (should be a real MISS) request = %d, want 2 — L3 served a cache hit across a different dollar amount", got)
	}
}

// TestIntegrationGuardrailBlocksCredentialShapedRequest is the real-HTTP,
// real-production-wiring (buildPipeline, exactly as cmd/gateway/main.go
// wires it) proof for
// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md: a request
// whose content matches a Block-tier pattern (a real, Luhn-valid test
// credit-card number) is rejected before ever reaching the mock upstream.
func TestIntegrationGuardrailBlocksCredentialShapedRequest(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "gr-secret", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_GR")

	reqBody, err := json.Marshal(map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "my card number is 4111111111111111"}},
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer gr-secret")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d for a Block-tier request; body: %s", resp.StatusCode, http.StatusBadRequest, body)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("upstream calls = %d, want 0 — a Block-tier request must never reach the upstream", got)
	}
}

// newMockOpenAICompatUpstream starts an httptest.Server that speaks the
// same OpenAI-shaped wire format newMockUpstream does, but decodes into
// openaicompat.Request/Response — proving the real openaicompat adapter
// (not just the openai one) round-trips against a real HTTP call, per
// docs/rfcs/2026-09-04-openaicompat-adapter.md.
func newMockOpenAICompatUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}
		var req openaicompat.Request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid upstream request body: %v", err), http.StatusBadRequest)
			return
		}

		resp := openaicompat.Response{
			ID:    "chatcmpl-openaicompat-integration-test",
			Model: req.Model,
			Choices: []openaicompat.Choice{
				{
					Index:        0,
					Message:      openaicompat.Message{Role: "assistant", Content: "hello from the self-hosted mock upstream"},
					FinishReason: "stop",
				},
			},
			Usage: openaicompat.Usage{PromptTokens: 7, CompletionTokens: 4, TotalTokens: 11},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// newMockOpenAICompatStreamingUpstream mirrors newMockStreamingUpstream,
// decoding into openaicompat.Request instead — the wire shape is
// identical (SSE, "[DONE]" sentinel, stream_options.include_usage), per
// this RFC's own grounding research confirming that shape is uniform
// across every self-hosted runtime surveyed.
func newMockOpenAICompatStreamingUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}
		var req openaicompat.Request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid upstream request body: %v", err), http.StatusBadRequest)
			return
		}
		if !req.Stream {
			http.Error(w, "expected a streaming request", http.StatusBadRequest)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "ResponseWriter does not support flushing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		events := []string{
			fmt.Sprintf(`{"id":"chatcmpl-openaicompat-streaming-integration-test","model":%q,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`, req.Model),
			fmt.Sprintf(`{"id":"chatcmpl-openaicompat-streaming-integration-test","model":%q,"choices":[{"index":0,"delta":{"content":"self-hosted "},"finish_reason":null}]}`, req.Model),
			fmt.Sprintf(`{"id":"chatcmpl-openaicompat-streaming-integration-test","model":%q,"choices":[{"index":0,"delta":{"content":"stream"},"finish_reason":null}]}`, req.Model),
			fmt.Sprintf(`{"id":"chatcmpl-openaicompat-streaming-integration-test","model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, req.Model),
			fmt.Sprintf(`{"id":"chatcmpl-openaicompat-streaming-integration-test","model":%q,"choices":[],"usage":{"prompt_tokens":6,"completion_tokens":2,"total_tokens":8}}`, req.Model),
		}
		for _, e := range events {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", e); err != nil {
				return
			}
			flusher.Flush()
		}
		if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
			return
		}
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// newIntegrationServerOpenAICompat builds the same real buildPipeline +
// chatCompletionsHandler wiring as newIntegrationServer, but for a single
// "openaicompat" deployment — modeled on newIntegrationServerAnthropic
// (the "second provider" builder precedent), since openaicompat is the
// third real provider adapter, not the default.
func newIntegrationServerOpenAICompat(t *testing.T, upstreamURL, gatewayKey, upstreamKeyEnvVar string) *httptest.Server {
	t.Helper()
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")

	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash(gatewayKey), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "self-hosted-primary",
				Model:         "llama-3.1-70b-instruct",
				Provider:      "openaicompat",
				UpstreamModel: "llama-3.1-70b-instruct",
				BaseURL:       upstreamURL,
				APIKeyEnv:     upstreamKeyEnvVar,
			},
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

// TestIntegrationOpenAICompatRequestSucceeds drives a real, non-streaming
// end-to-end HTTP round trip through the real openaicompat adapter — the
// direct analogue of TestIntegrationWellFormedRequestSucceeds, proving the
// dataplane.go responseUnmarshalers wiring this RFC added actually works,
// not just that the adapter's own unit tests pass in isolation.
func TestIntegrationOpenAICompatRequestSucceeds(t *testing.T) {
	upstream, calls := newMockOpenAICompatUpstream(t)
	gw := newIntegrationServerOpenAICompat(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_OC")

	reqBody := `{"model":"llama-3.1-70b-instruct","messages":[{"role":"user","content":"integration hello, openaicompat"}]}`
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
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}
	if len(decoded.Choices) != 1 {
		t.Fatalf("Choices len = %d, want 1", len(decoded.Choices))
	}
	if got := decoded.Choices[0].Message.Content; got != "hello from the self-hosted mock upstream" {
		t.Errorf("Message.Content = %q, want the mock upstream's real content", got)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("mock openaicompat upstream calls = %d, want exactly 1", got)
	}
}

// TestIntegrationStreamingRequestSucceedsOpenAICompat drives the same real
// end-to-end SSE round trip as TestIntegrationStreamingRequestSucceeds,
// but against the openaicompat adapter's own StreamDecoder instead of
// openai's — proving the streaming path is genuinely provider-agnostic
// for this third real adapter too, not just the first two.
func TestIntegrationStreamingRequestSucceedsOpenAICompat(t *testing.T) {
	upstream, calls := newMockOpenAICompatStreamingUpstream(t)
	gw := newIntegrationServerOpenAICompat(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_OCS")

	reqBody := `{"model":"llama-3.1-70b-instruct","stream":true,"messages":[{"role":"user","content":"integration streaming hello, openaicompat"}]}`
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
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/event-stream")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	bodyStr := string(body)

	if !strings.Contains(bodyStr, `"content":"self-hosted "`) || !strings.Contains(bodyStr, `"content":"stream"`) {
		t.Errorf("SSE body missing expected content deltas: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `"finish_reason":"stop"`) {
		t.Errorf("SSE body missing finish_reason: %s", bodyStr)
	}
	if !strings.HasSuffix(strings.TrimSpace(bodyStr), "data: [DONE]") {
		t.Errorf("SSE body does not end with the gateway's own [DONE] sentinel: %s", bodyStr)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("mock openaicompat streaming upstream calls = %d, want exactly 1", got)
	}
}

// newMockGeminiUpstream decodes into gemini.Request and responds with a
// real-shaped gemini.Response — the buffered (:generateContent) path.
func newMockGeminiUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}
		var req gemini.Request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid upstream request body: %v", err), http.StatusBadRequest)
			return
		}

		resp := gemini.Response{
			ResponseID:   "resp-gemini-integration-test",
			ModelVersion: "gemini-2.5-flash",
			Candidates: []gemini.Candidate{
				{
					Content: gemini.Content{
						Role:  "model",
						Parts: []gemini.Part{{Text: "hello from the mock Gemini upstream"}},
					},
					FinishReason: "STOP",
				},
			},
			UsageMetadata: gemini.UsageMetadata{PromptTokenCount: 7, CandidatesTokenCount: 4, TotalTokenCount: 11},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// newMockGeminiStreamingUpstream asserts the incoming request's URL path
// and query actually carry :streamGenerateContent and alt=sse — the real,
// load-bearing proof that dataplane.go's streamUpstreamURL derivation is
// correctly wired end-to-end, not just unit-tested in isolation — then
// responds with real Gemini-shaped SSE frames (each a complete
// GenerateContentResponse, per docs/rfcs/2026-09-04-gemini-adapter.md).
func newMockGeminiStreamingUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		if !strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			t.Errorf("streaming request path = %q, want it to end in %q", r.URL.Path, ":streamGenerateContent")
		}
		if r.URL.Query().Get("alt") != "sse" {
			t.Errorf("streaming request query = %q, want alt=sse", r.URL.RawQuery)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}
		var req gemini.Request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid upstream request body: %v", err), http.StatusBadRequest)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "ResponseWriter does not support flushing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		events := []string{
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"streamed "}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"gemini"}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":6,"candidatesTokenCount":2,"totalTokenCount":8}}`,
		}
		for _, e := range events {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", e); err != nil {
				return
			}
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// newIntegrationServerGemini builds the same real buildPipeline +
// chatCompletionsHandler wiring as newIntegrationServerOpenAICompat, for a
// single "gemini" deployment. BaseURL is configured as the buffered
// (:generateContent) endpoint, per this adapter's own convention — the
// streaming URL is derived by dataplane.go's streamUpstreamURL, never
// separately configured.
func newIntegrationServerGemini(t *testing.T, upstreamURL, gatewayKey, upstreamKeyEnvVar string) *httptest.Server {
	t.Helper()
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")

	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash(gatewayKey), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "gemini-primary",
				Model:         "gemini-2.5-flash",
				Provider:      "gemini",
				UpstreamModel: "gemini-2.5-flash",
				BaseURL:       upstreamURL + "/v1beta/models/gemini-2.5-flash:generateContent",
				APIKeyEnv:     upstreamKeyEnvVar,
			},
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

// TestIntegrationGeminiRequestSucceeds drives a real, non-streaming
// end-to-end HTTP round trip through the real gemini adapter, proving the
// dataplane.go responseUnmarshalers + setUpstreamAuthHeaders wiring this
// RFC added actually works, not just that the adapter's own unit tests
// pass in isolation.
func TestIntegrationGeminiRequestSucceeds(t *testing.T) {
	upstream, calls := newMockGeminiUpstream(t)
	gw := newIntegrationServerGemini(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_GEM")

	reqBody := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"integration hello, gemini"}]}`
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
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}
	if len(decoded.Choices) != 1 {
		t.Fatalf("Choices len = %d, want 1", len(decoded.Choices))
	}
	if got := decoded.Choices[0].Message.Content; got != "hello from the mock Gemini upstream" {
		t.Errorf("Message.Content = %q, want the mock upstream's real content", got)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("mock gemini upstream calls = %d, want exactly 1", got)
	}
}

// TestIntegrationStreamingRequestSucceedsGemini drives a real end-to-end
// SSE round trip against the gemini adapter's own StreamDecoder, and — via
// newMockGeminiStreamingUpstream's own assertions — proves the real
// streamUpstreamURL derivation actually reached the upstream as a
// genuinely different URL, not just dep.BaseURL reused unmodified.
func TestIntegrationStreamingRequestSucceedsGemini(t *testing.T) {
	upstream, calls := newMockGeminiStreamingUpstream(t)
	gw := newIntegrationServerGemini(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_GEMS")

	reqBody := `{"model":"gemini-2.5-flash","stream":true,"messages":[{"role":"user","content":"integration streaming hello, gemini"}]}`
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
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/event-stream")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	bodyStr := string(body)

	if !strings.Contains(bodyStr, `"content":"streamed "`) || !strings.Contains(bodyStr, `"content":"gemini"`) {
		t.Errorf("SSE body missing expected content deltas: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `"finish_reason":"stop"`) {
		t.Errorf("SSE body missing finish_reason: %s", bodyStr)
	}
	if !strings.HasSuffix(strings.TrimSpace(bodyStr), "data: [DONE]") {
		t.Errorf("SSE body does not end with the gateway's own [DONE] sentinel: %s", bodyStr)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("mock gemini streaming upstream calls = %d, want exactly 1", got)
	}
}

// TestIntegrationGeminiToolCallRoundTrip drives a real two-turn tool-call
// exchange through the full HTTP pipeline: the first request gets back a
// functionCall-shaped response; the client (as any real OpenAI-shaped
// caller would) then sends a second request appending the assistant's
// tool_calls message plus a role:"tool" result — the exact path this
// adapter's own FunctionResponse.name-resolution hazard (see
// docs/rfcs/2026-09-04-gemini-adapter.md) could silently break if
// implemented wrong. The mock upstream asserts, on the second call, that
// the resolved functionResponse.name genuinely arrived correctly.
func TestIntegrationGeminiToolCallRoundTrip(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}
		var req gemini.Request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid upstream request body: %v", err), http.StatusBadRequest)
			return
		}

		if n == 1 {
			resp := gemini.Response{
				Candidates: []gemini.Candidate{
					{
						Content: gemini.Content{
							Role: "model",
							Parts: []gemini.Part{
								{FunctionCall: &gemini.FunctionCall{
									ID: "call_1", Name: "get_weather",
									Args: map[string]any{"city": "Boston"},
								}},
							},
						},
						FinishReason: "STOP",
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// Second call: the real, load-bearing proof. The gateway's own
		// ToProvider must have resolved the tool result's functionResponse
		// name from message history — never left empty.
		var sawFunctionResponse bool
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				if p.FunctionResponse != nil {
					sawFunctionResponse = true
					if p.FunctionResponse.Name != "get_weather" {
						t.Errorf("functionResponse.Name = %q, want %q", p.FunctionResponse.Name, "get_weather")
					}
				}
			}
		}
		if !sawFunctionResponse {
			t.Error("second upstream request contains no functionResponse part")
		}

		resp := gemini.Response{
			Candidates: []gemini.Candidate{
				{
					Content:      gemini.Content{Role: "model", Parts: []gemini.Part{{Text: "It's 72F and sunny in Boston."}}},
					FinishReason: "STOP",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(upstream.Close)

	gw := newIntegrationServerGemini(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_GEMTC")

	firstReqBody := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"what's the weather in Boston?"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get the weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`
	httpReq1, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(firstReqBody)))
	if err != nil {
		t.Fatalf("NewRequest 1: %v", err)
	}
	httpReq1.Header.Set("Authorization", "Bearer test-gateway-key")
	httpReq1.Header.Set("Content-Type", "application/json")

	resp1, err := http.DefaultClient.Do(httpReq1)
	if err != nil {
		t.Fatalf("Do 1: %v", err)
	}
	defer func() { _ = resp1.Body.Close() }()
	if resp1.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp1.Body)
		t.Fatalf("first request status = %d, want 200; body: %s", resp1.StatusCode, body)
	}

	var decoded1 struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp1.Body).Decode(&decoded1); err != nil {
		t.Fatalf("decoding first response: %v", err)
	}
	if len(decoded1.Choices) != 1 || len(decoded1.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("first response = %+v, want exactly one tool call", decoded1)
	}
	toolCallID := decoded1.Choices[0].Message.ToolCalls[0].ID
	toolCallName := decoded1.Choices[0].Message.ToolCalls[0].Function.Name
	toolCallArgs := decoded1.Choices[0].Message.ToolCalls[0].Function.Arguments

	secondReqBody := fmt.Sprintf(
		`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"what's the weather in Boston?"},{"role":"assistant","tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},{"role":"tool","content":"{\"temp_f\":72}","tool_call_id":%q}]}`,
		toolCallID, toolCallName, toolCallArgs, toolCallID,
	)
	httpReq2, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(secondReqBody)))
	if err != nil {
		t.Fatalf("NewRequest 2: %v", err)
	}
	httpReq2.Header.Set("Authorization", "Bearer test-gateway-key")
	httpReq2.Header.Set("Content-Type", "application/json")

	resp2, err := http.DefaultClient.Do(httpReq2)
	if err != nil {
		t.Fatalf("Do 2: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("second request status = %d, want 200; body: %s", resp2.StatusCode, body)
	}

	if got := calls.Load(); got != 2 {
		t.Errorf("mock gemini upstream calls = %d, want exactly 2", got)
	}
}
