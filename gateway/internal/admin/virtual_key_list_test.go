package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestListVirtualKeysHandlerRequiresAdminOrViewerToken proves GET
// /admin/virtual_keys uses the same viewer-or-admin tier as GET
// /admin/config and GET /admin/prompts -- a config-shaped read, broader
// than the narrower CostViewer tier GET .../spend also accepts.
func TestListVirtualKeysHandlerRequiresAdminOrViewerToken(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{
		Admin:      fakeAdminCredential(),
		Viewer:     fakeViewerCredential(),
		CostViewer: fakeCostViewerCredential(),
	}, discardLogger(), nil)

	for _, cred := range []string{fakeAdminCredential(), fakeViewerCredential()} {
		rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys", cred, "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET /admin/virtual_keys with credential %q: status = %d, want 200, body: %s", cred, rec.Code, rec.Body.String())
		}
	}

	rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys", fakeCostViewerCredential(), "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /admin/virtual_keys with a cost-viewer credential: status = %d, want 401 -- this route is broader than cost-viewer's own scope", rec.Code)
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/virtual_keys", "wrong-value-entirely", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /admin/virtual_keys with a wrong credential: status = %d, want 401", rec.Code)
	}
}

// TestListVirtualKeysHandlerNeverExposesKeyHash is the security-critical
// proof: the response body must never contain a key_hash field or its
// value at all, for any configured key.
func TestListVirtualKeysHandlerNeverExposesKeyHash(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)

	hash := testHashOf("list-secret-check-credential")
	body := `{"key_hash":"` + hash + `","budget_usd":"5"}`
	if rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/list-secret-check", fakeAdminCredential(), body); rec.Code != http.StatusNoContent {
		t.Fatalf("creating list-secret-check: status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/virtual_keys: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	if strings.Contains(got, "key_hash") || strings.Contains(got, hash) {
		t.Errorf("response body leaks key_hash or its value: %s", got)
	}
}

// TestListVirtualKeysHandlerReturnsSortedByIDAndRealBudgetShape proves
// the response is deterministically ordered and carries the real,
// current per-key config -- not a stub.
func TestListVirtualKeysHandlerReturnsSortedByIDAndRealBudgetShape(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)

	body := `{"key_hash":"` + testHashOf("zzz-key") + `","budget_usd":"25","budget_reset_interval_seconds":3600,"budget_warn_percent":80,"allowed_models":["gpt-4o","claude-sonnet-5"]}`
	if rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/zzz-key", fakeAdminCredential(), body); rec.Code != http.StatusNoContent {
		t.Fatalf("creating zzz-key: status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}
	body = `{"key_hash":"` + testHashOf("aaa-key") + `","budget_usd":"1"}`
	if rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/aaa-key", fakeAdminCredential(), body); rec.Code != http.StatusNoContent {
		t.Fatalf("creating aaa-key: status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/virtual_keys: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var got []virtualKeyListEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}
	if len(got) != 3 { // test-key (from newTestPipeline) + aaa-key + zzz-key.
		t.Fatalf("len(got) = %d, want 3, body: %s", len(got), rec.Body.String())
	}
	var ids []string
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	wantIDs := []string{"aaa-key", "test-key", "zzz-key"}
	for i, want := range wantIDs {
		if ids[i] != want {
			t.Errorf("ids = %v, want %v (sorted by ID)", ids, wantIDs)
			break
		}
	}

	for _, e := range got {
		if e.ID != "zzz-key" {
			continue
		}
		if e.BudgetUSD != "25" {
			t.Errorf("zzz-key BudgetUSD = %q, want %q", e.BudgetUSD, "25")
		}
		if e.BudgetResetIntervalSeconds != 3600 {
			t.Errorf("zzz-key BudgetResetIntervalSeconds = %d, want 3600", e.BudgetResetIntervalSeconds)
		}
		if e.BudgetWarnPercent != 80 {
			t.Errorf("zzz-key BudgetWarnPercent = %v, want 80", e.BudgetWarnPercent)
		}
		wantModels := []string{"claude-sonnet-5", "gpt-4o"} // sorted.
		if len(e.AllowedModels) != 2 || e.AllowedModels[0] != wantModels[0] || e.AllowedModels[1] != wantModels[1] {
			t.Errorf("zzz-key AllowedModels = %v, want %v (sorted)", e.AllowedModels, wantModels)
		}
	}
}
