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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kelvran/gateway/internal/adapter/anthropic"
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

	const apiKeyEnv = "KELVRAN_INTEGRATION_TEST_GATEWAY_KEY_PROVIDER"
	t.Setenv(apiKeyEnv, gatewayKey)
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")

	cfg := &controlplane.Config{
		ListenAddr: ":0",
		APIKeyEnv:  apiKeyEnv,
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

	const apiKeyEnv = "KELVRAN_INTEGRATION_TEST_GATEWAY_KEY_ANTHROPIC"
	t.Setenv(apiKeyEnv, gatewayKey)
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")

	cfg := &controlplane.Config{
		ListenAddr: ":0",
		APIKeyEnv:  apiKeyEnv,
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
// server and its writeErrorResponse status-code mapping.
func TestIntegrationStreamingUnsupportedProviderReturnsBadRequest(t *testing.T) {
	gw := newIntegrationServerWithProvider(t, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_F", "gemini", "gemini-pro")

	reqBody := `{"model":"gemini-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`
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
