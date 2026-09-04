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

	if len(cfg.Deployments) != 3 {
		t.Fatalf("len(Deployments) = %d, want 3", len(cfg.Deployments))
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

	geminiDep, ok := byName["gemini-flash-primary"]
	if !ok {
		t.Fatal("missing deployment \"gemini-flash-primary\"")
	}
	if geminiDep.Model != "gemini-2.5-flash" || geminiDep.Provider != "gemini" || geminiDep.UpstreamModel != "gemini-2.5-flash" {
		t.Errorf("gemini-flash-primary = %+v, unexpected fields", geminiDep)
	}
	if geminiDep.BaseURL != "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Errorf("gemini-flash-primary.BaseURL = %q", geminiDep.BaseURL)
	}
	if geminiDep.APIKeyEnv != "GEMINI_API_KEY" {
		t.Errorf("gemini-flash-primary.APIKeyEnv = %q, want %q", geminiDep.APIKeyEnv, "GEMINI_API_KEY")
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
	if cfg.Budget != (BudgetConfig{}) {
		t.Errorf("Budget = %+v, want the zero value", cfg.Budget)
	}
	if cfg.RateLimit != (RateLimitConfig{}) {
		t.Errorf("RateLimit = %+v, want the zero value", cfg.RateLimit)
	}
	if cfg.Cache != (CacheConfig{}) {
		t.Errorf("Cache = %+v, want the zero value", cfg.Cache)
	}
}

// TestLoadCacheSectionParsesL1AndNestedL2 proves the cache: section,
// including its nested l2: and l3: sub-sections, is parsed correctly — the
// mirror-image proof to TestLoadWithoutTelemetrySectionDefaultsToZeroValue's
// "genuinely optional" proof above, per
// docs/rfcs/2026-09-03-cache-l2-normalized-match.md and
// docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md.
func TestLoadCacheSectionParsesL1AndNestedL2(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\ncache:\n  ttl_seconds: 300\n  max_entries: 5000\n  l2:\n    ttl_seconds: 75\n    max_entries: 2000\n  l3:\n    ttl_seconds: 300\n    max_entries: 1000\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with a cache section: %v", err)
	}
	if cfg.Cache.TTLSeconds != 300 {
		t.Errorf("Cache.TTLSeconds = %d, want 300", cfg.Cache.TTLSeconds)
	}
	if cfg.Cache.MaxEntries != 5000 {
		t.Errorf("Cache.MaxEntries = %d, want 5000", cfg.Cache.MaxEntries)
	}
	if cfg.Cache.L2.TTLSeconds != 75 {
		t.Errorf("Cache.L2.TTLSeconds = %d, want 75", cfg.Cache.L2.TTLSeconds)
	}
	if cfg.Cache.L2.MaxEntries != 2000 {
		t.Errorf("Cache.L2.MaxEntries = %d, want 2000", cfg.Cache.L2.MaxEntries)
	}
	if cfg.Cache.L3.TTLSeconds != 300 {
		t.Errorf("Cache.L3.TTLSeconds = %d, want 300", cfg.Cache.L3.TTLSeconds)
	}
	if cfg.Cache.L3.MaxEntries != 1000 {
		t.Errorf("Cache.L3.MaxEntries = %d, want 1000", cfg.Cache.L3.MaxEntries)
	}
}

// TestLoadGuardrailsSectionParsesPolicyVersionAndOverrides proves the
// guardrails: section, when present, is parsed correctly, per
// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md.
func TestLoadGuardrailsSectionParsesPolicyVersionAndOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nguardrails:\n  policy_version: \"v2\"\n  category_overrides:\n    contact_info: \"block\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with a guardrails section: %v", err)
	}
	if cfg.Guardrails.PolicyVersion != "v2" {
		t.Errorf("Guardrails.PolicyVersion = %q, want %q", cfg.Guardrails.PolicyVersion, "v2")
	}
	if got := cfg.Guardrails.CategoryOverrides["contact_info"]; got != "block" {
		t.Errorf(`Guardrails.CategoryOverrides["contact_info"] = %q, want "block"`, got)
	}
}

