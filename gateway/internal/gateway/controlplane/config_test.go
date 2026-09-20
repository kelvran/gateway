package controlplane

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	if alpha.BudgetResetIntervalSeconds != 2592000 {
		t.Errorf("team-alpha.BudgetResetIntervalSeconds = %d, want 2592000 (30 days)", alpha.BudgetResetIntervalSeconds)
	}
	if alpha.BudgetWarnPercent != 0.8 {
		t.Errorf("team-alpha.BudgetWarnPercent = %v, want 0.8", alpha.BudgetWarnPercent)
	}
	if alpha.RateLimitBurst != 20 || alpha.RateLimitRefill != 10 {
		t.Errorf("team-alpha rate limit = burst=%v refill=%v, want 20/10", alpha.RateLimitBurst, alpha.RateLimitRefill)
	}
	if alpha.TPMCapacity != 100000 || alpha.TPMRefillPerSecond != 1000 {
		t.Errorf("team-alpha TPM rate limit = capacity=%v refill=%v, want 100000/1000", alpha.TPMCapacity, alpha.TPMRefillPerSecond)
	}
	if len(alpha.PerModelRateLimits) != 1 {
		t.Fatalf("len(team-alpha.PerModelRateLimits) = %d, want 1", len(alpha.PerModelRateLimits))
	}
	if gpt4o := alpha.PerModelRateLimits["gpt-4o"]; gpt4o.Burst != 5 || gpt4o.RefillPerSecond != 1 || gpt4o.TPMCapacity != 50000 || gpt4o.TPMRefillPerSecond != 500 {
		t.Errorf("team-alpha.PerModelRateLimits[gpt-4o] = %+v, want {Burst:5 RefillPerSecond:1 TPMCapacity:50000 TPMRefillPerSecond:500}", gpt4o)
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

	if len(cfg.Deployments) != 4 {
		t.Fatalf("len(Deployments) = %d, want 4", len(cfg.Deployments))
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
	if got := openaiDep.FallbackChains["context_window_exceeded"]; len(got) != 1 || got[0] != "claude-opus-primary" {
		t.Errorf("gpt4o-primary.FallbackChains[context_window_exceeded] = %v, want [claude-opus-primary]", got)
	}
	if got := openaiDep.FallbackChains["content_policy"]; len(got) != 2 || got[0] != "claude-opus-primary" || got[1] != "gemini-flash-primary" {
		t.Errorf("gpt4o-primary.FallbackChains[content_policy] = %v, want [claude-opus-primary gemini-flash-primary]", got)
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

	bedrockDep, ok := byName["claude-bedrock-primary"]
	if !ok {
		t.Fatal("missing deployment \"claude-bedrock-primary\"")
	}
	if bedrockDep.Provider != "bedrock" || bedrockDep.UpstreamModel != "anthropic.claude-3-5-sonnet-20241022-v2:0" {
		t.Errorf("claude-bedrock-primary = %+v, unexpected fields", bedrockDep)
	}
	if bedrockDep.APIKeyEnv != "" {
		t.Errorf("claude-bedrock-primary.APIKeyEnv = %q, want empty for bedrock", bedrockDep.APIKeyEnv)
	}
	if bedrockDep.AccessKeyIDEnv != "AWS_ACCESS_KEY_ID" || bedrockDep.SecretAccessKeyEnv != "AWS_SECRET_ACCESS_KEY" {
		t.Errorf("claude-bedrock-primary credential env fields = %+v, unexpected", bedrockDep)
	}
	if bedrockDep.Region != "us-east-1" {
		t.Errorf("claude-bedrock-primary.Region = %q, want %q", bedrockDep.Region, "us-east-1")
	}

	priceGPT, ok := cfg.PriceTable["gpt-4o"]
	if !ok {
		t.Fatal("missing price_table entry \"gpt-4o\"")
	}
	if !priceGPT.PromptPerToken.Equal(decimal.RequireFromString("0.0000025")) || !priceGPT.CompletionPerToken.Equal(decimal.RequireFromString("0.00001")) {
		t.Errorf("gpt-4o price = %+v", priceGPT)
	}

	// claude-fable-5-1's cache_read_per_token must be exactly 0.025x its own
	// prompt_per_token -- Anthropic's real, model-specific discounted
	// cache-read tier (not the standard 0.1x every other model, including
	// claude-opus-4 above, uses), per
	// docs/upgrade-research/cache-provider-native-caching-audit-round4-2026-09-11.md's
	// Finding 5.
	priceFable, ok := cfg.PriceTable["claude-fable-5-1"]
	if !ok {
		t.Fatal("missing price_table entry \"claude-fable-5-1\"")
	}
	wantCacheRead := priceFable.PromptPerToken.Mul(decimal.RequireFromString("0.025"))
	if priceFable.CacheReadPerToken == nil || !priceFable.CacheReadPerToken.Equal(wantCacheRead) {
		t.Errorf("claude-fable-5-1.CacheReadPerToken = %v, want %v (0.025x PromptPerToken)", priceFable.CacheReadPerToken, wantCacheRead)
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
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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

// TestLoadPriceTableParsesCacheTokenRates proves the cache-token
// cost-accounting fix's config surface: cache_read_per_token/
// cache_creation_per_token, when present under a price_table entry, are
// parsed into non-nil pointers -- distinguishable from the omitted case
// below, which must resolve to nil (unset), never a bare decimal.Zero.
func TestLoadPriceTableParsesCacheTokenRates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nprice_table:\n  claude-opus-4:\n    prompt_per_token: 0.000015\n    completion_per_token: 0.000075\n    cache_read_per_token: 0.0000003\n    cache_creation_per_token: 0.00001\n  gpt-4o:\n    prompt_per_token: 0.0000025\n    completion_per_token: 0.00001\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with a price_table section: %v", err)
	}

	opus, ok := cfg.PriceTable["claude-opus-4"]
	if !ok {
		t.Fatal("missing price_table entry \"claude-opus-4\"")
	}
	if opus.CacheReadPerToken == nil {
		t.Fatal("claude-opus-4.CacheReadPerToken = nil, want a set rate")
	}
	if !opus.CacheReadPerToken.Equal(decimal.RequireFromString("0.0000003")) {
		t.Errorf("claude-opus-4.CacheReadPerToken = %v, want 0.0000003", *opus.CacheReadPerToken)
	}
	if opus.CacheCreationPerToken == nil {
		t.Fatal("claude-opus-4.CacheCreationPerToken = nil, want a set rate")
	}
	if !opus.CacheCreationPerToken.Equal(decimal.RequireFromString("0.00001")) {
		t.Errorf("claude-opus-4.CacheCreationPerToken = %v, want 0.00001", *opus.CacheCreationPerToken)
	}

	gpt4o, ok := cfg.PriceTable["gpt-4o"]
	if !ok {
		t.Fatal("missing price_table entry \"gpt-4o\"")
	}
	if gpt4o.CacheReadPerToken != nil {
		t.Errorf("gpt-4o.CacheReadPerToken = %v, want nil (unset -- omitted from config)", *gpt4o.CacheReadPerToken)
	}
	if gpt4o.CacheCreationPerToken != nil {
		t.Errorf("gpt-4o.CacheCreationPerToken = %v, want nil (unset -- omitted from config)", *gpt4o.CacheCreationPerToken)
	}
}

// TestLoadRejectsPriceTableEntryMissingPromptPerToken proves a real,
// live-discovered gap: prompt_per_token/completion_per_token are
// REQUIRED fields (costaccounting.ModelPrice's own doc comment says so —
// only the two cache-rate fields are optional/pointer-typed), but getDecimal
// silently returns (decimal.Zero, false) for a missing key, and until this
// fix Load() discarded that `false` and proceeded as if 0 were a real,
// intentional rate. A typo'd key name (e.g. "prompt_per_tokn") must fail
// config load loudly, never silently price every prompt token at $0.
func TestLoadRejectsPriceTableEntryMissingPromptPerToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nprice_table:\n  gpt-4o:\n    completion_per_token: 0.00001\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with a price_table entry missing prompt_per_token returned nil error, want a real config error")
	}
}

// TestLoadRejectsPriceTableEntryMalformedCompletionPerToken proves the
// same gap for a PRESENT but unparseable value (e.g. a quoted currency
// symbol) — getDecimal returns the identical (decimal.Zero, false) for
// this case as for a missing key, so it must be rejected the same way.
func TestLoadRejectsPriceTableEntryMalformedCompletionPerToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nprice_table:\n  gpt-4o:\n    prompt_per_token: 0.0000025\n    completion_per_token: \"$0.00001\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with a malformed completion_per_token returned nil error, want a real config error")
	}
}

