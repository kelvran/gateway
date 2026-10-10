package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// POST /v1/messages (item 11 slice S10a): every test here drives the real
// mux, the real pipeline and a mock upstream speaking the provider's wire
// format, so the Anthropic Messages ingress is proven end to end through
// a translate hop -- the only hop that exists until S11 relays the raw body
// to an anthropic deployment.

// newMessagesIntegrationServer serves one openai deployment (gpt-4o, the
// mock upstream) with the lossy flag as given, and five keys: all-secret,
// mini-secret (allowed_models gpt-4o-mini only), burst-secret (one request
// then a 429), tiny-budget-secret (one request then insufficient_quota),
// scoped-secret (cache_scope_to_end_user), elsewhere-secret (allowed_source_cidrs
// that exclude the loopback peer every test request comes from).
func newMessagesIntegrationServer(t *testing.T, upstreamURL string, acceptLossy bool, logger *slog.Logger) *httptest.Server {
	t.Helper()
	t.Setenv("KELVRAN_MESSAGES_INTEGRATION_TEST_KEY", "fake-upstream-key-not-a-real-secret")
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "all", KeyHash: testKeyHash("all-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
			{Name: "mini-only", KeyHash: testKeyHash("mini-secret"), RateLimitBurst: 100, RateLimitRefill: 100, AllowedModels: []string{"gpt-4o-mini"}},
			{Name: "burst-one", KeyHash: testKeyHash("burst-secret"), RateLimitBurst: 1, RateLimitRefill: 0.001},
			{Name: "tiny-budget", KeyHash: testKeyHash("tiny-budget-secret"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("0.00001")},
			{Name: "scoped", KeyHash: testKeyHash("scoped-secret"), RateLimitBurst: 100, RateLimitRefill: 100, CacheScopeToEndUser: true},
			{Name: "elsewhere-only", KeyHash: testKeyHash("elsewhere-secret"), RateLimitBurst: 100, RateLimitRefill: 100, AllowedSourceCIDRs: []string{"10.0.0.0/8"}},
		},
		Deployments: []controlplane.DeploymentConfig{
			{Name: "gpt4o-primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: upstreamURL, APIKeyEnv: "KELVRAN_MESSAGES_INTEGRATION_TEST_KEY", AcceptLossyAnthropicIngress: acceptLossy},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"gpt-4o": {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
		},
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}
	srv := httptest.NewServer(newDataPlaneMux(pipeline))
	t.Cleanup(srv.Close)
	return srv
}

// postMessages posts body to /v1/messages with the given headers and returns
// status, headers and body. A nil headers map sends x-api-key all-secret.
func postMessages(t *testing.T, gwURL string, headers map[string]string, body string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, gwURL+"/v1/messages?beta=true", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if headers == nil {
		headers = map[string]string{"x-api-key": "all-secret"}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw
}

type anthropicErrorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Code    string `json:"code"`
		Param   string `json:"param"`
	} `json:"error"`
}

func decodeAnthropicError(t *testing.T, raw []byte) anthropicErrorBody {
	t.Helper()
	var e anthropicErrorBody
	if err := json.Unmarshal(raw, &e); err != nil || e.Type != "error" {
		t.Fatalf("not an Anthropic error envelope (%v): %s", err, raw)
	}
	return e
}

type anthropicMessageBody struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

const messagesBody = `{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`

func TestIntegrationMessagesBufferedTurnThroughTheOpenAIMock(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	status, header, raw := postMessages(t, gw.URL, nil, messagesBody)
	if status != http.StatusOK {
		t.Fatalf("status = %d; body %s", status, raw)
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if header.Get(overheadDurationHeader) == "" {
		t.Errorf("%s missing on a buffered turn", overheadDurationHeader)
	}
	var msg anthropicMessageBody
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, raw)
	}
	if msg.Type != "message" || msg.Role != "assistant" || msg.Model != "gpt-4o" || msg.ID == "" {
		t.Errorf("envelope = type %q role %q model %q id %q", msg.Type, msg.Role, msg.Model, msg.ID)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" || msg.Content[0].Text != "hello from the mock upstream" {
		t.Errorf("content = %+v, want one text block with the mock's text", msg.Content)
	}
	if msg.StopReason != "end_turn" || msg.Usage.InputTokens != 7 || msg.Usage.OutputTokens != 4 {
		t.Errorf("stop_reason/usage = %q %d/%d, want end_turn 7/4", msg.StopReason, msg.Usage.InputTokens, msg.Usage.OutputTokens)
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", calls.Load())
	}
}

