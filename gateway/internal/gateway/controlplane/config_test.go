package controlplane

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shopspring/decimal"
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
	if !alpha.BudgetUSD.Equal(decimal.RequireFromString("100.0")) {
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
	if !beta.BudgetUSD.IsZero() {
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
	if !priceGPT.PromptPerToken.Equal(decimal.RequireFromString("0.0000025")) || !priceGPT.CompletionPerToken.Equal(decimal.RequireFromString("0.00001")) {
		t.Errorf("gpt-4o price = %+v", priceGPT)
	}

	if cfg.Telemetry.Exporter != "stdout" {
		t.Errorf("Telemetry.Exporter = %q, want %q", cfg.Telemetry.Exporter, "stdout")
	}
}

// TestLoadWithoutTelemetrySectionDefaultsToZeroValue proves the
// telemetry: section is genuinely optional — a config that omits it
// entirely must still load successfully, with Config.Telemetry left at
// its zero value (internal/telemetry.Init, not this package, is
// responsible for turning "" into "stdout").
func TestLoadWithoutTelemetrySectionDefaultsToZeroValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with no telemetry section: %v", err)
	}
	if cfg.Telemetry != (TelemetryConfig{}) {
		t.Errorf("Telemetry = %+v, want the zero value", cfg.Telemetry)
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

// TestLoadBudgetUSDBareDigitIsNotMisreadAsBool is the load-bearing
// regression test for the real bug documented in
// docs/rfcs/2026-09-02-decimal-cost-accounting.md's Motivation: an
// earlier version of this parser used strconv.ParseBool for boolean
// detection, which also accepts "0"/"1" as valid booleans. A config line
// like "budget_usd: 1" would silently parse as the bool true, fail
// getDecimal's type switch, and fall back to decimal.Zero — which
// internal/budget.Tracker's own convention treats as "unlimited" budget.
// A one-digit budget cap must never silently become no cap at all.
func TestLoadBudgetUSDBareDigitIsNotMisreadAsBool(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\n    budget_usd: 1\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.VirtualKeys) != 1 {
		t.Fatalf("len(VirtualKeys) = %d, want 1", len(cfg.VirtualKeys))
	}
	got := cfg.VirtualKeys[0].BudgetUSD
	want := decimal.RequireFromString("1")
	if !got.Equal(want) {
		t.Fatalf("BudgetUSD = %v, want %v — a bare \"1\" must parse as the decimal 1, not collide with boolean true and silently fall back to 0 (unlimited)", got, want)
	}
}

// TestLoadNumericBooleanLiteralsStillParseCorrectly proves the parser fix
// (explicit true/false matching instead of strconv.ParseBool) doesn't
// regress genuine boolean fields — allowed_models' "true" values and
// rate_limit's numeric burst/refill fields must behave identically to
// before this change.
func TestLoadNumericBooleanLiteralsStillParseCorrectly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\n" +
		"virtual_keys:\n" +
		"  team-alpha:\n" +
		"    key_hash: \"aa\"\n" +
		"    rate_limit:\n" +
		"      burst: 20\n" +
		"      refill_per_second: 10\n" +
		"    allowed_models:\n" +
		"      gpt-4o: true\n" +
		"      gpt-4o-mini: false\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"m\"\n" +
		"    provider: \"openai\"\n" +
		"    upstream_model: \"m\"\n" +
		"    base_url: \"https://x\"\n" +
		"    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	vk := cfg.VirtualKeys[0]
	if vk.RateLimitBurst != 20 || vk.RateLimitRefill != 10 {
		t.Errorf("rate limit = burst=%v refill=%v, want 20/10", vk.RateLimitBurst, vk.RateLimitRefill)
	}
	if len(vk.AllowedModels) != 1 || vk.AllowedModels[0] != "gpt-4o" {
		t.Errorf("AllowedModels = %v, want exactly [gpt-4o] (gpt-4o-mini: false must be excluded)", vk.AllowedModels)
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
