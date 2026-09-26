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

// TestBuildPipelineParsesIPv6AllowedSourceCIDR closes a real, small gap:
// buildPipeline's own net.ParseCIDR conversion loop
// (VirtualKeyConfig.AllowedSourceCIDRs []string ->
// identity.VirtualKey.AllowedSourceCIDRs []*net.IPNet) had zero IPv6
// coverage anywhere in this codebase -- only
// TestLoadParsesAllowedSourceCIDRs' own IPv4-only strings
// (controlplane/config_test.go), and that test only proves the raw
// YAML-to-string-list collection step, never this package's own actual
// net.ParseCIDR conversion. A real IPv6 CIDR string must build cleanly
// here exactly like an IPv4 one already does.
func TestBuildPipelineParsesIPv6AllowedSourceCIDR(t *testing.T) {
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{
				Name: "test-key", KeyHash: testKeyHash("test-key"), RateLimitBurst: 100, RateLimitRefill: 100,
				AllowedSourceCIDRs: []string{"2001:db8::/32"},
			},
		},
		Deployments: []controlplane.DeploymentConfig{
			{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", APIKeyEnv: "UNUSED_API_KEY"},
		},
	}
	t.Setenv("UNUSED_API_KEY", "fake-key-not-a-real-secret")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := buildPipeline(cfg, logger); err != nil {
		t.Fatalf("buildPipeline with a real IPv6 CIDR string: %v", err)
	}
}

// TestBuildPipelineRejectsMalformedAllowedSourceCIDR proves this
// package's own doc comment's claim ("a malformed CIDR string is a
// config-load-time failure, loud, aborts startup, never a
// silently-ignored entry that would quietly weaken this key's own
// allowlist") for real, for both an IPv4-shaped and an IPv6-shaped
// malformed string -- not previously exercised by any test.
func TestBuildPipelineRejectsMalformedAllowedSourceCIDR(t *testing.T) {
	for _, malformed := range []string{"not-a-cidr", "2001:db8::/not-a-prefix"} {
		t.Run(malformed, func(t *testing.T) {
			cfg := &controlplane.Config{
				ListenAddr: ":0",
				VirtualKeys: []controlplane.VirtualKeyConfig{
					{
						Name: "test-key", KeyHash: testKeyHash("test-key"), RateLimitBurst: 100, RateLimitRefill: 100,
						AllowedSourceCIDRs: []string{malformed},
					},
				},
				Deployments: []controlplane.DeploymentConfig{
					{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", APIKeyEnv: "UNUSED_API_KEY"},
				},
			}
			t.Setenv("UNUSED_API_KEY", "fake-key-not-a-real-secret")

			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			if _, err := buildPipeline(cfg, logger); err == nil {
				t.Fatalf("buildPipeline with malformed allowed_source_cidrs entry %q returned nil error, want an error", malformed)
			}
		})
	}
}