func TestIntegrationMessagesStreamedTurnThroughTheOpenAIMock(t *testing.T) {
	upstream, calls := newMockStreamingUpstream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	status, header, raw := postMessages(t, gw.URL, nil, `{"model":"gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d; body %s", status, raw)
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	s := string(raw)
	last := -1
	for _, ev := range []string{"event: message_start", "event: content_block_start", "event: content_block_delta", "event: content_block_stop", "event: message_delta", "event: message_stop"} {
		i := strings.Index(s, ev)
		if i < 0 || i < last {
			t.Fatalf("event sequence out of order or missing at %q:\n%s", ev, s)
		}
		last = i
	}
	if strings.Contains(s, "[DONE]") || strings.Contains(s, "event: error") {
		t.Errorf("an Anthropic stream carries no [DONE] and no error event on success:\n%s", s)
	}
	if !strings.Contains(s, `"stop_reason":"end_turn"`) {
		t.Errorf("message_delta lacks stop_reason end_turn:\n%s", s)
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", calls.Load())
	}
}

// TestIntegrationMessagesCredentialHeaders: x-api-key is the bearer's alias
// on this route; a non-empty Authorization wins, so a wrong bearer beside a
// valid x-api-key is a 401 with no fallback (Q2), and the 401 is the
// Anthropic envelope.
func TestIntegrationMessagesCredentialHeaders(t *testing.T) {
	upstream, _ := newMockUpstream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	for name, tc := range map[string]struct {
		headers  map[string]string
		want     int
		wantCode string
	}{
		"x-api-key alone":         {map[string]string{"x-api-key": "all-secret"}, http.StatusOK, ""},
		"bearer alone":            {map[string]string{"Authorization": "Bearer all-secret"}, http.StatusOK, ""},
		"wrong bearer + good key": {map[string]string{"Authorization": "Bearer wrong", "x-api-key": "all-secret"}, http.StatusUnauthorized, "invalid_api_key"},
		"no credential":           {map[string]string{"Authorization": ""}, http.StatusUnauthorized, ""},
	} {
		status, _, raw := postMessages(t, gw.URL, tc.headers, messagesBody)
		if status != tc.want {
			t.Errorf("%s: status = %d, want %d; body %s", name, status, tc.want, raw)
			continue
		}
		if status == http.StatusUnauthorized {
			e := decodeAnthropicError(t, raw)
			if e.Error.Type != "authentication_error" || e.Error.Code != tc.wantCode {
				t.Errorf("%s: envelope type/code = %q/%q, want authentication_error/%q", name, e.Error.Type, e.Error.Code, tc.wantCode)
			}
		}
	}
}

// TestIntegrationMessagesErrorEnvelopeIsAnthropicShaped: the pipeline's
// statuses map to Anthropic's types with Kelvran's code kept, so kelvran
// connect's probe (400 + model_not_found) and Claude Code both read it.
func TestIntegrationMessagesErrorEnvelopeIsAnthropicShaped(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	unknownModel := `{"model":"kelvran-connect-check","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`
	for name, tc := range map[string]struct {
		headers            map[string]string
		body               string
		want               int
		wantType, wantCode string
		wantParam          string
	}{
		"unknown model":      {nil, unknownModel, http.StatusBadRequest, "invalid_request_error", "model_not_found", ""},
		"model not allowed":  {map[string]string{"x-api-key": "mini-secret"}, messagesBody, http.StatusForbidden, "permission_error", "model_not_allowed", ""},
		"malformed json":     {nil, `{"model":`, http.StatusBadRequest, "invalid_request_error", "invalid_json", ""},
		"missing max_tokens": {nil, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, http.StatusBadRequest, "invalid_request_error", "missing_required_parameter", "max_tokens"},
		"missing model":      {nil, `{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, http.StatusBadRequest, "invalid_request_error", "missing_required_parameter", "model"},
	} {
		status, _, raw := postMessages(t, gw.URL, tc.headers, tc.body)
		if status != tc.want {
			t.Errorf("%s: status = %d, want %d; body %s", name, status, tc.want, raw)
			continue
		}
		e := decodeAnthropicError(t, raw)
		if e.Error.Type != tc.wantType || e.Error.Code != tc.wantCode || e.Error.Param != tc.wantParam {
			t.Errorf("%s: envelope = %+v, want %s/%s param %q", name, e.Error, tc.wantType, tc.wantCode, tc.wantParam)
		}
		if e.Error.Message == "" {
			t.Errorf("%s: empty message", name)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("upstream calls = %d, want 0: every row fails before the upstream", calls.Load())
	}
	// 405 with Allow: POST and the envelope; 422 on a reused Idempotency-Key.
	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/v1/messages", nil)
	req.Header.Set("x-api-key", "all-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != http.MethodPost {
		t.Errorf("GET = %d Allow %q, want 405 Allow POST", resp.StatusCode, resp.Header.Get("Allow"))
	}
	if e := decodeAnthropicError(t, raw); e.Error.Type != "invalid_request_error" || e.Error.Code != "method_not_allowed" {
		t.Errorf("405 envelope = %+v", e.Error)
	}
	first := map[string]string{"x-api-key": "all-secret", "Idempotency-Key": "messages-k1"}
	if status, _, raw := postMessages(t, gw.URL, first, messagesBody); status != http.StatusOK {
		t.Fatalf("first idempotent request = %d; body %s", status, raw)
	}
	status, _, raw := postMessages(t, gw.URL, first, `{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"a different body"}]}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("reused key with another body = %d; body %s", status, raw)
	}
	if e := decodeAnthropicError(t, raw); e.Error.Type != "invalid_request_error" || e.Error.Code != "idempotency_key_reused" || e.Error.Param != "Idempotency-Key" {
		t.Errorf("422 envelope = %+v", e.Error)
	}
}

// TestIntegrationMessagesLossyBodyIs400WithTheQ5Message: a body with
// members the shadow cannot hold, on a pool with no eligible deployment, is
// the S9b 400 through the Anthropic envelope: the sentence names the
// top-level member and the remedies, counts the nested one, carries the
// pointers in param, and never the strings Claude Code matches on (incl.
// "adaptive", cc-connect.txt:240); with the flag on the turn is served and
// the log line records what was dropped.
func TestIntegrationMessagesLossyBodyIs400WithTheQ5Message(t *testing.T) {
	lossy := `{"model":"gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"service_tier":"auto","thinking":{"type":"adaptive","display":"summarized"}}`
	upstream, calls := newMockUpstream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	status, _, raw := postMessages(t, gw.URL, nil, lossy)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", status, raw)
	}
	e := decodeAnthropicError(t, raw)
	if e.Error.Type != "invalid_request_error" || e.Error.Code != "lossy_ingress_rejected" || e.Error.Param != "/service_tier,/thinking/display" {
		t.Errorf("envelope = %+v", e.Error)
	}
	for _, want := range []string{"service_tier", "accept_lossy_anthropic_ingress", "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1", "1 nested member"} {
		if !strings.Contains(e.Error.Message, want) {
			t.Errorf("message lacks %q: %s", want, e.Error.Message)
		}
	}
	lower := strings.ToLower(e.Error.Message)
	for _, forbidden := range []string{"thinking", "cache_control", "system", "output_config.effort", "extra inputs are not permitted", "input tag", "bound to a different conversation", "capability_rejected:", "adaptive", "gpt4o-primary"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("message contains %q: %s", forbidden, e.Error.Message)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("upstream calls = %d, want 0", calls.Load())
	}
	var logs bytes.Buffer
	accepting := newMessagesIntegrationServer(t, upstream.URL, true, slog.New(slog.NewJSONHandler(&logs, nil)))
	if status, _, raw := postMessages(t, accepting.URL, nil, lossy); status != http.StatusOK {
		t.Fatalf("with the flag: status = %d; body %s", status, raw)
	}
	for _, want := range []string{`"ingress_format":"anthropic-messages"`, `"passthrough":false`, `"dropped_fields":"/service_tier,/thinking/display"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %s:\n%s", want, logs.String())
		}
	}
}

// TestIntegrationMessagesRetryAfterRows: integer seconds on a rate-limit
// 429; an upstream Retry-After above 60 s relayed as 60; no header on a
// budget 429 (RFC-1 §9 records both consequences).
func TestIntegrationMessagesRetryAfterRows(t *testing.T) {
	upstream, _ := newMockUpstream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	burst := map[string]string{"x-api-key": "burst-secret"}
	if status, _, raw := postMessages(t, gw.URL, burst, messagesBody); status != http.StatusOK {
		t.Fatalf("burst first = %d; body %s", status, raw)
	}
	status, header, raw := postMessages(t, gw.URL, burst, messagesBody)
	if status != http.StatusTooManyRequests {
		t.Fatalf("burst second = %d, want 429; body %s", status, raw)
	}
	if e := decodeAnthropicError(t, raw); e.Error.Type != "rate_limit_error" || e.Error.Code != "rate_limit_exceeded" {
		t.Errorf("rate-limit envelope = %+v", e.Error)
	}
	if n, err := strconv.Atoi(header.Get("Retry-After")); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want integer seconds >= 1", header.Get("Retry-After"))
	}
	tiny := map[string]string{"x-api-key": "tiny-budget-secret"}
	if status, _, raw := postMessages(t, gw.URL, tiny, messagesBody); status != http.StatusOK {
		t.Fatalf("budget first = %d; body %s", status, raw)
	}
	status, header, raw = postMessages(t, gw.URL, tiny, messagesBody)
	if status != http.StatusTooManyRequests {
		t.Fatalf("budget second = %d, want 429; body %s", status, raw)
	}
	if e := decodeAnthropicError(t, raw); e.Error.Type != "rate_limit_error" || e.Error.Code != "insufficient_quota" {
		t.Errorf("budget envelope = %+v (Anthropic has no quota type; the code tells it apart)", e.Error)
	}
	if header.Get("Retry-After") != "" {
		t.Errorf("a budget 429 carries no Retry-After, got %q", header.Get("Retry-After"))
	}
	throttled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		http.Error(w, `{"error":{"message":"slow down","type":"rate_limit_error"}}`, http.StatusTooManyRequests)
	}))
	t.Cleanup(throttled.Close)
	gw2 := newMessagesIntegrationServer(t, throttled.URL, false, nil)
	status, header, raw = postMessages(t, gw2.URL, nil, messagesBody)
	if status != http.StatusBadGateway {
		t.Fatalf("upstream 429 = %d, want 502; body %s", status, raw)
	}
	if e := decodeAnthropicError(t, raw); e.Error.Type != "api_error" || e.Error.Code != "upstream_error" {
		t.Errorf("upstream-429 envelope = %+v", e.Error)
	}
	if header.Get("Retry-After") != "60" {
		t.Errorf("Retry-After = %q, want 60 (the upstream's 3600 capped)", header.Get("Retry-After"))
	}
}

// TestIntegrationMessagesEndUserIDPrecedence: X-Kelvran-End-User-Id wins;
// metadata.user_id fills the end-user scope only when the header is absent.
// Observed through cache_scope_to_end_user: same scope → a hit.
func TestIntegrationMessagesEndUserIDPrecedence(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	scoped := func(extra map[string]string) map[string]string {
		h := map[string]string{"x-api-key": "scoped-secret"}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}
	withUser := func(id string) string {
		return `{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"scope me"}],"metadata":{"user_id":"` + id + `"}}`
	}
	for i, step := range []struct {
		headers   map[string]string
		body      string
		wantCalls int64
	}{
		{scoped(nil), withUser("a"), 1},
		{scoped(nil), withUser("a"), 1},
		{scoped(nil), withUser("b"), 2},
		{scoped(map[string]string{"X-Kelvran-End-User-Id": "h"}), withUser("b"), 3},
		{scoped(map[string]string{"X-Kelvran-End-User-Id": "h"}), withUser("c"), 3},
	} {
		if status, _, raw := postMessages(t, gw.URL, step.headers, step.body); status != http.StatusOK {
			t.Fatalf("step %d: status = %d; body %s", i, status, raw)
		}
		if calls.Load() != step.wantCalls {
			t.Fatalf("step %d: upstream calls = %d, want %d", i, calls.Load(), step.wantCalls)
		}
	}
}

// TestIntegrationMessagesNoRedirectOnTrailingSlash: Claude Code posts to
// /v1/messages?beta=true and treats a redirect as failure, so the pattern is
// the exact path -- the query is ignored, a trailing slash is a plain 404.
func TestIntegrationMessagesNoRedirectOnTrailingSlash(t *testing.T) {
	upstream, _ := newMockUpstream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect is a failure") }}
	for path, want := range map[string]int{"/v1/messages?beta=true": http.StatusOK, "/v1/messages/": http.StatusNotFound, "/v1/messages/count_tokens": http.StatusNotFound} {
		req, _ := http.NewRequest(http.MethodPost, gw.URL+path, strings.NewReader(messagesBody))
		req.Header.Set("x-api-key", "all-secret")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("POST %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func TestIntegrationMessagesRequestTooLargeIs413(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	huge := `{"model":"gpt-4o","max_tokens":8,"messages":[{"role":"user","content":"` + strings.Repeat("a", maxRequestBodyBytes+1) + `"}]}`
	status, _, raw := postMessages(t, gw.URL, nil, huge)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", status)
	}
	if e := decodeAnthropicError(t, raw); e.Error.Type != "request_too_large" || e.Error.Code != "request_too_large" {
		t.Errorf("413 envelope = %+v", e.Error)
	}
	if calls.Load() != 0 {
		t.Errorf("upstream calls = %d, want 0", calls.Load())
	}
}

// TestIntegrationMessagesMidStreamFailureEmitsErrorEvent: an upstream dying
// after the first chunk ends the Anthropic stream with one error event and
// no message_stop (a clean end before message_delta reads as a dropped
// connection, RFC-1 §7).
func TestIntegrationMessagesMidStreamFailureEmitsErrorEvent(t *testing.T) {
	// A 1 ms keepalive makes a ping likely to coincide with the failure, so
	// the race detector would catch a tracker read that races the ping
	// goroutine (the handler joins the keepalive before reading it).
	previous := anthropicKeepAlivePeriod
	anthropicKeepAlivePeriod = time.Millisecond
	t.Cleanup(func() { anthropicKeepAlivePeriod = previous })
	upstream, calls := newMockStreamingUpstreamFailingMidStream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	status, _, raw := postMessages(t, gw.URL, nil, `{"model":"gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"mid-stream"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the first chunk commits it); body %s", status, raw)
	}
	s := string(raw)
	if !strings.Contains(s, "event: message_start") || !strings.Contains(s, `"text":"partial"`) {
		t.Errorf("client never received the chunk sent before the failure:\n%s", s)
	}
	if strings.Count(s, "event: error") != 1 || !strings.Contains(s, `"type":"api_error"`) {
		t.Errorf("want exactly one api_error event:\n%s", s)
	}
	if strings.Contains(s, "message_stop") || strings.Contains(s, "[DONE]") {
		t.Errorf("a failed stream must not end cleanly:\n%s", s)
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", calls.Load())
	}
}

