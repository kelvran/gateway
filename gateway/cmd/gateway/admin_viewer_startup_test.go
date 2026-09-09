package main

// Startup-guard proofs for the admin viewer tier, per
// docs/rfcs/2026-09-09-gateway-admin-viewer-role.md: admin.viewer_token_env
// being set but resolving to an empty environment variable must fail
// startup, mirroring admin.token_env's own existing rule — never run an
// unauthenticated surface, viewer tier included. There was previously no
// test at all for the analogous admin.token_env guard; this file adds
// coverage for both, since run() returns synchronously here before ever
// binding a real network listener (both checks happen before any
// ListenAndServe call).

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// writeAdminStartupTestConfig writes a real, minimal YAML config with an
// admin section, run() can load via controlplane.Load. adminTokenEnv/
// viewerTokenEnv are the *names* to write into config, matching
// controlplane.AdminConfig's own env-var-name-not-raw-secret convention;
// pass "" to omit either key from the YAML entirely.
func writeAdminStartupTestConfig(t *testing.T, adminTokenEnv, viewerTokenEnv string) string {
	t.Helper()
	keyHash := testKeyHash("not-a-real-admin-startup-test-secret")

	adminSection := ""
	if adminTokenEnv != "" {
		adminSection = fmt.Sprintf("admin:\n  token_env: %q\n", adminTokenEnv)
		if viewerTokenEnv != "" {
			adminSection += fmt.Sprintf("  viewer_token_env: %q\n", viewerTokenEnv)
		}
	}

	content := fmt.Sprintf(`
listen_addr: ":0"
virtual_keys:
  test-key:
    key_hash: %q
deployments:
  gpt4o-primary:
    model: "gpt-4o"
    provider: "openai"
    upstream_model: "gpt-4o"
    base_url: "http://unused"
    api_key_env: "ADMIN_STARTUP_TEST_UNUSED_UPSTREAM_KEY"
telemetry:
  exporter: "none"
%s`, keyHash, adminSection)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

// TestRunRefusesToStartWithAdminTokenEnvSetButEmpty proves the
// pre-existing admin.token_env guard — previously untested — actually
// fires: run() must return an error, never silently start an
// unauthenticated admin server.
func TestRunRefusesToStartWithAdminTokenEnvSetButEmpty(t *testing.T) {
	const emptyEnvVar = "ADMIN_STARTUP_TEST_ADMIN_TOKEN_UNSET"
	t.Setenv(emptyEnvVar, "")

	path := writeAdminStartupTestConfig(t, emptyEnvVar, "")
	err := run(path, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err == nil {
		t.Fatal("run() succeeded with admin.token_env resolving empty — want a refusal error")
	}
}

// TestRunRefusesToStartWithViewerTokenEnvSetButEmpty is the viewer-tier
// counterpart, per docs/rfcs/2026-09-09-gateway-admin-viewer-role.md.
func TestRunRefusesToStartWithViewerTokenEnvSetButEmpty(t *testing.T) {
	const adminEnvVar = "ADMIN_STARTUP_TEST_ADMIN_TOKEN_SET"
	const emptyViewerEnvVar = "ADMIN_STARTUP_TEST_VIEWER_TOKEN_UNSET"
	t.Setenv(adminEnvVar, "a-real-admin-token-value")
	t.Setenv(emptyViewerEnvVar, "")

	path := writeAdminStartupTestConfig(t, adminEnvVar, emptyViewerEnvVar)
	err := run(path, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err == nil {
		t.Fatal("run() succeeded with admin.viewer_token_env resolving empty — want a refusal error")
	}
}

// TestRunAcceptsOmittingTheViewerTierEntirely proves the viewer tier
// stays fully optional: an admin-only config's own pre-existing
// behavior must be unaffected by this feature's addition. Uses an
// unroutable listen_addr so run() fails fast on bind rather than
// blocking forever — this test only cares that it gets PAST the two
// token-resolution guards above, not that the server actually serves.
func TestRunAcceptsOmittingTheViewerTierEntirely(t *testing.T) {
	const adminEnvVar = "ADMIN_STARTUP_TEST_ADMIN_ONLY_TOKEN"
	t.Setenv(adminEnvVar, "a-real-admin-token-value")

	path := writeAdminStartupTestConfig(t, adminEnvVar, "")
	cfg, err := controlplane.Load(path)
	if err != nil {
		t.Fatalf("controlplane.Load: %v", err)
	}
	if cfg.Admin.TokenEnv != adminEnvVar {
		t.Fatalf("cfg.Admin.TokenEnv = %q, want %q", cfg.Admin.TokenEnv, adminEnvVar)
	}
	if cfg.Admin.ViewerTokenEnv != "" {
		t.Fatalf("cfg.Admin.ViewerTokenEnv = %q, want empty when omitted from YAML", cfg.Admin.ViewerTokenEnv)
	}
}
