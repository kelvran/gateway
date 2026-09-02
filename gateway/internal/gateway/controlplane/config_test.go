package controlplane

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadExampleConfig is a real round-trip test against the checked-in
// config.example.yaml, proving the hand-rolled YAML-subset parser
// actually parses this config's shape correctly, not just a synthetic
// fixture.
func TestLoadExampleConfig(t *testing.T) {
	// gateway/config.example.yaml, relative to this package's directory.
	path := filepath.Join("..", "..", "..", "config.example.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s): %v", path, err)
	}

	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, ":8080")
	}
	if cfg.APIKeyEnv != "KELVRAN_GATEWAY_API_KEY" {
		t.Errorf("APIKeyEnv = %q, want %q", cfg.APIKeyEnv, "KELVRAN_GATEWAY_API_KEY")
	}
	if len(cfg.Deployments) != 2 {
		t.Fatalf("len(Deployments) = %d, want 2", len(cfg.Deployments))
	}

	byName := map[string]DeploymentConfig{}
	for _, d := range cfg.Deployments {
		byName[d.Name] = d
	}

	openaiDep, ok := byName["gpt4o-primary"]
	if !ok {
		t.Fatal("missing deployment \"gpt4o-primary\"")
	}
	if openaiDep.Model != "gpt-4o" || openaiDep.Provider != "openai" || openaiDep.UpstreamModel != "gpt-4o" {
		t.Errorf("gpt4o-primary = %+v, unexpected fields", openaiDep)
	}
	if openaiDep.BaseURL != "https://api.openai.com/v1/chat/completions" {
		t.Errorf("gpt4o-primary.BaseURL = %q", openaiDep.BaseURL)
	}
	if openaiDep.APIKeyEnv != "OPENAI_API_KEY" {
		t.Errorf("gpt4o-primary.APIKeyEnv = %q, want %q", openaiDep.APIKeyEnv, "OPENAI_API_KEY")
	}

	anthropicDep, ok := byName["claude-opus-primary"]
	if !ok {
		t.Fatal("missing deployment \"claude-opus-primary\"")
	}
	if anthropicDep.UpstreamModel != "claude-opus-4-20250514" {
		t.Errorf("claude-opus-primary.UpstreamModel = %q", anthropicDep.UpstreamModel)
	}

	priceGPT, ok := cfg.PriceTable["gpt-4o"]
	if !ok {
		t.Fatal("missing price_table entry \"gpt-4o\"")
	}
	if priceGPT.PromptPerToken != 0.0000025 || priceGPT.CompletionPerToken != 0.00001 {
		t.Errorf("gpt-4o price = %+v", priceGPT)
	}
}

func TestLoadMissingRequiredField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// Missing api_key_env.
	content := "listen_addr: \":8080\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with missing api_key_env returned nil error")
	}
}

func TestLoadRejectsDeploymentMissingFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\napi_key_env: \"K\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with incomplete deployment returned nil error")
	}
}