// TestIntegrationMessagesLogLineCarriesIngressAndNoCredential: the
// chat_completion line records the ingress and neither credential header's
// value ever reaches a log field on this route.
func TestIntegrationMessagesLogLineCarriesIngressAndNoCredential(t *testing.T) {
	upstream, _ := newMockUpstream(t)
	var logs bytes.Buffer
	gw := newMessagesIntegrationServer(t, upstream.URL, false, slog.New(slog.NewJSONHandler(&logs, nil)))
	for _, tc := range []struct {
		headers map[string]string
		want    int
	}{{map[string]string{"x-api-key": "all-secret"}, http.StatusOK}, {map[string]string{"x-api-key": "wrong-" + "s3cr3t-value-y"}, http.StatusUnauthorized}} {
		before := strings.Count(logs.String(), "\n")
		if status, _, raw := postMessages(t, gw.URL, tc.headers, messagesBody); status != tc.want {
			t.Fatalf("status = %d, want %d; body %s", status, tc.want, raw)
		}
		if strings.Count(logs.String(), "\n") == before {
			t.Fatal("no log line written; the absence check would be vacuous")
		}
	}
	out := logs.String()
	for _, want := range []string{`"msg":"chat_completion"`, `"ingress_format":"anthropic-messages"`, `"passthrough":false`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "s3cr3t-value-y") || strings.Contains(out, "all-secret") {
		t.Errorf("a credential value reached the logs:\n%s", out)
	}
}

