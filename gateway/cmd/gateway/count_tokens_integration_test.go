package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// POST /v1/messages/count_tokens (item 11 slice S10b, RFC-1 §9): the route
// authenticates, applies the key's source-IP and model allowlists, consumes
// one RPM token, resolves the model, and -- until slice S11 adds the
// anthropic branch -- answers 404 not_found_error for every deployment,
// which Claude Code reads as "estimate locally". It never touches a budget,
// a cache layer or an upstream.

func postCountTokens(t *testing.T, gwURL string, headers map[string]string, body string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, gwURL+"/v1/messages/count_tokens?beta=true", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
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

const countTokensBody = `{"model":"gpt-4o","messages":[{"role":"user","content":"how many tokens"}]}`

// TestIntegrationCountTokensIs404UntilTheAnthropicBranchLands: a 404
// not_found_error with Kelvran's code, no upstream call; the call still
// consumed one RPM token (the burst key's next turn is a 429) and moved no
// budget (the tiny-budget key's one paid turn still succeeds afterwards).
func TestIntegrationCountTokensIs404UntilTheAnthropicBranchLands(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	status, header, raw := postCountTokens(t, gw.URL, nil, countTokensBody)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", status, raw)
	}
	if e := decodeAnthropicError(t, raw); e.Error.Type != "not_found_error" || e.Error.Code != "count_tokens_unavailable" || e.Error.Message == "" {
		t.Errorf("envelope = %+v", e.Error)
	}
	if header.Get("Retry-After") != "" {
		t.Errorf("a 404 carries no Retry-After, got %q", header.Get("Retry-After"))
	}
	if calls.Load() != 0 {
		t.Errorf("upstream calls = %d, want 0", calls.Load())
	}
	// One RPM token consumed: the burst-one key's following turn is a 429.
	burst := map[string]string{"x-api-key": "burst-secret"}
	if status, _, raw := postCountTokens(t, gw.URL, burst, countTokensBody); status != http.StatusNotFound {
		t.Fatalf("burst count_tokens = %d; body %s", status, raw)
	}
	status, header, raw = postMessages(t, gw.URL, burst, messagesBody)
	if status != http.StatusTooManyRequests {
		t.Fatalf("turn after count_tokens on the burst key = %d, want 429 (count_tokens consumes an RPM token); body %s", status, raw)
	}
	if header.Get("Retry-After") == "" {
		t.Errorf("the rate-limit 429 lacks Retry-After")
	}
	// No budget movement: the tiny-budget key still has its one paid turn.
	tiny := map[string]string{"x-api-key": "tiny-budget-secret"}
	if status, _, raw := postCountTokens(t, gw.URL, tiny, countTokensBody); status != http.StatusNotFound {
		t.Fatalf("tiny count_tokens = %d; body %s", status, raw)
	}
	if status, _, raw := postMessages(t, gw.URL, tiny, messagesBody); status != http.StatusOK {
		t.Fatalf("tiny key's first paid turn after count_tokens = %d, want 200 (count_tokens debits no budget); body %s", status, raw)
	}
}

// TestIntegrationCountTokensRateLimitIs429WithRetryAfter: the RPM rejection
// on this route is the same 429 envelope and Retry-After rule as a turn.
func TestIntegrationCountTokensRateLimitIs429WithRetryAfter(t *testing.T) {
	upstream, _ := newMockUpstream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	burst := map[string]string{"x-api-key": "burst-secret"}
	if status, _, raw := postCountTokens(t, gw.URL, burst, countTokensBody); status != http.StatusNotFound {
		t.Fatalf("first = %d; body %s", status, raw)
	}
	status, header, raw := postCountTokens(t, gw.URL, burst, countTokensBody)
	if status != http.StatusTooManyRequests {
		t.Fatalf("second = %d, want 429; body %s", status, raw)
	}
	if e := decodeAnthropicError(t, raw); e.Error.Type != "rate_limit_error" || e.Error.Code != "rate_limit_exceeded" {
		t.Errorf("envelope = %+v", e.Error)
	}
	if header.Get("Retry-After") == "" {
		t.Errorf("Retry-After missing on the count_tokens 429")
	}
}

