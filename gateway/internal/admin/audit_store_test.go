package admin

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/admin/auditstore"
)

// TestAdminMutationAlsoLandsInDurableAuditStore is the direct proof for
// this feature's whole point: internal/admin/auditstore's own doc
// comment describes the pre-existing slog line as genuinely write-only.
// A real virtual-key upsert through the live HTTP handler must produce
// BOTH the existing slog line (already proven by
// TestVirtualKeyUpsertLogsAnAuditEntry-shaped tests elsewhere in this
// package) AND a durable, later-queryable JSONL record with matching
// fields.
func TestAdminMutationAlsoLandsInDurableAuditStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	store, err := auditstore.Open(path)
	if err != nil {
		t.Fatalf("auditstore.Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger(), store)

	body := `{"key_hash":"` + testHashOf("durable-audit-cred") + `"}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-durable", fakeAdminCredential(), body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	entries, err := store.Query(auditstore.Filter{Msg: "admin_virtual_key_upserted"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want exactly 1 durable record for the upsert", len(entries))
	}
	if entries[0].Fields["name"] != "team-durable" {
		t.Errorf(`entries[0].Fields["name"] = %q, want %q`, entries[0].Fields["name"], "team-durable")
	}
}

// TestDisablingAuditLogAlsoSuppressesDurableAppend proves the single-
// switch contract: EnableAuditLog=false must suppress BOTH the slog
// line and the durable JSONL append, never just one of the two.
func TestDisablingAuditLogAlsoSuppressesDurableAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	store, err := auditstore.Open(path)
	if err != nil {
		t.Fatalf("auditstore.Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	h := Handler(auditLogDisabledConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger(), store)

	body := `{"key_hash":"` + testHashOf("suppressed-audit-cred") + `"}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-suppressed", fakeAdminCredential(), body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	entries, err := store.Query(auditstore.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("len(entries) = %d, want 0 -- EnableAuditLog=false must suppress the durable append too, not just the slog line", len(entries))
	}
}

// TestGetAdminAuditRouteFiltersByFieldAndMsg is the end-to-end proof for
// GET /admin/audit itself, not just the underlying auditstore package
// (already covered by that package's own tests): real admin mutations
// through the live HTTP handler, then a real HTTP query against the new
// route, filtered by both action-type and field value together.
func TestGetAdminAuditRouteFiltersByFieldAndMsg(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	store, err := auditstore.Open(path)
	if err != nil {
		t.Fatalf("auditstore.Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger(), store)

	for _, name := range []string{"team-one", "team-two"} {
		body := `{"key_hash":"` + testHashOf("cred-"+name) + `"}`
		rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/"+name, fakeAdminCredential(), body)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("POST %s status = %d, want 204", name, rec.Code)
		}
	}
	rec := doRequest(t, h, http.MethodDelete, "/admin/virtual_keys/team-one", fakeAdminCredential(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE team-one status = %d, want 204", rec.Code)
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/audit?msg=admin_virtual_key_upserted&field=name&value=team-two", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/audit status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var got []auditEntryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want exactly 1 (the team-two upsert only, not team-one's upsert or delete)", len(got))
	}
	if got[0].Fields["name"] != "team-two" {
		t.Errorf(`got[0].Fields["name"] = %q, want %q`, got[0].Fields["name"], "team-two")
	}
}

// TestGetAdminAuditRouteAbsentWhenNoAuditStoreConfigured proves the
// route itself is simply not registered at all when the operator never
// configured AuditLogPath -- mirrors every other optional admin
// capability's own "off unless configured" posture (see BackupDir).
func TestGetAdminAuditRouteAbsentWhenNoAuditStoreConfigured(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)

	rec := doRequest(t, h, http.MethodGet, "/admin/audit", fakeAdminCredential(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /admin/audit with no auditStore configured: status = %d, want 404", rec.Code)
	}
}
