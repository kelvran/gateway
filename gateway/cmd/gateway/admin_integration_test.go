// See integration_test.go's own doc comment for why this lives in
// package main, not an external _test package: it needs buildPipeline
// and chatCompletionsHandler, both unexported on purpose.
package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/admin"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// fakeAdminCredentialForIntegrationTest returns a fake, non-secret admin
// bearer credential, assembled from parts rather than one literal, per
// this codebase's own established pattern for avoiding false-positive
// secret-scanning matches on obviously-fake test values.
func fakeAdminCredentialForIntegrationTest() string {
	parts := []string{"not", "a", "real", "admin", "credential", "integration", "test"}
	return strings.Join(parts, "-")
}

// newAdminIntegrationServers builds ONE real pipeline (via buildPipeline,
// the exact function main.run() calls) and serves it through TWO
// separate httptest.Server instances — a client-facing one
// (chatCompletionsHandler) and an admin one (admin.Handler) — mirroring
// main.run()'s real two-separate-listener shape, per
// docs/rfcs/2026-09-05-gateway-admin-api.md's "never the same mux/port"
// rule. Returns both servers plus the admin bearer credential.
func newAdminIntegrationServers(t *testing.T, upstreamURL, upstreamKeyEnvVar string) (clientSrv, adminSrv *httptest.Server, adminToken string) {
	t.Helper()
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")

	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
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

	clientMux := http.NewServeMux()
	clientMux.HandleFunc("/v1/chat/completions", chatCompletionsHandler(pipeline))
	clientSrv = httptest.NewServer(clientMux)
	t.Cleanup(clientSrv.Close)

	adminToken = fakeAdminCredentialForIntegrationTest()
	adminSrv = httptest.NewServer(admin.Handler(cfg, pipeline, adminToken))
	t.Cleanup(adminSrv.Close)

	return clientSrv, adminSrv, adminToken
}

// TestIntegrationAdminAPIAddsVirtualKeyUsableOnMainServer proves the
// RFC's core end-to-end claim: a virtual key added through a real HTTP
// POST against the admin server's own separate listener is immediately
// usable for a real HTTP request against the main, client-facing
// listener — no restart, no shared process state beyond the one
// pipeline both servers were built from.
func TestIntegrationAdminAPIAddsVirtualKeyUsableOnMainServer(t *testing.T) {
	upstream, _ := newMockUpstream(t)
	clientSrv, adminSrv, adminToken := newAdminIntegrationServers(t, upstream.URL, "OPENAI_API_KEY_ADMIN_TEST")

	newSecret := "brand-new-secret-from-admin-api"
	body := `{"key_hash":"` + testKeyHash(newSecret) + `","rate_limit":{"burst":50,"refill_per_second":50}}`
	req, err := http.NewRequest(http.MethodPost, adminSrv.URL+"/admin/virtual_keys/team-gamma", strings.NewReader(body))
	if err != nil {
		t.Fatalf("building admin request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("admin POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("admin POST status = %d, want 204", resp.StatusCode)
	}

	clientReq, err := http.NewRequest(http.MethodPost, clientSrv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("building client request: %v", err)
	}
	clientReq.Header.Set("Authorization", "Bearer "+newSecret)
	clientReq.Header.Set("Content-Type", "application/json")
	clientResp, err := http.DefaultClient.Do(clientReq)
	if err != nil {
		t.Fatalf("client request with the newly-admin-added key: %v", err)
	}
	defer func() { _ = clientResp.Body.Close() }()
	if clientResp.StatusCode != http.StatusOK {
		t.Fatalf("client request status = %d, want 200 — the admin-added key should authenticate on the real main-listener path", clientResp.StatusCode)
	}
}

// TestIntegrationAdminAPIRejectsClientVirtualKeyCredential proves the
// RFC's auth-separation claim over real HTTP, not just at the handler-
// unit level: a genuine client-facing virtual key's own bearer secret
// must never authenticate against the admin server's separate listener.
func TestIntegrationAdminAPIRejectsClientVirtualKeyCredential(t *testing.T) {
	upstream, _ := newMockUpstream(t)
	_, adminSrv, _ := newAdminIntegrationServers(t, upstream.URL, "OPENAI_API_KEY_ADMIN_TEST2")

	req, err := http.NewRequest(http.MethodGet, adminSrv.URL+"/admin/config", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /admin/config: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /admin/config with a client virtual key's own secret: status = %d, want 401", resp.StatusCode)
	}
}