// TestLoadRejectsNegativePriceTableRate proves the second real gap:
// no parsed price_table rate was ever checked for sign, so a fat-fingered
// negative rate (e.g. a rebate figure copy-pasted without flipping its
// sign) parsed successfully and would have silently driven
// costaccounting.Calculate's returned cost negative -- which
// budget.Tracker.Reconcile then treats as an entirely non-billable
// request (Sign() >= 0 check), permanently distorting that key's
// historical-average reservation sizing for every future request.
// Covers one base-rate field and one cache-rate field, proving both of
// the two separate code paths (unconditional assignment vs. the
// ok-gated pointer assignment) are guarded.
func TestLoadRejectsNegativePriceTableRate(t *testing.T) {
	tests := []struct {
		name       string
		priceBlock string
	}{
		{"negative prompt_per_token", "    prompt_per_token: -0.0000025\n    completion_per_token: 0.00001\n"},
		{"negative completion_per_token", "    prompt_per_token: 0.0000025\n    completion_per_token: -0.00001\n"},
		{"negative cache_read_per_token", "    prompt_per_token: 0.0000025\n    completion_per_token: 0.00001\n    cache_read_per_token: -0.0000003\n"},
		{"negative cache_creation_per_token", "    prompt_per_token: 0.0000025\n    completion_per_token: 0.00001\n    cache_creation_per_token: -0.00001\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nprice_table:\n  gpt-4o:\n" + tt.priceBlock + "deployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			if _, err := Load(path); err == nil {
				t.Fatalf("Load with %s returned nil error, want a real config error", tt.name)
			}
		})
	}
}

// TestLoadCacheSectionParsesJitterFraction proves the new
// jitter_fraction key (L1, L2, and L3), per
// docs/rfcs/2026-09-10-gateway-cache-ttl-jitter.md, is parsed correctly
// -- mirroring TestLoadCacheSectionParsesL1AndNestedL2's own convention.
func TestLoadCacheSectionParsesJitterFraction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\ncache:\n  ttl_seconds: 300\n  jitter_fraction: 0.15\n  l2:\n    ttl_seconds: 75\n    jitter_fraction: 0.2\n  l3:\n    ttl_seconds: 300\n    jitter_fraction: 0.05\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with cache jitter_fraction keys: %v", err)
	}
	if cfg.Cache.JitterFraction != 0.15 {
		t.Errorf("Cache.JitterFraction = %v, want 0.15", cfg.Cache.JitterFraction)
	}
	if cfg.Cache.L2.JitterFraction != 0.2 {
		t.Errorf("Cache.L2.JitterFraction = %v, want 0.2", cfg.Cache.L2.JitterFraction)
	}
	if cfg.Cache.L3.JitterFraction != 0.05 {
		t.Errorf("Cache.L3.JitterFraction = %v, want 0.05", cfg.Cache.L3.JitterFraction)
	}
}

// TestLoadCacheSectionWithoutJitterFractionDefaultsToZero proves
// omitting jitter_fraction leaves Config.Cache.JitterFraction at its
// zero value -- resolveJitterFraction (cmd/gateway/main.go), not this
// package, is responsible for turning that into the real 10% default,
// mirroring TTLSeconds/MaxEntries's own resolution-happens-elsewhere
// convention.
func TestLoadCacheSectionWithoutJitterFractionDefaultsToZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\ncache:\n  ttl_seconds: 300\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load without jitter_fraction: %v", err)
	}
	if cfg.Cache.JitterFraction != 0 {
		t.Errorf("Cache.JitterFraction = %v, want 0 (absent -- resolved elsewhere)", cfg.Cache.JitterFraction)
	}
}

// TestLoadGuardrailsSectionParsesPolicyVersionAndOverrides proves the
// guardrails: section, when present, is parsed correctly, per
// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md.
func TestLoadGuardrailsSectionParsesPolicyVersionAndOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nguardrails:\n  policy_version: \"v2\"\n  category_overrides:\n    contact_info: \"block\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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

// TestLoadGuardrailsBedrockGuardrailsSectionParsesAllFields proves the
// optional guardrails.bedrock_guardrails: sub-section, when present, is
// parsed correctly, per
// docs/rfcs/2026-09-13-gateway-bedrock-guardrails-ml-detector-design.md.
func TestLoadGuardrailsBedrockGuardrailsSectionParsesAllFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nguardrails:\n  bedrock_guardrails:\n    region: \"us-east-1\"\n    access_key_id_env: \"AWS_ACCESS_KEY_ID\"\n    secret_access_key_env: \"AWS_SECRET_ACCESS_KEY\"\n    guardrail_id: \"gr-abc123\"\n    guardrail_version: \"1\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with a guardrails.bedrock_guardrails section: %v", err)
	}
	bg := cfg.Guardrails.BedrockGuardrails
	if bg == nil {
		t.Fatal("Guardrails.BedrockGuardrails = nil, want a populated *BedrockGuardrailsConfig")
	}
	if bg.Region != "us-east-1" || bg.AccessKeyIDEnv != "AWS_ACCESS_KEY_ID" || bg.SecretAccessKeyEnv != "AWS_SECRET_ACCESS_KEY" || bg.GuardrailID != "gr-abc123" || bg.GuardrailVersion != "1" {
		t.Errorf("BedrockGuardrails = %+v, want all 5 fields populated from YAML", bg)
	}
}

// TestLoadWithoutBedrockGuardrailsSubsectionLeavesItNil proves the
// "genuinely optional" half: a guardrails: section present but without
// its own bedrock_guardrails: sub-key leaves BedrockGuardrails nil,
// reproducing today's regex-only detector set exactly.
func TestLoadWithoutBedrockGuardrailsSubsectionLeavesItNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nguardrails:\n  policy_version: \"v2\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load without a bedrock_guardrails sub-section: %v", err)
	}
	if cfg.Guardrails.BedrockGuardrails != nil {
		t.Errorf("Guardrails.BedrockGuardrails = %+v, want nil", cfg.Guardrails.BedrockGuardrails)
	}
}

// TestLoadBedrockGuardrailsMissingRequiredFieldErrors proves a partially-
// specified bedrock_guardrails: section fails loudly at load time, never
// silently constructing a Detector that can only ever error on every real
// call.
func TestLoadBedrockGuardrailsMissingRequiredFieldErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// Missing guardrail_version.
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nguardrails:\n  bedrock_guardrails:\n    region: \"us-east-1\"\n    access_key_id_env: \"AWS_ACCESS_KEY_ID\"\n    secret_access_key_env: \"AWS_SECRET_ACCESS_KEY\"\n    guardrail_id: \"gr-abc123\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with a missing guardrail_version returned nil error, want an error")
	}
}

// TestLoadGuardrailsEmbedSimSectionParsesAllFields proves the optional
// guardrails.embed_sim: sub-section, when present, is parsed correctly,
// mirroring TestLoadGuardrailsBedrockGuardrailsSectionParsesAllFields.
func TestLoadGuardrailsEmbedSimSectionParsesAllFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nguardrails:\n  embed_sim:\n    region: \"us-east-1\"\n    access_key_id_env: \"AWS_ACCESS_KEY_ID\"\n    secret_access_key_env: \"AWS_SECRET_ACCESS_KEY\"\n    similarity_threshold: 0.9\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with a guardrails.embed_sim section: %v", err)
	}
	es := cfg.Guardrails.EmbedSim
	if es == nil {
		t.Fatal("Guardrails.EmbedSim = nil, want a populated *EmbedSimConfig")
	}
	if es.Region != "us-east-1" || es.AccessKeyIDEnv != "AWS_ACCESS_KEY_ID" || es.SecretAccessKeyEnv != "AWS_SECRET_ACCESS_KEY" || es.SimilarityThreshold != 0.9 {
		t.Errorf("EmbedSim = %+v, want all 4 fields populated from YAML", es)
	}
}

// TestLoadGuardrailsEmbedSimDefaultsSimilarityThresholdWhenUnset proves
// SimilarityThreshold resolves to the documented 0.82 default when the
// YAML omits it, rather than the zero-value 0 (which would report a
// Finding on every possible input).
func TestLoadGuardrailsEmbedSimDefaultsSimilarityThresholdWhenUnset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nguardrails:\n  embed_sim:\n    region: \"us-east-1\"\n    access_key_id_env: \"AWS_ACCESS_KEY_ID\"\n    secret_access_key_env: \"AWS_SECRET_ACCESS_KEY\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with an embed_sim section omitting similarity_threshold: %v", err)
	}
	if cfg.Guardrails.EmbedSim == nil || cfg.Guardrails.EmbedSim.SimilarityThreshold != 0.82 {
		t.Errorf("EmbedSim.SimilarityThreshold = %v, want the 0.82 default", cfg.Guardrails.EmbedSim)
	}
}

// TestLoadWithoutEmbedSimSubsectionLeavesItNil proves the "genuinely
// optional" half: a guardrails: section present but without its own
// embed_sim: sub-key leaves EmbedSim nil, reproducing today's behavior
// exactly.
func TestLoadWithoutEmbedSimSubsectionLeavesItNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nguardrails:\n  policy_version: \"v2\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load without an embed_sim sub-section: %v", err)
	}
	if cfg.Guardrails.EmbedSim != nil {
		t.Errorf("Guardrails.EmbedSim = %+v, want nil", cfg.Guardrails.EmbedSim)
	}
}

// TestLoadEmbedSimMissingRequiredFieldErrors proves a partially-specified
// embed_sim: section fails loudly at load time, never silently
// constructing a Detector that can only ever error on every real call.
func TestLoadEmbedSimMissingRequiredFieldErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// Missing secret_access_key_env.
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nguardrails:\n  embed_sim:\n    region: \"us-east-1\"\n    access_key_id_env: \"AWS_ACCESS_KEY_ID\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with a missing secret_access_key_env returned nil error, want an error")
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
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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

