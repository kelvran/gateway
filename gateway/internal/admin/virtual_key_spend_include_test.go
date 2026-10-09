package admin

// GET /admin/virtual_keys?include=spend and GET /admin/deployments (RFC-3
// decision 5, slice (c)): spend joined into the list entries from the same
// read the per-key route performs, a `spend_unavailable` fail-open shape on
// both spend routes when the budget backend errors, the audit field, the
// two list-entry fields that make `keys create --replace` safe, and a
// read-only deployments route with live router state.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/adminapi"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/router"
)

// failingBudgetBackend is a budget.RedisBackend whose spend read fails,
// so the fail-open shape of both spend routes can be driven without Redis.
type failingBudgetBackend struct{ err error }

func (f *failingBudgetBackend) Reserve(context.Context, string, int64, int64) (bool, int64, int64, error) {
	return true, 0, 0, nil
}
func (f *failingBudgetBackend) ReserveFixed(context.Context, string, int64, int64, int64) (bool, error) {
	return true, nil
}
func (f *failingBudgetBackend) Adjust(context.Context, string, int64, int64) error { return nil }
func (f *failingBudgetBackend) SpentNanoUSD(context.Context, string) (int64, error) {
	return 0, f.err
}
func (f *failingBudgetBackend) MarkAlertBucket(context.Context, string, float64, int64) (bool, error) {
	return false, nil
}
func (f *failingBudgetBackend) MarkWarnAlerted(context.Context, string, int64) (bool, error) {
	return false, nil
}
func (f *failingBudgetBackend) Delete(context.Context, string) error { return nil }
func (f *failingBudgetBackend) Close() error                         { return nil }

// newSpendTestPipeline mirrors newTestPipeline with a priced gpt-4o (so a
// request records real spend), caller-supplied keys and budget tracker, and
// returns the router so tests can drive probe results and weights.
func newSpendTestPipeline(t *testing.T, keys []identity.VirtualKey, tracker *budget.Tracker) (*dataplane.Pipeline, *router.Router) {
	t.Helper()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	keyConfigs := make([]ratelimit.KeyConfig, 0, len(keys))
	for _, k := range keys {
		keyConfigs = append(keyConfigs, ratelimit.KeyConfig{ID: k.ID, Capacity: 100, RefillPerSecond: 100})
	}
	deployments := []dataplane.Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "d2", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o-mini", BaseURL: "http://unused"},
	}
	rt := router.New([]router.Deployment{{Name: "d1", Model: "gpt-4o", Weight: 2, Sticky: true}, {Name: "d2", Model: "gpt-4o"}}, router.HealthConfig{})
	p, err := dataplane.NewPipeline(dataplane.Config{
		Verifier:    verifier,
		Limiter:     ratelimit.NewInMemoryKeyLimiter(keyConfigs),
		Budget:      tracker,
		Cache:       inprocess.New(0),
		CacheL2:     inprocess.New(0),
		CacheL3:     inprocess.NewLexicalCache(0),
		Guardrails:  guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:    adapter.Registry{"openai": openai.New()},
		Router:      rt,
		Deployments: deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{
			"gpt-4o": {PromptPerToken: decimal.RequireFromString("0.5"), CompletionPerToken: decimal.RequireFromString("0.5")},
		}),
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
	return p, rt
}

func spendTestKeys() []identity.VirtualKey {
	return []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), BudgetUSD: decimal.RequireFromString("10"), RateLimitBurst: 100, RateLimitRefill: 100, MaxConcurrentRequests: 4},
		{ID: "idle-key", KeyHash: testHashOf("idle-key"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
}

func decodeEntries(t *testing.T, body string) map[string]adminapi.VirtualKeyListEntry {
	t.Helper()
	var entries []adminapi.VirtualKeyListEntry
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		t.Fatalf("decoding list: %v (%s)", err, body)
	}
	out := map[string]adminapi.VirtualKeyListEntry{}
	for _, e := range entries {
		out[e.ID] = e
	}
	return out
}

