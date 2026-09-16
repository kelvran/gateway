package main

import (
	"path/filepath"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// TestValidateConfigAcceptsAWellFormedConfig proves the common case never
// false-positives.
func TestValidateConfigAcceptsAWellFormedConfig(t *testing.T) {
	cfg := &controlplane.Config{
		Deployments: []controlplane.DeploymentConfig{
			{
				Name: "primary", Model: "gpt-4o", Provider: "openai",
				FallbackChains: map[string][]string{"content_policy": {"secondary"}},
			},
			{Name: "secondary", Model: "gpt-4o", Provider: "openai"},
		},
	}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
}

// TestValidateConfigRejectsAFallbackChainTargetingAnUnknownDeployment
// mirrors TestBuildPipelineRejectsFallbackChainTargetingUnknownDeployment
// (fallback_chain_validation_test.go) but calls validateConfig directly —
// buildPipeline itself now delegates this exact check to validateConfig,
// per that function's own doc comment.
func TestValidateConfigRejectsAFallbackChainTargetingAnUnknownDeployment(t *testing.T) {
	cfg := &controlplane.Config{
		Deployments: []controlplane.DeploymentConfig{
			{
				Name: "primary", Model: "gpt-4o", Provider: "openai",
				FallbackChains: map[string][]string{"content_policy": {"does-not-exist"}},
			},
		},
	}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("validateConfig with a fallback_chains target naming a nonexistent deployment returned nil error, want an error")
	}
}

// TestValidateConfigRejectsAnUnregisteredProvider mirrors
// TestBuildPipelineRejectsUnregisteredProvider (provider_validation_test.go)
// but calls validateConfig directly.
func TestValidateConfigRejectsAnUnregisteredProvider(t *testing.T) {
	cfg := &controlplane.Config{
		Deployments: []controlplane.DeploymentConfig{
			{Name: "typo-primary", Model: "gpt-4o", Provider: "openia"}, // deliberate typo
		},
	}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("validateConfig with an unregistered provider returned nil error, want an error")
	}
}

// TestValidateConfigNeverOpensAnyBoltStore is the actual regression proof
// for validateConfig's own "config-only, zero side effects" contract: it
// must succeed or fail PURELY on cfg.Deployments' own content, never on
// whether a durable store's own path happens to be openable. Every one of
// this config's 3 PersistPath-shaped fields points at a path whose parent
// directory doesn't exist — bolt.Open would fail outright on any of them
// — yet validateConfig must still succeed, because it never reads any of
// these fields at all.
func TestValidateConfigNeverOpensAnyBoltStore(t *testing.T) {
	unwritable := filepath.Join(t.TempDir(), "does-not-exist-as-a-directory", "store.db")

	cfg := &controlplane.Config{
		Deployments: []controlplane.DeploymentConfig{
			{Name: "primary", Model: "gpt-4o", Provider: "openai"},
		},
	}
	cfg.Admin.PersistPath = unwritable
	cfg.Budget.PersistPath = unwritable
	cfg.Prompt.PersistPath = unwritable

	if err := validateConfig(cfg); err != nil {
		t.Fatalf("validateConfig: %v (an unopenable PersistPath must not affect this result — validateConfig never opens any store)", err)
	}
}