// TestLoadBudgetResetIntervalSecondsUnsetDefaultsToZero proves a virtual
// key with no budget_reset_interval_seconds key parses to 0 — the
// existing "lifetime cap, never resets" default, exactly as before this
// field existed.
func TestLoadBudgetResetIntervalSecondsUnsetDefaultsToZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\n    budget_usd: 10\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.VirtualKeys[0].BudgetResetIntervalSeconds; got != 0 {
		t.Errorf("BudgetResetIntervalSeconds = %d, want 0 (unset)", got)
	}
}

// TestLoadBudgetResetIntervalSecondsParsesPositiveValue proves an
// explicit budget_reset_interval_seconds key parses through — e.g. a
// 30-day "monthly" rolling window as 2592000 seconds.
func TestLoadBudgetResetIntervalSecondsParsesPositiveValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\n    budget_usd: 10\n    budget_reset_interval_seconds: 2592000\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.VirtualKeys[0].BudgetResetIntervalSeconds; got != 2592000 {
		t.Errorf("BudgetResetIntervalSeconds = %d, want 2592000", got)
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
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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

// TestLoadParsesAllowedRegions mirrors TestLoadNumericBooleanLiteralsStillParseCorrectly's
// own allowed_models proof, for the new allowed_regions section, per
// docs/upgrade-research/data-residency-regional-routing-2026-09-15.md.
func TestLoadParsesAllowedRegions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\n" +
		"virtual_keys:\n" +
		"  team-alpha:\n" +
		"    key_hash: \"aa\"\n" +
		"    allowed_regions:\n" +
		"      eu-west-1: true\n" +
		"      us-east-1: false\n" +
		"  team-beta:\n" +
		"    key_hash: \"bb\"\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"m\"\n" +
		"    provider: \"openai\"\n" +
		"    upstream_model: \"m\"\n" +
		"    base_url: \"https://x\"\n" +
		"    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	alpha, beta := cfg.VirtualKeys[0], cfg.VirtualKeys[1]
	if len(alpha.AllowedRegions) != 1 || alpha.AllowedRegions[0] != "eu-west-1" {
		t.Errorf("team-alpha.AllowedRegions = %v, want exactly [eu-west-1] (us-east-1: false must be excluded)", alpha.AllowedRegions)
	}
	if len(beta.AllowedRegions) != 0 {
		t.Errorf("team-beta.AllowedRegions = %v, want empty (no constraint declared)", beta.AllowedRegions)
	}
}

func TestLoadRejectsDeploymentMissingFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig("")), 0o600); err != nil {
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
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig("    weight: 3\n")), 0o600); err != nil {
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
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig("    weight: -1\n")), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with a negative deployment weight returned nil error")
	}
}

// TestLoadDeploymentCostTierUnsetDefaultsToZero proves a deployment with
// no cost_tier key parses to CostTier: 0 — the "unset" sentinel
// router.Router treats as "no tier configured," per DECISIONS.md's
// [2026-09-12] entry, mirroring TestLoadDeploymentWeightUnsetDefaultsToZero's
// own convention.
func TestLoadDeploymentCostTierUnsetDefaultsToZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig("")), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Deployments[0].CostTier; got != 0 {
		t.Errorf("CostTier = %d, want 0 (unset)", got)
	}
}

// TestLoadDeploymentCostTierParsesPositiveValue proves an explicit
// cost_tier key parses through.
func TestLoadDeploymentCostTierParsesPositiveValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig("    cost_tier: 2\n")), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Deployments[0].CostTier; got != 2 {
		t.Errorf("CostTier = %d, want 2", got)
	}
}

// TestLoadRejectsNegativeDeploymentCostTier proves a negative cost_tier
// is a real config error, never silently clamped — mirroring
// TestLoadRejectsNegativeDeploymentWeight's own convention.
func TestLoadRejectsNegativeDeploymentCostTier(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig("    cost_tier: -1\n")), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with a negative deployment cost_tier returned nil error")
	}
}

// TestLoadDeploymentDisableCacheControlAutoPopulateUnsetDefaultsToFalse
// proves a deployment with no disable_cache_control_auto_populate key
// parses to false -- auto-populate stays ON, matching every deployment
// configured before this field existed, per
// docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md.
func TestLoadDeploymentDisableCacheControlAutoPopulateUnsetDefaultsToFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig("")), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Deployments[0].DisableCacheControlAutoPopulate; got {
		t.Errorf("DisableCacheControlAutoPopulate = %v, want false (unset -- auto-populate stays ON)", got)
	}
}

// TestLoadDeploymentDisableCacheControlAutoPopulateParsesTrue proves an
// explicit disable_cache_control_auto_populate: true key parses through
// -- the per-deployment opt-out, per
// docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md.
func TestLoadDeploymentDisableCacheControlAutoPopulateParsesTrue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := minimalDeploymentConfig("    disable_cache_control_auto_populate: true\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Deployments[0].DisableCacheControlAutoPopulate; !got {
		t.Errorf("DisableCacheControlAutoPopulate = %v, want true", got)
	}
}

// TestLoadDeploymentSharedAcrossTenantsUnsetDefaultsToFalse proves a
// deployment with no shared_across_tenants key parses to false —
// matching every deployment configured before this field existed, per
// docs/rfcs/2026-09-09-gateway-cache-shared-tenant-flag.md.
func TestLoadDeploymentSharedAcrossTenantsUnsetDefaultsToFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig("")), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Deployments[0].SharedAcrossTenants; got {
		t.Errorf("SharedAcrossTenants = %v, want false (unset -- not declared shared)", got)
	}
}

// TestLoadDeploymentSharedAcrossTenantsParsesTrue proves an explicit
// shared_across_tenants: true key parses through, per
// docs/rfcs/2026-09-09-gateway-cache-shared-tenant-flag.md.
func TestLoadDeploymentSharedAcrossTenantsParsesTrue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := minimalDeploymentConfig("    shared_across_tenants: true\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Deployments[0].SharedAcrossTenants; !got {
		t.Errorf("SharedAcrossTenants = %v, want true", got)
	}
}

// TestLoadDeploymentStickyUnsetDefaultsToFalse mirrors
// TestLoadDeploymentSharedAcrossTenantsUnsetDefaultsToFalse for the new
// sticky field -- every deployment configured before this feature
// existed must parse to false, participating only in plain WRR.
func TestLoadDeploymentStickyUnsetDefaultsToFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig("")), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Deployments[0].Sticky; got {
		t.Errorf("Sticky = %v, want false (unset -- plain WRR only)", got)
	}
}

// TestLoadDeploymentStickyParsesTrue mirrors
// TestLoadDeploymentSharedAcrossTenantsParsesTrue for the new sticky
// field.
func TestLoadDeploymentStickyParsesTrue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := minimalDeploymentConfig("    sticky: true\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Deployments[0].Sticky; !got {
		t.Errorf("Sticky = %v, want true", got)
	}
}

// TestLoadDeploymentKindUnsetDefaultsToChat proves a deployment with no
// kind key parses as "chat" -- every deployment configured before this
// field existed.
func TestLoadDeploymentKindUnsetDefaultsToChat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig("")), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Deployments[0].Kind; got != "chat" {
		t.Errorf("Kind = %q, want %q", got, "chat")
	}
}

// TestLoadDeploymentKindEmbeddingParsesForOpenAI proves the real,
// buildable case: an openai deployment with kind: embedding parses
// cleanly.
func TestLoadDeploymentKindEmbeddingParsesForOpenAI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := minimalDeploymentConfig("    kind: \"embedding\"\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Deployments[0].Kind; got != "embedding" {
		t.Errorf("Kind = %q, want %q", got, "embedding")
	}
}

// TestLoadRejectsAnthropicDeploymentConfiguredWithEmbeddingKind proves
// the config-time rejection: Anthropic has no native embeddings model at
// all (see adapter.EmbeddingAdapter's own doc comment), so kind:
// embedding on an anthropic deployment must fail to load, not surface as
// a runtime 501 on the first real request.
func TestLoadRejectsAnthropicDeploymentConfiguredWithEmbeddingKind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"anthropic\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n    kind: \"embedding\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with an anthropic deployment configured kind: embedding returned nil error, want a real error")
	}
}

// TestLoadRejectsEmbeddingDeploymentsSharingAModelWithDifferentProviders
// is the regression proof for a real gap an audit found: two Kind==
// "embedding" deployments sharing one canonical Model name but pointing
// at different providers (here openai vs. bedrock) can silently produce
// different output shape (dimensionality, batch support) depending on
// which one a WRR pick lands on -- rejected at config-load time, per
// validateEmbeddingModelGroupsAreProviderConsistent's own doc comment.
func TestLoadRejectsEmbeddingDeploymentsSharingAModelWithDifferentProviders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\n" +
		"virtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"shared-embed\"\n    provider: \"openai\"\n    upstream_model: \"text-embedding-3-small\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n    kind: \"embedding\"\n" +
		"  d2:\n" +
		"    model: \"shared-embed\"\n    provider: \"bedrock\"\n    upstream_model: \"amazon.titan-embed-text-v2:0\"\n    base_url: \"https://y\"\n    access_key_id_env: \"A\"\n    secret_access_key_env: \"S\"\n    region: \"us-east-1\"\n    kind: \"embedding\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with two embedding deployments sharing a model but different providers: got nil error, want a real error")
	}
}

