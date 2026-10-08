package embedsim

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReloadCredentialsKeepsLastKnownGoodOnEmptyFile mirrors the dataplane
// test of the same name: an empty credential file is a failed read, never a
// rotation to "".
func TestReloadCredentialsKeepsLastKnownGoodOnEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access-key-id")
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg := testEmbedderConfig("")
	cfg.AccessKeyID = "good-key"
	cfg.AccessKeyIDFile = path
	e := NewBedrockEmbedder(cfg, nil)

	e.reloadCredentials(discardLogger())

	if got := e.effectiveCredentials().AccessKeyID; got != "good-key" {
		t.Errorf("effectiveCredentials().AccessKeyID after reading an empty file = %q, want %q (last-known-good)", got, "good-key")
	}
}
