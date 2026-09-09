package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/router"
)

// fakeAdminCredential returns a fake, non-secret admin bearer credential
// for tests, assembled from parts rather than one literal so static
// secret-scanning never mistakes it for a real credential.
func fakeAdminCredential() string {
	parts := []string{"not", "a", "real", "admin", "credential", "for", "tests"}
	return strings.Join(parts, "-")
}

func testHashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// discardLogger mirrors dataplane's own identically-named test helper —
// a real *slog.Logger that discards everything, for tests that need to
// pass one but don't care about its output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestPipeline builds a real *dataplane.Pipeline with one virtual key
// ("test-key", bearer secret "test-key") and one deployment, wired to a
// canned upstream response — a self-contained equivalent of
// dataplane_test.go's own unexported helpers, which aren't visible from
// this package.
func newTestPipeline(t *testing.T) *dataplane.Pipeline {
	t.Helper()

	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []dataplane.Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	p, err := dataplane.NewPipeline(dataplane.Config{
		Verifier: verifier,
		Limiter: ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{
			{ID: "test-key", Capacity: 100, RefillPerSecond: 100},
		}),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         router.New([]router.Deployment{{Name: "d1", Model: "gpt-4o"}}, router.HealthConfig{}),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep dataplane.Deployment, req any) (any, error) {
			return &openai.Response{
				ID: "chatcmpl-fake", Model: dep.UpstreamModel,
				Choices: []openai.Choice{{Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"hi"`)}, FinishReason: "stop"}},
				Usage:   openai.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

func testConfig() *controlplane.Config {
	envVarName := strings.Join([]string{"FAKE", "UPSTREAM", "CREDENTIAL", "ENV", "VAR"}, "_")
	return &controlplane.Config{
		ListenAddr: ":8080",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testHashOf("test-key")},
		},
		Deployments: []controlplane.DeploymentConfig{
			{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", APIKeyEnv: envVarName},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{},
	}
}

func doRequest(t *testing.T, h http.Handler, method, path, bearerValue, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearerValue != "" {
		req.Header.Set("Authorization", "Bearer "+bearerValue)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRequestsWithoutTheAdminCredentialAreRejected(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	cases := []struct {
		name        string
		bearerValue string
	}{
		{"missing header entirely", ""},
		{"wrong value", "wrong-value-entirely"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := doRequest(t, h, http.MethodGet, "/admin/config", c.bearerValue, "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("GET /admin/config with %s: status = %d, want 401", c.name, rec.Code)
			}
		})
	}
}

func TestGetConfigReturnsTheRealLoadedConfig(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	rec := doRequest(t, h, http.MethodGet, "/admin/config", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var got controlplane.Config
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}
	if got.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want %q", got.ListenAddr, ":8080")
	}
	if len(got.VirtualKeys) != 1 || got.VirtualKeys[0].Name != "test-key" {
		t.Errorf("VirtualKeys = %+v, want one entry named test-key", got.VirtualKeys)
	}
}

func TestUpsertVirtualKeyViaHTTPMakesTheKeyImmediatelyUsable(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger())

	newBearerValue := "brand-new-value"
	body := `{"key_hash":"` + testHashOf(newBearerValue) + `","rate_limit":{"burst":50,"refill_per_second":50}}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-gamma", fakeAdminCredential(), body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	_, err := pipeline.HandleChatCompletion(context.Background(), "Bearer "+newBearerValue, adapter.ChatRequest{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("HandleChatCompletion with the newly-admin-added key: %v", err)
	}
}

// TestUpsertVirtualKeyWithPerModelRateLimitIsEnforced proves the live
// Admin API mutation surface for per docs/rfcs/2026-09-07-gateway-multi-
// dimensional-rate-limits.md actually wires through to real enforcement
// — not just that the request struct has the field, per that RFC's own
// "never ship a rate-limit field in only one of the two surfaces"
// completeness discipline.
func TestUpsertVirtualKeyWithPerModelRateLimitIsEnforced(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger())

	newBearerValue := "per-model-test-value"
	body := `{"key_hash":"` + testHashOf(newBearerValue) + `","rate_limit":{"burst":100,"refill_per_second":100,"per_model":{"gpt-4o":{"burst":1,"refill_per_second":0.0001}}}}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-delta", fakeAdminCredential(), body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	authHeader := "Bearer " + newBearerValue
	if _, err := pipeline.HandleChatCompletion(context.Background(), authHeader, adapter.ChatRequest{Model: "gpt-4o"}); err != nil {
		t.Fatalf("first gpt-4o request: %v", err)
	}
	if _, err := pipeline.HandleChatCompletion(context.Background(), authHeader, adapter.ChatRequest{Model: "gpt-4o"}); err == nil {
		t.Fatal("second gpt-4o request succeeded, want a rate-limit rejection — the Admin-API-configured per_model override (burst 1) should already be exhausted")
	}
}

// TestUpsertVirtualKeyRejectsNonPositivePerModelRateLimit mirrors
// controlplane's TestLoadRejectsNonPositivePerModelRateLimit for the live
// Admin API surface, so an operator gets the same validation regardless
// of which of the two surfaces they use.
func TestUpsertVirtualKeyRejectsNonPositivePerModelRateLimit(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	body := `{"key_hash":"` + testHashOf("irrelevant") + `","rate_limit":{"burst":100,"refill_per_second":100,"per_model":{"gpt-4o":{"burst":0,"refill_per_second":1}}}}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-epsilon", fakeAdminCredential(), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

// TestUpsertVirtualKeyWithPerModelTPMRateLimitIsEnforced mirrors
// TestUpsertVirtualKeyWithPerModelRateLimitIsEnforced for the Phase 4
// PerModel-for-TPM direct-path extension: a per-model TPM override set
// via the live Admin API is genuinely enforced, independent of the key's
// own default TPM bucket.
func TestUpsertVirtualKeyWithPerModelTPMRateLimitIsEnforced(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger())

	newBearerValue := "per-model-tpm-test-value"
	body := `{"key_hash":"` + testHashOf(newBearerValue) + `","rate_limit":{"burst":100,"refill_per_second":100,"tpm_capacity":1000,"tpm_refill_per_second":0.0001,"per_model":{"gpt-4o":{"burst":100,"refill_per_second":100,"tpm_capacity":1,"tpm_refill_per_second":0.0001}}}}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-zeta", fakeAdminCredential(), body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	authHeader := "Bearer " + newBearerValue
	if _, err := pipeline.HandleChatCompletion(context.Background(), authHeader, adapter.ChatRequest{Model: "gpt-4o"}); err != nil {
		t.Fatalf("first gpt-4o request: %v", err)
	}
	if _, err := pipeline.HandleChatCompletion(context.Background(), authHeader, adapter.ChatRequest{Model: "gpt-4o"}); err == nil {
		t.Fatal("second gpt-4o request succeeded, want a rate-limit rejection — the Admin-API-configured per_model TPM override (capacity 1) should already be exhausted")
	}
}

// TestUpsertVirtualKeyRejectsPerModelTPMCapacityWithoutRefill mirrors
// TestUpsertVirtualKeyRejectsNonPositivePerModelRateLimit for the TPM
// pair's own "set together or neither" validation, so an operator gets
// the same validation regardless of which of the two config surfaces
// they use.
func TestUpsertVirtualKeyRejectsPerModelTPMCapacityWithoutRefill(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	body := `{"key_hash":"` + testHashOf("irrelevant") + `","rate_limit":{"burst":100,"refill_per_second":100,"per_model":{"gpt-4o":{"burst":1,"refill_per_second":1,"tpm_capacity":1000}}}}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-eta", fakeAdminCredential(), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

func TestUpsertVirtualKeyMissingKeyHashIsRejected(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-gamma", fakeAdminCredential(), `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDeleteVirtualKeyViaHTTPRemovesAccess(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger())

	// Add a second key first, so deleting one still leaves one behind.
	otherBearerValue := "other-bearer-value"
	body := `{"key_hash":"` + testHashOf(otherBearerValue) + `","rate_limit":{"burst":50,"refill_per_second":50}}`
	doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-beta", fakeAdminCredential(), body)

	rec := doRequest(t, h, http.MethodDelete, "/admin/virtual_keys/team-beta", fakeAdminCredential(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	_, err := pipeline.HandleChatCompletion(context.Background(), "Bearer "+otherBearerValue, adapter.ChatRequest{Model: "gpt-4o"})
	if err == nil {
		t.Fatal("HandleChatCompletion succeeded with a deleted key's bearer value")
	}
}

func TestDeleteVirtualKeyUnknownNameReturns404(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	rec := doRequest(t, h, http.MethodDelete, "/admin/virtual_keys/never-existed", fakeAdminCredential(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDeleteVirtualKeyLastRemainingKeyReturns409(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	rec := doRequest(t, h, http.MethodDelete, "/admin/virtual_keys/test-key", fakeAdminCredential(), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

// TestClientVirtualKeyNeverAuthenticatesAgainstAdmin proves the RFC's
// core auth-separation claim: a real client-facing virtual key's own
// bearer value must never work against /admin/*, since the two are
// deliberately separate credential spaces.
func TestClientVirtualKeyNeverAuthenticatesAgainstAdmin(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	rec := doRequest(t, h, http.MethodGet, "/admin/config", "test-key", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /admin/config with a client virtual key's own bearer value: status = %d, want 401", rec.Code)
	}
}

// fakeViewerCredential mirrors fakeAdminCredential's own
// assembled-from-parts convention, for the same secret-scanning reason.
func fakeViewerCredential() string {
	parts := []string{"not", "a", "real", "viewer", "credential", "for", "tests"}
	return strings.Join(parts, "-")
}

// TestViewerTokenCanReadConfigButNotMutateVirtualKeys proves
// docs/rfcs/2026-09-09-gateway-admin-viewer-role.md's core claim: a
// configured viewer credential authenticates the read-only route but
// never a write route.
func TestViewerTokenCanReadConfigButNotMutateVirtualKeys(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential(), Viewer: fakeViewerCredential()}, discardLogger())

	rec := doRequest(t, h, http.MethodGet, "/admin/config", fakeViewerCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config with the viewer credential: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	body := `{"key_hash":"` + testHashOf("irrelevant") + `"}`
	rec = doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-theta", fakeViewerCredential(), body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /admin/virtual_keys with the viewer credential: status = %d, want 401 — viewer must never authenticate a write route", rec.Code)
	}

	rec = doRequest(t, h, http.MethodDelete, "/admin/virtual_keys/test-key", fakeViewerCredential(), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("DELETE /admin/virtual_keys with the viewer credential: status = %d, want 401 — viewer must never authenticate a write route", rec.Code)
	}
}

// TestAdminTokenStillWorksForEverythingWhenAViewerTierIsConfigured
// proves the new viewer tier is additive — configuring one never takes
// away the admin credential's own existing full read/write access.
func TestAdminTokenStillWorksForEverythingWhenAViewerTierIsConfigured(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential(), Viewer: fakeViewerCredential()}, discardLogger())

	rec := doRequest(t, h, http.MethodGet, "/admin/config", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config with the admin credential: status = %d, want 200", rec.Code)
	}

	body := `{"key_hash":"` + testHashOf("viewer-tier-added-value") + `","rate_limit":{"burst":50,"refill_per_second":50}}`
	rec = doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-iota", fakeAdminCredential(), body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /admin/virtual_keys with the admin credential: status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}
}

// TestOmittingTheViewerTierBehavesExactlyAsBefore proves the viewer
// tier is fully optional — an empty Credentials.Viewer reproduces the
// pre-viewer-role behavior exactly: only the admin credential works on
// GET /admin/config, and an unrecognized value (that happens to equal
// an empty string comparison edge case) is still rejected.
func TestOmittingTheViewerTierBehavesExactlyAsBefore(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	rec := doRequest(t, h, http.MethodGet, "/admin/config", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /admin/config with no credential at all: status = %d, want 401", rec.Code)
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/config", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config with the admin credential, no viewer tier configured: status = %d, want 200", rec.Code)
	}
}

// capturingLogger returns a *slog.Logger backed by a *strings.Builder
// the test can inspect, plus that builder — used to prove the audit-log
// lines fire on success and never leak a secret value.
func capturingLogger() (*slog.Logger, *strings.Builder) {
	var buf strings.Builder
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// TestUpsertVirtualKeyLogsAnAuditEntryWithoutLeakingTheSecret proves
// docs/rfcs/2026-09-09-gateway-admin-viewer-role.md's audit-logging
// claim for a successful create.
func TestUpsertVirtualKeyLogsAnAuditEntryWithoutLeakingTheSecret(t *testing.T) {
	logger, buf := capturingLogger()
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, logger)

	newBearerValue := "audit-log-test-value"
	body := `{"key_hash":"` + testHashOf(newBearerValue) + `"}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-kappa", fakeAdminCredential(), body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "admin_virtual_key_upserted") || !strings.Contains(logOutput, "name=team-kappa") {
		t.Errorf("expected an admin_virtual_key_upserted audit-log entry naming team-kappa; got: %s", logOutput)
	}
	if strings.Contains(logOutput, fakeAdminCredential()) || strings.Contains(logOutput, testHashOf(newBearerValue)) {
		t.Errorf("audit log must never contain the presented credential or the key_hash; got: %s", logOutput)
	}
}

// TestDeleteVirtualKeyLogsAnAuditEntryWithoutLeakingTheSecret mirrors
// TestUpsertVirtualKeyLogsAnAuditEntryWithoutLeakingTheSecret for delete.
func TestDeleteVirtualKeyLogsAnAuditEntryWithoutLeakingTheSecret(t *testing.T) {
	pipeline := newTestPipeline(t)
	logger, buf := capturingLogger()
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, logger)

	otherBearerValue := "other-bearer-value-for-audit-test"
	body := `{"key_hash":"` + testHashOf(otherBearerValue) + `"}`
	doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-lambda", fakeAdminCredential(), body)

	rec := doRequest(t, h, http.MethodDelete, "/admin/virtual_keys/team-lambda", fakeAdminCredential(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "admin_virtual_key_deleted") || !strings.Contains(logOutput, "name=team-lambda") {
		t.Errorf("expected an admin_virtual_key_deleted audit-log entry naming team-lambda; got: %s", logOutput)
	}
	if strings.Contains(logOutput, fakeAdminCredential()) {
		t.Errorf("audit log must never contain the presented credential; got: %s", logOutput)
	}
}