// TestLoadRejectsEmbeddingDeploymentsSharingAModelWithDifferentUpstreamModels
// mirrors the provider-mismatch proof for the same-provider,
// different-upstream_model case (e.g. two distinct OpenAI embedding
// models with different native dimensionality sharing one canonical
// name) -- the same real hazard, a different specific cause.
func TestLoadRejectsEmbeddingDeploymentsSharingAModelWithDifferentUpstreamModels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\n" +
		"virtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"shared-embed\"\n    provider: \"openai\"\n    upstream_model: \"text-embedding-3-small\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n    kind: \"embedding\"\n" +
		"  d2:\n" +
		"    model: \"shared-embed\"\n    provider: \"openai\"\n    upstream_model: \"text-embedding-3-large\"\n    base_url: \"https://y\"\n    api_key_env: \"Y\"\n    kind: \"embedding\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with two embedding deployments sharing a model but different upstream_model: got nil error, want a real error")
	}
}

// TestLoadAllowsEmbeddingDeploymentsSharingAModelWithIdenticalProviderAndUpstreamModel
// is the negative proof: real redundancy (same provider, same
// upstream_model, different region/credentials/weight) across a shared
// canonical embedding model name must keep working -- this validation
// must never reject the legitimate case it's not about.
func TestLoadAllowsEmbeddingDeploymentsSharingAModelWithIdenticalProviderAndUpstreamModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\n" +
		"virtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"shared-embed\"\n    provider: \"bedrock\"\n    upstream_model: \"amazon.titan-embed-text-v2:0\"\n    base_url: \"https://x\"\n    access_key_id_env: \"A\"\n    secret_access_key_env: \"S\"\n    region: \"us-east-1\"\n    kind: \"embedding\"\n    weight: 3\n" +
		"  d2:\n" +
		"    model: \"shared-embed\"\n    provider: \"bedrock\"\n    upstream_model: \"amazon.titan-embed-text-v2:0\"\n    base_url: \"https://y\"\n    access_key_id_env: \"A\"\n    secret_access_key_env: \"S\"\n    region: \"us-west-2\"\n    kind: \"embedding\"\n    weight: 1\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with two identical-provider/upstream_model embedding deployments: %v", err)
	}
	if len(cfg.Deployments) != 2 {
		t.Fatalf("len(cfg.Deployments) = %d, want 2", len(cfg.Deployments))
	}
}

// TestLoadRejectsUnknownDeploymentKind proves an unrecognized kind value
// is a real config error, not silently treated as "chat."
func TestLoadRejectsUnknownDeploymentKind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := minimalDeploymentConfig("    kind: \"something-else\"\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with an unknown kind returned nil error, want a real error")
	}
}

// TestVirtualKeyConfigParsesOptionalBillingSubjectID proves
// billing_subject_id parses through when present.
func TestVirtualKeyConfigParsesOptionalBillingSubjectID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\n" +
		"virtual_keys:\n" +
		"  team-alpha:\n" +
		"    key_hash: \"aa\"\n" +
		"    billing_subject_id: \"cust_12345\"\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"m\"\n" +
		"    provider: \"openai\"\n" +
		"    upstream_model: \"m\"\n" +
		"    base_url: \"https://x\"\n" +
		"    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.VirtualKeys[0].BillingSubjectID; got != "cust_12345" {
		t.Errorf("BillingSubjectID = %q, want %q", got, "cust_12345")
	}
}

// TestBillingSubjectIDDefaultsEmptyWhenUnconfigured proves every virtual
// key configured before this field existed parses to "" -- no
// unexpected default value.
func TestBillingSubjectIDDefaultsEmptyWhenUnconfigured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\n" +
		"virtual_keys:\n" +
		"  team-alpha:\n" +
		"    key_hash: \"aa\"\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"m\"\n" +
		"    provider: \"openai\"\n" +
		"    upstream_model: \"m\"\n" +
		"    base_url: \"https://x\"\n" +
		"    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.VirtualKeys[0].BillingSubjectID; got != "" {
		t.Errorf("BillingSubjectID = %q, want empty", got)
	}
}

// TestLoadDeploymentFallbackChainsParsesOrderedCommaSeparatedLists proves
// each error-class key parses into an ORDERED slice (not just a set) —
// this file's YAML-subset parser has no list support, so fallback_chains
// values are comma-separated strings split at parse time, per
// docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md.
func TestLoadDeploymentFallbackChainsParsesOrderedCommaSeparatedLists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	extra := "    fallback_chains:\n" +
		"      content_policy: \"safety-alt\"\n" +
		"      context_window_exceeded: \"large-context-alt\"\n" +
		"      generic: \"hop-1, hop-2 , hop-3\"\n"
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig(extra)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	chains := cfg.Deployments[0].FallbackChains
	if got := chains["content_policy"]; len(got) != 1 || got[0] != "safety-alt" {
		t.Errorf("content_policy = %v, want [safety-alt]", got)
	}
	if got := chains["context_window_exceeded"]; len(got) != 1 || got[0] != "large-context-alt" {
		t.Errorf("context_window_exceeded = %v, want [large-context-alt]", got)
	}
	want := []string{"hop-1", "hop-2", "hop-3"}
	got := chains["generic"]
	if len(got) != len(want) {
		t.Fatalf("generic = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("generic = %v, want %v (in exact order, whitespace trimmed)", got, want)
		}
	}
}

// TestLoadDeploymentWithoutFallbackChainsLeavesFieldNil is the direct
// backward-compatibility proof at the parser level: a deployment with no
// fallback_chains section at all must parse to a nil map, never an
// empty-but-non-nil one — dataplane.fallbackTargets treats
// len(dep.FallbackChains) == 0 as "not configured," which nil and an
// empty map both satisfy, but this pins the parser's own literal output.
func TestLoadDeploymentWithoutFallbackChainsLeavesFieldNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig("")), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Deployments[0].FallbackChains != nil {
		t.Errorf("FallbackChains = %v, want nil", cfg.Deployments[0].FallbackChains)
	}
}

// TestLoadRejectsUnknownFallbackChainClass proves a config typo (an
// error-class name that isn't one of the three known constants) fails
// fast at Load time, matching this file's existing negative-weight/
// missing-required-field discipline.
func TestLoadRejectsUnknownFallbackChainClass(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	extra := "    fallback_chains:\n" +
		"      contentpolicy_typo: \"safety-alt\"\n"
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig(extra)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with an unknown fallback_chains class returned nil error")
	}
}

// TestLoadRejectsNonPositivePerModelRateLimit proves a per_model entry
// missing (or non-positive on) burst/refill_per_second fails fast at Load
// time, matching this file's existing negative-weight/missing-required-
// field discipline (TestLoadRejectsUnknownFallbackChainClass et al.) —
// per ModelRateLimitConfig's own doc comment: unlike the key's own
// top-level burst/refill_per_second (where 0 resolves to the gateway's
// operational default), a per-model entry has no such fallback.
func TestLoadRejectsNonPositivePerModelRateLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\n" +
		"virtual_keys:\n" +
		"  team-alpha:\n" +
		"    key_hash: \"aa\"\n" +
		"    rate_limit:\n" +
		"      burst: 20\n" +
		"      refill_per_second: 10\n" +
		"      per_model:\n" +
		"        gpt-4o:\n" +
		"          burst: 0\n" +
		"          refill_per_second: 1\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"m\"\n" +
		"    provider: \"openai\"\n" +
		"    upstream_model: \"m\"\n" +
		"    base_url: \"https://x\"\n" +
		"    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with a non-positive per_model burst returned nil error")
	}
}

// TestLoadPerModelRateLimitParsesOptionalTPMFields proves a per_model
// entry's optional tpm_capacity/tpm_refill_per_second pair parses
// correctly alongside the mandatory burst/refill_per_second — the Phase 4
// PerModel-for-TPM direct-path extension, per docs/upgrade-research/
// gateway-per-deployment-concurrency-2026-09-09.md's own follow-on.
func TestLoadPerModelRateLimitParsesOptionalTPMFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\n" +
		"virtual_keys:\n" +
		"  team-alpha:\n" +
		"    key_hash: \"aa\"\n" +
		"    rate_limit:\n" +
		"      burst: 20\n" +
		"      refill_per_second: 10\n" +
		"      per_model:\n" +
		"        gpt-4o:\n" +
		"          burst: 5\n" +
		"          refill_per_second: 1\n" +
		"          tpm_capacity: 50000\n" +
		"          tpm_refill_per_second: 500\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"m\"\n" +
		"    provider: \"openai\"\n" +
		"    upstream_model: \"m\"\n" +
		"    base_url: \"https://x\"\n" +
		"    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	mrl := cfg.VirtualKeys[0].PerModelRateLimits["gpt-4o"]
	if mrl.Burst != 5 || mrl.RefillPerSecond != 1 {
		t.Errorf("Burst/RefillPerSecond = %v/%v, want 5/1", mrl.Burst, mrl.RefillPerSecond)
	}
	if mrl.TPMCapacity != 50000 || mrl.TPMRefillPerSecond != 500 {
		t.Errorf("TPMCapacity/TPMRefillPerSecond = %v/%v, want 50000/500", mrl.TPMCapacity, mrl.TPMRefillPerSecond)
	}
}

