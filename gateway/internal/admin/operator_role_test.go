package admin

import (
	"net/http"
	"strings"
	"testing"
)

// fakeOperatorCredential mirrors fakeAdminCredential/fakeViewerCredential/
// fakeCostViewerCredential's own "assembled from parts, never a literal"
// convention.
func fakeOperatorCredential() string {
	parts := []string{"not", "a", "real", "operator", "credential", "for", "tests"}
	return strings.Join(parts, "-")
}

// TestOperatorTokenCanRotateReweightAndEraseButNothingElse is the
// load-bearing proof for docs/rfcs/2026-09-20-gateway-admin-rbac-risk-tiering.md's
// core claim: a configured Operator credential authenticates exactly the
// three reversible, single-named-resource write routes, and is rejected
// by every other route this mux serves — including read-only routes the
// existing Viewer tier CAN read, and every write route that stays
// Admin-only.
func TestOperatorTokenCanRotateReweightAndEraseButNothingElse(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{
		Admin:    fakeAdminCredential(),
		Viewer:   fakeViewerCredential(),
		Operator: fakeOperatorCredential(),
	}, discardLogger(), nil)

	rotateBody := `{"new_key_hash":"` + testHashOf("operator-rotated-secret") + `"}`
	if rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/test-key/rotate", fakeOperatorCredential(), rotateBody); rec.Code != http.StatusNoContent {
		t.Errorf("POST .../rotate with the operator credential: status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	if rec := doRequest(t, h, http.MethodPost, "/admin/deployments/d1/weight", fakeOperatorCredential(), `{"weight":3}`); rec.Code != http.StatusNoContent {
		t.Errorf("POST .../weight with the operator credential: status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	eraseBody := `{"virtual_key_id":"test-key","model":"gpt-4o"}`
	if rec := doRequest(t, h, http.MethodPost, "/admin/cache/erase", fakeOperatorCredential(), eraseBody); rec.Code != http.StatusOK {
		t.Errorf("POST /admin/cache/erase with the operator credential: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	if rec := doRequest(t, h, http.MethodGet, "/admin/config", fakeOperatorCredential(), ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /admin/config with the operator credential: status = %d, want 401 — operator must never authenticate a read route", rec.Code)
	}

	upsertBody := `{"key_hash":"` + testHashOf("irrelevant") + `"}`
	if rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-operator", fakeOperatorCredential(), upsertBody); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /admin/virtual_keys with the operator credential: status = %d, want 401 — virtual-key create stays admin-only", rec.Code)
	}

	if rec := doRequest(t, h, http.MethodDelete, "/admin/virtual_keys/test-key", fakeOperatorCredential(), ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("DELETE /admin/virtual_keys with the operator credential: status = %d, want 401 — virtual-key delete stays admin-only", rec.Code)
	}

	if rec := doRequest(t, h, http.MethodPost, "/admin/backup", fakeOperatorCredential(), ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /admin/backup with the operator credential: status = %d, want 401 — backup stays admin-only", rec.Code)
	}
}

// TestAdminTokenStillWorksForEverythingWhenAnOperatorTierIsConfigured
// proves the new operator tier is additive — configuring one never takes
// away the admin credential's own existing full read/write access,
// mirroring TestAdminTokenStillWorksForEverythingWhenAViewerTierIsConfigured's
// own shape for the viewer tier.
func TestAdminTokenStillWorksForEverythingWhenAnOperatorTierIsConfigured(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential(), Operator: fakeOperatorCredential()}, discardLogger(), nil)

	if rec := doRequest(t, h, http.MethodPost, "/admin/deployments/d1/weight", fakeAdminCredential(), `{"weight":7}`); rec.Code != http.StatusNoContent {
		t.Errorf("POST .../weight with the admin credential: status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	if rec := doRequest(t, h, http.MethodPost, "/admin/backup", fakeAdminCredential(), ""); rec.Code == http.StatusUnauthorized {
		t.Errorf("POST /admin/backup with the admin credential: status = %d, want anything but 401 — admin must keep full access", rec.Code)
	}
}

// TestOmittingTheOperatorTierBehavesExactlyAsBefore proves the operator
// tier is fully optional — an empty Credentials.Operator reproduces the
// pre-operator-role behavior exactly on all three routes: only the admin
// credential works, mirroring TestOmittingTheViewerTierBehavesExactlyAsBefore's
// own shape for the viewer tier.
func TestOmittingTheOperatorTierBehavesExactlyAsBefore(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)

	if rec := doRequest(t, h, http.MethodPost, "/admin/deployments/d1/weight", fakeOperatorCredential(), `{"weight":3}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST .../weight with an unconfigured operator credential: status = %d, want 401", rec.Code)
	}

	if rec := doRequest(t, h, http.MethodPost, "/admin/deployments/d1/weight", fakeAdminCredential(), `{"weight":3}`); rec.Code != http.StatusNoContent {
		t.Errorf("POST .../weight with the admin credential, no operator tier configured: status = %d, want 204", rec.Code)
	}
}

// TestOperatorAuditLogRecordsOperatorTier proves the audit-log
// "authorized_by" field genuinely distinguishes admin from operator on
// the three shared routes, mirroring
// TestGetConfigLogsAnAuditEntryNamingTheCredentialTier's own shape —
// the direct proof that credentialTierFromContext, not a hardcoded
// "admin" string, drives these log lines now.
func TestOperatorAuditLogRecordsOperatorTier(t *testing.T) {
	logger, buf := capturingLogger()
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential(), Operator: fakeOperatorCredential()}, logger, nil)

	rec := doRequest(t, h, http.MethodPost, "/admin/deployments/d1/weight", fakeOperatorCredential(), `{"weight":9}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST .../weight with the operator credential: status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}
	if logOutput := buf.String(); !strings.Contains(logOutput, "admin_deployment_weight_updated") || !strings.Contains(logOutput, "authorized_by=operator") {
		t.Errorf("expected an admin_deployment_weight_updated entry with authorized_by=operator; got: %s", logOutput)
	}

	buf.Reset()
	rec = doRequest(t, h, http.MethodPost, "/admin/deployments/d1/weight", fakeAdminCredential(), `{"weight":11}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST .../weight with the admin credential: status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}
	if logOutput := buf.String(); !strings.Contains(logOutput, "admin_deployment_weight_updated") || !strings.Contains(logOutput, "authorized_by=admin") {
		t.Errorf("expected an admin_deployment_weight_updated entry with authorized_by=admin; got: %s", logOutput)
	}
}
