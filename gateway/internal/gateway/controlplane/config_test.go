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

	if len(cfg.VirtualKeys) != 2 {
		t.Fatalf("len(VirtualKeys) = %d, want 2", len(cfg.VirtualKeys))
	}
	vkByName := map[string]VirtualKeyConfig{}
	for _, vk := range cfg.VirtualKeys {
		vkByName[vk.Name] = vk
	}
	alpha, ok := vkByName["team-alpha"]
	if !ok {
		t.Fatal("missing virtual key \"team-alpha\"")
	}
	if alpha.KeyHash != "6701a1ecc6b08958fa24e13f267aac7233d47f390e92e71f8cc8fb3144672cf1" {
		t.Errorf("team-alpha.KeyHash = %q", alpha.KeyHash)
	}
	if alpha.BudgetUSD != 100.0 {
		t.Errorf("team-alpha.BudgetUSD = %v, want 100.0", alpha.BudgetUSD)
	}
	if alpha.RateLimitBurst != 20 || alpha.RateLimitRefill != 10 {
		t.Errorf("team-alpha rate limit = burst=%v refill=%v, want 20/10", alpha.RateLimitBurst, alpha.RateLimitRefill)
	}
	wantModels := []string{"claude-opus-4", "gpt-4o"}
	if len(alpha.AllowedModels) != len(wantModels) {
		t.Fatalf("team-alpha.AllowedModels = %v, want %v", alpha.AllowedModels, wantModels)
	}
	for i, m := range wantModels {
		if alpha.AllowedModels[i] != m {
			t.Errorf("team-alpha.AllowedModels[%d] = %q, want %q", i, alpha.AllowedModels[i], m)
		}
	}

	beta, ok := vkByName["team-beta"]
	if !ok {
		t.Fatal("missing virtual key \"team-beta\"")
	}
	if beta.BudgetUSD != 0 {
		t.Errorf("team-beta.BudgetUSD = %v, want 0 (unlimited)", beta.BudgetUSD)
	}
	if len(beta.AllowedModels) != 0 {
		t.Errorf("team-beta.AllowedModels = %v, want empty (all models allowed)", beta.AllowedModels)
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
	// Missing virtual_keys entirely.
	content := "listen_addr: \":8080\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with missing virtual_keys returned nil error")
	}
}

func TestLoadRejectsVirtualKeyMissingHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    budget_usd: 10.0\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with a virtual key missing key_hash returned nil error")
	}
}

func TestLoadRejectsDeploymentMissingFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with incomplete deployment returned nil error")
	}
}