// TestIntegrationCountTokensAuthAllowlistAndShape: the same gates and
// envelope rows as /v1/messages, before any routing.
func TestIntegrationCountTokensAuthAllowlistAndShape(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	for name, tc := range map[string]struct {
		headers            map[string]string
		body               string
		want               int
		wantType, wantCode string
		wantParam          string
	}{
		"no credential":         {map[string]string{"Authorization": ""}, countTokensBody, http.StatusUnauthorized, "authentication_error", "", ""},
		"wrong key":             {map[string]string{"x-api-key": "wrong"}, countTokensBody, http.StatusUnauthorized, "authentication_error", "invalid_api_key", ""},
		"bearer alone":          {map[string]string{"Authorization": "Bearer all-secret"}, countTokensBody, http.StatusNotFound, "not_found_error", "count_tokens_unavailable", ""},
		"model not allowed":     {map[string]string{"x-api-key": "mini-secret"}, countTokensBody, http.StatusForbidden, "permission_error", "model_not_allowed", ""},
		"source ip not allowed": {map[string]string{"x-api-key": "elsewhere-secret"}, countTokensBody, http.StatusForbidden, "permission_error", "source_ip_not_allowed", ""},
		"unknown model":         {nil, `{"model":"kelvran-connect-check","messages":[]}`, http.StatusBadRequest, "invalid_request_error", "model_not_found", ""},
		"missing model":         {nil, `{"messages":[{"role":"user","content":"hi"}]}`, http.StatusBadRequest, "invalid_request_error", "missing_required_parameter", "model"},
		"malformed json":        {nil, `{"model":`, http.StatusBadRequest, "invalid_request_error", "invalid_json", ""},
	} {
		status, _, raw := postCountTokens(t, gw.URL, tc.headers, tc.body)
		if status != tc.want {
			t.Errorf("%s: status = %d, want %d; body %s", name, status, tc.want, raw)
			continue
		}
		e := decodeAnthropicError(t, raw)
		if e.Error.Type != tc.wantType || e.Error.Code != tc.wantCode || e.Error.Param != tc.wantParam {
			t.Errorf("%s: envelope = %+v, want %s/%s param %q", name, e.Error, tc.wantType, tc.wantCode, tc.wantParam)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("upstream calls = %d, want 0", calls.Load())
	}
	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/v1/messages/count_tokens", nil)
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
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect is a failure") }}
	req, _ = http.NewRequest(http.MethodPost, gw.URL+"/v1/messages/count_tokens/", strings.NewReader(countTokensBody))
	req.Header.Set("x-api-key", "all-secret")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("trailing slash: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Errorf("POST /v1/messages/count_tokens/ = %d %q, want Go's plain 404, never a redirect", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// TestIntegrationCountTokensLogsWithoutTheCredential: the route writes a
// count_tokens line per call (so the never-logs check is not vacuous) and
// neither credential header's value reaches it.
func TestIntegrationCountTokensLogsWithoutTheCredential(t *testing.T) {
	upstream, _ := newMockUpstream(t)
	var logs bytes.Buffer
	gw := newMessagesIntegrationServer(t, upstream.URL, false, slog.New(slog.NewJSONHandler(&logs, nil)))
	for _, tc := range []struct {
		headers map[string]string
		want    int
	}{{map[string]string{"x-api-key": "all-secret"}, http.StatusNotFound}, {map[string]string{"x-api-key": "wrong-" + "s3cr3t-value-z"}, http.StatusUnauthorized}} {
		before := strings.Count(logs.String(), "\n")
		if status, _, raw := postCountTokens(t, gw.URL, tc.headers, countTokensBody); status != tc.want {
			t.Fatalf("status = %d, want %d; body %s", status, tc.want, raw)
		}
		if strings.Count(logs.String(), "\n") == before {
			t.Fatal("no log line written; the absence check would be vacuous")
		}
	}
	out := logs.String()
	if !strings.Contains(out, `"msg":"count_tokens"`) || !strings.Contains(out, `"msg":"count_tokens_auth_failed"`) {
		t.Errorf("log lacks the count_tokens lines:\n%s", out)
	}
	if strings.Contains(out, "s3cr3t-value-z") || strings.Contains(out, "all-secret") {
		t.Errorf("a credential value reached the logs:\n%s", out)
	}
}