// TestLoadPerModelRateLimitWithoutTPMFieldsLeavesThemZero is the direct
// backward-compatibility proof: a per_model entry that sets only
// burst/refill_per_second (every per_model entry written before this
// field existed) must parse with TPMCapacity/TPMRefillPerSecond both
// zero, never an error and never a silently-inferred value.
func TestLoadPerModelRateLimitWithoutTPMFieldsLeavesThemZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\n" +
		"virtual_keys:\n" +
		"  team-alpha:\n" +
		"    key_hash: \"aa\"\n" +
		"    rate_limit:\n" +
		"      burst: 20\n" +
		"      refill_per_second: 10\n" +
		"      per_model:\n" +
		"        gpt-4o:\n" +
		"          burst: 5\n" +
		"          refill_per_second: 1\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"m\"\n" +
		"    provider: \"openai\"\n" +
		"    upstream_model: \"m\"\n" +
		"    base_url: \"https://x\"\n" +
		"    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	mrl := cfg.VirtualKeys[0].PerModelRateLimits["gpt-4o"]
	if mrl.TPMCapacity != 0 || mrl.TPMRefillPerSecond != 0 {
		t.Errorf("TPMCapacity/TPMRefillPerSecond = %v/%v, want 0/0", mrl.TPMCapacity, mrl.TPMRefillPerSecond)
	}
}

// TestLoadRejectsPerModelRateLimitTPMCapacityWithoutRefill mirrors
// TestLoadRejectsDeploymentRateLimitTPMCapacityWithoutRefill for the
// per-model TPM pair: half-set is a config error, since (unlike
// burst/refill_per_second's own top-level fallback) there is no
// "unset means use some other default" resolution to fall back to.
func TestLoadRejectsPerModelRateLimitTPMCapacityWithoutRefill(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\n" +
		"virtual_keys:\n" +
		"  team-alpha:\n" +
		"    key_hash: \"aa\"\n" +
		"    rate_limit:\n" +
		"      burst: 20\n" +
		"      refill_per_second: 10\n" +
		"      per_model:\n" +
		"        gpt-4o:\n" +
		"          burst: 5\n" +
		"          refill_per_second: 1\n" +
		"          tpm_capacity: 50000\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"m\"\n" +
		"    provider: \"openai\"\n" +
		"    upstream_model: \"m\"\n" +
		"    base_url: \"https://x\"\n" +
		"    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with per_model.gpt-4o.tpm_capacity set but tpm_refill_per_second unset returned nil error")
	}
}

// TestLoadDeploymentRateLimitParsesAllFields proves a deployment's
// rate_limit mapping parses into DeploymentConfig's burst/refill/TPM/
// max_concurrent_requests fields — the backward-compatible additive
// config surface per docs/upgrade-research/gateway-per-deployment-
// concurrency-2026-09-09.md.
func TestLoadDeploymentRateLimitParsesAllFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	extra := "    rate_limit:\n" +
		"      burst: 500\n" +
		"      refill_per_second: 200\n" +
		"      tpm_capacity: 100000\n" +
		"      tpm_refill_per_second: 1000\n" +
		"      max_concurrent_requests: 50\n"
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig(extra)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dep := cfg.Deployments[0]
	if dep.RateLimitBurst != 500 || dep.RateLimitRefill != 200 {
		t.Errorf("RateLimitBurst/RateLimitRefill = %v/%v, want 500/200", dep.RateLimitBurst, dep.RateLimitRefill)
	}
	if dep.TPMCapacity != 100000 || dep.TPMRefillPerSecond != 1000 {
		t.Errorf("TPMCapacity/TPMRefillPerSecond = %v/%v, want 100000/1000", dep.TPMCapacity, dep.TPMRefillPerSecond)
	}
	if dep.MaxConcurrentRequests != 50 {
		t.Errorf("MaxConcurrentRequests = %d, want 50", dep.MaxConcurrentRequests)
	}
}

// TestLoadDeploymentWithoutRateLimitLeavesFieldsZero is the direct
// backward-compatibility proof at the parser level, mirroring
// TestLoadDeploymentWithoutFallbackChainsLeavesFieldNil: a deployment
// with no rate_limit section at all — every config file written before
// this feature existed — must parse identically to before (all four
// fields zero, meaning "no ceiling at all", never a load error).
func TestLoadDeploymentWithoutRateLimitLeavesFieldsZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig("")), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dep := cfg.Deployments[0]
	if dep.RateLimitBurst != 0 || dep.RateLimitRefill != 0 || dep.TPMCapacity != 0 || dep.TPMRefillPerSecond != 0 || dep.MaxConcurrentRequests != 0 {
		t.Errorf("dep = %+v, want every rate_limit field zero", dep)
	}
}

// TestLoadRejectsDeploymentRateLimitBurstWithoutRefill and its sibling
// below prove the "must be set together or neither" load-time validation
// parseDeploymentRateLimit enforces — mirroring
// TestLoadRejectsNonPositivePerModelRateLimit's discipline, but for the
// deployment-level (not per-model) burst/refill pair, which — unlike a
// virtual key's own top-level burst/refill — has no "0 means use the
// gateway's default" fallback to silently resolve a half-set pair to.
func TestLoadRejectsDeploymentRateLimitBurstWithoutRefill(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	extra := "    rate_limit:\n      burst: 500\n"
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig(extra)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with deployment rate_limit.burst set but refill_per_second unset returned nil error")
	}
}

func TestLoadRejectsDeploymentRateLimitTPMCapacityWithoutRefill(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	extra := "    rate_limit:\n      tpm_capacity: 100000\n"
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig(extra)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with deployment rate_limit.tpm_capacity set but tpm_refill_per_second unset returned nil error")
	}
}

// TestLoadWithoutPerModelRateLimitsLeavesFieldNil is the direct
// backward-compatibility proof at the parser level, mirroring
// TestLoadDeploymentWithoutFallbackChainsLeavesFieldNil: a virtual key
// with no rate_limit.per_model section at all must parse to a nil map.
func TestLoadWithoutPerModelRateLimitsLeavesFieldNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\n" +
		"virtual_keys:\n" +
		"  team-alpha:\n" +
		"    key_hash: \"aa\"\n" +
		"    rate_limit:\n" +
		"      burst: 20\n" +
		"      refill_per_second: 10\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"m\"\n" +
		"    provider: \"openai\"\n" +
		"    upstream_model: \"m\"\n" +
		"    base_url: \"https://x\"\n" +
		"    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.VirtualKeys[0].PerModelRateLimits != nil {
		t.Errorf("PerModelRateLimits = %v, want nil", cfg.VirtualKeys[0].PerModelRateLimits)
	}
}

func minimalBedrockDeploymentConfig(extraDeploymentLines string) string {
	return "listen_addr: \":8080\"\n" +
		"virtual_keys:\n" +
		"  team-alpha:\n" +
		"    key_hash: \"aa\"\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"m\"\n" +
		"    provider: \"bedrock\"\n" +
		"    upstream_model: \"m\"\n" +
		"    base_url: \"https://x\"\n" +
		"    access_key_id_env: \"AWS_ACCESS_KEY_ID\"\n" +
		"    secret_access_key_env: \"AWS_SECRET_ACCESS_KEY\"\n" +
		"    region: \"us-east-1\"\n" +
		extraDeploymentLines
}

// TestLoadBedrockDeploymentDoesNotRequireAPIKeyEnv proves the real,
// provider-conditional relaxation: a bedrock deployment parses
// successfully with no api_key_env at all, per
// docs/rfcs/2026-09-04-bedrock-adapter.md -- Bedrock's real
// authentication is AWS SigV4, which needs
// access_key_id_env/secret_access_key_env/region instead.
func TestLoadBedrockDeploymentDoesNotRequireAPIKeyEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalBedrockDeploymentConfig("")), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Deployments) != 1 {
		t.Fatalf("len(Deployments) = %d, want 1", len(cfg.Deployments))
	}
	dep := cfg.Deployments[0]
	if dep.APIKeyEnv != "" {
		t.Errorf("APIKeyEnv = %q, want empty for a bedrock deployment", dep.APIKeyEnv)
	}
	if dep.AccessKeyIDEnv != "AWS_ACCESS_KEY_ID" || dep.SecretAccessKeyEnv != "AWS_SECRET_ACCESS_KEY" || dep.Region != "us-east-1" {
		t.Errorf("bedrock credential fields = %+v, unexpected", dep)
	}
}

// TestLoadBedrockDeploymentMissingAccessKeyIDEnvFails proves the real,
// provider-conditional requirement: a bedrock deployment missing
// access_key_id_env is a real config error.
func TestLoadBedrockDeploymentMissingAccessKeyIDEnvFails(t *testing.T) {
	content := "listen_addr: \":8080\"\n" +
		"virtual_keys:\n" +
		"  team-alpha:\n" +
		"    key_hash: \"aa\"\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"m\"\n" +
		"    provider: \"bedrock\"\n" +
		"    upstream_model: \"m\"\n" +
		"    base_url: \"https://x\"\n" +
		"    secret_access_key_env: \"AWS_SECRET_ACCESS_KEY\"\n" +
		"    region: \"us-east-1\"\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with a bedrock deployment missing access_key_id_env returned nil error")
	}
	if !strings.Contains(err.Error(), "access_key_id_env") {
		t.Errorf("error = %v, want it to name access_key_id_env", err)
	}
}

