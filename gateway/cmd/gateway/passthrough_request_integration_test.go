package main

import (
	"bytes"
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

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// capturedUpstreamRequest is what the Anthropic mock saw: the raw body bytes
// and the request headers, for byte-identity assertions.
type capturedUpstreamRequest struct {
	body   []byte
	header http.Header
}

// newCapturingAnthropicUpstream answers like Anthropic -- a buffered message
// for a body without "stream": true, the event sequence otherwise -- and
// records every request it receives.
func newCapturingAnthropicUpstream(t *testing.T) (*httptest.Server, func() []capturedUpstreamRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []capturedUpstreamRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		seen = append(seen, capturedUpstreamRequest{body: body, header: r.Header.Clone()})
		mu.Unlock()
		var probe struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			http.Error(w, "invalid upstream request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !probe.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"msg_passthrough","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}`, probe.Model)
			return
		}
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range []string{
			fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_passthrough_stream\",\"model\":%q,\"usage\":{\"input_tokens\":3}}}\n\n", probe.Model),
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		} {
			_, _ = fmt.Fprint(w, f)
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []capturedUpstreamRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedUpstreamRequest(nil), seen...)
	}
}

// newPassthroughIntegrationServer serves one anthropic deployment whose
// upstream model id differs from the canonical name, so the model rewrite is
// observable.
func newPassthroughIntegrationServer(t *testing.T, upstreamURL string, logger *slog.Logger) *httptest.Server {
	t.Helper()
	t.Setenv("KELVRAN_PASSTHROUGH_TEST_KEY", "fake-upstream-key-not-a-real-secret")
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "all", KeyHash: testKeyHash("all-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{Name: "claude-primary", Model: "claude-sys", Provider: "anthropic", UpstreamModel: "claude-upstream-id", BaseURL: upstreamURL, APIKeyEnv: "KELVRAN_PASSTHROUGH_TEST_KEY"},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"claude-sys": {PromptPerToken: decimal.RequireFromString("0.000003"), CompletionPerToken: decimal.RequireFromString("0.000015")},
		},
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}
	gw := httptest.NewServer(newDataPlaneMux(pipeline))
	t.Cleanup(gw.Close)
	return gw
}

var passthroughClientHeaders = map[string]string{
	"Authorization":          "Bearer all-secret",
	"anthropic-beta":         "beta-a,beta-b",
	"anthropic-version":      "2024-06-01",
	"anthropic-workspace-id": "ws_1",
	"Idempotency-Key":        "pt-1",
	"X-Custom":               "nope",
}

// An anthropic deployment receives the client's body byte-for-byte except
// model and stream (RFC-1 §5, slice S11a): unknown members at any depth,
// the client's key order and even the whitespace inside a nested value
// survive; anthropic-* headers travel as an open list with the client's
// anthropic-version winning; the deployment's credential replaces the
// client's, and nothing outside the prefix is forwarded.
func TestIntegrationMessagesPassthroughRelaysTheRawRequestToAnAnthropicDeployment(t *testing.T) {
	upstream, seen := newCapturingAnthropicUpstream(t)
	var logs bytes.Buffer
	gw := newPassthroughIntegrationServer(t, upstream.URL, slog.New(slog.NewJSONHandler(&logs, nil)))

	body := `{"model":"claude-sys","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u-1","custom":{ "deep" : [1, 2] }},"service_tier":"auto"}`
	status, _, raw := postMessages(t, gw.URL, passthroughClientHeaders, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d; body %s", status, raw)
	}
	var msg anthropicMessageBody
	if err := json.Unmarshal(raw, &msg); err != nil || len(msg.Content) != 1 || msg.Content[0].Text != "ok" {
		t.Fatalf("response = %s (%v), want the mock's message", raw, err)
	}

	got := seen()
	if len(got) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(got))
	}
	wantBody := `{"model":"claude-upstream-id","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u-1","custom":{ "deep" : [1, 2] }},"service_tier":"auto","stream":false}`
	if string(got[0].body) != wantBody {
		t.Errorf("upstream body =\n%s\nwant\n%s", got[0].body, wantBody)
	}
	h := got[0].header
	for name, want := range map[string]string{
		"x-api-key":              "fake-upstream-key-not-a-real-secret",
		"anthropic-version":      "2024-06-01",
		"anthropic-beta":         "beta-a,beta-b",
		"anthropic-workspace-id": "ws_1",
	} {
		if h.Get(name) != want {
			t.Errorf("upstream %s = %q, want %q", name, h.Get(name), want)
		}
	}
	for _, name := range []string{"Authorization", "Idempotency-Key", "X-Custom"} {
		if v := h.Get(name); v != "" {
			t.Errorf("upstream %s = %q, want absent", name, v)
		}
	}
	// The carrier reports the hop truthfully: relayed as received, nothing
	// dropped -- even though service_tier is unknown to the canonical schema.
	out := logs.String()
	if !strings.Contains(out, `"msg":"chat_completion"`) || !strings.Contains(out, `"passthrough":true`) {
		t.Errorf("log lacks a chat_completion line with passthrough true:\n%s", out)
	}
	if strings.Contains(out, `"dropped_fields"`) {
		t.Errorf("log reports dropped_fields on a passthrough hop:\n%s", out)
	}
}

func TestIntegrationMessagesPassthroughStreamsWithTheRawRequest(t *testing.T) {
	upstream, seen := newCapturingAnthropicUpstream(t)
	gw := newPassthroughIntegrationServer(t, upstream.URL, nil)

	body := `{"model":"claude-sys","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}],"extra_member":{ "kept" : true }}`
	status, header, raw := postMessages(t, gw.URL, passthroughClientHeaders, body)
	if status != http.StatusOK || !strings.HasPrefix(header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status/content-type = %d %q; body %s", status, header.Get("Content-Type"), raw)
	}
	if !strings.Contains(string(raw), "event: message_stop") {
		t.Fatalf("stream did not end with message_stop:\n%s", raw)
	}
	got := seen()
	if len(got) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(got))
	}
	wantBody := `{"model":"claude-upstream-id","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}],"extra_member":{ "kept" : true }}`
	if string(got[0].body) != wantBody {
		t.Errorf("upstream body =\n%s\nwant\n%s", got[0].body, wantBody)
	}
	if got[0].header.Get("anthropic-beta") != "beta-a,beta-b" || got[0].header.Get("Accept") != "text/event-stream" {
		t.Errorf("upstream headers = %v, want the beta forwarded and Accept text/event-stream", got[0].header)
	}
}
