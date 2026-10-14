package costaccounting

import (
	"testing"

	"github.com/shopspring/decimal"
)

// tierPrice is memberPrice plus OpenAI-style service-tier multipliers
// (price_table.<model>.service_tier_multipliers): flex 0.5x, priority 2x
// (fast is its alias), ultrafast 6x -- gpt-6.1-sol's published ratios.
func tierPrice() ModelPrice {
	p := memberPrice()
	p.ServiceTierMultipliers = map[string]decimal.Decimal{
		"flex":      decimal.RequireFromString("0.5"),
		"priority":  decimal.RequireFromString("2"),
		"ultrafast": decimal.RequireFromString("6"),
	}
	return p
}

// cachedUsage(300) at memberPrice: 100 fresh x 0.00001 + 400 reads x
// 0.00000025 + 200 5m writes x 0.0000125 + 300 1h writes x 0.00002 + 10 out
// x 0.00005 = 0.0101. The tier multiplier scales every one of those
// categories (the pricing page's per-tier tables are exact multiples of
// Standard across input, cached input, cache write and output).
func TestCalculateServiceTierMultiplierScalesEveryTokenCategory(t *testing.T) {
	c := NewCalculator(PriceTable{"m": tierPrice()})
	base := decimal.RequireFromString("0.0101")
	for tier, factor := range map[string]string{
		"fast": "2", "priority": "2", "flex": "0.5", "ultrafast": "6",
		"default": "1", "standard": "1", "auto": "1", "": "1",
	} {
		u := cachedUsage(300)
		u.ServiceTier = tier
		want := base.Mul(decimal.RequireFromString(factor))
		if got := c.Calculate("m", u); !got.Equal(want) {
			t.Errorf("served %q: Calculate() = %v, want %v", tier, got, want)
		}
	}
}

// A served tier the operator did not price (scale, a future tier) bills at
// Standard: the never-undercount posture cannot invent a discount, and
// refusing a 200 the upstream already billed is not house style; the
// dataplane logs it from ServiceTierMultiplier's second return.
func TestCalculateUnpricedServiceTierAppliesNoMultiplier(t *testing.T) {
	c := NewCalculator(PriceTable{"m": tierPrice()})
	u := cachedUsage(300)
	u.ServiceTier = "scale"
	if got := c.Calculate("m", u); !got.Equal(decimal.RequireFromString("0.0101")) {
		t.Errorf("unpriced tier: Calculate() = %v, want the Standard cost", got)
	}
	if m, known := c.ServiceTierMultiplier("m", "scale"); known || !m.Equal(decimal.NewFromInt(1)) {
		t.Errorf("ServiceTierMultiplier(scale) = %v, %v; want 1, false", m, known)
	}
	if m, known := c.ServiceTierMultiplier("m", "fast"); !known || !m.Equal(decimal.NewFromInt(2)) {
		t.Errorf("ServiceTierMultiplier(fast) = %v, %v; want 2 (priority's alias), true", m, known)
	}
	for _, baseTier := range []string{"", "default", "standard", "auto"} {
		if m, known := c.ServiceTierMultiplier("m", baseTier); !known || !m.Equal(decimal.NewFromInt(1)) {
			t.Errorf("ServiceTierMultiplier(%q) = %v, %v; want 1, true (the base tier)", baseTier, m, known)
		}
	}
	// An unpriced model bills $0 by design and kelvran doctor names it; there
	// is no multiplier to add before the row exists, so nothing to warn about.
	if m, known := c.ServiceTierMultiplier("unknown-model", "fast"); !known || !m.Equal(decimal.NewFromInt(1)) {
		t.Errorf("ServiceTierMultiplier on an unpriced model = %v, %v; want 1, true", m, known)
	}
	if m, known := c.ServiceTierMultiplier("unknown-model", "default"); !known || !m.Equal(decimal.NewFromInt(1)) {
		t.Errorf("ServiceTierMultiplier(default) on an unpriced model = %v, %v; want 1, true", m, known)
	}
}

// A backend's echo is its own say-so: case and padding around a priced tier
// must not turn it into an unknown one.
func TestCalculateServiceTierIsCaseAndWhitespaceInsensitive(t *testing.T) {
	c := NewCalculator(PriceTable{"m": tierPrice()})
	base := decimal.RequireFromString("0.0101")
	for tier, mult := range map[string]string{" priority ": "2", "Priority": "2", "FAST": "2", "\tflex\n": "0.5"} {
		u := cachedUsage(300)
		u.ServiceTier = tier
		if got, want := c.Calculate("m", u), base.Mul(decimal.RequireFromString(mult)); !got.Equal(want) {
			t.Errorf("served %q: Calculate() = %v, want %v", tier, got, want)
		}
		if _, known := c.ServiceTierMultiplier("m", tier); !known {
			t.Errorf("served %q reported as unknown", tier)
		}
	}
}

// Without multipliers configured every tier bills at Standard (the behaviour
// before this field existed).
func TestCalculateWithoutServiceTierMultipliersIgnoresTheTier(t *testing.T) {
	c := NewCalculator(PriceTable{"m": memberPrice()})
	u := cachedUsage(300)
	u.ServiceTier = "ultrafast"
	if got := c.Calculate("m", u); !got.Equal(decimal.RequireFromString("0.0101")) {
		t.Errorf("no multipliers: Calculate() = %v, want 0.0101", got)
	}
}

// The tier multiplier composes with the US-inference multiplier on the token
// sum and never scales the per-request web-search fee (the same place the US
// multiplier applies): (0.001 + 0.0005) x 2 x 1.1 = 0.0033, plus 0.03 of
// searches untouched.
func TestCalculateServiceTierMultiplierComposesWithUSAndSparesSearchFees(t *testing.T) {
	c := NewCalculator(PriceTable{"m": tierPrice()})
	u := Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, WebSearchRequests: 3, InferenceGeo: InferenceGeoUS, ServiceTier: "fast"}
	if got, want := c.Calculate("m", u), decimal.RequireFromString("0.0333"); !got.Equal(want) {
		t.Errorf("fast + us + searches: Calculate() = %v, want %v", got, want)
	}
}

// The served tier wins over the requested one (a ramp-downgraded fast request
// is served and billed as default); when the response is silent, a requested
// PREMIUM tier is billed at its multiplier (never undercount a lever the
// client pulled) while a requested discount is never applied unconfirmed.
func TestCalculateFallsBackToTheRequestedPremiumTierOnlyWhenTheResponseIsSilent(t *testing.T) {
	c := NewCalculator(PriceTable{"m": tierPrice()})
	base := decimal.RequireFromString("0.0101")
	for name, tc := range map[string]struct {
		served, requested string
		factor            string
	}{
		"served default beats requested fast":      {"default", "fast", "1"},
		"served flex beats requested fast":         {"flex", "fast", "0.5"},
		"silent response, requested fast":          {"", "fast", "2"},
		"silent response, requested ultrafast":     {"", "ultrafast", "6"},
		"silent response, requested flex (no cut)": {"", "flex", "1"},
		"silent response, requested auto":          {"", "auto", "1"},
		"silent response, nothing requested":       {"", "", "1"},
	} {
		u := cachedUsage(300)
		u.ServiceTier, u.RequestedServiceTier = tc.served, tc.requested
		want := base.Mul(decimal.RequireFromString(tc.factor))
		if got := c.Calculate("m", u); !got.Equal(want) {
			t.Errorf("%s: Calculate() = %v, want %v", name, got, want)
		}
	}
}
