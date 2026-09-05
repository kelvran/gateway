package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// TestBuildPipelineRejectsUnregisteredProvider is the load-bearing proof
// for docs/rfcs/2026-09-05-gateway-gen-ai-provider-name-validation.md's
// config-time fail-fast check: before this change, an unregistered
// provider only failed once a real request happened to route to that
// deployment — this proves it now fails at startup instead.
func TestBuildPipelineRejectsUnregisteredProvider(t *testing.T) {
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "typo-primary",
				Model:         "gpt-4o",
				Provider:      "openia", // deliberate typo — not a registered adapter
				UpstreamModel: "gpt-4o",
				BaseURL:       "http://unused",
				APIKeyEnv:     "UNUSED_API_KEY",
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := buildPipeline(cfg, logger)
	if err == nil {
		t.Fatal("buildPipeline with an unregistered provider returned nil error, want an error")
	}
}

// TestBuildPipelineAcceptsEveryRealRegisteredProvider proves the
// negative case's counterpart: every provider this codebase actually
// registers an adapter for must still build cleanly, so the new check
// never false-positives against a real, valid config.
func TestBuildPipelineAcceptsEveryRealRegisteredProvider(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic", "gemini", "bedrock", "openaicompat"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv("UNUSED_API_KEY", "fake-key-not-a-real-secret")
			t.Setenv("UNUSED_ACCESS_KEY_ID", "fake-access-key-not-a-real-secret")
			t.Setenv("UNUSED_SECRET_ACCESS_KEY", "fake-secret-key-not-a-real-secret")

			dep := controlplane.DeploymentConfig{
				Name:          provider + "-primary",
				Model:         "some-model",
				Provider:      provider,
				UpstreamModel: "some-model",
				BaseURL:       "http://unused",
				APIKeyEnv:     "UNUSED_API_KEY",
			}
			if provider == "bedrock" {
				dep.AccessKeyIDEnv = "UNUSED_ACCESS_KEY_ID"
				dep.SecretAccessKeyEnv = "UNUSED_SECRET_ACCESS_KEY"
				dep.Region = "us-east-1"
			}

			cfg := &controlplane.Config{
				ListenAddr: ":0",
				VirtualKeys: []controlplane.VirtualKeyConfig{
					{Name: "test-key", KeyHash: testKeyHash("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
				},
				Deployments: []controlplane.DeploymentConfig{dep},
			}

			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			if _, err := buildPipeline(cfg, logger); err != nil {
				t.Fatalf("buildPipeline(%q): %v", provider, err)
			}
		})
	}
}
