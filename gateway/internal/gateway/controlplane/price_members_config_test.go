package controlplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

const priceMembersPreamble = "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\nprice_table:\n"

func loadPriceTableFixture(t *testing.T, priceTable string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(priceMembersPreamble+priceTable), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return Load(path)
}

// The three optional rates item (i) adds: a 1-hour cache-write price, a
// per-request web-search fee and the US-only inference multiplier. Absent
// keys stay nil (unset), like cache_read_per_token/cache_creation_per_token.
func TestLoadPriceTableParsesThePricingMemberRates(t *testing.T) {
	cfg, err := loadPriceTableFixture(t, "  claude-sonnet-5-5:\n    prompt_per_token: 0.000002\n    completion_per_token: 0.00001\n    cache_creation_1h_per_token: 0.000004\n    web_search_per_request: 0.01\n    us_inference_multiplier: 1.1\n  gpt-4o:\n    prompt_per_token: 0.0000025\n    completion_per_token: 0.00001\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sonnet, ok := cfg.PriceTable["claude-sonnet-5-5"]
	if !ok {
		t.Fatal("missing price_table entry \"claude-sonnet-5-5\"")
	}
	for name, got := range map[string]*decimal.Decimal{
		"cache_creation_1h_per_token": sonnet.CacheCreation1hPerToken,
		"web_search_per_request":      sonnet.WebSearchPerRequest,
		"us_inference_multiplier":     sonnet.USInferenceMultiplier,
	} {
		want := map[string]string{"cache_creation_1h_per_token": "0.000004", "web_search_per_request": "0.01", "us_inference_multiplier": "1.1"}[name]
		if got == nil || !got.Equal(decimal.RequireFromString(want)) {
			t.Errorf("claude-sonnet-5-5 %s = %v, want %s", name, got, want)
		}
	}
	gpt, ok := cfg.PriceTable["gpt-4o"]
	if !ok {
		t.Fatal("missing price_table entry \"gpt-4o\"")
	}
	if gpt.CacheCreation1hPerToken != nil || gpt.WebSearchPerRequest != nil || gpt.USInferenceMultiplier != nil {
		t.Errorf("gpt-4o without the keys = %+v, want every member rate nil", gpt)
	}
}

// A negative price is the same sign-flip typo the existing rates reject; a
// multiplier below 1 would make US-only inference cheaper than standard,
// the undercounting direction every rate here refuses.
func TestLoadPriceTableRejectsBadPricingMemberRates(t *testing.T) {
	base := "  claude-sonnet-5-5:\n    prompt_per_token: 0.000002\n    completion_per_token: 0.00001\n"
	for _, tc := range []struct{ name, line, wantSubstr string }{
		{"negative cache_creation_1h_per_token", "    cache_creation_1h_per_token: -0.000004\n", "negative cache_creation_1h_per_token"},
		{"negative web_search_per_request", "    web_search_per_request: -0.01\n", "negative web_search_per_request"},
		{"us_inference_multiplier below one", "    us_inference_multiplier: 0.9\n", "us_inference_multiplier"},
		{"negative us_inference_multiplier", "    us_inference_multiplier: -1\n", "us_inference_multiplier"},
		{"cache_creation_1h_per_token below cache_creation_per_token", "    cache_creation_per_token: 0.0000025\n    cache_creation_1h_per_token: 0.000002\n", "cache_creation_1h_per_token"},
		{"cache_creation_1h_per_token below prompt_per_token with no 5m rate", "    cache_creation_1h_per_token: 0.000001\n", "cache_creation_1h_per_token"},
	} {
		_, err := loadPriceTableFixture(t, base+tc.line)
		if err == nil {
			t.Errorf("%s: Load returned nil error, want a config error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSubstr) || !strings.Contains(err.Error(), "claude-sonnet-5-5") {
			t.Errorf("%s: error %q, want it to name the key and the model", tc.name, err)
		}
	}
	if _, err := loadPriceTableFixture(t, base+"    us_inference_multiplier: 1\n"); err != nil {
		t.Errorf("us_inference_multiplier: 1 (an explicit no-op) must load, got %v", err)
	}
	// Equal to the 5-minute rate is the floor, not below it.
	if _, err := loadPriceTableFixture(t, base+"    cache_creation_per_token: 0.0000025\n    cache_creation_1h_per_token: 0.0000025\n"); err != nil {
		t.Errorf("cache_creation_1h_per_token equal to cache_creation_per_token must load, got %v", err)
	}
}

// The example config shows the three rates on claude-fable-5-1 at the
// pricing page's documented values: 1h writes at 2x base input, $10 per
// 1,000 searches, 1.1x for US-only inference.
func TestLoadExampleConfigPricesFableOneHourWritesSearchesAndUSInference(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fable, ok := cfg.PriceTable["claude-fable-5-1"]
	if !ok {
		t.Fatal("missing price_table entry \"claude-fable-5-1\"")
	}
	if fable.CacheCreation1hPerToken == nil || !fable.CacheCreation1hPerToken.Equal(fable.PromptPerToken.Mul(decimal.NewFromInt(2))) {
		t.Errorf("claude-fable-5-1.CacheCreation1hPerToken = %v, want 2x PromptPerToken %v", fable.CacheCreation1hPerToken, fable.PromptPerToken)
	}
	if fable.WebSearchPerRequest == nil || !fable.WebSearchPerRequest.Equal(decimal.RequireFromString("0.01")) {
		t.Errorf("claude-fable-5-1.WebSearchPerRequest = %v, want 0.01", fable.WebSearchPerRequest)
	}
	if fable.USInferenceMultiplier == nil || !fable.USInferenceMultiplier.Equal(decimal.RequireFromString("1.1")) {
		t.Errorf("claude-fable-5-1.USInferenceMultiplier = %v, want 1.1", fable.USInferenceMultiplier)
	}
}