func TestListVirtualKeysIncludeSpendMatchesThePerKeyRouteAndIsAbsentOtherwise(t *testing.T) {
	pipeline, _ := newSpendTestPipeline(t, spendTestKeys(), budget.NewTracker())
	logger, logBuf := capturingLogger()
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential(), Viewer: fakeViewerCredential(), CostViewer: fakeCostViewerCredential()}, logger, nil)
	if _, err := pipeline.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}, ""); err != nil {
		t.Fatalf("priming request: %v", err)
	}

	// Without the query: no spend fields, no include audit field.
	rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys", fakeViewerCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET list: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "spent_usd") || strings.Contains(rec.Body.String(), "percent_used") || strings.Contains(rec.Body.String(), "spend_unavailable") {
		t.Errorf("list without include=spend must carry no spend fields: %s", rec.Body.String())
	}
	if strings.Contains(logBuf.String(), "include=") {
		t.Errorf("audit line without the query must not carry include: %s", logBuf.String())
	}

	// With the query: the same figures the per-key route serves.
	rec = doRequest(t, h, http.MethodGet, "/admin/virtual_keys?include=spend", fakeViewerCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET list include=spend: %d %s", rec.Code, rec.Body.String())
	}
	entries := decodeEntries(t, rec.Body.String())
	perKey := doRequest(t, h, http.MethodGet, "/admin/virtual_keys/test-key/spend", fakeViewerCredential(), "")
	var spend adminapi.VirtualKeySpendResponse
	if err := json.Unmarshal(perKey.Body.Bytes(), &spend); err != nil {
		t.Fatalf("decoding per-key spend: %v", err)
	}
	if spend.SpentUSD == "" || spend.SpentUSD == "0" || spend.PercentUsed == nil || *spend.PercentUsed <= 0 {
		t.Fatalf("the priming request must have recorded spend; per-key = %+v", spend)
	}
	tk := entries["test-key"]
	if tk.SpentUSD != spend.SpentUSD || tk.PercentUsed == nil || *tk.PercentUsed != *spend.PercentUsed || tk.SpendUnavailable {
		t.Errorf("list entry spend %+v != per-key route %+v", tk, spend)
	}
	idle := entries["idle-key"]
	if idle.SpentUSD != "0" || idle.PercentUsed == nil || *idle.PercentUsed != 0 {
		t.Errorf("an idle, unbudgeted key must report spent_usd \"0\" and percent_used 0 (present, not omitted): %+v", idle)
	}
	if tk.MaxConcurrentRequests != 4 {
		t.Errorf("max_concurrent_requests from the configured key = %d, want 4", tk.MaxConcurrentRequests)
	}
	if !strings.Contains(rec.Body.String(), `"max_concurrent_requests":4`) || strings.Contains(rec.Body.String(), `"max_concurrent_requests":0`) {
		t.Errorf("max_concurrent_requests must be present for test-key and omitted for idle-key: %s", rec.Body.String())
	}
	if !strings.Contains(logBuf.String(), "include=spend") || !strings.Contains(logBuf.String(), "admin_virtual_keys_read") {
		t.Errorf("audit line for ?include=spend must carry include=spend: %s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "spent_usd") || strings.Contains(logBuf.String(), "percent_used") {
		t.Errorf("spend figures must never be logged, only the include marker: %s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "admin_spend_read_failed") {
		t.Errorf("no warning when every read succeeds: %s", logBuf.String())
	}

	// Tier: CostViewer still cannot list, with or without the query.
	for _, path := range []string{"/admin/virtual_keys", "/admin/virtual_keys?include=spend"} {
		if rec := doRequest(t, h, http.MethodGet, path, fakeCostViewerCredential(), ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("CostViewer GET %s = %d, want 401", path, rec.Code)
		}
	}
	// An unrecognised include value is a 400, not a silent full list.
	rec = doRequest(t, h, http.MethodGet, "/admin/virtual_keys?include=spent", fakeAdminCredential(), "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `include must be "spend"`) {
		t.Errorf("include=spent: %d %q, want 400 naming the one accepted value", rec.Code, rec.Body.String())
	}
}

func TestSpendRoutesReportSpendUnavailableWhenTheBudgetBackendFails(t *testing.T) {
	tracker := budget.NewRedisTracker(&failingBudgetBackend{err: errors.New("redis: i/o timeout")}, discardLogger())
	pipeline, _ := newSpendTestPipeline(t, spendTestKeys(), tracker)
	logger, logBuf := capturingLogger()
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, logger, nil)

	rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys?include=spend", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list with a failing backend must stay 200: %d %s", rec.Code, rec.Body.String())
	}
	entries := decodeEntries(t, rec.Body.String())
	if len(entries) != 2 {
		t.Fatalf("every key must still be listed, got %d", len(entries))
	}
	for id, e := range entries {
		if !e.SpendUnavailable || e.SpentUSD != "" || e.PercentUsed != nil {
			t.Errorf("%s: want spend_unavailable:true and no spent_usd/percent_used, got %+v", id, e)
		}
	}
	if strings.Contains(rec.Body.String(), `"spent_usd"`) || strings.Contains(rec.Body.String(), `"percent_used"`) {
		t.Errorf("a false zero must not be on the wire: %s", rec.Body.String())
	}
	// The failure is never silent: one Warn per request (not per key) with
	// the count and the first error, and still no figure.
	if strings.Count(logBuf.String(), "admin_spend_read_failed") != 1 || !strings.Contains(logBuf.String(), "count=2") || !strings.Contains(logBuf.String(), "redis: i/o timeout") {
		t.Errorf("list with a failing backend must warn exactly once with count=2 and the backend error: %s", logBuf.String())
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/virtual_keys/test-key/spend", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("per-key spend with a failing backend must stay 200: %d %s", rec.Code, rec.Body.String())
	}
	var spend adminapi.VirtualKeySpendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &spend); err != nil {
		t.Fatal(err)
	}
	if !spend.SpendUnavailable || spend.SpentUSD != "" || spend.PercentUsed != nil || spend.BudgetUSD != "10" {
		t.Errorf("per-key fail-open shape: %+v, want spend_unavailable with budget fields intact", spend)
	}
	if strings.Count(logBuf.String(), "admin_spend_read_failed") != 2 || !strings.Contains(logBuf.String(), "name=test-key") {
		t.Errorf("per-key route with a failing backend must warn with the key name: %s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "spent_usd") || strings.Contains(logBuf.String(), "percent_used") {
		t.Errorf("the warning must carry no spend figure: %s", logBuf.String())
	}
}

