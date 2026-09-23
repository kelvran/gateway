package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// fakeCostViewerCredential mirrors fakeAdminCredential/fakeViewerCredential's
// own "assembled from parts, never a literal" convention.
func fakeCostViewerCredential() string {
	parts := []string{"not", "a", "real", "cost", "viewer", "credential", "for", "tests"}
	return strings.Join(parts, "-")
}

// TestCostViewerTokenCanReadSpendButNothingElse is the load-bearing proof
// for docs/upgrade-research/multi-tenancy-access-control-2026-09-14.md's
// narrow-third-tier design: a CostViewer credential authenticates the new
// spend route and is rejected by every other route this mux serves,
// including the ones the existing Viewer tier CAN read.
func TestCostViewerTokenCanReadSpendButNothingElse(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{
		Admin:      fakeAdminCredential(),
		Viewer:     fakeViewerCredential(),
		CostViewer: fakeCostViewerCredential(),
	}, discardLogger(), nil)

	rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys/test-key/spend", fakeCostViewerCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET .../spend with the cost-viewer credential: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"config", http.MethodGet, "/admin/config"},
		{"prompts", http.MethodGet, "/admin/prompts"},
		{"virtual key upsert", http.MethodPost, "/admin/virtual_keys/team-theta"},
		{"virtual key delete", http.MethodDelete, "/admin/virtual_keys/test-key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := doRequest(t, h, c.method, c.path, fakeCostViewerCredential(), "{}")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s with the cost-viewer credential: status = %d, want 401 — cost-viewer must authenticate ONLY the spend route", c.method, c.path, rec.Code)
			}
		})
	}
}

// TestAdminAndViewerTokensStillReadSpendWhenCostViewerIsConfigured proves
// the new tier is additive: configuring CostViewer never takes away
// Admin's or the existing Viewer's own ability to read the same route.
func TestAdminAndViewerTokensStillReadSpendWhenCostViewerIsConfigured(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{
		Admin:      fakeAdminCredential(),
		Viewer:     fakeViewerCredential(),
		CostViewer: fakeCostViewerCredential(),
	}, discardLogger(), nil)

	for _, cred := range []string{fakeAdminCredential(), fakeViewerCredential()} {
		rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys/test-key/spend", cred, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET .../spend with credential %q: status = %d, want 200, body: %s", cred, rec.Code, rec.Body.String())
		}
	}
}

// TestOmittingCostViewerBehavesExactlyAsBefore proves the tier is fully
// optional -- an empty Credentials.CostViewer means the spend route only
// authenticates Admin/Viewer, exactly as if the field didn't exist.
func TestOmittingCostViewerBehavesExactlyAsBefore(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)

	rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys/test-key/spend", fakeCostViewerCredential(), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET .../spend with an unconfigured cost-viewer value: status = %d, want 401", rec.Code)
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/virtual_keys/test-key/spend", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET .../spend with the admin credential, no cost-viewer tier configured: status = %d, want 200", rec.Code)
	}
}

// TestGetVirtualKeySpendUnknownNameReturns404 proves the negative case
// mirroring deleteVirtualKeyHandler's own not-found convention.
func TestGetVirtualKeySpendUnknownNameReturns404(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)

	rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys/does-not-exist/spend", fakeAdminCredential(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET .../spend for an unknown key: status = %d, want 404", rec.Code)
	}
}

// TestGetVirtualKeySpendReflectsRealBudgetShape proves the response body
// carries the real, current budget shape -- not a stub -- for a key with
// a positive budget cap.
func TestGetVirtualKeySpendReflectsRealBudgetShape(t *testing.T) {
	pipeline := newTestPipeline(t)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)

	body := `{"key_hash":"` + testHashOf("spend-key") + `","budget_usd":"10","budget_reset_interval_seconds":3600}`
	if rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/spend-key", fakeAdminCredential(), body); rec.Code != http.StatusNoContent {
		t.Fatalf("creating spend-key: status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys/spend-key/spend", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET .../spend: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var got virtualKeySpendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}
	if got.BudgetUSD != "10" {
		t.Errorf("BudgetUSD = %q, want %q", got.BudgetUSD, "10")
	}
	if got.BudgetResetIntervalSeconds != 3600 {
		t.Errorf("BudgetResetIntervalSeconds = %d, want 3600", got.BudgetResetIntervalSeconds)
	}
	if got.SpentUSD != "0" {
		t.Errorf("SpentUSD = %q, want %q (a never-billed key)", got.SpentUSD, "0")
	}
	if got.PercentUsed != 0 {
		t.Errorf("PercentUsed = %v, want 0 (a never-billed key)", got.PercentUsed)
	}
}
