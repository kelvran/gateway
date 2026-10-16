package controlplane

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadParsesAuditSigningSecretEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nadmin:\n  audit_log_path: \"/var/log/kelvran/audit.jsonl\"\n  audit_signing_secret_env: \"KELVRAN_AUDIT_SIGNING_SECRET\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Admin.AuditSigningSecretEnv != "KELVRAN_AUDIT_SIGNING_SECRET" {
		t.Errorf("Admin.AuditSigningSecretEnv = %q, want the variable NAME from admin.audit_signing_secret_env", cfg.Admin.AuditSigningSecretEnv)
	}
}

func TestLoadWithoutAuditSigningSecretEnvLeavesItEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nadmin:\n  audit_log_path: \"/var/log/kelvran/audit.jsonl\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Admin.AuditSigningSecretEnv != "" {
		t.Errorf("Admin.AuditSigningSecretEnv = %q, want empty when the key is absent (an unsigned chain, today's deployments unchanged)", cfg.Admin.AuditSigningSecretEnv)
	}
}