func TestSpendReadFailureWarnsEvenWhenTheAuditLogIsDisabled(t *testing.T) {
	// auditLogger.Warn is an operational line, not an audit entry: the
	// EnableAuditLog switch silences the audit Info lines but must never
	// hide a failed budget-backend read.
	tracker := budget.NewRedisTracker(&failingBudgetBackend{err: errors.New("redis: connection refused")}, discardLogger())
	pipeline, _ := newSpendTestPipeline(t, spendTestKeys(), tracker)
	cfg := testConfig()
	cfg.Admin.EnableAuditLog = false
	logger, logBuf := capturingLogger()
	h := Handler(cfg, pipeline, Credentials{Admin: fakeAdminCredential()}, logger, nil)

	rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys/test-key/spend", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"spend_unavailable":true`) {
		t.Fatalf("per-key spend with a failing backend: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(logBuf.String(), "admin_virtual_key_spend_read") {
		t.Errorf("the audit entry must be suppressed while the audit log is disabled: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "admin_spend_read_failed") || !strings.Contains(logBuf.String(), "name=test-key") {
		t.Errorf("the operational warning must still fire with the audit log disabled: %s", logBuf.String())
	}
}

func TestListVirtualKeysCarriesRotationGraceAndExpiryState(t *testing.T) {
	pipeline, _ := newSpendTestPipeline(t, spendTestKeys(), budget.NewTracker())
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)
	rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys", fakeAdminCredential(), "")
	if strings.Contains(rec.Body.String(), "previous_key_hash_expires_at") {
		t.Fatalf("no rotation pending: field must be omitted: %s", rec.Body.String())
	}
	rec = doRequest(t, h, http.MethodPost, "/admin/virtual_keys/test-key/rotate", fakeAdminCredential(), `{"new_key_hash":"`+testHashOf("test-key-v2")+`","grace_period_seconds":600}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/virtual_keys", fakeAdminCredential(), "")
	entries := decodeEntries(t, rec.Body.String())
	until, err := time.Parse(time.RFC3339, entries["test-key"].PreviousKeyHashExpiresAt)
	if err != nil || until.Before(time.Now().Add(9*time.Minute)) || until.After(time.Now().Add(11*time.Minute)) {
		t.Errorf("previous_key_hash_expires_at = %q, want an RFC 3339 instant about ten minutes ahead (err %v)", entries["test-key"].PreviousKeyHashExpiresAt, err)
	}
	if strings.Contains(rec.Body.String(), "previous_key_hash\"") {
		t.Errorf("the previous hash itself must never be listed: %s", rec.Body.String())
	}
}

func TestGetDeploymentsListsLiveRouterStateForAdminAndViewerOnly(t *testing.T) {
	pipeline, rt := newSpendTestPipeline(t, spendTestKeys(), budget.NewTracker())
	logger, logBuf := capturingLogger()
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential(), Viewer: fakeViewerCredential(), CostViewer: fakeCostViewerCredential(), Operator: fakeOperatorCredential()}, logger, nil)

	rec := doRequest(t, h, http.MethodGet, "/admin/deployments", fakeViewerCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/deployments: %d %s", rec.Code, rec.Body.String())
	}
	var entries []adminapi.DeploymentEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decoding: %v (%s)", err, rec.Body.String())
	}
	if len(entries) != 2 || entries[0].Name != "d1" || entries[1].Name != "d2" {
		t.Fatalf("entries = %+v, want d1 then d2", entries)
	}
	d1 := entries[0]
	if d1.Model != "gpt-4o" || d1.UpstreamModel != "gpt-4o" || d1.Provider != "openai" || d1.Kind != "chat" || !d1.Healthy || d1.Weight != 2 || d1.LatencyFactorPercent != 0 || !d1.Sticky {
		t.Errorf("d1 = %+v, want static fields + healthy, weight 2, latency 0, sticky", d1)
	}
	if entries[1].Weight != 1 || entries[1].Sticky {
		t.Errorf("d2 = %+v, want default weight 1, not sticky", entries[1])
	}
	if !strings.Contains(logBuf.String(), "admin_deployments_read") {
		t.Errorf("audit line missing: %s", logBuf.String())
	}

	// Live state: a weight update through the admin API and probe failures.
	if rec := doRequest(t, h, http.MethodPost, "/admin/deployments/d2/weight", fakeAdminCredential(), `{"weight":5}`); rec.Code != http.StatusNoContent {
		t.Fatalf("weight update: %d %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < 3; i++ {
		rt.ReportProbeResult("d1", false)
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/deployments", fakeAdminCredential(), "")
	entries = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	if entries[0].Healthy || entries[1].Weight != 5 {
		t.Errorf("after probes and a weight update: %+v", entries)
	}

	for name, cred := range map[string]string{"CostViewer": fakeCostViewerCredential(), "Operator": fakeOperatorCredential()} {
		if rec := doRequest(t, h, http.MethodGet, "/admin/deployments", cred, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s GET /admin/deployments = %d, want 401", name, rec.Code)
		}
	}
}
