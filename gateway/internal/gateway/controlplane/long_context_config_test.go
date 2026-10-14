package controlplane

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// price_table.<model>.long_context (round-5 Tier A item 5): above_tokens is
// the cache-inclusive prompt count a request must EXCEED, input_multiplier
// scales the prompt and every cache category, output_multiplier the
// completion -- Haiku 5.5's 5x/5x over 100,000 and the GPT-6 lines' 2x/1.5x
// over 272K, as the pricing pages read on 2026-10-14. Absent stays nil.
func TestLoadPriceTableParsesTheLongContextTier(t *testing.T) {
	cfg, err := loadPriceTableFixture(t, "  claude-haiku-5-5:\n    prompt_per_token: 0.0000001\n    completion_per_token: 0.0000005\n    long_context:\n      above_tokens: 100000\n      input_multiplier: 5\n      output_multiplier: 5\n  gpt-6.1-sol:\n    prompt_per_token: 0.000002\n    completion_per_token: 0.00001\n    long_context:\n      above_tokens: 272000\n      input_multiplier: 2\n      output_multiplier: 1.5\n  gpt-4o:\n    prompt_per_token: 0.0000025\n    completion_per_token: 0.00001\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for model, want := range map[string]LongContextTierConfig{
		"claude-haiku-5-5": {AboveTokens: 100000, InputMultiplier: decimal.RequireFromString("5"), OutputMultiplier: decimal.RequireFromString("5")},
		"gpt-6.1-sol":      {AboveTokens: 272000, InputMultiplier: decimal.RequireFromString("2"), OutputMultiplier: decimal.RequireFromString("1.5")},
	} {
		got := cfg.PriceTable[model].LongContext
		if got == nil {
			t.Fatalf("%s: LongContext = nil, want %+v", model, want)
		}
		if got.AboveTokens != want.AboveTokens || !got.InputMultiplier.Equal(want.InputMultiplier) || !got.OutputMultiplier.Equal(want.OutputMultiplier) {
			t.Errorf("%s: LongContext = %+v, want %+v", model, *got, want)
		}
	}
	if cfg.PriceTable["gpt-4o"].LongContext != nil {
		t.Errorf("gpt-4o: LongContext = %+v, want nil (absent key)", cfg.PriceTable["gpt-4o"].LongContext)
	}
}

// Like service_tier_multipliers, a malformed nested value is a load error
// (not "unset"): a silently dropped tier undercounts every long request 2x
// to 5x. All three keys are required; a multiplier below 1 is the
// undercounting direction; above_tokens must be a positive integer; an
// unknown key is refused because the only thing it can be is a misspelt
// required one or an expectation this shape does not carry.
func TestLoadPriceTableRejectsBadLongContextTiers(t *testing.T) {
	base := "  gpt-6.1-sol:\n    prompt_per_token: 0.000002\n    completion_per_token: 0.00001\n"
	valid := "    long_context:\n      above_tokens: 272000\n      input_multiplier: 2\n      output_multiplier: 1.5\n"
	for _, tc := range []struct{ name, lines, wantSubstr string }{
		{"not a mapping", "    long_context: 272000\n", "long_context"},
		{"missing above_tokens", "    long_context:\n      input_multiplier: 2\n      output_multiplier: 1.5\n", "above_tokens"},
		{"above_tokens zero", "    long_context:\n      above_tokens: 0\n      input_multiplier: 2\n      output_multiplier: 1.5\n", "above_tokens"},
		{"above_tokens negative", "    long_context:\n      above_tokens: -1\n      input_multiplier: 2\n      output_multiplier: 1.5\n", "above_tokens"},
		{"above_tokens not an integer", "    long_context:\n      above_tokens: 272K\n      input_multiplier: 2\n      output_multiplier: 1.5\n", "above_tokens"},
		{"missing input_multiplier", "    long_context:\n      above_tokens: 272000\n      output_multiplier: 1.5\n", "input_multiplier"},
		{"input_multiplier below 1", "    long_context:\n      above_tokens: 272000\n      input_multiplier: 0.9\n      output_multiplier: 1.5\n", "input_multiplier"},
		{"output_multiplier malformed", "    long_context:\n      above_tokens: 272000\n      input_multiplier: 2\n      output_multiplier: x\n", "output_multiplier"},
		{"output_multiplier below 1", "    long_context:\n      above_tokens: 272000\n      input_multiplier: 2\n      output_multiplier: 0.5\n", "output_multiplier"},
		{"unknown key", valid + "      cache_multiplier: 2\n", "cache_multiplier"},
	} {
		_, err := loadPriceTableFixture(t, base+tc.lines)
		if err == nil {
			t.Errorf("%s: Load returned nil error, want a config error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSubstr) {
			t.Errorf("%s: error %q does not name %q", tc.name, err, tc.wantSubstr)
		}
	}
	// Exactly 1 is the Standard rate itself: accepted (a no-op tier an operator
	// may write while a model's premium is being confirmed).
	if _, err := loadPriceTableFixture(t, base+"    long_context:\n      above_tokens: 1\n      input_multiplier: 1\n      output_multiplier: 1\n"); err != nil {
		t.Errorf("multipliers of exactly 1 must load, got %v", err)
	}
}
