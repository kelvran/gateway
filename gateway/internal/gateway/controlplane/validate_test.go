package controlplane

import (
	"path/filepath"
	"strings"
	"testing"
)

// The four tests below moved from cmd/gateway/validate_config_test.go with
// the checks themselves (RFC-3 slice (a), 2026-10-10), retargeted at
// Validate with an explicit provider set; cmd/gateway keeps the glue tests
// that tie Validate to the real adapter registry and the -validate flag.

var openaiOnly = map[string]struct{}{"openai": {}}

// TestValidateAcceptsAWellFormedConfig proves the common case never
// false-positives.
func TestValidateAcceptsAWellFormedConfig(t *testing.T) {
	cfg := &Config{
		Deployments: []DeploymentConfig{
			{
				Name: "primary", Model: "gpt-4o", Provider: "openai",
				FallbackChains: map[string][]string{"content_policy": {"secondary"}},
			},
			{Name: "secondary", Model: "gpt-4o", Provider: "openai"},
		},
	}
	if err := Validate(cfg, openaiOnly); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestValidateRejectsAFallbackChainTargetingAnUnknownDeployment mirrors
// cmd/gateway's TestBuildPipelineRejectsFallbackChainTargetingUnknownDeployment
// but calls Validate directly — buildPipeline delegates this exact check
// here. The error string is part of what -validate prints and is pinned.
func TestValidateRejectsAFallbackChainTargetingAnUnknownDeployment(t *testing.T) {
	cfg := &Config{
		Deployments: []DeploymentConfig{
			{
				Name: "primary", Model: "gpt-4o", Provider: "openai",
				FallbackChains: map[string][]string{"content_policy": {"does-not-exist"}},
			},
		},
	}
	err := Validate(cfg, openaiOnly)
	if err == nil {
		t.Fatal("Validate with a fallback_chains target naming a nonexistent deployment returned nil error, want an error")
	}
	if want := `deployment "primary" fallback_chains.content_policy names "does-not-exist", which is not a configured deployment`; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// TestValidateRejectsAnUnregisteredProvider mirrors cmd/gateway's
// TestBuildPipelineRejectsUnregisteredProvider but calls Validate directly;
// a provider check failure wins over a fallback failure on a later
// deployment because providers are checked for every deployment first.
func TestValidateRejectsAnUnregisteredProvider(t *testing.T) {
	cfg := &Config{
		Deployments: []DeploymentConfig{
			{Name: "ok", Model: "gpt-4o", Provider: "openai", FallbackChains: map[string][]string{"generic": {"nowhere"}}},
			{Name: "typo-primary", Model: "gpt-4o", Provider: "openia"}, // deliberate typo
		},
	}
	err := Validate(cfg, openaiOnly)
	if err == nil {
		t.Fatal("Validate with an unregistered provider returned nil error, want an error")
	}
	if want := `deployment "typo-primary": no adapter registered for provider "openia"`; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if err := Validate(cfg, nil); err == nil || !strings.Contains(err.Error(), "no adapter registered") {
		t.Errorf("a nil provider set must reject every deployment; got %v", err)
	}
}

// TestValidateNeverOpensAnyBoltStore is the regression proof for Validate's
// "config-only, zero side effects" contract: it must succeed or fail PURELY
// on cfg.Deployments' own content, never on whether a durable store's path
// happens to be openable. Every one of this config's PersistPath-shaped
// fields points at a path whose parent directory does not exist — bolt.Open
// would fail outright on any of them — yet Validate must still succeed,
// because it never reads any of these fields at all.
func TestValidateNeverOpensAnyBoltStore(t *testing.T) {
	unwritable := filepath.Join(t.TempDir(), "does-not-exist-as-a-directory", "store.db")

	cfg := &Config{
		Deployments: []DeploymentConfig{
			{Name: "primary", Model: "gpt-4o", Provider: "openai"},
		},
	}
	cfg.Admin.PersistPath = unwritable
	cfg.Budget.PersistPath = unwritable
	cfg.Prompt.PersistPath = unwritable

	if err := Validate(cfg, openaiOnly); err != nil {
		t.Fatalf("Validate: %v (an unopenable PersistPath must not affect this result — Validate never opens any store)", err)
	}
}
