package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// TestBuildPipelineRejectsFallbackChainTargetingUnknownDeployment is the
// load-bearing proof for
// docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md's
// referential-integrity check: controlplane.Load has no visibility into
// the full deployment set while parsing one deployment's own mapping, so
// this must fail at buildPipeline time instead — before this change, a
// typo'd fallback target would have silently never fired at runtime
// (fallbackTargets/attemptFallbackChain simply skip an unresolvable
// name), the same class of "fails silently instead of at startup" bug
// docs/rfcs/2026-09-05-gateway-gen-ai-provider-name-validation.md already
// fixed for an unregistered provider.
func TestBuildPipelineRejectsFallbackChainTargetingUnknownDeployment(t *testing.T) {
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "primary",
				Model:         "gpt-4o",
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       "http://unused",
				APIKeyEnv:     "UNUSED_API_KEY",
				FallbackChains: map[string][]string{
					"content_policy": {"does-not-exist"},
				},
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := buildPipeline(cfg, logger); err == nil {
		t.Fatal("buildPipeline with a fallback_chains target naming a nonexistent deployment returned nil error, want an error")
	}
}

// TestBuildPipelineAcceptsFallbackChainTargetingRealDeployment proves the
// positive counterpart: a chain naming a real, configured deployment
// builds cleanly, so the new check never false-positives.
func TestBuildPipelineAcceptsFallbackChainTargetingRealDeployment(t *testing.T) {
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "primary",
				Model:         "gpt-4o",
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       "http://unused",
				APIKeyEnv:     "UNUSED_API_KEY",
				FallbackChains: map[string][]string{
					"content_policy": {"secondary"},
				},
			},
			{
				Name:          "secondary",
				Model:         "gpt-4o",
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       "http://unused",
				APIKeyEnv:     "UNUSED_API_KEY",
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := buildPipeline(cfg, logger); err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}
}