// TestLoadBedrockDeploymentMissingRegionFails proves region is real and
// required for bedrock, not silently optional.
func TestLoadBedrockDeploymentMissingRegionFails(t *testing.T) {
	content := "listen_addr: \":8080\"\n" +
		"virtual_keys:\n" +
		"  team-alpha:\n" +
		"    key_hash: \"aa\"\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"m\"\n" +
		"    provider: \"bedrock\"\n" +
		"    upstream_model: \"m\"\n" +
		"    base_url: \"https://x\"\n" +
		"    access_key_id_env: \"AWS_ACCESS_KEY_ID\"\n" +
		"    secret_access_key_env: \"AWS_SECRET_ACCESS_KEY\"\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with a bedrock deployment missing region returned nil error")
	}
}

// TestLoadNonBedrockDeploymentStillRequiresAPIKeyEnv is the decisive
// backward-compatibility proof: every non-bedrock provider's existing
// api_key_env requirement is completely unchanged.
func TestLoadNonBedrockDeploymentStillRequiresAPIKeyEnv(t *testing.T) {
	content := "listen_addr: \":8080\"\n" +
		"virtual_keys:\n" +
		"  team-alpha:\n" +
		"    key_hash: \"aa\"\n" +
		"deployments:\n" +
		"  d1:\n" +
		"    model: \"m\"\n" +
		"    provider: \"openai\"\n" +
		"    upstream_model: \"m\"\n" +
		"    base_url: \"https://x\"\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with a non-bedrock deployment missing api_key_env returned nil error")
	}
	if !strings.Contains(err.Error(), "api_key_env") {
		t.Errorf("error = %v, want it to name api_key_env", err)
	}
}

// TestLoadBedrockDeploymentWithSessionTokenEnv proves the optional
// session-token env var parses when present.
func TestLoadBedrockDeploymentWithSessionTokenEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := minimalBedrockDeploymentConfig("    session_token_env: \"AWS_SESSION_TOKEN\"\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Deployments[0].SessionTokenEnv; got != "AWS_SESSION_TOKEN" {
		t.Errorf("SessionTokenEnv = %q, want %q", got, "AWS_SESSION_TOKEN")
	}
}

// TestLoadWithoutAdminSectionDefaultsToZeroValue proves admin: is
// genuinely optional — a bare config with no admin: section at all must
// parse with every AdminConfig field at its zero value, EXCEPT
// EnableAuditLog (defaults true, see that field's own doc comment) and
// OnCorruptStore (defaults "fail", see that field's own doc comment —
// corrected here, not silently left stale, per this field's addition,
// the same "priority over this test's own previously-bare zero-value
// framing" precedent EnableAuditLog's own correction already
// established). Otherwise mirrors
// TestLoadWithoutTelemetrySectionDefaultsToZeroValue's own proof for a
// different optional section.
func TestLoadWithoutAdminSectionDefaultsToZeroValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load without an admin section: %v", err)
	}
	if want := (AdminConfig{EnableAuditLog: true, OnCorruptStore: "fail"}); cfg.Admin != want {
		t.Errorf("Admin = %+v, want %+v", cfg.Admin, want)
	}
}

// TestLoadAdminSectionParsesListenAddrAndTokenEnv proves the admin:
// section, when present, is parsed correctly — the mirror-image proof to
// TestLoadWithoutAdminSectionDefaultsToZeroValue above.
func TestLoadAdminSectionParsesListenAddrAndTokenEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nadmin:\n  listen_addr: \"127.0.0.1:8081\"\n  token_env: \"KELVRAN_ADMIN_TOKEN\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with an admin section: %v", err)
	}
	if cfg.Admin.ListenAddr != "127.0.0.1:8081" {
		t.Errorf("Admin.ListenAddr = %q, want %q", cfg.Admin.ListenAddr, "127.0.0.1:8081")
	}
	if cfg.Admin.TokenEnv != "KELVRAN_ADMIN_TOKEN" {
		t.Errorf("Admin.TokenEnv = %q, want %q", cfg.Admin.TokenEnv, "KELVRAN_ADMIN_TOKEN")
	}
}

// TestLoadAdminSectionPresentButEnableAuditLogUnsetStillDefaultsTrue is
// the mirror-image case TestLoadWithoutAdminSectionDefaultsToZeroValue
// doesn't cover: an admin: section that exists (for some other field)
// but never mentions enable_audit_log at all must still default true,
// not fall back to Go's bare zero value the way every OTHER AdminConfig
// field does.
func TestLoadAdminSectionPresentButEnableAuditLogUnsetStillDefaultsTrue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nadmin:\n  listen_addr: \"127.0.0.1:8081\"\n  token_env: \"KELVRAN_ADMIN_TOKEN\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Admin.EnableAuditLog {
		t.Error("Admin.EnableAuditLog = false, want true (admin: present but enable_audit_log unset must still default true)")
	}
}

// TestLoadAdminSectionExplicitlyDisablesAuditLog proves the actual opt-out
// path: enable_audit_log: false is honored, not silently overridden back
// to the default.
func TestLoadAdminSectionExplicitlyDisablesAuditLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nadmin:\n  enable_audit_log: false\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Admin.EnableAuditLog {
		t.Error("Admin.EnableAuditLog = true, want false (enable_audit_log: false must be honored)")
	}
}

// TestLoadOnCorruptStoreDefaultsToFailRegardlessOfAdminSectionPresence
// proves admin.on_corrupt_store defaults to "fail" -- both when the
// admin section is absent entirely, and when it's present but this key
// is unset -- preserving every config file written before this field
// existed exactly the same fatal-on-corruption behavior it always had,
// mirroring EnableAuditLog's own "default regardless of section
// presence" precedent.
func TestLoadOnCorruptStoreDefaultsToFailRegardlessOfAdminSectionPresence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{
			name:    "admin section absent",
			content: "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n",
		},
		{
			name:    "admin section present, on_corrupt_store unset",
			content: "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nadmin:\n  token_env: \"KELVRAN_ADMIN_TOKEN\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Admin.OnCorruptStore != "fail" {
				t.Errorf("Admin.OnCorruptStore = %q, want \"fail\"", cfg.Admin.OnCorruptStore)
			}
		})
	}
}

// TestLoadOnCorruptStoreParsesReset proves the actual opt-in path.
func TestLoadOnCorruptStoreParsesReset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nadmin:\n  on_corrupt_store: \"reset\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Admin.OnCorruptStore != "reset" {
		t.Errorf("Admin.OnCorruptStore = %q, want \"reset\"", cfg.Admin.OnCorruptStore)
	}
}

// TestLoadRejectsUnknownOnCorruptStoreValue proves a typo or invalid
// value fails config load loudly at startup, rather than silently
// falling back to a default the operator never intended.
func TestLoadRejectsUnknownOnCorruptStoreValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nadmin:\n  on_corrupt_store: \"resett\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with an invalid admin.on_corrupt_store value, want an error")
	}
}

// TestLoadWithoutHealthProbeSectionDefaultsToZeroValue mirrors
// TestLoadWithoutAdminSectionDefaultsToZeroValue for health_probe:
// omitting the section entirely must mean probing stays disabled
// (IntervalSeconds == 0), per
// docs/rfcs/2026-09-07-gateway-active-health-probing.md.
func TestLoadWithoutHealthProbeSectionDefaultsToZeroValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load without a health_probe section: %v", err)
	}
	if cfg.HealthProbe != (HealthProbeConfig{}) {
		t.Errorf("HealthProbe = %+v, want the zero value", cfg.HealthProbe)
	}
}

// TestLoadHealthProbeSectionParsesFields proves the health_probe:
// section, when present, is parsed correctly — the mirror-image proof to
// TestLoadWithoutHealthProbeSectionDefaultsToZeroValue above.
func TestLoadHealthProbeSectionParsesFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nhealth_probe:\n  interval_seconds: 60\n  unhealthy_threshold: 5\n  healthy_threshold: 4\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with a health_probe section: %v", err)
	}
	if cfg.HealthProbe.IntervalSeconds != 60 {
		t.Errorf("HealthProbe.IntervalSeconds = %d, want 60", cfg.HealthProbe.IntervalSeconds)
	}
	if cfg.HealthProbe.UnhealthyThreshold != 5 {
		t.Errorf("HealthProbe.UnhealthyThreshold = %d, want 5", cfg.HealthProbe.UnhealthyThreshold)
	}
	if cfg.HealthProbe.HealthyThreshold != 4 {
		t.Errorf("HealthProbe.HealthyThreshold = %d, want 4", cfg.HealthProbe.HealthyThreshold)
	}
}

