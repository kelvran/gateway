package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// newScriptedAnthropicUpstream answers every request with one fixed status,
// body and header set, and counts the calls.
func newScriptedAnthropicUpstream(t *testing.T, status int, body string, headers map[string]string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

const passthroughBody = `{"model":"claude-sys","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

var upstreamRelayHeaders = map[string]string{
	"x-should-retry":                     "false",
	"anthropic-ratelimit-unified-status": "allowed",
	"anthropic-organization-id":          "org_1",
	"request-id":                         "req_1",
	"x-upstream-internal":                "nope",
}

func assertRelayHeaders(t *testing.T, h http.Header) {
	t.Helper()
	if h.Get("X-Should-Retry") != "false" || h.Get("Anthropic-Ratelimit-Unified-Status") != "allowed" {
		t.Errorf("relay headers missing: %v", h)
	}
	for _, name := range []string{"Anthropic-Organization-Id", "Request-Id", "X-Upstream-Internal"} {
		if h.Get(name) != "" {
			t.Errorf("%s forwarded, want dropped", name)
		}
	}
}

// An anthropic deployment's 2xx answer reaches the client byte-for-byte --
// a block the canonical schema cannot represent, the whitespace inside its
// value, Anthropic's own id and model -- with x-should-retry and
// anthropic-ratelimit-unified-* forwarded and every other upstream header
// dropped (RFC-1 §9, slice S11b). An unrepresentable response is never
// cached (the S6 flag), so the same turn goes upstream again.
func TestIntegrationMessagesPassthroughRelaysTheRawResponse(t *testing.T) {
	raw := `{"id":"msg_raw","type":"message","role":"assistant","model":"claude-upstream-id","content":[{"type":"text","text":"ok"},{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{ "query" : "x" }}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}`
	upstream, calls := newScriptedAnthropicUpstream(t, http.StatusOK, raw, upstreamRelayHeaders)
	gw := newPassthroughIntegrationServer(t, upstream.URL, nil)
	for i := 1; i <= 2; i++ {
		status, header, body := postMessages(t, gw.URL, passthroughClientHeaders, passthroughBody)
		if status != http.StatusOK {
			t.Fatalf("call %d: status = %d; body %s", i, status, body)
		}
		if string(body) != raw {
			t.Fatalf("call %d: body =\n%s\nwant the upstream bytes\n%s", i, body, raw)
		}
		assertRelayHeaders(t, header)
		if header.Get(overheadDurationHeader) == "" {
			t.Errorf("call %d: %s missing", i, overheadDurationHeader)
		}
		if got := calls.Load(); got != int64(i) {
			t.Fatalf("call %d: upstream calls = %d, want %d (an unrepresentable response is never served from the cache)", i, got, i)
		}
	}
}

// A representable response is relayed once and re-encoded on every later
// short-circuit: with an Idempotency-Key the second identical request is a
// replay (claimIdempotency runs before the cache lookup, so the stored
// canonical response is what comes back); without one it is a cache hit. In
// both cases the carrier is empty and the body is EncodeResponse's shape --
// the canonical model name replaces the upstream id -- never the raw bytes.
func TestIntegrationMessagesPassthroughReplayAndCacheHitAreReEncoded(t *testing.T) {
	raw := `{"id":"msg_rep","type":"message","role":"assistant","model":"claude-upstream-id","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}`
	cacheOnly := map[string]string{}
	for k, v := range passthroughClientHeaders {
		if k != "Idempotency-Key" {
			cacheOnly[k] = v
		}
	}
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"idempotency replay", passthroughClientHeaders},
		{"cache hit", cacheOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, calls := newScriptedAnthropicUpstream(t, http.StatusOK, raw, nil)
			gw := newPassthroughIntegrationServer(t, upstream.URL, nil)
			status, _, first := postMessages(t, gw.URL, tc.headers, passthroughBody)
			if status != http.StatusOK || string(first) != raw {
				t.Fatalf("first call: status %d body %s, want the raw upstream body", status, first)
			}
			status, _, second := postMessages(t, gw.URL, tc.headers, passthroughBody)
			if status != http.StatusOK {
				t.Fatalf("second call: status = %d; body %s", status, second)
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want 1: the second turn is a %s", calls.Load(), tc.name)
			}
			var msg anthropicMessageBody
			if err := json.Unmarshal(second, &msg); err != nil || string(second) == raw || msg.Model != "claude-sys" || len(msg.Content) != 1 || msg.Content[0].Text != "ok" {
				t.Errorf("%s = %s (%v), want Kelvran's re-encoded message (canonical model, the same text), never the raw bytes", tc.name, second, err)
			}
		})
	}
}

