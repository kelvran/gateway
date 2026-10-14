package costaccounting

import (
	"testing"

	"github.com/shopspring/decimal"
)

// longContextPrice is tierPrice plus an OpenAI-shaped long-context tier
// (price_table.<model>.long_context; round-5 Tier A item 5): above 1000
// cache-inclusive prompt tokens the whole request's input side (fresh prompt,
// cache reads, 5m and 1h cache writes) is 2x and its completion 1.5x -- the
// published rule for the GPT-6 and GPT-5.6 lines above 272K input tokens.
func longContextPrice() ModelPrice {
	p := tierPrice()
	p.LongContext = &LongContextTier{
		AboveTokens:      1000,
		InputMultiplier:  decimal.RequireFromString("2"),
		OutputMultiplier: decimal.RequireFromString("1.5"),
	}
	return p
}

// longUsage is cachedUsage(300) with the prompt count moved: the 400 reads and
// 500 writes stay, so the fresh slice is prompt - 900.
func longUsage(prompt int) Usage {
	u := cachedUsage(300)
	u.PromptTokens = prompt
	u.TotalTokens = prompt + 10
	return u
}

// At 1001 prompt tokens: fresh 101 x 0.00001 + 400 reads x 0.00000025 + 200
// 5m writes x 0.0000125 + 300 1h writes x 0.00002 = 0.00961 input-side, x2 =
// 0.01922; 10 out x 0.00005 = 0.0005, x1.5 = 0.00075; 0.01997 in all. The two
// multipliers are separate: a single factor on the sum would be wrong on both
// providers (Anthropic 5x/5x is the only symmetric case).
func TestCalculateLongContextTierScalesInputAndOutputSeparately(t *testing.T) {
	c := NewCalculator(PriceTable{"m": longContextPrice()})
	if got, want := c.Calculate("m", longUsage(1001)), decimal.RequireFromString("0.01997"); !got.Equal(want) {
		t.Errorf("Calculate(1001 prompt tokens) = %v, want %v", got, want)
	}
}

// "Prompts over 100,000 tokens" / "> 272K input tokens": the threshold is
// strictly greater, so a request exactly at above_tokens pays Standard.
func TestCalculateLongContextThresholdIsStrictlyGreater(t *testing.T) {
	c := NewCalculator(PriceTable{"m": longContextPrice()})
	for prompt, want := range map[int]string{1000: "0.0101", 999: "0.01009", 1001: "0.01997"} {
		if got := c.Calculate("m", longUsage(prompt)); !got.Equal(decimal.RequireFromString(want)) {
			t.Errorf("Calculate(%d prompt tokens) = %v, want %s", prompt, got, want)
		}
	}
}

// A row without long_context prices every prompt length at Standard: the
// behaviour before this field existed, so an operator who sets nothing sees
// no change (and Claude 4.6+ bill the 1M window at Standard by design).
func TestCalculateWithoutLongContextIgnoresPromptLength(t *testing.T) {
	c := NewCalculator(PriceTable{"m": tierPrice()})
	if got, want := c.Calculate("m", longUsage(1001)), decimal.RequireFromString("0.01011"); !got.Equal(want) {
		t.Errorf("Calculate(1001 prompt tokens, no tier) = %v, want %v", got, want)
	}
}

// The long-context tier picks the rates; the US multiplier and the served
// service tier then scale the token sum as before, and the search fee is
// still added unscaled: 0.01997 x 1.1 x 2 + 3 x 0.01 = 0.073934. Both
// pricing pages say the multipliers stack on the long-context prices.
func TestCalculateLongContextComposesWithUSAndTierAndSparesSearchFees(t *testing.T) {
	c := NewCalculator(PriceTable{"m": longContextPrice()})
	u := longUsage(1001)
	u.InferenceGeo = InferenceGeoUS
	u.ServiceTier = "fast"
	u.WebSearchRequests = 3
	if got, want := c.Calculate("m", u), decimal.RequireFromString("0.073934"); !got.Equal(want) {
		t.Errorf("Calculate(long + us + fast + 3 searches) = %v, want %v", got, want)
	}
}

// The threshold reads the cache-inclusive prompt count even when the cache
// counts violate the invariant and are discarded: 1001 fresh x 0.00001 x 2 +
// 0.00075 = 0.02077 -- the whole prompt as fresh (the existing rule) at the
// long rate (the prompt is still over the threshold).
func TestCalculateLongContextUsesThePromptCountWhenTheCacheCountsAreInvalid(t *testing.T) {
	c := NewCalculator(PriceTable{"m": longContextPrice()})
	u := longUsage(1001)
	u.CacheReadTokens = 2000
	if got, want := c.Calculate("m", u), decimal.RequireFromString("0.02077"); !got.Equal(want) {
		t.Errorf("Calculate(invalid cache counts) = %v, want %v", got, want)
	}
}

// LongContextEngaged is the dataplane's input for the span attribute: true
// only for a priced model whose row carries the tier and whose prompt
// exceeds it.
func TestLongContextEngagedReportsTheTierDecision(t *testing.T) {
	c := NewCalculator(PriceTable{"m": longContextPrice(), "flat": tierPrice()})
	for name, tc := range map[string]struct {
		model  string
		prompt int
		want   bool
	}{
		"over":           {"m", 1001, true},
		"at":             {"m", 1000, false},
		"under":          {"m", 1, false},
		"flat row":       {"flat", 1001, false},
		"unpriced model": {"unknown", 1001, false},
	} {
		if got := c.LongContextEngaged(tc.model, tc.prompt); got != tc.want {
			t.Errorf("%s: LongContextEngaged(%q, %d) = %v, want %v", name, tc.model, tc.prompt, got, tc.want)
		}
	}
}