// TestLoadHealthProbeSectionParsesRecoveryRampFields is the same proof as
// TestLoadHealthProbeSectionParsesFields, extended to the two
// recovery-ramp fields added alongside the post-recovery weight-ramp
// feature (see docs/rfcs/2026-09-07-gateway-active-health-probing.md and
// gateway/internal/router/health.go's own recovery-ramp doc comments).
func TestLoadHealthProbeSectionParsesRecoveryRampFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nhealth_probe:\n  interval_seconds: 60\n  unhealthy_threshold: 5\n  healthy_threshold: 4\n  recovery_ramp_steps: 6\n  recovery_ramp_initial_percent: 10\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with a health_probe section's recovery-ramp fields: %v", err)
	}
	if cfg.HealthProbe.RecoveryRampSteps != 6 {
		t.Errorf("HealthProbe.RecoveryRampSteps = %d, want 6", cfg.HealthProbe.RecoveryRampSteps)
	}
	if cfg.HealthProbe.RecoveryRampInitialPercent != 10 {
		t.Errorf("HealthProbe.RecoveryRampInitialPercent = %d, want 10", cfg.HealthProbe.RecoveryRampInitialPercent)
	}
}

// TestLoadRejectsBothNegativeDeploymentRateLimitPair is the regression
// proof for the real bug fixed in validateRateLimitPair's own doc
// comment: burst/refill_per_second both negative used to slip past the
// half-set-only check ((a > 0) != (b > 0) is false when both are
// negative), silently accepted as if it were a valid "disabled" (0/0)
// pair.
func TestLoadRejectsBothNegativeDeploymentRateLimitPair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	extra := "    rate_limit:\n      burst: -5\n      refill_per_second: -3\n"
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig(extra)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with a deployment rate_limit burst/refill_per_second pair that is both negative returned nil error")
	}
}

// TestLoadRejectsBothNegativeVirtualKeyRateLimitPair is the same
// regression proof at the virtual-key level, which — unlike the
// deployment level — had ZERO rate_limit.burst/refill_per_second
// validation at all before this fix, not even the half-set check.
func TestLoadRejectsBothNegativeVirtualKeyRateLimitPair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\n    rate_limit:\n      burst: -5\n      refill_per_second: -3\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with a virtual key rate_limit burst/refill_per_second pair that is both negative returned nil error")
	}
}

// TestLoadRejectsHalfSetVirtualKeyRateLimitPair proves the virtual-key
// level now also rejects the half-set case (burst without
// refill_per_second) — previously accepted silently, since this level
// had no validation of any kind before this fix.
func TestLoadRejectsHalfSetVirtualKeyRateLimitPair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\n    rate_limit:\n      burst: 500\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with virtual key rate_limit.burst set but refill_per_second unset returned nil error")
	}
}

// TestLoadRejectsBothNegativePerModelTPMPair is validateRateLimitPair's
// third call site — parsePerModelRateLimits' own tpm_capacity/
// tpm_refill_per_second check had the identical half-set-only gap.
func TestLoadRejectsBothNegativePerModelTPMPair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\n    rate_limit:\n      per_model:\n        gpt-4o:\n          burst: 10\n          refill_per_second: 5\n          tpm_capacity: -100\n          tpm_refill_per_second: -50\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with a per-model tpm_capacity/tpm_refill_per_second pair that is both negative returned nil error")
	}
}

// TestGetIntOnAFloatFarBeyondIntRangeReturnsNotOKInsteadOfGarbage is the
// regression proof for the real bug fixed in getInt's own doc comment: a
// bare int(n) conversion on a float64 far outside int's representable
// range is implementation-defined per the Go spec, not merely clamped —
// getInt must now report (0, false) rather than silently returning
// whatever garbage that conversion happened to produce.
func TestGetIntOnAFloatFarBeyondIntRangeReturnsNotOKInsteadOfGarbage(t *testing.T) {
	m := map[string]any{"weight": 1e300}
	v, ok := getInt(m, "weight")
	if ok {
		t.Errorf("getInt(1e300) = (%d, true), want ok=false", v)
	}
	if v != 0 {
		t.Errorf("getInt(1e300) = (%d, _), want 0", v)
	}
}

// TestGetIntOnANegativeFloatFarBeyondIntRangeReturnsNotOK is the same
// proof for the negative side of the range.
func TestGetIntOnANegativeFloatFarBeyondIntRangeReturnsNotOK(t *testing.T) {
	m := map[string]any{"weight": -1e300}
	if v, ok := getInt(m, "weight"); ok {
		t.Errorf("getInt(-1e300) = (%d, true), want ok=false", v)
	}
}

// TestGetIntOnAnOrdinaryValueStillWorks is a regression guard: the
// overflow bound must not reject any realistic config value.
func TestGetIntOnAnOrdinaryValueStillWorks(t *testing.T) {
	m := map[string]any{"weight": float64(42)}
	v, ok := getInt(m, "weight")
	if !ok || v != 42 {
		t.Errorf("getInt(42) = (%d, %v), want (42, true)", v, ok)
	}
}

// TestLoadDeploymentWithAnOverflowingWeightDefaultsToZeroNotGarbage is
// the end-to-end proof, at the Load level, that an absurd weight value
// degrades to this codebase's own established "0 means unset" sentinel
// rather than an arbitrary, platform-dependent int.
func TestLoadDeploymentWithAnOverflowingWeightDefaultsToZeroNotGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalDeploymentConfig("    weight: 99999999999999999999\n")), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with an overflowing deployment weight: %v", err)
	}
	if len(cfg.Deployments) != 1 {
		t.Fatalf("len(Deployments) = %d, want 1", len(cfg.Deployments))
	}
	if got := cfg.Deployments[0].Weight; got != 0 {
		t.Errorf("Weight = %d, want 0 (the safe overflow fallback, not garbage)", got)
	}
}

// TestLoadRejectsDuplicateTopLevelKey is the deliberate decision
// TestLoadDuplicateTopLevelKeyLastValueWins's own doc comment invited:
// a full-codebase audit re-surfaced silent last-value-wins on a
// duplicate key as operationally significant enough to fix now.
// parseYAMLMini must reject a duplicate key at the same nesting level
// loudly, never silently let the second occurrence win.
func TestLoadRejectsDuplicateTopLevelKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nlisten_addr: \":9090\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with a duplicate top-level key returned nil error, want a loud duplicate-key error")
	}
	if !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("Load error = %v, want it to mention a duplicate key", err)
	}
}

// TestLoadRejectsDuplicateDeploymentName is the more operationally
// significant case TestLoadDuplicateDeploymentNameSecondEntrySilentlyWins's
// own doc comment named: two deployment entries under the SAME name
// must now be rejected loudly, never silently collapsed into one (an
// operator's copy-paste mistake that used to drop an entire
// deployment's intended config with no warning).
func TestLoadRejectsDuplicateDeploymentName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\ndeployments:\n  d1:\n    model: \"first-model\"\n    provider: \"openai\"\n    upstream_model: \"first-model\"\n    base_url: \"https://first\"\n    api_key_env: \"X\"\n  d1:\n    model: \"second-model\"\n    provider: \"openai\"\n    upstream_model: \"second-model\"\n    base_url: \"https://second\"\n    api_key_env: \"Y\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with a duplicate deployment name returned nil error, want a loud duplicate-key error")
	}
	if !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("Load error = %v, want it to mention a duplicate key", err)
	}
}

// TestLoadRejectsTopLevelYAMLList proves Load returns a real,
// human-readable error for valid-YAML-but-wrong-shape content: a
// top-level list instead of a mapping. parseYAMLMini's own "key: value"
// line-parsing rejects a bare "- item" list-item line (no colon) with
// its own "expected \"key: value\"" error.
func TestLoadRejectsTopLevelYAMLList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "- listen_addr\n- virtual_keys\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with a top-level YAML list returned nil error, want a real error")
	}
}

// TestLoadRejectsVirtualKeysAsYAMLList mirrors
// TestLoadRejectsTopLevelYAMLList for a nested section: virtual_keys
// must be a mapping (keyed by virtual key name), not a list.
func TestLoadRejectsVirtualKeysAsYAMLList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  - team-alpha\n  - team-beta\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load with virtual_keys as a YAML list returned nil error, want a real error")
	}
}

// TestLoadPreservesUnicodeInVirtualKeyNameAndModelName proves non-ASCII
// string fields round-trip correctly through parseYAMLMini -- correctly
// handled already (this parser works on Go strings/runes throughout,
// never byte-indexed ASCII assumptions), but never previously exercised
// with a real non-ASCII value.
func TestLoadPreservesUnicodeInVirtualKeyNameAndModelName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  \"team-éé\":\n    key_hash: \"aa\"\ndeployments:\n  d1:\n    model: \"gemini-日本語\"\n    provider: \"openai\"\n    upstream_model: \"gemini-日本語\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with non-ASCII virtual key name and model name: %v", err)
	}
	if len(cfg.VirtualKeys) != 1 || cfg.VirtualKeys[0].Name != "team-éé" {
		t.Errorf("VirtualKeys = %+v, want one entry named %q", cfg.VirtualKeys, "team-éé")
	}
	if len(cfg.Deployments) != 1 || cfg.Deployments[0].Model != "gemini-日本語" {
		t.Errorf("Deployments = %+v, want one entry with model %q", cfg.Deployments, "gemini-日本語")
	}
}

