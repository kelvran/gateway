package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/backup"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/identity"
	identityboltstore "github.com/kelvran/gateway/gateway/internal/identity/boltstore"
)

// TestRestorablePersistPathResolvesEachKnownStoreKind proves the field
// resolution matches exactly what buildPipeline's own identity-store
// switch / newBudgetTracker / newPromptStore each read for that store.
func TestRestorablePersistPathResolvesEachKnownStoreKind(t *testing.T) {
	cfg := &controlplane.Config{}
	cfg.Admin.PersistPath = "/tmp/identity.db"
	cfg.Budget.PersistPath = "/tmp/budget.db"
	cfg.Prompt.PersistPath = "/tmp/prompt.db"

	cases := []struct {
		storeKind string
		want      string
	}{
		{"identity", "/tmp/identity.db"},
		{"budget", "/tmp/budget.db"},
		{"prompt", "/tmp/prompt.db"},
	}
	for _, tc := range cases {
		got, err := restorablePersistPath(cfg, tc.storeKind)
		if err != nil {
			t.Errorf("restorablePersistPath(%q): %v", tc.storeKind, err)
			continue
		}
		if got != tc.want {
			t.Errorf("restorablePersistPath(%q) = %q, want %q", tc.storeKind, got, tc.want)
		}
	}
}

// TestRestorablePersistPathRejectsAnUnknownStoreKind proves a typo'd
// -restore-store value fails clearly rather than silently resolving to
// an empty path.
func TestRestorablePersistPathRejectsAnUnknownStoreKind(t *testing.T) {
	cfg := &controlplane.Config{}
	if _, err := restorablePersistPath(cfg, "identityy"); err == nil {
		t.Fatal("restorablePersistPath with an unknown store kind returned nil error, want an error")
	}
}

// TestRestorablePersistPathRejectsAStoreWithNoConfiguredPersistPath
// proves a store that's in-memory-only in this config file (no
// persist_path set) is refused, rather than resolving to an empty
// destPath that internal/backup.Restore would then try to write to.
func TestRestorablePersistPathRejectsAStoreWithNoConfiguredPersistPath(t *testing.T) {
	cfg := &controlplane.Config{} // every PersistPath field left at its zero value

	for _, storeKind := range []string{"identity", "budget", "prompt"} {
		if _, err := restorablePersistPath(cfg, storeKind); err == nil {
			t.Errorf("restorablePersistPath(%q) with no configured persist_path returned nil error, want an error", storeKind)
		}
	}
}

// TestRunRestoreRequiresFromPath proves -restore-from is validated
// before configPath is ever loaded -- an operator running
// `-restore-store=identity` alone (forgetting -restore-from) gets an
// immediate, specific error, not a confusing downstream one.
func TestRunRestoreRequiresFromPath(t *testing.T) {
	if err := runRestore("does-not-matter.yaml", "identity", "", false); err == nil {
		t.Fatal("runRestore with an empty fromPath returned nil error, want an error")
	}
}

// TestRunRestoreEndToEndThroughTheRealIdentityConstructor is the actual
// missing proof this task closes: a real backup.CopyFile-produced
// backup, restored via runRestore's own config-driven path resolution,
// loads correctly through identity/boltstore.Open -- the exact
// constructor this binary's own buildPipeline uses at every normal
// startup.
func TestRunRestoreEndToEndThroughTheRealIdentityConstructor(t *testing.T) {
	destPath := filepath.Join(t.TempDir(), "identity-restored.db")

	sourcePath := filepath.Join(t.TempDir(), "identity-source.db")
	source, err := identityboltstore.Open(sourcePath)
	if err != nil {
		t.Fatalf("identityboltstore.Open (source): %v", err)
	}
	wantKey := identity.VirtualKey{ID: "vk-cli-restore-proof", KeyHash: "cafef00d"}
	if err := source.Save(context.Background(), wantKey); err != nil {
		t.Fatalf("Save: %v", err)
	}

	backupPath := filepath.Join(t.TempDir(), "identity-backup.db")
	if err := backup.CopyFile(source.DB(), backupPath); err != nil {
		t.Fatalf("backing up the source store: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("closing source store: %v", err)
	}

	configPath := writeMinimalConfigWithIdentityPersistPath(t, destPath)

	if err := runRestore(configPath, "identity", backupPath, false); err != nil {
		t.Fatalf("runRestore: %v", err)
	}

	restored, err := identityboltstore.Open(destPath)
	if err != nil {
		t.Fatalf("identityboltstore.Open (restored destination): %v", err)
	}
	defer func() { _ = restored.Close() }()

	keys, err := restored.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := keys[wantKey.ID]
	if !ok {
		t.Fatalf("restored store has no entry for %q, want the round-tripped virtual key", wantKey.ID)
	}
	if got.KeyHash != wantKey.KeyHash {
		t.Errorf("restored KeyHash = %q, want %q", got.KeyHash, wantKey.KeyHash)
	}
}

// TestRunRestoreErrorsWhenTheTargetStoreHasNoPersistPathConfigured
// proves runRestore surfaces restorablePersistPath's own error rather
// than attempting (and failing some other, more confusing way) to
// restore into an empty destPath.
func TestRunRestoreErrorsWhenTheTargetStoreHasNoPersistPathConfigured(t *testing.T) {
	configPath := writeMinimalConfigWithIdentityPersistPath(t, "") // no admin/persist_path section at all

	backupPath := filepath.Join(t.TempDir(), "backup.db")
	source, err := identityboltstore.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatalf("identityboltstore.Open (source): %v", err)
	}
	defer func() { _ = source.Close() }()
	if err := backup.CopyFile(source.DB(), backupPath); err != nil {
		t.Fatalf("backing up the source store: %v", err)
	}

	if err := runRestore(configPath, "budget", backupPath, false); err == nil {
		t.Fatal("runRestore against a store with no configured persist_path returned nil error, want an error")
	}
}

// writeMinimalConfigWithIdentityPersistPath writes the smallest config
// file controlplane.Load will accept, with admin.persist_path set to
// identityPersistPath (or omitted entirely when identityPersistPath is
// ""), to exercise runRestore/restorablePersistPath against a real
// loaded controlplane.Config rather than a hand-built one.
func writeMinimalConfigWithIdentityPersistPath(t *testing.T, identityPersistPath string) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	content := "listen_addr: \":0\"\n" +
		"virtual_keys:\n" +
		"  test-key:\n" +
		"    key_hash: \"0000000000000000000000000000000000000000000000000000000000000000\"\n" +
		"deployments:\n" +
		"  primary:\n" +
		"    model: \"gpt-4o\"\n" +
		"    provider: \"openai\"\n" +
		"    upstream_model: \"gpt-4o\"\n" +
		"    base_url: \"https://api.openai.com/v1/chat/completions\"\n" +
		"    api_key_env: \"OPENAI_API_KEY\"\n"
	if identityPersistPath != "" {
		content += "admin:\n  persist_path: \"" + identityPersistPath + "\"\n"
	}
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}
