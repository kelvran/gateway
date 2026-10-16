package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/admin/auditstore"
)

// TestAuditLoggerWritesTheChainHeadIntoTheSlogLine: with a durable store,
// the slog audit line carries audit_seq and audit_hash -- the chain head
// after the append -- so the stdout sink holds an independent copy of
// the head that a truncated or deleted file cannot erase.
func TestAuditLoggerWritesTheChainHeadIntoTheSlogLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	store, err := auditstore.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	var buf strings.Builder
	a := auditLogger{logger: slog.New(slog.NewTextHandler(&buf, nil)), enabled: true, store: store}

	a.Info("admin_config_read", "authorized_by", "admin")
	a.Info("admin_config_read", "authorized_by", "viewer")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("log lines = %d, want 2:\n%s", len(lines), buf.String())
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a t.TempDir() path this test created
	if err != nil {
		t.Fatal(err)
	}
	fileLines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	first := sha256.Sum256([]byte(fileLines[0]))
	if !strings.Contains(lines[0], "audit_seq=1") || !strings.Contains(lines[0], "audit_hash="+hex.EncodeToString(first[:])) {
		t.Errorf("first slog line lacks the chain head (audit_seq=1, audit_hash=<sha256 of the file's first line>): %s", lines[0])
	}
	if !strings.Contains(lines[1], "audit_seq=2") {
		t.Errorf("second slog line lacks audit_seq=2: %s", lines[1])
	}
	if !strings.Contains(lines[0], "authorized_by=admin") || !strings.Contains(lines[0], "msg=admin_config_read") {
		t.Errorf("the audit fields themselves must be unchanged: %s", lines[0])
	}
}

func TestAuditLoggerWithoutAStoreCarriesNoChainHead(t *testing.T) {
	var buf strings.Builder
	a := auditLogger{logger: slog.New(slog.NewTextHandler(&buf, nil)), enabled: true}
	a.Info("admin_config_read", "authorized_by", "admin")
	if strings.Contains(buf.String(), "audit_seq") || strings.Contains(buf.String(), "audit_hash") {
		t.Errorf("no store, no chain head: %s", buf.String())
	}
}

// TestGetAdminAuditResponseCarriesNoIntegrityFields pins the owner's pick:
// seq, prev_hash and mac live in the file and are read by kelvran audit
// verify; the route's wire shape is unchanged.
func TestGetAdminAuditResponseCarriesNoIntegrityFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	store, err := auditstore.Open(path, []byte("route-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger(), store)
	body := `{"key_hash":"` + testHashOf("chain-route-cred") + `"}`
	if rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-chain", fakeAdminCredential(), body); rec.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, body: %s", rec.Code, rec.Body.String())
	}
	rec := doRequest(t, h, http.MethodGet, "/admin/audit", fakeAdminCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/audit status = %d, body: %s", rec.Code, rec.Body.String())
	}
	for _, field := range []string{`"seq"`, `"prev_hash"`, `"mac"`} {
		if strings.Contains(rec.Body.String(), field) {
			t.Errorf("GET /admin/audit exposes %s; the integrity fields are file-only: %s", field, rec.Body.String())
		}
	}
	if !strings.Contains(rec.Body.String(), `"admin_virtual_key_upserted"`) {
		t.Errorf("the entry itself must still be served: %s", rec.Body.String())
	}
}