// A response the post-call guardrail blocks is never relayed: the client
// gets the 400 content_policy_violation envelope and none of the upstream's
// bytes, exactly as on a translate hop.
func TestIntegrationMessagesPassthroughPostCallBlockRelaysNothing(t *testing.T) {
	raw := `{"id":"msg_pii","type":"message","role":"assistant","model":"claude-upstream-id","content":[{"type":"text","text":"sure, here it is: 4111111111111111"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":9}}`
	upstream, calls := newScriptedAnthropicUpstream(t, http.StatusOK, raw, upstreamRelayHeaders)
	gw := newPassthroughIntegrationServer(t, upstream.URL, nil)
	status, _, body := postMessages(t, gw.URL, passthroughClientHeaders, `{"model":"claude-sys","max_tokens":16,"messages":[{"role":"user","content":"give me a test card number"}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", status, body)
	}
	e := decodeAnthropicError(t, body)
	if e.Error.Code != "content_policy_violation" || strings.Contains(string(body), "4111111111111111") {
		t.Errorf("body = %s, want content_policy_violation and none of the upstream bytes", body)
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", calls.Load())
	}
}

// RFC-1 §9's verbatim exception is exactly {anthropic, 400|422, parsed
// Anthropic error object}: those two statuses pass through with the
// upstream's own body and status, so Claude Code's recovery can match
// Anthropic's wording; the relay headers travel on every anthropic error;
// every other status, and a 400 whose body is not Anthropic's error object,
// stays the redacted envelope.
func TestIntegrationMessagesPassthroughVerbatimUpstream400And422Only(t *testing.T) {
	anthropicErr := `{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.0.thinking: signature bound to a different conversation"}}`
	errHeaders := map[string]string{"x-should-retry": "false", "anthropic-ratelimit-unified-status": "allowed", "request-id": "req_e"}
	cases := []struct {
		name       string
		status     int
		body       string
		verbatim   bool
		wantStatus int
	}{
		{"400 anthropic envelope", http.StatusBadRequest, anthropicErr, true, http.StatusBadRequest},
		{"422 anthropic envelope", http.StatusUnprocessableEntity, anthropicErr, true, http.StatusUnprocessableEntity},
		{"400 other shape", http.StatusBadRequest, `{"message":"The system field can't be null."}`, false, http.StatusBadGateway},
		{"429 anthropic envelope", http.StatusTooManyRequests, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, false, 0},
		{"500 anthropic envelope", http.StatusInternalServerError, `{"type":"error","error":{"type":"api_error","message":"boom"}}`, false, 0},
	}
	for _, tc := range cases {
		upstream, _ := newScriptedAnthropicUpstream(t, tc.status, tc.body, errHeaders)
		gw := newPassthroughIntegrationServer(t, upstream.URL, nil)
		status, header, body := postMessages(t, gw.URL, passthroughClientHeaders, passthroughBody)
		if tc.verbatim {
			if status != tc.wantStatus || string(body) != tc.body {
				t.Errorf("%s: status %d body %s, want %d and the upstream body verbatim", tc.name, status, body, tc.wantStatus)
			}
		} else {
			if string(body) == tc.body || strings.Contains(string(body), "slow down") || strings.Contains(string(body), "boom") || strings.Contains(string(body), "can't be null") {
				t.Errorf("%s: body %s relays upstream text, want the redacted envelope", tc.name, body)
			}
			if tc.wantStatus != 0 && status != tc.wantStatus {
				t.Errorf("%s: status = %d, want %d", tc.name, status, tc.wantStatus)
			}
			e := decodeAnthropicError(t, body)
			if e.Error.Message == "" {
				t.Errorf("%s: redacted envelope has no message: %s", tc.name, body)
			}
		}
		if header.Get("X-Should-Retry") != "false" || header.Get("Anthropic-Ratelimit-Unified-Status") != "allowed" || header.Get("Request-Id") != "" {
			t.Errorf("%s: relay headers = %v, want x-should-retry and the unified rate-limit header only", tc.name, header)
		}
	}
}

// The same Anthropic-shaped 400 from a non-anthropic deployment stays
// redacted: the exception is scoped to the provider, not to the body shape.
func TestIntegrationMessagesNonAnthropicUpstream400StaysRedacted(t *testing.T) {
	upstream, _ := newScriptedAnthropicUpstream(t, http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"from an openai mock"}}`, nil)
	gw := newMessagesIntegrationServer(t, upstream.URL, true, nil)
	status, _, body := postMessages(t, gw.URL, nil, messagesBody)
	if status != http.StatusBadGateway || strings.Contains(string(body), "from an openai mock") {
		t.Fatalf("status %d body %s, want 502 and the redacted envelope", status, body)
	}
}

// newPassthroughFallbackIntegrationServer wires an anthropic deployment whose
// fallback_chains[generic] leads to an openai deployment of another canonical
// model, so the first pick is always the anthropic hop.
func newPassthroughFallbackIntegrationServer(t *testing.T, anthropicURL, openaiURL string) *httptest.Server {
	t.Helper()
	t.Setenv("KELVRAN_PASSTHROUGH_TEST_KEY", "fake-upstream-key-not-a-real-secret")
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "all", KeyHash: testKeyHash("all-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{Name: "claude-primary", Model: "claude-sys", Provider: "anthropic", UpstreamModel: "claude-upstream-id", BaseURL: anthropicURL, APIKeyEnv: "KELVRAN_PASSTHROUGH_TEST_KEY",
				FallbackChains: map[string][]string{"generic": {"gpt-fallback"}}},
			{Name: "gpt-fallback", Model: "gpt-fallback-sys", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: openaiURL, APIKeyEnv: "KELVRAN_PASSTHROUGH_TEST_KEY"},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"claude-sys":       {PromptPerToken: decimal.RequireFromString("0.000003"), CompletionPerToken: decimal.RequireFromString("0.000015")},
			"gpt-fallback-sys": {PromptPerToken: decimal.RequireFromString("0.000003"), CompletionPerToken: decimal.RequireFromString("0.000015")},
		},
	}
	pipeline, err := buildPipeline(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}
	gw := httptest.NewServer(newDataPlaneMux(pipeline))
	t.Cleanup(gw.Close)
	return gw
}

// A 2xx an anthropic deployment answers with a body the adapter cannot decode
// (a proxy's HTML page, truncated JSON) is an upstream failure like any other:
// the fallback hop serves the turn and the client receives that hop's
// response re-encoded -- never the undecodable bytes the failed hop had
// already handed to the carrier, and none of its headers.
func TestIntegrationMessagesPassthroughFallbackAfterUndecodableAnthropic2xxRelaysNothing(t *testing.T) {
	anthropicUp, anthropicCalls := newScriptedAnthropicUpstream(t, http.StatusOK, "<html>upstream proxy</html>", upstreamRelayHeaders)
	openaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-fb","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"from the fallback"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	}))
	t.Cleanup(openaiUp.Close)
	gw := newPassthroughFallbackIntegrationServer(t, anthropicUp.URL, openaiUp.URL)

	headers := map[string]string{"Authorization": "Bearer all-secret", "anthropic-version": "2024-06-01"}
	status, header, raw := postMessages(t, gw.URL, headers, passthroughBody)
	if status != http.StatusOK {
		t.Fatalf("status = %d; body %s", status, raw)
	}
	if strings.Contains(string(raw), "<html>") {
		t.Fatalf("the failed anthropic hop's bytes reached the client: %s", raw)
	}
	var msg anthropicMessageBody
	if err := json.Unmarshal(raw, &msg); err != nil || len(msg.Content) != 1 || msg.Content[0].Text != "from the fallback" {
		t.Fatalf("body is not the fallback's re-encoded message: %s (%v)", raw, err)
	}
	if msg.Model != "gpt-fallback-sys" {
		t.Errorf("model = %q, want the fallback's canonical gpt-fallback-sys", msg.Model)
	}
	if header.Get("X-Should-Retry") != "" || header.Get("Anthropic-Ratelimit-Unified-Status") != "" {
		t.Errorf("the failed hop's relay headers reached the client: %v", header)
	}
	if anthropicCalls.Load() != 1 {
		t.Errorf("anthropic upstream calls = %d, want 1", anthropicCalls.Load())
	}
}
