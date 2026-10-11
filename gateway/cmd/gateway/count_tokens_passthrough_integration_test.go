package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type capturedCountTokensRequest struct {
	path    string
	headers http.Header
	body    string
}

// newScriptedCountTokensUpstream answers /count_tokens with one fixed status,
// body and header set, recording every request; any other path is a 500.
func newScriptedCountTokensUpstream(t *testing.T, status int, body string, headers map[string]string) (*httptest.Server, func() []capturedCountTokensRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []capturedCountTokensRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, capturedCountTokensRequest{path: r.URL.Path, headers: r.Header.Clone(), body: string(raw)})
		mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/count_tokens") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []capturedCountTokensRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedCountTokensRequest(nil), seen...)
	}
}

const countTokensPassthroughBody = `{"model":"claude-sys","messages":[{"role":"user","content":"how many tokens"}],"future":{ "x" : 1}}`

var countTokensClientHeaders = map[string]string{"Authorization": "Bearer all-secret", "anthropic-version": "2024-06-01", "anthropic-beta": "beta-a", "X-Custom": "nope"}

// On an anthropic deployment count_tokens is the deployment's own answer:
// the request body reaches the upstream's /count_tokens as received with the
// model rewritten and no stream member, the client's anthropic-* headers
// forwarded and the deployment's own x-api-key; the answer comes back
// byte-for-byte with its relayable headers (item 11 slice S11c).
func TestIntegrationCountTokensRelaysAnAnthropicDeploymentsAnswer(t *testing.T) {
	upstream, seen := newScriptedCountTokensUpstream(t, http.StatusOK, `{ "input_tokens" : 42 }`, upstreamRelayHeaders)
	gw := newPassthroughIntegrationServer(t, upstream.URL+"/v1/messages", nil)
	status, header, raw := postCountTokens(t, gw.URL, countTokensClientHeaders, countTokensPassthroughBody)
	if status != http.StatusOK || string(raw) != `{ "input_tokens" : 42 }` {
		t.Fatalf("status %d body %q, want 200 with the upstream's bytes", status, raw)
	}
	if header.Get("X-Should-Retry") != "false" || header.Get("Anthropic-Ratelimit-Unified-Status") != "allowed" || header.Get("Request-Id") != "" {
		t.Errorf("relay headers = %v, want x-should-retry and the unified header only", header)
	}
	reqs := seen()
	if len(reqs) != 1 || reqs[0].path != "/v1/messages/count_tokens" {
		t.Fatalf("upstream requests = %+v, want one to /v1/messages/count_tokens", reqs)
	}
	wantBody := `{"model":"claude-upstream-id","messages":[{"role":"user","content":"how many tokens"}],"future":{ "x" : 1}}`
	if reqs[0].body != wantBody {
		t.Errorf("upstream body = %s\nwant          %s", reqs[0].body, wantBody)
	}
	h := reqs[0].headers
	if h.Get("x-api-key") != "fake-upstream-key-not-a-real-secret" || h.Get("anthropic-version") != "2024-06-01" || h.Get("anthropic-beta") != "beta-a" || h.Get("Authorization") != "" || h.Get("X-Custom") != "" {
		t.Errorf("upstream headers = %v, want the deployment's key, the client's anthropic-* and nothing else of the client's", h)
	}
}