// TestLoadBudgetUSDExtremeMagnitudePreservesPrecision proves an
// extreme-magnitude budget_usd literal round-trips exactly through
// decimal.Decimal -- getDecimal parses from the raw source string via
// decimal.NewFromString, never through an intermediate float64, so this
// is already correct; never previously exercised at this magnitude.
func TestLoadBudgetUSDExtremeMagnitudePreservesPrecision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\n    budget_usd: \"1234567890123456.78\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with an extreme-magnitude budget_usd: %v", err)
	}
	want := decimal.RequireFromString("1234567890123456.78")
	if len(cfg.VirtualKeys) != 1 || !cfg.VirtualKeys[0].BudgetUSD.Equal(want) {
		t.Errorf("VirtualKeys[0].BudgetUSD = %v, want %v (exact, not rounded)", cfg.VirtualKeys[0].BudgetUSD, want)
	}
}

// nestedYAMLMapping builds depth nested-mapping levels, each 2 spaces
// more indented than the last ("level0:\n  level1:\n    level2:\n..."),
// with no leaf value on the innermost line -- every line is itself an
// empty-value "key:" line, so parseYAMLMini pushes exactly depth new
// frames, one per line, deterministically.
func nestedYAMLMapping(depth int) string {
	var b strings.Builder
	for i := 0; i < depth; i++ {
		b.WriteString(strings.Repeat("  ", i))
		fmt.Fprintf(&b, "level%d:\n", i)
	}
	return b.String()
}

// TestParseYAMLMiniRejectsNestingBeyondMaxDepth proves the new
// maxYAMLNestingDepth guard actually rejects a pathologically deep
// config -- 70 levels, well past the 64-level ceiling.
func TestParseYAMLMiniRejectsNestingBeyondMaxDepth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(nestedYAMLMapping(70)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with 70 levels of nesting returned nil error, want a max-nesting-depth error")
	}
	if !strings.Contains(err.Error(), "nesting depth") {
		t.Errorf("Load error = %v, want it to mention nesting depth", err)
	}
}

// TestParseYAMLMiniAcceptsNestingAtExactlyMaxDepth is the boundary proof:
// exactly 64 levels (maxYAMLNestingDepth's own value) must still parse
// successfully -- the guard must not be off-by-one in the stricter
// direction.
func TestParseYAMLMiniAcceptsNestingAtExactlyMaxDepth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(nestedYAMLMapping(maxYAMLNestingDepth)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// This deeply-nested content has no listen_addr/deployments/etc, so
	// Load fails validation past the parse step -- the point of this
	// test is only that parseYAMLMini itself never rejects exactly 64
	// levels, proven by asserting the error is NOT a nesting-depth one.
	_, err := Load(path)
	if err != nil && strings.Contains(err.Error(), "nesting depth") {
		t.Errorf("Load with exactly %d levels of nesting was rejected as too deep: %v", maxYAMLNestingDepth, err)
	}
}

// TestParseYAMLMiniRejectsTabIndentation is the regression proof for a
// real bug a full-codebase audit found: a tab-indented line previously
// computed indent 0 (TrimLeft's cutset was space-only), silently
// collapsing the nesting stack back to the document root and
// misattributing that line -- and everything after it -- to the wrong
// parent, with zero parse error. A tab anywhere in a line's own leading
// whitespace must now be rejected loudly instead.
func TestParseYAMLMiniRejectsTabIndentation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// A tab before "budget_usd" -- exactly the audit's own reproduction
	// case (a virtual key's nested field indented with a tab instead of
	// spaces, the ordinary editor/copy-paste mistake this guards against).
	content := "virtual_keys:\n  key1:\n    key_hash: \"abc\"\n\tbudget_usd: \"100\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with a tab-indented line returned nil error, want a loud tab-indentation error")
	}
	if !strings.Contains(err.Error(), "tab") {
		t.Errorf("Load error = %v, want it to mention tabs", err)
	}
}

// TestParseYAMLMiniAcceptsPureSpaceIndentationUnaffected is the
// no-regression proof: the tab guard above must never reject or alter
// parsing of a line indented purely with spaces, regardless of depth.
func TestParseYAMLMiniAcceptsPureSpaceIndentationUnaffected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "virtual_keys:\n  key1:\n    key_hash: \"abc\"\n    budget_usd: \"100\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	root, err := parseYAMLMini([]byte(content))
	if err != nil {
		t.Fatalf("parseYAMLMini with pure-space indentation: %v", err)
	}
	virtualKeys, ok := root["virtual_keys"].(map[string]any)
	if !ok {
		t.Fatalf("root[\"virtual_keys\"] = %#v, want a map", root["virtual_keys"])
	}
	key1, ok := virtualKeys["key1"].(map[string]any)
	if !ok {
		t.Fatalf("virtual_keys[\"key1\"] = %#v, want a map", virtualKeys["key1"])
	}
	if key1["budget_usd"] != "100" {
		t.Errorf("key1[\"budget_usd\"] = %#v, want \"100\" nested correctly under key1, not the root", key1["budget_usd"])
	}
}

// TestLoadAlertingSectionDefaultsEmptyWhenUnconfigured proves alerting:
// is genuinely optional -- a bare config with no alerting: section
// parses with AlertingConfig at its zero value, matching every other
// optional section's own convention.
func TestLoadAlertingSectionDefaultsEmptyWhenUnconfigured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Alerting != (AlertingConfig{}) {
		t.Errorf("Alerting = %+v, want the zero value", cfg.Alerting)
	}
}

// TestLoadAlertingSectionParsesWebhookURLEnvAndSigningSecretEnv proves
// the actual opt-in path parses both fields.
func TestLoadAlertingSectionParsesWebhookURLEnvAndSigningSecretEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nalerting:\n  webhook_url_env: \"KELVRAN_ALERT_WEBHOOK_URL\"\n  signing_secret_env: \"KELVRAN_ALERT_WEBHOOK_SECRET\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Alerting.WebhookURLEnv != "KELVRAN_ALERT_WEBHOOK_URL" {
		t.Errorf("Alerting.WebhookURLEnv = %q, want %q", cfg.Alerting.WebhookURLEnv, "KELVRAN_ALERT_WEBHOOK_URL")
	}
	if cfg.Alerting.SigningSecretEnv != "KELVRAN_ALERT_WEBHOOK_SECRET" {
		t.Errorf("Alerting.SigningSecretEnv = %q, want %q", cfg.Alerting.SigningSecretEnv, "KELVRAN_ALERT_WEBHOOK_SECRET")
	}
}

// TestLoadRejectsNonCanonicalBooleanSpellingForSharedAcrossTenants is a
// real-bug regression test, per a full-codebase audit: parseYAMLScalar
// only recognizes true/True/TRUE/false/False/FALSE as booleans (see its
// own doc comment for why the set deliberately excludes yes/no/on/off/
// 1/0 -- widening it would reintroduce the budget_usd-collides-with-bool
// bug that comment already documents fixing). Before the fix, getBool
// did a bare v.(bool) assertion and returned (false, false) for a
// string value that fell through that set -- IDENTICAL to "key not
// present" -- so a typo'd/non-canonical spelling on a security-relevant
// field like shared_across_tenants (this deployment's cross-tenant
// cache-pollution protection, per SharedAcrossTenants' own doc comment)
// silently and undetectably resolved to false with zero error. Load
// must now fail loudly instead, distinguishing "key absent" (fine,
// resolves to the false default) from "key present with an unparseable
// value" (a config error, not a silent misconfiguration).
func TestLoadRejectsNonCanonicalBooleanSpellingForSharedAcrossTenants(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := minimalDeploymentConfig("    shared_across_tenants: yes\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with shared_across_tenants: yes returned nil error, want a loud config error -- \"yes\" must never silently resolve to false")
	}
	if !strings.Contains(err.Error(), "shared_across_tenants") {
		t.Errorf("Load error = %v, want it to mention shared_across_tenants", err)
	}
}

// TestLoadParsesQuotedDeploymentKeyContainingColon is a real-bug
// regression test, per the same audit: the line-parsing code found the
// first colon in a raw line via a plain strings.Index BEFORE any
// quote-awareness, so a quoted key containing a colon (e.g.
// "my:deployment":) split on the colon INSIDE the quotes, producing a
// garbage key ("\"my, with the leading quote still attached) and a
// garbage value (the tail of the real key plus its closing quote).
// unquoteYAMLScalar is applied to every extracted key specifically to
// support quoted keys -- that support was incomplete without
// findKeyColon's quote-aware search.
func TestLoadParsesQuotedDeploymentKeyContainingColon(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\n" +
		"virtual_keys:\n" +
		"  team-alpha:\n" +
		"    key_hash: \"aa\"\n" +
		"deployments:\n" +
		"  \"my:deployment\":\n" +
		"    model: \"m\"\n" +
		"    provider: \"openai\"\n" +
		"    upstream_model: \"m\"\n" +
		"    base_url: \"https://x\"\n" +
		"    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Deployments) != 1 {
		t.Fatalf("len(Deployments) = %d, want 1", len(cfg.Deployments))
	}
	if got := cfg.Deployments[0].Name; got != "my:deployment" {
		t.Errorf("Deployments[0].Name = %q, want %q (quoted key's colon must not be mistaken for the key/value separator)", got, "my:deployment")
	}
}