// TestLoadWithoutGuardrailsSectionDefaultsToZeroValue is the mirror-image
// "genuinely optional" proof: a config with no guardrails: section at
// all must parse successfully with the zero value, never an error.
func TestLoadWithoutGuardrailsSectionDefaultsToZeroValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load without a guardrails section: %v", err)
	}
	if cfg.Guardrails.PolicyVersion != "" {
		t.Errorf("Guardrails.PolicyVersion = %q, want empty (zero value)", cfg.Guardrails.PolicyVersion)
	}
	if len(cfg.Guardrails.CategoryOverrides) != 0 {
		t.Errorf("Guardrails.CategoryOverrides = %v, want empty", cfg.Guardrails.CategoryOverrides)
	}
}

// TestLoadRateLimitSectionParsesRedisAddr proves the rate_limit: section,
// when present, is parsed correctly — the mirror-image proof to
// TestLoadWithoutTelemetrySectionDefaultsToZeroValue's "genuinely
// optional" proof above.
func TestLoadRateLimitSectionParsesRedisAddr(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nrate_limit:\n  redis_addr: \"localhost:6379\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with a rate_limit section: %v", err)
	}
	if cfg.RateLimit.RedisAddr != "localhost:6379" {
		t.Errorf("RateLimit.RedisAddr = %q, want %q", cfg.RateLimit.RedisAddr, "localhost:6379")
	}
}

// TestLoadBudgetSectionParsesPersistPath proves the budget: section, when
// present, is parsed correctly — the mirror-image proof to
// TestLoadWithoutTelemetrySectionDefaultsToZeroValue's "genuinely
// optional" proof above.
func TestLoadBudgetSectionParsesPersistPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nbudget:\n  persist_path: \"kelvran-budget.db\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with a budget section: %v", err)
	}
	if cfg.Budget.PersistPath != "kelvran-budget.db" {
		t.Errorf("Budget.PersistPath = %q, want %q", cfg.Budget.PersistPath, "kelvran-budget.db")
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

func minimalDeploymentConfig(extraDeploymentLines string) string {
	return "listen_addr: \":8080\"\n" +
		"virtual_keys:\n" +
		"  team-alpha:\n" +
		"    key_hash: \"aa\"\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"m\"\n" +
		"    provider: \"openai\"\n" +
		"    upstream_model: \"m\"\n" +
		"    base_url: \"https://x\"\n" +
		"    api_key_env: \"X\"\n" +
		extraDeploymentLines
}

// TestLoadDeploymentWeightUnsetDefaultsToZero proves a deployment with no
// weight key parses to Weight: 0 — the "unset" sentinel router.New
// normalizes to 1, not something this package resolves itself, per
// docs/rfcs/2026-09-04-weighted-routing.md.
func TestLoadDeploymentWeightUnsetDefaultsToZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig("")), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Deployments) != 1 {
		t.Fatalf("len(Deployments) = %d, want 1", len(cfg.Deployments))
	}
	if got := cfg.Deployments[0].Weight; got != 0 {
		t.Errorf("Weight = %d, want 0 (unset)", got)
	}
}

// TestLoadDeploymentWeightParsesPositiveValue proves an explicit weight
// key parses through.
func TestLoadDeploymentWeightParsesPositiveValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig("    weight: 3\n")), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Deployments[0].Weight; got != 3 {
		t.Errorf("Weight = %d, want 3", got)
	}
}

// TestLoadRejectsNegativeDeploymentWeight proves a negative weight is a
// real config error, never silently clamped.
func TestLoadRejectsNegativeDeploymentWeight(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig("    weight: -1\n")), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with a negative deployment weight returned nil error")
	}
}