// TestIntegrationMessagesPromptTooLongCarriesTheRecoveryToken: an upstream
// context-window rejection reaches the client with the capability_rejected
// token Claude Code's recovery matches on (protocol page), ahead of the
// redacted message; the status stays what errorStatus decides.
func TestIntegrationMessagesPromptTooLongCarriesTheRecoveryToken(t *testing.T) {
	var calls atomic.Int64
	tooLong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":{"message":"This model's maximum context length is 8192 tokens; prompt is too long","type":"invalid_request_error"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(tooLong.Close)
	gw := newMessagesIntegrationServer(t, tooLong.URL, false, nil)
	status, _, raw := postMessages(t, gw.URL, nil, messagesBody)
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (an upstream 4xx is upstream_error today); body %s", status, raw)
	}
	e := decodeAnthropicError(t, raw)
	if !strings.HasPrefix(e.Error.Message, "capability_rejected: prompt_too_long") || e.Error.Type != "api_error" {
		t.Errorf("envelope = %+v, want the recovery token first", e.Error)
	}
	if strings.Contains(e.Error.Message, "8192") {
		t.Errorf("the upstream body leaked into the message: %s", e.Error.Message)
	}
}

// TestIntegrationMessagesStreamPingsDuringUpstreamSilence: an upstream that
// goes quiet mid-message (Bedrock's event stream has no pings) must not
// leave the client without bytes; the encoder pings after the keepalive
// period, between message_start and message_stop. The period is shortened
// for the test; the mock stages a pause ten times longer than it.
func TestIntegrationMessagesStreamPingsDuringUpstreamSilence(t *testing.T) {
	previous := anthropicKeepAlivePeriod
	anthropicKeepAlivePeriod = 20 * time.Millisecond
	t.Cleanup(func() { anthropicKeepAlivePeriod = previous })
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write := func(e string) {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", e)
			flusher.Flush()
		}
		write(`{"id":"chatcmpl-slow","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"first"},"finish_reason":null}]}`)
		time.Sleep(200 * time.Millisecond)
		write(`{"id":"chatcmpl-slow","model":"gpt-4o","choices":[{"index":0,"delta":{"content":" second"},"finish_reason":null}]}`)
		write(`{"id":"chatcmpl-slow","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		write(`{"id":"chatcmpl-slow","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":6,"completion_tokens":2,"total_tokens":8}}`)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	t.Cleanup(slow.Close)
	gw := newMessagesIntegrationServer(t, slow.URL, false, nil)
	status, _, raw := postMessages(t, gw.URL, nil, `{"model":"gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"pause"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d; body %s", status, raw)
	}
	s := string(raw)
	start, ping, stop := strings.Index(s, "event: message_start"), strings.Index(s, "event: ping"), strings.Index(s, "event: message_stop")
	if start < 0 || ping < 0 || stop < 0 || start >= ping || ping >= stop {
		t.Fatalf("want a ping between message_start and message_stop (start %d ping %d stop %d):\n%s", start, ping, stop, s)
	}
	if !strings.Contains(s, `"text":" second"`) {
		t.Errorf("the delta after the pause never reached the client:\n%s", s)
	}
}

