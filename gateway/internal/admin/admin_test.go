package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
		Router:         router.New([]router.Deployment{{Name: "d1", Model: "gpt-4o"}}),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep dataplane.Deployment, req any) (any, error) {
			return &openai.Response{
				ID: "chatcmpl-fake", Model: dep.UpstreamModel,
				Choices: []openai.Choice{{Message: openai.Message{Role: "assistant", Content: "hi"}, FinishReason: "stop"}},
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
	h := Handler(testConfig(), newTestPipeline(t), fakeAdminCredential())

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
	h := Handler(testConfig(), newTestPipeline(t), fakeAdminCredential())

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
	h := Handler(testConfig(), pipeline, fakeAdminCredential())

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

func TestUpsertVirtualKeyMissingKeyHashIsRejected(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), fakeAdminCredential())

	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-gamma", fakeAdminCredential(), `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDeleteVirtualKeyViaHTTPRemovesAccess(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, fakeAdminCredential())

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
	h := Handler(testConfig(), newTestPipeline(t), fakeAdminCredential())

	rec := doRequest(t, h, http.MethodDelete, "/admin/virtual_keys/never-existed", fakeAdminCredential(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDeleteVirtualKeyLastRemainingKeyReturns409(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), fakeAdminCredential())

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
	h := Handler(testConfig(), newTestPipeline(t), fakeAdminCredential())

	rec := doRequest(t, h, http.MethodGet, "/admin/config", "test-key", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /admin/config with a client virtual key's own bearer value: status = %d, want 401", rec.Code)
	}
}
