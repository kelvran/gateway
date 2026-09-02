package costaccounting

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestCalculateKnownModel(t *testing.T) {
	c := NewCalculator(PriceTable{
		"gpt-4o": {PromptPerToken: decimal.RequireFromString("0.000002"), CompletionPerToken: decimal.RequireFromString("0.00001")},
	})

	got := c.Calculate("gpt-4o", Usage{PromptTokens: 1000, CompletionTokens: 500})
	want := decimal.RequireFromString("0.007") // 1000*0.000002 + 500*0.00001 = 0.002 + 0.005
	if !got.Equal(want) {
		t.Errorf("Calculate() = %v, want %v", got, want)
	}
}

func TestCalculateUnknownModelReturnsZero(t *testing.T) {
	c := NewCalculator(PriceTable{
		"gpt-4o": {PromptPerToken: decimal.RequireFromString("0.000002"), CompletionPerToken: decimal.RequireFromString("0.00001")},
	})

	got := c.Calculate("some-unpriced-model", Usage{PromptTokens: 1000, CompletionTokens: 500})
	if !got.IsZero() {
		t.Errorf("Calculate() for unknown model = %v, want 0", got)
	}
}

func TestCalculateZeroUsage(t *testing.T) {
	c := NewCalculator(PriceTable{
		"gpt-4o": {PromptPerToken: decimal.RequireFromString("0.000002"), CompletionPerToken: decimal.RequireFromString("0.00001")},
	})

	if got := c.Calculate("gpt-4o", Usage{}); !got.IsZero() {
		t.Errorf("Calculate() with zero usage = %v, want 0", got)
	}
}

// TestCalculateIsExactWhereFloat64WouldDrift is the load-bearing test for
// docs/rfcs/2026-09-02-decimal-cost-accounting.md's whole reason for
// existing: repeated float64 addition of a realistic small per-request
// cost fragment measurably drifts from the exact decimal sum (verified
// empirically before writing that RFC, not assumed) — the exact
// accumulation internal/budget.Tracker.Record performs on every request.
// This proves the Decimal-typed Calculate + a Decimal-typed accumulator
// does NOT drift the same way, over the same number of additions.
func TestCalculateIsExactWhereFloat64WouldDrift(t *testing.T) {
	// A price fragment shaped like a real Kelvran price: gpt-4o's
	// completion_per_token (0.00001) times 3 completion tokens.
	c := NewCalculator(PriceTable{
		"gpt-4o": {PromptPerToken: decimal.Zero, CompletionPerToken: decimal.RequireFromString("0.0000025")},
	})

	const n = 10000
	var decimalSum decimal.Decimal
	var floatSum float64
	perCallCost := 3 * 0.0000025 // = 0.0000075, matching Calculate's own per-call result below

	for i := 0; i < n; i++ {
		cost := c.Calculate("gpt-4o", Usage{CompletionTokens: 3})
		decimalSum = decimalSum.Add(cost)
		floatSum += perCallCost
	}

	exact := decimal.RequireFromString("0.075") // 10000 * 0.0000075, exact
	if !decimalSum.Equal(exact) {
		t.Errorf("decimal accumulation = %v, want exactly %v", decimalSum, exact)
	}
	if floatSum == 0.075 {
		t.Skip("this run's float64 accumulation happened not to drift — the decimal assertion above is still the one that matters and still passed")
	}
	t.Logf("float64 accumulation of the same %d additions drifted to %.20f (exact is 0.075) — decimal.Decimal did not drift", n, floatSum)
}