// The upstream's own 400 passes through verbatim with its status, as on
// /v1/messages; the guardrail still runs first on the shadow, so a blocked
// body never reaches the upstream.
func TestIntegrationCountTokensVerbatim400AndGuardrailBlock(t *testing.T) {
	upstream, seen := newScriptedCountTokensUpstream(t, http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"messages: too many tokens to count"},"request_id":"req_1"}`, nil)
	gw := newPassthroughIntegrationServer(t, upstream.URL+"/v1/messages", nil)
	status, _, raw := postCountTokens(t, gw.URL, countTokensClientHeaders, countTokensPassthroughBody)
	if status != http.StatusBadRequest || !strings.Contains(string(raw), "too many tokens to count") {
		t.Errorf("status %d body %q, want the upstream's 400 verbatim", status, raw)
	}
	blocked := `{"model":"claude-sys","messages":[{"role":"user","content":"my card is 4111111111111111"}]}`
	status, _, raw = postCountTokens(t, gw.URL, countTokensClientHeaders, blocked)
	if status != http.StatusBadRequest || !strings.Contains(string(raw), "content_policy_violation") {
		t.Errorf("guardrail: status %d body %q, want 400 content_policy_violation", status, raw)
	}
	if n := len(seen()); n != 1 {
		t.Errorf("upstream requests = %d, want 1 (the blocked body never reached it)", n)
	}
}

// A body the Messages parser rejects is a 400 with the parser's code, and
// the upstream is never called.
func TestIntegrationCountTokensBodyErrorIs400(t *testing.T) {
	upstream, seen := newScriptedCountTokensUpstream(t, http.StatusOK, `{"input_tokens":1}`, nil)
	gw := newPassthroughIntegrationServer(t, upstream.URL+"/v1/messages", nil)
	status, _, raw := postCountTokens(t, gw.URL, countTokensClientHeaders, `{"model":"claude-sys"}`)
	if status != http.StatusBadRequest || !strings.Contains(string(raw), `"missing_required_parameter"`) || !strings.Contains(string(raw), `"messages"`) {
		t.Errorf("status %d body %q, want 400 missing_required_parameter on messages", status, raw)
	}
	if n := len(seen()); n != 0 {
		t.Errorf("upstream requests = %d, want 0", n)
	}
}

// A count_tokens answer is a few dozen bytes; one past the 64 KiB cap is an
// upstream failure, redacted like any other.
func TestIntegrationCountTokensOversizedAnswerIsAnUpstreamError(t *testing.T) {
	upstream, _ := newScriptedCountTokensUpstream(t, http.StatusOK, `{"input_tokens":1,"pad":"`+strings.Repeat("x", 70<<10)+`"}`, nil)
	gw := newPassthroughIntegrationServer(t, upstream.URL+"/v1/messages", nil)
	status, _, raw := postCountTokens(t, gw.URL, countTokensClientHeaders, countTokensPassthroughBody)
	if status != http.StatusBadGateway || !strings.Contains(string(raw), "upstream_error") || strings.Contains(string(raw), "xxxx") || strings.Contains(string(raw), "claude-primary") || strings.Contains(string(raw), "65536") {
		t.Errorf("status %d body %.200q, want 502 upstream_error without the upstream bytes, the deployment name or the cap", status, raw)
	}
}

// A transport failure on the count path is redacted exactly as on
// /v1/messages: the same "upstream call failed" wording, never the deployment
// name, the upstream URL or the dial target.
func TestIntegrationCountTokensTransportFailureIsRedacted(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	gw := newPassthroughIntegrationServer(t, deadURL+"/v1/messages", nil)
	status, _, raw := postCountTokens(t, gw.URL, countTokensClientHeaders, countTokensPassthroughBody)
	s := string(raw)
	if status != http.StatusBadGateway || !strings.Contains(s, "upstream_error") || !strings.Contains(s, "upstream call failed for model") {
		t.Fatalf("status %d body %q, want 502 upstream_error with the /v1/messages wording", status, s)
	}
	for _, leak := range []string{"claude-primary", "http://", "127.0.0.1", "connection refused", "/count_tokens"} {
		if strings.Contains(s, leak) {
			t.Errorf("operator detail %q reached the client: %q", leak, s)
		}
	}
}

// Kelvran's own request validators run on the count path too: a body
// /v1/messages refuses is refused here before any upstream byte.
func TestIntegrationCountTokensRunsTheRequestValidators(t *testing.T) {
	upstream, seen := newScriptedCountTokensUpstream(t, http.StatusOK, `{"input_tokens":1}`, nil)
	gw := newPassthroughIntegrationServer(t, upstream.URL+"/v1/messages", nil)
	var b strings.Builder
	b.WriteString(`{"model":"claude-sys","messages":[`)
	for i := 0; i < 2001; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		if i%2 == 0 {
			b.WriteString(`{"role":"user","content":"a"}`)
		} else {
			b.WriteString(`{"role":"assistant","content":"b"}`)
		}
	}
	b.WriteString(`]}`)
	status, _, raw := postCountTokens(t, gw.URL, countTokensClientHeaders, b.String())
	if status != http.StatusBadRequest || !strings.Contains(string(raw), `"invalid_request"`) || !strings.Contains(string(raw), "2000") {
		t.Errorf("status %d body %.300q, want 400 invalid_request naming the 2000-message limit", status, raw)
	}
	if n := len(seen()); n != 0 {
		t.Errorf("upstream requests = %d, want 0", n)
	}
}

// A tool_choice the validators reject carries the same code and param as on
// /v1/messages, so the same body gets the same error on both routes.
func TestIntegrationCountTokensToolChoiceRejectionMatchesMessages(t *testing.T) {
	upstream, seen := newScriptedCountTokensUpstream(t, http.StatusOK, `{"input_tokens":1}`, nil)
	gw := newPassthroughIntegrationServer(t, upstream.URL+"/v1/messages", nil)
	body := `{"model":"claude-sys","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"tool","name":"missing"}}`
	status, _, raw := postCountTokens(t, gw.URL, countTokensClientHeaders, body)
	if status != http.StatusBadRequest || !strings.Contains(string(raw), `"invalid_tool_choice"`) || !strings.Contains(string(raw), `"tool_choice"`) {
		t.Errorf("status %d body %.300q, want 400 invalid_tool_choice with param tool_choice", status, raw)
	}
	if n := len(seen()); n != 0 {
		t.Errorf("upstream requests = %d, want 0", n)
	}
}