// TestIntegrationMessagesAnthropicBetaChangesTheCacheKey: two turns that
// differ only in anthropic-beta are two requests to the model, so the second
// beta set is a miss and the repeat of each is a hit (RFC-1 §8 through the
// S10a header fold).
func TestIntegrationMessagesAnthropicBetaChangesTheCacheKey(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	withBeta := func(beta string) map[string]string {
		return map[string]string{"x-api-key": "all-secret", "anthropic-beta": beta}
	}
	for i, step := range []struct {
		headers   map[string]string
		wantCalls int64
	}{
		{withBeta("interleaved-thinking-2025-05-14"), 1},
		{withBeta("interleaved-thinking-2025-05-14"), 1},
		{withBeta("context-1m-2025-08-07, interleaved-thinking-2025-05-14"), 2},
		{withBeta("interleaved-thinking-2025-05-14,context-1m-2025-08-07"), 2},
	} {
		if status, _, raw := postMessages(t, gw.URL, step.headers, messagesBody); status != http.StatusOK {
			t.Fatalf("step %d: status = %d; body %s", i, status, raw)
		}
		if calls.Load() != step.wantCalls {
			t.Fatalf("step %d: upstream calls = %d, want %d", i, calls.Load(), step.wantCalls)
		}
	}
}

// TestIntegrationMessagesHandlerLevelValidatorsRun: the same bounds the
// OpenAI route enforces before the pipeline apply here -- more than 2000
// messages is a 400 invalid_request before any upstream call.
func TestIntegrationMessagesHandlerLevelValidatorsRun(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	var b strings.Builder
	b.WriteString(`{"model":"gpt-4o","max_tokens":8,"messages":[`)
	for i := 0; i < 2001; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		if i%2 == 0 {
			b.WriteString(`{"role":"user","content":"u"}`)
		} else {
			b.WriteString(`{"role":"assistant","content":"a"}`)
		}
	}
	b.WriteString(`]}`)
	status, _, raw := postMessages(t, gw.URL, nil, b.String())
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %.200s", status, raw)
	}
	if e := decodeAnthropicError(t, raw); e.Error.Type != "invalid_request_error" || e.Error.Code != "invalid_request" {
		t.Errorf("envelope = %+v", e.Error)
	}
	if calls.Load() != 0 {
		t.Errorf("upstream calls = %d, want 0", calls.Load())
	}
}
