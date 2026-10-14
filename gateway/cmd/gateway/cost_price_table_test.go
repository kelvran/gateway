package main

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// Every optional rate the loader accepts must reach the calculator; a rate
// parsed but dropped here would be silently unpriced.
func TestCostPriceTableCarriesEveryOptionalRate(t *testing.T) {
	d := func(s string) *decimal.Decimal { v := decimal.RequireFromString(s); return &v }
	cfg := map[string]controlplane.ModelPriceConfig{
		"full": {
			PromptPerToken: decimal.RequireFromString("0.00001"), CompletionPerToken: decimal.RequireFromString("0.00005"),
			CacheReadPerToken: d("0.00000025"), CacheCreationPerToken: d("0.0000125"),
			CacheCreation1hPerToken: d("0.00002"), WebSearchPerRequest: d("0.01"), USInferenceMultiplier: d("1.1"),
			ServiceTierMultipliers: map[string]decimal.Decimal{"flex": decimal.RequireFromString("0.5"), "priority": decimal.RequireFromString("2")},
		},
		"bare": {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
	}
	got := costPriceTable(cfg)
	full := got["full"]
	for name, pair := range map[string][2]*decimal.Decimal{
		"CacheReadPerToken":       {full.CacheReadPerToken, cfg["full"].CacheReadPerToken},
		"CacheCreationPerToken":   {full.CacheCreationPerToken, cfg["full"].CacheCreationPerToken},
		"CacheCreation1hPerToken": {full.CacheCreation1hPerToken, cfg["full"].CacheCreation1hPerToken},
		"WebSearchPerRequest":     {full.WebSearchPerRequest, cfg["full"].WebSearchPerRequest},
		"USInferenceMultiplier":   {full.USInferenceMultiplier, cfg["full"].USInferenceMultiplier},
	} {
		if pair[0] == nil || !pair[0].Equal(*pair[1]) {
			t.Errorf("full.%s = %v, want %v", name, pair[0], pair[1])
		}
	}
	if !full.PromptPerToken.Equal(cfg["full"].PromptPerToken) || !full.CompletionPerToken.Equal(cfg["full"].CompletionPerToken) {
		t.Errorf("full base rates = %v/%v", full.PromptPerToken, full.CompletionPerToken)
	}
	if len(full.ServiceTierMultipliers) != 2 || !full.ServiceTierMultipliers["flex"].Equal(decimal.RequireFromString("0.5")) || !full.ServiceTierMultipliers["priority"].Equal(decimal.RequireFromString("2")) {
		t.Errorf("full.ServiceTierMultipliers = %v, want flex 0.5 and priority 2", full.ServiceTierMultipliers)
	}
	bare := got["bare"]
	if bare.CacheReadPerToken != nil || bare.CacheCreationPerToken != nil || bare.CacheCreation1hPerToken != nil || bare.WebSearchPerRequest != nil || bare.USInferenceMultiplier != nil || bare.ServiceTierMultipliers != nil {
		t.Errorf("bare = %+v, want every optional rate nil", bare)
	}
}
