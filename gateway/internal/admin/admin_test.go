package admin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	identityboltstore "github.com/kelvran/gateway/gateway/internal/identity/boltstore"
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

	_, err := pipeline.HandleChatCompletion(context.Background(), "Bearer "+newBearerValue, adapter.ChatRequest{Model: "gpt-4o"}, "")
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
	if _, err := pipeline.HandleChatCompletion(context.Background(), authHeader, adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Fatalf("first gpt-4o request: %v", err)
	}
	if _, err := pipeline.HandleChatCompletion(context.Background(), authHeader, adapter.ChatRequest{Model: "gpt-4o"}, ""); err == nil {
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
	if _, err := pipeline.HandleChatCompletion(context.Background(), authHeader, adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Fatalf("first gpt-4o request: %v", err)
	}
	if _, err := pipeline.HandleChatCompletion(context.Background(), authHeader, adapter.ChatRequest{Model: "gpt-4o"}, ""); err == nil {
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

// TestUpsertVirtualKeyRejectsMalformedJSONBody proves the handler's own
// json.NewDecoder(r.Body).Decode error path against genuinely
// syntactically invalid JSON -- distinct from
// TestUpsertVirtualKeyMissingKeyHashIsRejected above, which sends
// well-formed JSON missing a required field.
func TestUpsertVirtualKeyRejectsMalformedJSONBody(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-x", fakeAdminCredential(), `{not valid json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

// truncatedBodyReader yields a fixed prefix of bytes, then a genuine
// non-EOF read error -- simulating a real client connection dropping
// mid-body (a Content-Length/actual-bytes mismatch), which
// json.Decoder.Decode must surface as a real decode error, never a 500
// or a hang.
type truncatedBodyReader struct {
	prefix []byte
	sent   bool
}

func (r *truncatedBodyReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		n := copy(p, r.prefix)
		return n, nil
	}
	return 0, io.ErrUnexpectedEOF
}

// TestUpsertVirtualKeyRejectsTruncatedBodyContentLengthMismatch proves
// this handler's own json.Decode call handles a body that ends
// (errors) partway through, well-formed-looking JSON prefix included --
// never a 500, never a hang.
func TestUpsertVirtualKeyRejectsTruncatedBodyContentLengthMismatch(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	body := &truncatedBodyReader{prefix: []byte(`{"key_hash":"` + testHashOf("irrelevant"))}
	req := httptest.NewRequest(http.MethodPost, "/admin/virtual_keys/team-truncated", io.NopCloser(body))
	req.Header.Set("Authorization", "Bearer "+fakeAdminCredential())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

// TestUpsertVirtualKeyRejectsOversizedName is the regression proof for
// the real bug fixed in maxAdminIdentifierLen's own doc comment: this
// handler previously validated neither length nor character content on
// the name path parameter at all before it became a map key in
// identity.Verifier/budget.Tracker/ratelimit.KeyLimiter and every future
// log line referencing it.
func TestUpsertVirtualKeyRejectsOversizedName(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	oversizedName := strings.Repeat("a", maxAdminIdentifierLen+1)
	body := `{"key_hash":"` + testHashOf("irrelevant") + `"}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/"+oversizedName, fakeAdminCredential(), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

// TestUpsertVirtualKeyRejectsNegativeBudgetUSD is the regression proof
// for the real bug fixed alongside it: this handler previously performed
// no validation at all on budget_usd, silently accepting a negative
// value that lands in decimal.Decimal.IsPositive()'s own "unlimited"
// bucket for the wrong reason.
func TestUpsertVirtualKeyRejectsNegativeBudgetUSD(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	body := `{"key_hash":"` + testHashOf("irrelevant") + `","budget_usd":"-10"}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-negative-budget", fakeAdminCredential(), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

// TestUpsertVirtualKeyRejectsNegativeBudgetResetIntervalSeconds proves
// the more severe half of the same finding: a negative
// budget_reset_interval_seconds becomes a negative time.Duration, which
// budget.Tracker's resetIfNeeded compares via now.Sub(start) >=
// resetInterval -- trivially true on every check, silently forcing a
// permanent reset rather than erroring on the operator mistake.
func TestUpsertVirtualKeyRejectsNegativeBudgetResetIntervalSeconds(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	body := `{"key_hash":"` + testHashOf("irrelevant") + `","budget_reset_interval_seconds":-3600}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-negative-reset", fakeAdminCredential(), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

// TestUpsertVirtualKeyRejectsOutOfRangeBudgetWarnPercent proves the third
// field of the same finding: budget_warn_percent outside [0, 100] is a
// non-fatal but confusing operator mistake (an alert threshold that can
// never fire, or fires immediately) worth rejecting up front.
func TestUpsertVirtualKeyRejectsOutOfRangeBudgetWarnPercent(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	body := `{"key_hash":"` + testHashOf("irrelevant") + `","budget_warn_percent":150}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-bad-warn-percent", fakeAdminCredential(), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

// TestUpsertVirtualKeyAcceptsZeroBudgetFieldsAsUnlimitedDefault is the
// regression guard: 0 (the common, "no budget configured" default for
// every one of these 3 fields) must still be accepted, not swept up by
// an overly strict >= 0 boundary mistake.
func TestUpsertVirtualKeyAcceptsZeroBudgetFieldsAsUnlimitedDefault(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	body := `{"key_hash":"` + testHashOf("irrelevant") + `","budget_usd":"0","budget_reset_interval_seconds":0,"budget_warn_percent":0}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-zero-budget", fakeAdminCredential(), body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}
}

// TestUpsertVirtualKeyRejectsMalformedBudgetUSDValue proves a budget_usd
// value that fails decimal parsing is rejected -- decimal.Decimal's own
// UnmarshalJSON returns an error for a non-numeric string, which
// propagates up through this handler's outer json.Decode call as an
// ordinary malformed-body 400, never silently defaulting to zero.
func TestUpsertVirtualKeyRejectsMalformedBudgetUSDValue(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	body := `{"key_hash":"` + testHashOf("irrelevant") + `","budget_usd":"not-a-number"}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-bad-budget-usd", fakeAdminCredential(), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
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

	_, err := pipeline.HandleChatCompletion(context.Background(), "Bearer "+otherBearerValue, adapter.ChatRequest{Model: "gpt-4o"}, "")
	if err == nil {
		t.Fatal("HandleChatCompletion succeeded with a deleted key's bearer value")
	}
}

// TestConcurrentRotateVirtualKeyRequestsForSameNameDoNotCorruptState is
// the load-bearing proof, at the real HTTP-handler level, that
// dataplane.Pipeline.virtualKeyMutationMu (fixed earlier this round)
// correctly serializes two concurrent rotate calls for the SAME virtual
// key: both requests must succeed, and the final state must
// deterministically reflect exactly one of the two new secrets, never a
// corrupted mix of both.
func TestConcurrentRotateVirtualKeyRequestsForSameNameDoNotCorruptState(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger())

	secretA := "rotated-secret-a"
	secretB := "rotated-secret-b"
	bodyA := `{"new_key_hash":"` + testHashOf(secretA) + `"}`
	bodyB := `{"new_key_hash":"` + testHashOf(secretB) + `"}`

	var wg sync.WaitGroup
	codes := make([]int, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		codes[0] = doRequest(t, h, http.MethodPost, "/admin/virtual_keys/test-key/rotate", fakeAdminCredential(), bodyA).Code
	}()
	go func() {
		defer wg.Done()
		codes[1] = doRequest(t, h, http.MethodPost, "/admin/virtual_keys/test-key/rotate", fakeAdminCredential(), bodyB).Code
	}()
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusNoContent {
			t.Errorf("rotate call %d status = %d, want 204", i, code)
		}
	}

	vk, ok := pipeline.GetVirtualKey("test-key")
	if !ok {
		t.Fatal("test-key is gone after concurrent rotate calls")
	}
	hashA, hashB := testHashOf(secretA), testHashOf(secretB)
	if vk.KeyHash != hashA && vk.KeyHash != hashB {
		t.Fatalf("final KeyHash %q matches NEITHER rotated secret's hash — state corrupted, not just a race on which one won", vk.KeyHash)
	}
}

// TestConcurrentUpsertVirtualKeyRequestsForSameNameDoNotCorruptState
// mirrors the rotate test above for two concurrent upsert calls with
// genuinely different bodies for the SAME name.
func TestConcurrentUpsertVirtualKeyRequestsForSameNameDoNotCorruptState(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger())

	secretA := "upsert-race-secret-a"
	secretB := "upsert-race-secret-b"
	bodyA := `{"key_hash":"` + testHashOf(secretA) + `"}`
	bodyB := `{"key_hash":"` + testHashOf(secretB) + `"}`

	var wg sync.WaitGroup
	codes := make([]int, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		codes[0] = doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-race", fakeAdminCredential(), bodyA).Code
	}()
	go func() {
		defer wg.Done()
		codes[1] = doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-race", fakeAdminCredential(), bodyB).Code
	}()
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusNoContent {
			t.Errorf("upsert call %d status = %d, want 204", i, code)
		}
	}

	vk, ok := pipeline.GetVirtualKey("team-race")
	if !ok {
		t.Fatal("team-race is missing after concurrent upsert calls")
	}
	hashA, hashB := testHashOf(secretA), testHashOf(secretB)
	if vk.KeyHash != hashA && vk.KeyHash != hashB {
		t.Fatalf("final KeyHash %q matches NEITHER request body's hash — state corrupted, not a merge of both", vk.KeyHash)
	}
}

// TestConcurrentDeleteVirtualKeyRequestsForSameNameNeverBothSucceed
// proves the delete side: exactly one of two concurrent DELETE calls for
// the same name must succeed (204), the other must correctly observe the
// key already gone (404), and the key must be genuinely gone afterward.
func TestConcurrentDeleteVirtualKeyRequestsForSameNameNeverBothSucceed(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger())

	// A second key is required -- DeleteVirtualKey rejects deleting the
	// only remaining virtual key.
	otherBody := `{"key_hash":"` + testHashOf("team-race-sibling-secret") + `"}`
	doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-race-sibling", fakeAdminCredential(), otherBody)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		codes[0] = doRequest(t, h, http.MethodDelete, "/admin/virtual_keys/test-key", fakeAdminCredential(), "").Code
	}()
	go func() {
		defer wg.Done()
		codes[1] = doRequest(t, h, http.MethodDelete, "/admin/virtual_keys/test-key", fakeAdminCredential(), "").Code
	}()
	wg.Wait()

	var successCount int
	for _, code := range codes {
		switch code {
		case http.StatusNoContent:
			successCount++
		case http.StatusNotFound:
			// Expected for whichever call lost the race against the
			// other's already-committed deletion.
		default:
			t.Errorf("delete call status = %d, want 204 or 404", code)
		}
	}
	if successCount != 1 {
		t.Errorf("successCount = %d, want exactly 1 — both concurrent deletes must never both report success", successCount)
	}

	if _, ok := pipeline.GetVirtualKey("test-key"); ok {
		t.Fatal("test-key is still present after concurrent delete calls")
	}
}

