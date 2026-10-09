package main

// The config-only checks behind -validate moved to controlplane.Validate
// on 2026-10-10 (RFC-3 slice (a)) and are tested there
// (internal/gateway/controlplane/validate_test.go). What stays here is the
// binary's glue: -validate and buildPipeline validate against the key set
// of the REAL adapter registry, adapter.ProviderNames() (the kelvran CLI's
// copy of that set) matches it exactly, and the -validate flag's public
// output (docs/VERSIONING.md, docs/reference/config.md) is unchanged.

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// TestValidateConfigRejectsAnUnregisteredProviderWithTheRealRegistry pins
// the wrapper: validateConfig consults newAdapterRegistry's key set, so a
// provider the gateway does not ship is refused with the same error
// string controlplane.Validate produces.
func TestValidateConfigRejectsAnUnregisteredProviderWithTheRealRegistry(t *testing.T) {
	cfg := &controlplane.Config{
		Deployments: []controlplane.DeploymentConfig{
			{Name: "typo-primary", Model: "gpt-4o", Provider: "openia"}, // deliberate typo
		},
	}
	err := validateConfig(cfg)
	if err == nil {
		t.Fatal("validateConfig with an unregistered provider returned nil error, want an error")
	}
	if !strings.Contains(err.Error(), `no adapter registered for provider "openia"`) {
		t.Errorf("error = %q, want the controlplane.Validate provider message", err.Error())
	}
	cfg.Deployments[0].Provider = "openai"
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("validateConfig with a registered provider: %v", err)
	}
}

// TestAdapterRegistryMatchesProviderNames pins adapter.ProviderNames() to
// the real registry in both directions, and each registry key to that
// adapter's own Name(): the CLI validates a config against
// adapter.ProviderNames() (controlplane.Validate's doc comment), so the
// two sets drifting apart would make `kelvran doctor` and
// `kelvran-gateway -validate` disagree about the same file.
func TestAdapterRegistryMatchesProviderNames(t *testing.T) {
	registry := newAdapterRegistry()
	got := providerNames(registry)
	want := adapter.ProviderNames()
	if !maps.Equal(got, want) {
		t.Fatalf("registry key set %v != adapter.ProviderNames() %v", keysSorted(got), keysSorted(want))
	}
	for name, a := range registry {
		if a.Name() != name {
			t.Errorf("registry key %q but adapter.Name() = %q", name, a.Name())
		}
	}
	if len(want) != 5 {
		t.Errorf("ProviderNames has %d entries, want the five shipped providers", len(want))
	}
}

func keysSorted(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// deterministic for the failure message only
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// TestValidateFlagPrintsConfigIsValid is the public-surface pin for the
// -validate flag (docs/VERSIONING.md lists it; docs/reference/config.md
// documents its output): until this test, no test, script or workflow
// asserted the literal "config is valid" line — release.yml's acceptance
// step and scripts/bench-ci.sh check only the exit code. main() parses
// global flags and calls os.Exit, so this drives the real binary built by
// buildGatewayBinaryForTest: a well-formed file prints exactly
// "config is valid" on stdout and exits 0; a file naming an unregistered
// provider exits 1 with nothing on stdout and "config error:" on stderr.
func TestValidateFlagPrintsConfigIsValid(t *testing.T) {
	bin := buildGatewayBinaryForTest(t)
	dir := t.TempDir()
	const skeleton = "listen_addr: \":0\"\n" +
		"virtual_keys:\n" +
		"  k:\n" +
		"    key_hash: \"aa\"\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"m\"\n" +
		"    provider: \"%s\"\n" +
		"    upstream_model: \"m\"\n" +
		"    base_url: \"https://x\"\n" +
		"    api_key_env: \"X\"\n"
	valid := filepath.Join(dir, "valid.yaml")
	invalid := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(valid, []byte(strings.Replace(skeleton, "%s", "openai", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalid, []byte(strings.Replace(skeleton, "%s", "openia", 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "-config", valid, "-validate")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("-validate on a valid config: %v (stderr %q)", err, stderr.String())
	}
	if string(out) != "config is valid\n" {
		t.Errorf("stdout = %q, want exactly \"config is valid\\n\"", out)
	}

	cmd = exec.Command(bin, "-config", invalid, "-validate")
	stderr.Reset()
	cmd.Stderr = &stderr
	out, err = cmd.Output()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("-validate on an invalid config: err = %v, want exit code 1", err)
	}
	if len(out) != 0 {
		t.Errorf("stdout = %q on failure, want empty", out)
	}
	if !strings.HasPrefix(stderr.String(), "config error:") || !strings.Contains(stderr.String(), `no adapter registered for provider "openia"`) {
		t.Errorf("stderr = %q, want a \"config error:\" line naming the unregistered provider", stderr.String())
	}
}
