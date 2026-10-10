package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// TestNewDataPlaneMuxRegistersExactlyTheDataPlaneRoutes: every data-plane
// route is registered by one builder, so integration tests and main.go are
// wired the same way and a later slice adds a route in one place; nothing
// else (a trailing-slash variant, /v1/messages/count_tokens before S10b) resolves.
func TestNewDataPlaneMuxRegistersExactlyTheDataPlaneRoutes(t *testing.T) {
	mux := newDataPlaneMux(nil)
	want := map[string]bool{"/v1/chat/completions": true, "/v1/embeddings": true, "/v1/models": true, "/healthz": true, "/readyz": true, "/api/hello": true, "/v1/messages": true}
	for _, path := range dataPlaneRoutes {
		if !want[path] {
			t.Errorf("unexpected route %q in dataPlaneRoutes", path)
		}
		delete(want, path)
		if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, path, nil)); pattern != path {
			t.Errorf("mux.Handler(%s) pattern = %q, want the exact path", path, pattern)
		}
	}
	for path := range want {
		t.Errorf("route %q missing from dataPlaneRoutes", path)
	}
	for _, path := range []string{"/v1/messages/", "/v1/messages/count_tokens", "/v1/models/", "/api/hello/"} {
		if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, path, nil)); pattern != "" {
			t.Errorf("%s resolved to pattern %q, want not found", path, pattern)
		}
	}
}

// TestIntegrationHelloHeadIs204WithoutAuth: Claude Code's gateway hint probe
// is `HEAD /api/hello`; it answers 204 with no body and no auth check, even
// with a bad bearer attached.
func TestIntegrationHelloHeadIs204WithoutAuth(t *testing.T) {
	gw := newModelsIntegrationServer(t, modelsTestKeys(), nil)
	for name, auth := range map[string]string{"no auth": "", "bad bearer": "Bearer definitely-wrong"} {
		req, _ := http.NewRequest(http.MethodHead, gw.URL+"/api/hello", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent || len(body) != 0 {
			t.Errorf("%s: HEAD /api/hello = %d body %d bytes, want 204 and no body", name, resp.StatusCode, len(body))
		}
	}
}

// TestIntegrationHelloOtherMethodsAre405: any method but HEAD gets the
// OpenAI-shaped method_not_allowed envelope with Allow: HEAD.
func TestIntegrationHelloOtherMethodsAre405(t *testing.T) {
	gw := newModelsIntegrationServer(t, modelsTestKeys(), nil)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req, _ := http.NewRequest(method, gw.URL+"/api/hello", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != http.MethodHead {
			t.Errorf("%s /api/hello = %d Allow %q, want 405 Allow HEAD", method, resp.StatusCode, resp.Header.Get("Allow"))
		}
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &env); err != nil || env.Error.Code != "method_not_allowed" {
			t.Errorf("%s /api/hello body = %s, want the method_not_allowed envelope", method, body)
		}
	}
}

// TestBearerFromRequestPrecedence: Authorization wins verbatim; x-api-key is
// the Anthropic SDK's and Claude Code's credential header and becomes the
// bearer only when Authorization is absent, so exactly one credential is
// ever verified and a wrong ANTHROPIC_AUTH_TOKEN is a 401 with no fallback.
func TestBearerFromRequestPrecedence(t *testing.T) {
	for name, tc := range map[string]struct {
		auth, apiKey, want string
	}{
		"authorization only":   {"Bearer a", "", "Bearer a"},
		"x-api-key only":       {"", "k", "Bearer k"},
		"both: auth wins":      {"Bearer a", "k", "Bearer a"},
		"neither":              {"", "", ""},
		"malformed auth wins":  {"Basic zz", "k", "Basic zz"},
		"empty auth is absent": {"", "k", "Bearer k"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		if tc.auth != "" || name == "empty auth is absent" {
			r.Header.Set("Authorization", tc.auth) // an explicit empty header is absent for Get
		}
		if tc.apiKey != "" {
			r.Header.Set("x-api-key", tc.apiKey)
		}
		if got := bearerFromRequest(r); got != tc.want {
			t.Errorf("%s: bearerFromRequest = %q, want %q", name, got, tc.want)
		}
	}
}

// TestIntegrationModelsAcceptsXAPIKeyAlias: /v1/models answers an x-api-key
// credential (the Anthropic SDK's and Claude Code's header) exactly like the
// bearer; with both headers the bearer is the one verified, so a wrong
// bearer beside a valid x-api-key is a 401.
func TestIntegrationModelsAcceptsXAPIKeyAlias(t *testing.T) {
	gw := newModelsIntegrationServer(t, modelsTestKeys(), nil)
	for name, tc := range map[string]struct {
		auth, apiKey string
		want         int
	}{
		"x-api-key alone":         {"", "all-secret", http.StatusOK},
		"wrong bearer + good key": {"Bearer wrong", "all-secret", http.StatusUnauthorized},
		"wrong x-api-key alone":   {"", "wrong", http.StatusUnauthorized},
	} {
		req, _ := http.NewRequest(http.MethodGet, gw.URL+"/v1/models", nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		req.Header.Set("x-api-key", tc.apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: GET /v1/models = %d, want %d", name, resp.StatusCode, tc.want)
		}
	}
}

// TestBearerFromRequestNeverLogs: neither header's value reaches any log
// field on a 401 or a 200. Non-vacuous: at least one log line per request
// is asserted before the absence check.
func TestBearerFromRequestNeverLogs(t *testing.T) {
	gw, logs := newModelsIntegrationServerCapturingLogs(t, modelsTestKeys(), nil)
	for name, tc := range map[string]struct {
		apiKey string
		want   int
	}{"401": {"wrong-" + "s3cr3t-value-x", http.StatusUnauthorized}, "200": {"all-secret", http.StatusOK}} {
		before := strings.Count(logs.String(), "\n")
		req, _ := http.NewRequest(http.MethodGet, gw.URL+"/v1/models", nil)
		req.Header.Set("x-api-key", tc.apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: status %d, want %d", name, resp.StatusCode, tc.want)
		}
		if strings.Count(logs.String(), "\n") == before {
			t.Fatalf("%s: no log line was written for the request; the absence check below would be vacuous", name)
		}
	}
	if out := logs.String(); strings.Contains(out, "s3cr3t-value-x") || strings.Contains(out, "all-secret") {
		t.Errorf("a credential value reached the logs:\n%s", out)
	}
}

// TestIntegrationModelsExpiredKeyLogsTheKeyID: the models route's 401 line
// carries what the chat route's does — the expired key's id and expiry —
// so an operator can tell which key is failing (RFC-3 decision 4); the
// credential itself never appears.
func TestIntegrationModelsExpiredKeyLogsTheKeyID(t *testing.T) {
	keys := append(modelsTestKeys(), controlplane.VirtualKeyConfig{Name: "team-expired", KeyHash: testKeyHash("expired-secret"), ExpiresAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	gw, logs := newModelsIntegrationServerCapturingLogs(t, keys, nil)
	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/v1/models", nil)
	req.Header.Set("x-api-key", "expired-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
	out := logs.String()
	if !strings.Contains(out, `"msg":"list_models_auth_failed"`) || !strings.Contains(out, `"virtual_key_id":"team-expired"`) || !strings.Contains(out, `"key_expired_at":"2026-01-01T00:00:00Z"`) {
		t.Errorf("the 401 line must name the expired key and its expiry:\n%s", out)
	}
	if strings.Contains(out, "expired-secret") {
		t.Errorf("the credential reached the logs:\n%s", out)
	}
}