// TestDeleteVirtualKeyViaHTTPAlsoErasesItsBudgetSpend is the real
// behavioral proof of the budget-data-erasure fix: a deleted key's real,
// non-zero recorded spend must not survive the deletion — for GDPR/CCPA
// erasure-request handling, per
// docs/upgrade-research/data-retention-right-to-erasure-2026-09-15.md.
// Builds its own pipeline (rather than newTestPipeline, whose empty
// price table would make every request cost exactly $0, proving
// nothing) with a real, non-zero completion-token price so a genuine
// bill is recorded before deletion.
func TestDeleteVirtualKeyViaHTTPAlsoErasesItsBudgetSpend(t *testing.T) {
	// A second key is required alongside "test-key" -- DeleteVirtualKey
	// (dataplane.go) rejects deleting the only remaining virtual key
	// (identity.NewVerifier's own "at least one key required" rule), and
	// this test's whole point is proving the delete itself succeeds.
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
		{ID: "other-key", KeyHash: testHashOf("other-key"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []dataplane.Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	pipeline, err := dataplane.NewPipeline(dataplane.Config{
		Verifier: verifier,
		Limiter: ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{
			{ID: "test-key", Capacity: 100, RefillPerSecond: 100},
			{ID: "other-key", Capacity: 100, RefillPerSecond: 100},
		}),
		Budget:      budget.NewTracker(),
		Cache:       inprocess.New(0),
		CacheL2:     inprocess.New(0),
		CacheL3:     inprocess.NewLexicalCache(0),
		Guardrails:  guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:    adapter.Registry{"openai": openai.New()},
		Router:      router.New([]router.Deployment{{Name: "d1", Model: "gpt-4o"}}, router.HealthConfig{}),
		Deployments: deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{
			"gpt-4o": {CompletionPerToken: decimal.NewFromFloat(0.01)},
		}),
		Upstream: func(ctx context.Context, dep dataplane.Deployment, req any) (any, error) {
			return &openai.Response{
				ID: "chatcmpl-fake", Model: dep.UpstreamModel,
				Choices: []openai.Choice{{Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"hi"`)}, FinishReason: "stop"}},
				Usage:   openai.Usage{PromptTokens: 1, CompletionTokens: 5, TotalTokens: 6},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	if _, err := pipeline.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if spent := pipeline.SpentUSD("test-key", 0); spent.IsZero() {
		t.Fatal("setup: SpentUSD is 0 after a real request — nothing was billed to erase")
	}

	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger())
	rec := doRequest(t, h, http.MethodDelete, "/admin/virtual_keys/test-key", fakeAdminCredential(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	if spent := pipeline.SpentUSD("test-key", 0); !spent.IsZero() {
		t.Errorf("SpentUSD after DELETE = %s, want 0 — the deleted key's real spend must not survive deletion", spent)
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

// TestUpsertPromptCreatesVersionOneThenVersionTwo proves POST
// /admin/prompts/{id} auto-bumps the version on a repeat upsert, per
// prompt.Store.Upsert's own doc comment, and returns the created
// version in its response body.
func TestUpsertPromptCreatesVersionOneThenVersionTwo(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	body := `{"messages":[{"role":"system","content":"You are {{persona}}."}]}`
	rec := doRequest(t, h, http.MethodPost, "/admin/prompts/greeting", fakeAdminCredential(), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("first POST status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var first promptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if first.ID != "greeting" || first.Version != 1 {
		t.Errorf("first upsert: ID=%q Version=%d, want ID=greeting Version=1", first.ID, first.Version)
	}

	rec = doRequest(t, h, http.MethodPost, "/admin/prompts/greeting", fakeAdminCredential(), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("second POST status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var second promptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if second.Version != 2 {
		t.Errorf("second upsert: Version = %d, want 2", second.Version)
	}
}

// TestUpsertPromptRejectsOversizedID is the regression proof for the
// real bug fixed in maxAdminIdentifierLen's own doc comment: this
// handler previously validated neither length nor character content on
// the id path parameter at all before it became a map key in
// prompt.Store and every future log line referencing it.
func TestUpsertPromptRejectsOversizedID(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())
	oversizedID := strings.Repeat("a", maxAdminIdentifierLen+1)
	rec := doRequest(t, h, http.MethodPost, "/admin/prompts/"+oversizedID, fakeAdminCredential(), `{"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

func TestUpsertPromptRejectsEmptyMessages(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())
	rec := doRequest(t, h, http.MethodPost, "/admin/prompts/greeting", fakeAdminCredential(), `{"messages":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

// TestUpsertPromptRejectsMalformedJSONBody mirrors
// TestUpsertVirtualKeyRejectsMalformedJSONBody's own proof for this
// handler's identical json.Decode error path.
func TestUpsertPromptRejectsMalformedJSONBody(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())
	rec := doRequest(t, h, http.MethodPost, "/admin/prompts/greeting", fakeAdminCredential(), `{not valid json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

// TestUpsertPromptRejectsMIMESpoofedContent is the write-time defense-in-
// depth half of a round-3 backlog-audit finding: a prompt author's own
// inline Parts[].Data/MediaType is validated at write time, the same
// declared-vs-detected MIME-spoof check every directly-client-supplied
// message must already pass, per adapter.ValidateContentParts. Fails
// fast for the prompt author rather than only ever being caught later,
// at every future request-time resolution of this same prompt.
func TestUpsertPromptRejectsMIMESpoofedContent(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	spoofedData := base64.StdEncoding.EncodeToString([]byte("this is plain text, not an image"))
	body := `{"messages":[{"role":"user","content":"here's an image","parts":[{"type":"image","media_type":"image/png","data":"` + spoofedData + `"}]}]}`
	rec := doRequest(t, h, http.MethodPost, "/admin/prompts/mime-spoofed", fakeAdminCredential(), body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

// TestGetPromptRoutesReturnLatestSpecificVersionAndNotFound exercises
// all three GET routes against the same upserted prompt.
func TestGetPromptRoutesReturnLatestSpecificVersionAndNotFound(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	doRequest(t, h, http.MethodPost, "/admin/prompts/greeting", fakeAdminCredential(), `{"messages":[{"role":"user","content":"v1"}]}`)
	doRequest(t, h, http.MethodPost, "/admin/prompts/greeting", fakeAdminCredential(), `{"messages":[{"role":"user","content":"v2"}]}`)

	rec := doRequest(t, h, http.MethodGet, "/admin/prompts/greeting", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET latest status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var latest promptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &latest); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if latest.Version != 2 || latest.Messages[0].Content != "v2" {
		t.Errorf("GET latest = %+v, want version 2 with content v2", latest)
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/prompts/greeting/versions/1", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET version 1 status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var pinned promptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pinned); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if pinned.Version != 1 || pinned.Messages[0].Content != "v1" {
		t.Errorf("GET version 1 = %+v, want version 1 with content v1", pinned)
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/prompts/greeting/versions/99", fakeAdminCredential(), "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET unknown version status = %d, want 404", rec.Code)
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/prompts/does-not-exist", fakeAdminCredential(), "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET unknown id status = %d, want 404", rec.Code)
	}
}

// TestGetPromptVersionRejectsNonNumericAndNonPositiveVersion proves the
// version path parameter's own validation (getPromptVersionHandler's
// strconv.Atoi + version <= 0 check) against every malformed shape --
// never previously exercised, though already correct.
func TestGetPromptVersionRejectsNonNumericAndNonPositiveVersion(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())
	doRequest(t, h, http.MethodPost, "/admin/prompts/greeting", fakeAdminCredential(), `{"messages":[{"role":"user","content":"v1"}]}`)

	cases := []string{"abc", "0", "-1", "99999999999999999999"}
	for _, version := range cases {
		t.Run(version, func(t *testing.T) {
			rec := doRequest(t, h, http.MethodGet, "/admin/prompts/greeting/versions/"+version, fakeAdminCredential(), "")
			if rec.Code != http.StatusBadRequest {
				t.Errorf("GET .../versions/%s: status = %d, want 400, body: %s", version, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestListPromptsReturnsLatestVersionOfEveryPromptSortedByID(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	doRequest(t, h, http.MethodPost, "/admin/prompts/zeta", fakeAdminCredential(), `{"messages":[{"role":"user","content":"z"}]}`)
	doRequest(t, h, http.MethodPost, "/admin/prompts/alpha", fakeAdminCredential(), `{"messages":[{"role":"user","content":"a1"}]}`)
	doRequest(t, h, http.MethodPost, "/admin/prompts/alpha", fakeAdminCredential(), `{"messages":[{"role":"user","content":"a2"}]}`)

	rec := doRequest(t, h, http.MethodGet, "/admin/prompts", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var list []promptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List returned %d entries, want 2", len(list))
	}
	if list[0].ID != "alpha" || list[1].ID != "zeta" {
		t.Errorf("order = [%s, %s], want [alpha, zeta]", list[0].ID, list[1].ID)
	}
	if list[0].Version != 2 {
		t.Errorf("alpha's Version = %d, want 2 (the latest)", list[0].Version)
	}
}

func TestDeletePromptRemovesItAndUnknownIDReturns404(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	doRequest(t, h, http.MethodPost, "/admin/prompts/greeting", fakeAdminCredential(), `{"messages":[{"role":"user","content":"hi"}]}`)

	rec := doRequest(t, h, http.MethodDelete, "/admin/prompts/greeting", fakeAdminCredential(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/prompts/greeting", fakeAdminCredential(), "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE status = %d, want 404", rec.Code)
	}

	rec = doRequest(t, h, http.MethodDelete, "/admin/prompts/does-not-exist", fakeAdminCredential(), "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown id status = %d, want 404", rec.Code)
	}
}

// TestViewerTokenCanReadPromptsButNotMutateThem mirrors
// TestViewerTokenCanReadConfigButNotMutateVirtualKeys for the new prompt
// routes -- viewer authenticates every GET but is rejected (401, this
// codebase's existing viewer-role-rejection status) on POST/DELETE.
func TestViewerTokenCanReadPromptsButNotMutateThem(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential(), Viewer: fakeViewerCredential()}, discardLogger())

	doRequest(t, h, http.MethodPost, "/admin/prompts/greeting", fakeAdminCredential(), `{"messages":[{"role":"user","content":"hi"}]}`)

	rec := doRequest(t, h, http.MethodGet, "/admin/prompts", fakeViewerCredential(), "")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /admin/prompts with the viewer credential: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/prompts/greeting", fakeViewerCredential(), "")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /admin/prompts/greeting with the viewer credential: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/prompts/greeting/versions/1", fakeViewerCredential(), "")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /admin/prompts/greeting/versions/1 with the viewer credential: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, h, http.MethodPost, "/admin/prompts/greeting", fakeViewerCredential(), `{"messages":[{"role":"user","content":"nope"}]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /admin/prompts with the viewer credential: status = %d, want 401 — viewer must never authenticate a write route", rec.Code)
	}
	rec = doRequest(t, h, http.MethodDelete, "/admin/prompts/greeting", fakeViewerCredential(), "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("DELETE /admin/prompts with the viewer credential: status = %d, want 401 — viewer must never authenticate a write route", rec.Code)
	}
}

// TestUpsertPromptLogsAnAuditEntryWithoutLeakingContent mirrors
// TestUpsertVirtualKeyLogsAnAuditEntryWithoutLeakingTheSecret: logs the
// prompt ID and resulting version, never the message content.
func TestUpsertPromptLogsAnAuditEntryWithoutLeakingContent(t *testing.T) {
	logger, buf := capturingLogger()
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, logger)

	const secretLookingContent = "the-eagle-has-landed-42"
	body := `{"messages":[{"role":"system","content":"` + secretLookingContent + `"}]}`
	rec := doRequest(t, h, http.MethodPost, "/admin/prompts/greeting", fakeAdminCredential(), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "admin_prompt_upserted") || !strings.Contains(logOutput, "id=greeting") || !strings.Contains(logOutput, "version=1") {
		t.Errorf("expected an admin_prompt_upserted audit-log entry naming greeting/version 1; got: %s", logOutput)
	}
	if strings.Contains(logOutput, secretLookingContent) {
		t.Errorf("audit log must never contain prompt message content; got: %s", logOutput)
	}
}

// TestDeletePromptLogsAnAuditEntry mirrors
// TestDeleteVirtualKeyLogsAnAuditEntryWithoutLeakingTheSecret.
func TestDeletePromptLogsAnAuditEntry(t *testing.T) {
	logger, buf := capturingLogger()
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, logger)

	doRequest(t, h, http.MethodPost, "/admin/prompts/greeting", fakeAdminCredential(), `{"messages":[{"role":"user","content":"hi"}]}`)
	rec := doRequest(t, h, http.MethodDelete, "/admin/prompts/greeting", fakeAdminCredential(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "admin_prompt_deleted") || !strings.Contains(logOutput, "id=greeting") {
		t.Errorf("expected an admin_prompt_deleted audit-log entry naming greeting; got: %s", logOutput)
	}
}

// TestGetConfigLogsAnAuditEntryNamingTheCredentialTier is a round-3
// backlog-audit finding: GET /admin/config previously logged nothing at
// all, even though every WRITE route already did — a leaked/misused
// admin OR viewer credential could read the full deployment topology,
// price table, and every virtual key's budget/rate-limit/allowed-models
// shape with zero trace an operator could ever detect afterward. Proves
// both the admin and viewer tiers each produce a distinguishable
// authorized_by value, per requireEitherBearerToken's own
// contextWithCredentialTier plumbing.
func TestGetConfigLogsAnAuditEntryNamingTheCredentialTier(t *testing.T) {
	logger, buf := capturingLogger()
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential(), Viewer: fakeViewerCredential()}, logger)

	rec := doRequest(t, h, http.MethodGet, "/admin/config", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config with the admin credential: status = %d, want 200", rec.Code)
	}
	if logOutput := buf.String(); !strings.Contains(logOutput, "admin_config_read") || !strings.Contains(logOutput, "authorized_by=admin") {
		t.Errorf("expected an admin_config_read entry with authorized_by=admin; got: %s", logOutput)
	}

	buf.Reset()
	rec = doRequest(t, h, http.MethodGet, "/admin/config", fakeViewerCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config with the viewer credential: status = %d, want 200", rec.Code)
	}
	if logOutput := buf.String(); !strings.Contains(logOutput, "admin_config_read") || !strings.Contains(logOutput, "authorized_by=viewer") {
		t.Errorf("expected an admin_config_read entry with authorized_by=viewer; got: %s", logOutput)
	}
}

// TestPromptReadRoutesLogAnAuditEntryWithoutLeakingContent covers all 3
// prompt-read routes (list/get-latest/get-version) — the same real gap
// as GET /admin/config, for the prompt-management surface.
func TestPromptReadRoutesLogAnAuditEntryWithoutLeakingContent(t *testing.T) {
	logger, buf := capturingLogger()
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, logger)

	const secretLookingContent = "the-eagle-has-landed-42"
	doRequest(t, h, http.MethodPost, "/admin/prompts/greeting", fakeAdminCredential(), `{"messages":[{"role":"system","content":"`+secretLookingContent+`"}]}`)
	buf.Reset()

	rec := doRequest(t, h, http.MethodGet, "/admin/prompts", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/prompts: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if logOutput := buf.String(); !strings.Contains(logOutput, "admin_prompts_read") || !strings.Contains(logOutput, "authorized_by=admin") {
		t.Errorf("expected an admin_prompts_read entry with authorized_by=admin after GET /admin/prompts; got: %s", logOutput)
	}

	buf.Reset()
	rec = doRequest(t, h, http.MethodGet, "/admin/prompts/greeting", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/prompts/greeting: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if logOutput := buf.String(); !strings.Contains(logOutput, "admin_prompts_read") || !strings.Contains(logOutput, "id=greeting") {
		t.Errorf("expected an admin_prompts_read entry naming greeting after GET /admin/prompts/greeting; got: %s", logOutput)
	}

	buf.Reset()
	rec = doRequest(t, h, http.MethodGet, "/admin/prompts/greeting/versions/1", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/prompts/greeting/versions/1: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if logOutput := buf.String(); !strings.Contains(logOutput, "admin_prompts_read") || !strings.Contains(logOutput, "id=greeting") || !strings.Contains(logOutput, "version=1") {
		t.Errorf("expected an admin_prompts_read entry naming greeting/version 1; got: %s", logOutput)
	}
	if strings.Contains(buf.String(), secretLookingContent) {
		t.Errorf("audit log must never contain prompt message content; got: %s", buf.String())
	}
}

// pprofEnabledConfig returns testConfig() with Admin.EnablePprof set --
// a small variant, not a second full config builder, since every other
// field stays identical to testConfig()'s own baseline.
func pprofEnabledConfig() *controlplane.Config {
	cfg := testConfig()
	cfg.Admin.EnablePprof = true
	return cfg
}

// TestPprofDisabledByDefaultReturns404 proves the off-by-default
// contract: testConfig() never sets EnablePprof, so no pprof route is
// registered on the mux at all -- ServeMux returns a plain 404, not a
// 401, since the route itself does not exist to be unauthorized against.
func TestPprofDisabledByDefaultReturns404(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	rec := doRequest(t, h, http.MethodGet, "/admin/debug/pprof/", fakeAdminCredential(), "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /admin/debug/pprof/ with pprof disabled: status = %d, want 404", rec.Code)
	}
}

// TestPprofEnabledRequiresAdminCredential proves pprof, once enabled,
// still goes through the same bearer-token gate as every other admin
// route -- missing or wrong credential is rejected before pprof.Index
// ever runs.
func TestPprofEnabledRequiresAdminCredential(t *testing.T) {
	h := Handler(pprofEnabledConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	cases := []struct {
		name        string
		bearerValue string
	}{
		{"missing header entirely", ""},
		{"wrong value", "wrong-value-entirely"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := doRequest(t, h, http.MethodGet, "/admin/debug/pprof/", c.bearerValue, "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("GET /admin/debug/pprof/ with %s: status = %d, want 401", c.name, rec.Code)
			}
		})
	}
}

// TestPprofEnabledViewerCredentialRejected proves the viewer tier can
// never authenticate a pprof route -- profiling data is a stronger
// information-disclosure/DoS-surface signal than anything the read-only
// viewer tier exposes elsewhere on this mux, so pprof always requires
// creds.Admin specifically, exactly like the write routes.
func TestPprofEnabledViewerCredentialRejected(t *testing.T) {
	h := Handler(pprofEnabledConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential(), Viewer: "a-viewer-credential"}, discardLogger())

	rec := doRequest(t, h, http.MethodGet, "/admin/debug/pprof/", "a-viewer-credential", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /admin/debug/pprof/ with the viewer credential: status = %d, want 401", rec.Code)
	}
}

// TestPprofEnabledWithAdminCredentialServesRealProfilingData proves the
// happy path end to end: once enabled and authenticated as admin, the
// index route serves pprof's own real output, and a named profile route
// (goroutine) serves a real profile, not a 404/501 stub.
func TestPprofEnabledWithAdminCredentialServesRealProfilingData(t *testing.T) {
	h := Handler(pprofEnabledConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	rec := doRequest(t, h, http.MethodGet, "/admin/debug/pprof/", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/debug/pprof/: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Types of profiles available") {
		t.Errorf("GET /admin/debug/pprof/ body does not look like pprof.Index's real output: %s", rec.Body.String())
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/debug/pprof/goroutine", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/debug/pprof/goroutine: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		t.Error("GET /admin/debug/pprof/goroutine returned an empty body, want a real goroutine profile")
	}
}

// TestBackupRouteDisabledReturns501WhenNoBackupDirConfigured proves the
// common no-persistence case is a clear, distinguishable 501 -- never a
// bare 404 indistinguishable from a typo'd path, and never a 200 that
// silently did nothing.
func TestBackupRouteDisabledReturns501WhenNoBackupDirConfigured(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger())

	rec := doRequest(t, h, http.MethodPost, "/admin/backup", fakeAdminCredential(), "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("POST /admin/backup with no backup_dir configured: status = %d, want 501, body: %s", rec.Code, rec.Body.String())
	}
}

// TestBackupRouteRequiresAdminCredential proves this write-shaped,
// disk-touching route is gated exactly like every other write route on
// this mux -- admin-only, never viewer or cost_viewer.
func TestBackupRouteRequiresAdminCredential(t *testing.T) {
	rec := doRequest(t, Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger()),
		http.MethodPost, "/admin/backup", "wrong-value-entirely", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /admin/backup with a wrong credential: status = %d, want 401", rec.Code)
	}
}

// TestBackupRouteWritesTimestampedFilesForEachConfiguredStore is the
// real behavioral proof: a genuinely bbolt-backed store (identity, wired
// via a real identityboltstore.Store, unlike newTestPipeline's own
// in-memory-only default) produces a real, independently-openable backup
// file in the configured directory.
func TestBackupRouteWritesTimestampedFilesForEachConfiguredStore(t *testing.T) {
	identityPath := filepath.Join(t.TempDir(), "identity.db")
	identityStore, err := identityboltstore.Open(identityPath)
	if err != nil {
		t.Fatalf("identityboltstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = identityStore.Close() })

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
	pipeline, err := dataplane.NewPipeline(dataplane.Config{
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
		IdentityStore:  identityStore,
		Upstream: func(ctx context.Context, dep dataplane.Deployment, req any) (any, error) {
			return &openai.Response{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	backupDir := t.TempDir()
	cfg := testConfig()
	cfg.Admin.BackupDir = backupDir
	h := Handler(cfg, pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger())

	rec := doRequest(t, h, http.MethodPost, "/admin/backup", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /admin/backup: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var resp backupResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(resp.Files) != 1 || !strings.HasPrefix(resp.Files[0], "identity-") {
		t.Fatalf("resp.Files = %v, want exactly one identity-prefixed filename (budget/prompt are in-memory-only in this test, correctly skipped)", resp.Files)
	}

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("reading backup dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("backup dir contains %d entries, want exactly 1", len(entries))
	}
}

// TestBackupRouteReturns500WhenBackupDirIsUnwritable is the regression
// proof for a live 2026-09-16 adversarial-audit finding: none of this
// file's 3 existing backup-route tests ever drove backupHandler's own
// err != nil -> 500 branch — only the success path (writable dir) and
// the two auth/config-gate short-circuits were covered. A configured but
// unwritable BackupDir is the real, reachable failure this branch exists
// for (e.g. a disk went read-only, or an operator misconfigured
// permissions), and must surface as a genuine 500 with the real error in
// the body, never a silent partial success.
func TestBackupRouteReturns500WhenBackupDirIsUnwritable(t *testing.T) {
	identityPath := filepath.Join(t.TempDir(), "identity.db")
	identityStore, err := identityboltstore.Open(identityPath)
	if err != nil {
		t.Fatalf("identityboltstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = identityStore.Close() })

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
	pipeline, err := dataplane.NewPipeline(dataplane.Config{
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
		IdentityStore:  identityStore,
		Upstream: func(ctx context.Context, dep dataplane.Deployment, req any) (any, error) {
			return &openai.Response{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	backupDir := t.TempDir()
	if err := os.Chmod(backupDir, 0o500); err != nil {
		t.Fatalf("os.Chmod(backupDir, 0o500): %v", err)
	}
	// Restore write permission BEFORE t.TempDir()'s own cleanup runs, or
	// that cleanup itself fails to remove the directory.
	t.Cleanup(func() { _ = os.Chmod(backupDir, 0o700) })

	cfg := testConfig()
	cfg.Admin.BackupDir = backupDir
	h := Handler(cfg, pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger())

	rec := doRequest(t, h, http.MethodPost, "/admin/backup", fakeAdminCredential(), "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST /admin/backup with an unwritable BackupDir: status = %d, want 500, body: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		t.Error("500 response body is empty, want the real underlying error")
	}
}

// TestUpdateDeploymentWeightViaHTTPChangesLiveRouting is the real
// behavioral proof, mirroring
// TestUpsertVirtualKeyViaHTTPMakesTheKeyImmediatelyUsable's own "HTTP
// mutation, then a real pipeline call observes the effect" pattern: a
// weight change via this route must actually shift which deployment
// router.Router.Select picks, not just return 204. Each call uses a
// distinct message body so every one is a genuine cache MISS (an
// identical repeated body would cache-hit after the first call and never
// re-select a deployment at all).
func TestUpdateDeploymentWeightViaHTTPChangesLiveRouting(t *testing.T) {
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []dataplane.Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "d2", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	pipeline, err := dataplane.NewPipeline(dataplane.Config{
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
		Router:         router.New([]router.Deployment{{Name: "d1", Model: "gpt-4o"}, {Name: "d2", Model: "gpt-4o"}}, router.HealthConfig{}),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		// The response CONTENT (never resp.Model -- callDeployment always
		// echoes back the client's own requested model there, per
		// finalize's own doc comment on realServingModel vs resp.Model)
		// encodes dep.Name -- the only way this test can tell which of
		// the two deployments actually served a given call.
		Upstream: func(ctx context.Context, dep dataplane.Deployment, req any) (any, error) {
			return &openai.Response{
				ID: "chatcmpl-" + dep.Name, Model: dep.Name,
				Choices: []openai.Choice{{Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"` + dep.Name + `"`)}, FinishReason: "stop"}},
				Usage:   openai.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger())

	rec := doRequest(t, h, http.MethodPost, "/admin/deployments/d1/weight", fakeAdminCredential(), `{"weight":3}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /admin/deployments/d1/weight: status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	want := []string{"d1", "d1", "d1", "d2"}
	for i, w := range want {
		req := adapter.ChatRequest{
			Model:    "gpt-4o",
			Messages: []adapter.Message{{Role: "user", Content: fmt.Sprintf("call %d", i)}},
		}
		resp, err := pipeline.HandleChatCompletion(context.Background(), "Bearer test-key", req, "")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		got := resp.Choices[0].Message.Content
		if got != w {
			t.Fatalf("call %d served by %q, want %q (full wanted 3:1 sequence: %v) — the weight update did not change live routing", i, got, w, want)
		}
	}
}

// TestUpdateDeploymentWeightRequiresAdminCredential proves this
// write-shaped, live-routing-mutating route is gated exactly like every
// other write route on this mux — admin-only.
func TestUpdateDeploymentWeightRequiresAdminCredential(t *testing.T) {
	rec := doRequest(t, Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger()),
		http.MethodPost, "/admin/deployments/d1/weight", "wrong-value-entirely", `{"weight":3}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /admin/deployments/d1/weight with a wrong credential: status = %d, want 401", rec.Code)
	}
}

// TestUpdateDeploymentWeightRejectsNegativeWeight, ...MalformedBody, and
// ...UnknownDeployment are the regression proof for a live 2026-09-16
// adversarial-audit finding: updateDeploymentWeightHandler has 3 named
// error branches (negative weight -> 400, invalid JSON body -> 400,
// unknown deployment -> 404), and this file's only 2 pre-existing tests
// for this route covered only the success path and the auth gate — none
// of these 3 branches had ever been exercised at the HTTP layer.
func TestUpdateDeploymentWeightRejectsNegativeWeight(t *testing.T) {
	rec := doRequest(t, Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger()),
		http.MethodPost, "/admin/deployments/d1/weight", fakeAdminCredential(), `{"weight":-1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /admin/deployments/d1/weight with weight=-1: status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateDeploymentWeightRejectsMalformedBody(t *testing.T) {
	rec := doRequest(t, Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger()),
		http.MethodPost, "/admin/deployments/d1/weight", fakeAdminCredential(), `{not valid json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /admin/deployments/d1/weight with a malformed body: status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateDeploymentWeightRejectsUnknownDeployment(t *testing.T) {
	rec := doRequest(t, Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger()),
		http.MethodPost, "/admin/deployments/does-not-exist/weight", fakeAdminCredential(), `{"weight":3}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /admin/deployments/does-not-exist/weight: status = %d, want 404, body: %s", rec.Code, rec.Body.String())
	}
}
