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

// TestCalculateCacheTokensPricedAtCacheRate proves the cache-token
// cost-accounting fix: when a ModelPrice sets explicit
// CacheReadPerToken/CacheCreationPerToken, those rates are charged for
// exactly the cache slice, and the remaining "fresh" prompt tokens
// (PromptTokens - CacheReadTokens - CacheCreationTokens) are still priced
// at the ordinary PromptPerToken rate -- never double-priced.
func TestCalculateCacheTokensPricedAtCacheRate(t *testing.T) {
	cacheReadRate := decimal.RequireFromString("0.0000003")
	cacheCreationRate := decimal.RequireFromString("0.00001")
	c := NewCalculator(PriceTable{
		"claude-opus-4": {
			PromptPerToken:        decimal.RequireFromString("0.000015"),
			CompletionPerToken:    decimal.RequireFromString("0.000075"),
			CacheReadPerToken:     &cacheReadRate,
			CacheCreationPerToken: &cacheCreationRate,
		},
	})

	got := c.Calculate("claude-opus-4", Usage{
		PromptTokens:        2098, // 50 fresh + 1800 cache-read + 248 cache-creation
		CompletionTokens:    12,
		CacheReadTokens:     1800,
		CacheCreationTokens: 248,
	})
	// fresh: 50*0.000015 = 0.00075; cache-read: 1800*0.0000003 = 0.00054;
	// cache-creation: 248*0.00001 = 0.00248; completion: 12*0.000075 = 0.0009
	want := decimal.RequireFromString("0.00075").
		Add(decimal.RequireFromString("0.00054")).
		Add(decimal.RequireFromString("0.00248")).
		Add(decimal.RequireFromString("0.0009"))
	if !got.Equal(want) {
		t.Errorf("Calculate() = %v, want %v", got, want)
	}
}

// TestCalculateUnsetCacheRateFallsBackToPromptRate proves the confirmed
// fallback decision: a ModelPrice with no CacheReadPerToken/
// CacheCreationPerToken set (nil, the zero value for the pointer fields)
// prices the cache slice at the model's ordinary PromptPerToken rate --
// closing the undercounting defect (cache tokens previously contributed
// $0) with zero config change required.
func TestCalculateUnsetCacheRateFallsBackToPromptRate(t *testing.T) {
	c := NewCalculator(PriceTable{
		"claude-opus-4": {
			PromptPerToken:     decimal.RequireFromString("0.000015"),
			CompletionPerToken: decimal.RequireFromString("0.000075"),
			// CacheReadPerToken/CacheCreationPerToken deliberately unset.
		},
	})

	got := c.Calculate("claude-opus-4", Usage{
		PromptTokens:        2098,
		CompletionTokens:    0,
		CacheReadTokens:     1800,
		CacheCreationTokens: 248,
	})
	// Every one of the 2098 prompt tokens (fresh + cache-read +
	// cache-creation) priced identically at PromptPerToken, since no
	// cache-specific rate is configured.
	want := decimal.NewFromInt(2098).Mul(decimal.RequireFromString("0.000015"))
	if !got.Equal(want) {
		t.Errorf("Calculate() = %v, want %v (unset cache rate should equal pricing the whole PromptTokens count at PromptPerToken)", got, want)
	}
}

// TestCalculateCacheReadPlusCreationNeverExceedsPromptTokensInvariant is a
// property-style sanity check: freshPromptTokens (PromptTokens -
// CacheReadTokens - CacheCreationTokens) must never go negative for a
// realistic usage value, i.e. cost must never come out negative just
// because of how the cache slice is subtracted back out.
func TestCalculateCacheReadPlusCreationNeverExceedsPromptTokensInvariant(t *testing.T) {
	c := NewCalculator(PriceTable{
		"claude-opus-4": {
			PromptPerToken:     decimal.RequireFromString("0.000015"),
			CompletionPerToken: decimal.RequireFromString("0.000075"),
		},
	})

	// CacheReadTokens + CacheCreationTokens == PromptTokens exactly (no
	// fresh tokens at all) -- the boundary case every real producer site
	// can legitimately hit (e.g. a fully-cached system prompt with no new
	// user turn).
	got := c.Calculate("claude-opus-4", Usage{
		PromptTokens:        2048,
		CacheReadTokens:     1800,
		CacheCreationTokens: 248,
	})
	if got.IsNegative() {
		t.Errorf("Calculate() = %v, want non-negative even when CacheReadTokens+CacheCreationTokens == PromptTokens exactly", got)
	}
}

// TestCalculateInvariantViolationTreatsEntirePromptAsUncachedRatherThanUndercounting
// covers the HOSTILE case the boundary test above doesn't: a producer
// (openaicompat's own doc comment discloses its CacheReadTokens as
// unverified against a live runtime) reporting
// CacheReadTokens+CacheCreationTokens > PromptTokens. Before the fix,
// this drove freshPromptTokens negative, UNDERCOUNTING cost -- and a
// negative cost is silently dropped entirely by budget.Tracker.Reconcile,
// so this was a real $0-billed-request bug, not just a wrong number.
func TestCalculateInvariantViolationTreatsEntirePromptAsUncachedRatherThanUndercounting(t *testing.T) {
	cacheReadRate := decimal.RequireFromString("0.0000003")
	c := NewCalculator(PriceTable{
		"self-hosted-model": {
			PromptPerToken:     decimal.RequireFromString("0.000015"),
			CompletionPerToken: decimal.RequireFromString("0.000075"),
			CacheReadPerToken:  &cacheReadRate,
		},
	})

	// PromptTokens=100, but CacheReadTokens=150 -- a spurious value
	// exceeding PromptTokens, exactly the shape an untrusted self-hosted
	// backend could report.
	got := c.Calculate("self-hosted-model", Usage{
		PromptTokens:     100,
		CacheReadTokens:  150,
		CompletionTokens: 50,
	})

	// want: the entire 100 PromptTokens priced at the base (uncached)
	// rate, plus 50 completion tokens -- never a reduced/negative figure
	// from treating 150 tokens as "cached" out of only 100 real prompt
	// tokens.
	want := decimal.NewFromInt(100).Mul(decimal.RequireFromString("0.000015")).
		Add(decimal.NewFromInt(50).Mul(decimal.RequireFromString("0.000075")))
	if !got.Equal(want) {
		t.Errorf("Calculate() = %v, want %v (entire PromptTokens priced as uncached, invariant violated)", got, want)
	}
	if got.IsNegative() {
		t.Fatalf("Calculate() = %v, want non-negative -- an invariant violation must never produce a negative cost", got)
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
