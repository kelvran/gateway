package dataplane

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReloadDeploymentCredentialsKeepsLastKnownGoodOnEmptyFile pins the half
// of the keep-last-good promise the read-error test cannot: a file that
// exists but is empty, or whitespace only (a rotation script that truncated
// before writing, a volume caught mid-update), must be treated like a
// failed read and never stored as a rotation to "". Before 2026-10-08 the
// empty value was stored and logged as a SUCCESSFUL rotation, which sent
// every request to the deployment upstream with no credential for up to
// one reload interval.
func TestReloadDeploymentCredentialsKeepsLastKnownGoodOnEmptyFile(t *testing.T) {
	cases := map[string]string{"zero bytes": "", "whitespace only": "\n  \t\n"}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			const goodValue = "last-known-good"
			path := filepath.Join(t.TempDir(), "api-key")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			state := NewDeploymentCredentialState(DeploymentCredentials{APIKey: goodValue})
			dep := Deployment{
				Name:            "d",
				CredentialFiles: DeploymentCredentialFiles{APIKey: path},
				CredentialState: state,
			}
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))

			reloadDeploymentCredentials(dep, logger)

			if got := state.Load().APIKey; got != goodValue {
				t.Errorf("APIKey = %q after reading an empty file, want the last-known-good value %q preserved", got, goodValue)
			}
			if !strings.Contains(logs.String(), "credential_reload_read_failed") {
				t.Errorf("want a credential_reload_read_failed warning for an empty file, got logs: %q", logs.String())
			}
			if strings.Contains(logs.String(), "credential_reload_rotated") {
				t.Errorf("an empty file must never be logged as a rotation, got logs: %q", logs.String())
			}
		})
	}
}
