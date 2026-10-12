package costaccounting

import (
	"testing"

	"github.com/shopspring/decimal"
)

// memberPrice is a claude-sonnet-5-5-shaped price with every optional rate
// set: 1h cache writes at 2x base input (the pricing page's uniform rule),
// web search at $0.01 per request ($10 per 1,000), US-only inference at 1.1x.
func memberPrice() ModelPrice {
	cacheRead := decimal.RequireFromString("0.00000025")
	cacheCreation := decimal.RequireFromString("0.0000125")
	cacheCreation1h := decimal.RequireFromString("0.00002")
	webSearch := decimal.RequireFromString("0.01")
	usMultiplier := decimal.RequireFromString("1.1")
	return ModelPrice{
		PromptPerToken:          decimal.RequireFromString("0.00001"),
		CompletionPerToken:      decimal.RequireFromString("0.00005"),
		CacheReadPerToken:       &cacheRead,
		CacheCreationPerToken:   &cacheCreation,
		CacheCreation1hPerToken: &cacheCreation1h,
		WebSearchPerRequest:     &webSearch,
		USInferenceMultiplier:   &usMultiplier,
	}
}

// cachedUsage is 1000 prompt tokens = 100 fresh + 400 cache-read + 500
// cache-creation, of which 300 were written with the 1-hour TTL, plus 10
// completion tokens.
func cachedUsage(oneHour int) Usage {
	return Usage{
		PromptTokens:          1000,
		CompletionTokens:      10,
		TotalTokens:           1010,
		CacheReadTokens:       400,
		CacheCreationTokens:   500,
		CacheCreation1hTokens: oneHour,
	}
}

func TestCalculateOneHourCacheWritesPricedAtTheirOwnRate(t *testing.T) {
	c := NewCalculator(PriceTable{"m": memberPrice()})
	got := c.Calculate("m", cachedUsage(300))
	// fresh 100*0.00001 = 0.001; read 400*0.00000025 = 0.0001;
	// 5m writes 200*0.0000125 = 0.0025; 1h writes 300*0.00002 = 0.006;
	// completion 10*0.00005 = 0.0005.
	want := decimal.RequireFromString("0.0101")
	if !got.Equal(want) {
		t.Errorf("Calculate() = %v, want %v", got, want)
	}
}

func TestCalculateUnsetOneHourRateFallsBackToTheCacheCreationRate(t *testing.T) {
	p := memberPrice()
	p.CacheCreation1hPerToken = nil
	c := NewCalculator(PriceTable{"m": p})
	// Every one of the 500 writes at the 5m rate: 500*0.0000125 = 0.00625.
	want := decimal.RequireFromString("0.00785")
	if got := c.Calculate("m", cachedUsage(300)); !got.Equal(want) {
		t.Errorf("unset 1h rate: Calculate() = %v, want %v (the cache-creation rate)", got, want)
	}
	p.CacheCreationPerToken = nil
	c = NewCalculator(PriceTable{"m": p})
	// ... and with no cache-creation rate either, the prompt rate: 500*0.00001 = 0.005.
	want = decimal.RequireFromString("0.0066")
	if got := c.Calculate("m", cachedUsage(300)); !got.Equal(want) {
		t.Errorf("unset 1h and creation rates: Calculate() = %v, want %v (the prompt rate)", got, want)
	}
}

func TestCalculateOneHourTokensBeyondCacheCreationTokensPriceEveryWriteAtTheOneHourRate(t *testing.T) {
	c := NewCalculator(PriceTable{"m": memberPrice()})
	// A 1h count above the write total is a provider/adapter invariant
	// violation; the conservative reading bills every write at 1h:
	// 500*0.00002 = 0.01.
	want := decimal.RequireFromString("0.0116")
	if got := c.Calculate("m", cachedUsage(900)); !got.Equal(want) {
		t.Errorf("1h > writes: Calculate() = %v, want %v (every write at the 1h rate)", got, want)
	}
	// A negative count is treated as none.
	want = decimal.RequireFromString("0.00785")
	if got := c.Calculate("m", cachedUsage(-7)); !got.Equal(want) {
		t.Errorf("negative 1h: Calculate() = %v, want %v (no 1h writes)", got, want)
	}
}

func TestCalculateWebSearchRequestsAddThePerRequestFee(t *testing.T) {
	c := NewCalculator(PriceTable{"m": memberPrice()})
	u := Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, WebSearchRequests: 3}
	// tokens 0.001 + 0.0005; searches 3*0.01 = 0.03.
	want := decimal.RequireFromString("0.0315")
	if got := c.Calculate("m", u); !got.Equal(want) {
		t.Errorf("Calculate() = %v, want %v", got, want)
	}
	p := memberPrice()
	p.WebSearchPerRequest = nil
	c = NewCalculator(PriceTable{"m": p})
	want = decimal.RequireFromString("0.0015")
	if got := c.Calculate("m", u); !got.Equal(want) {
		t.Errorf("unset web search rate: Calculate() = %v, want %v (searches unpriced)", got, want)
	}
}

func TestCalculateUSInferenceMultiplierAppliesToTokensNotToSearchFees(t *testing.T) {
	c := NewCalculator(PriceTable{"m": memberPrice()})
	u := Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, WebSearchRequests: 3, InferenceGeo: InferenceGeoUS}
	// (0.001 + 0.0005) * 1.1 = 0.00165, plus the 0.03 of searches untouched.
	want := decimal.RequireFromString("0.03165")
	if got := c.Calculate("m", u); !got.Equal(want) {
		t.Errorf("us: Calculate() = %v, want %v", got, want)
	}
	u.InferenceGeo = "global"
	want = decimal.RequireFromString("0.0315")
	if got := c.Calculate("m", u); !got.Equal(want) {
		t.Errorf("global: Calculate() = %v, want %v (no multiplier)", got, want)
	}
	p := memberPrice()
	p.USInferenceMultiplier = nil
	c = NewCalculator(PriceTable{"m": p})
	u.InferenceGeo = InferenceGeoUS
	if got := c.Calculate("m", u); !got.Equal(want) {
		t.Errorf("unset multiplier: Calculate() = %v, want %v (no multiplier)", got, want)
	}
}
