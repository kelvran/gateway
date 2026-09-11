// Package costaccounting calculates the dollar cost of a completion's
// token usage against a static, per-model price table.
//
// Decimal arithmetic (github.com/shopspring/decimal), not float64, per
// docs/rfcs/2026-09-02-decimal-cost-accounting.md — this fulfills the
// "Phase 1 upgrade" PRD.md's Decimal-precision requirement was deferred
// to since the initial scaffolding pass.
package costaccounting

import "github.com/shopspring/decimal"

// Usage is token accounting for a single completion. This is a local
// type (mirroring internal/adapter.Usage's shape, including the
// cache-inclusive PromptTokens convention -- see that type's doc comment)
// rather than a direct dependency on the adapter package, so
// costaccounting stays a leaf that doesn't need to know about the
// canonical request/response schema.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CacheReadTokens/CacheCreationTokens are the subset of PromptTokens
	// served from / spent creating a provider-side prompt cache entry --
	// see adapter.Usage's doc comment. Invariant, enforced by every
	// producer: CacheReadTokens+CacheCreationTokens <= PromptTokens.
	CacheReadTokens     int
	CacheCreationTokens int
}

// ModelPrice is the per-token price for one model's prompt and completion
// tokens, in USD, plus OPTIONAL per-cache-class rates.
// CacheReadPerToken/CacheCreationPerToken are *decimal.Decimal, not a bare
// decimal.Decimal: nil means "not configured for this model" -- distinct
// from an operator's explicit decimal.Zero (e.g. "cache reads are free
// for this model"), which a bare zero-value decimal.Decimal could never
// represent. See Calculate/resolveCacheRate for the fallback behavior
// when nil.
type ModelPrice struct {
	PromptPerToken        decimal.Decimal
	CompletionPerToken    decimal.Decimal
	CacheReadPerToken     *decimal.Decimal
	CacheCreationPerToken *decimal.Decimal
}

// PriceTable maps a model name to its ModelPrice.
type PriceTable map[string]ModelPrice

// Calculator computes cost against a fixed, static PriceTable loaded at
// startup (see internal/gateway/controlplane.Config).
type Calculator struct {
	prices PriceTable
}

// NewCalculator constructs a Calculator against the given price table.
func NewCalculator(prices PriceTable) *Calculator {
	return &Calculator{prices: prices}
}

// Calculate returns the dollar cost of usage for model. An unknown model
// (missing from the configured price table) returns decimal.Zero rather
// than guessing a price — this pass has no error-reporting path wired for
// pricing gaps yet, so a visibly-zero cost is the honest default, not a
// silently wrong estimate.
//
// freshPromptTokens excludes the cache slice already counted inside
// PromptTokens (see Usage's doc comment) -- priced at the base
// PromptPerToken rate; the cache slice is priced separately, at its own
// (possibly-unset) rate, per resolveCacheRate.
//
// The CacheReadTokens+CacheCreationTokens <= PromptTokens invariant Usage's
// own doc comment names is enforced BY EVERY PRODUCER Calculate has ever
// been re-verified against directly (anthropic.go, bedrock.go, gemini.go,
// openai.go) -- but openaicompat's own doc comment explicitly discloses its
// CacheReadTokens as "runtime-dependent... NOT independently verified
// against a live runtime," i.e. it is populated from a self-hosted
// backend's own unverified say-so, not from a value this codebase itself
// constructs. A buggy or non-conformant self-hosted backend reporting
// CacheReadTokens+CacheCreationTokens > PromptTokens would otherwise drive
// freshPromptTokens negative, understating (never overstating) cost -- and
// budget.Tracker.Reconcile silently drops a negative cost entirely rather
// than billing anything, so this isn't just a wrong number, it's a real
// $0-billed request. If the invariant is violated for a given usage value,
// this treats the ENTIRE PromptTokens as uncached (fresh) rather than try
// to salvage a partial cache split from data already shown to be
// untrustworthy -- deliberately the conservative direction (this can only
// ever price a request AT OR ABOVE what correct cache accounting would,
// never below), matching this package's own established "when in doubt,
// don't undercount" precedent (the cache-rate-unset fallback above).
func (c *Calculator) Calculate(model string, usage Usage) decimal.Decimal {
	price, ok := c.prices[model]
	if !ok {
		return decimal.Zero
	}
	cacheReadTokens, cacheCreationTokens := usage.CacheReadTokens, usage.CacheCreationTokens
	if cacheReadTokens < 0 || cacheCreationTokens < 0 || cacheReadTokens+cacheCreationTokens > usage.PromptTokens {
		cacheReadTokens, cacheCreationTokens = 0, 0
	}
	freshPromptTokens := usage.PromptTokens - cacheReadTokens - cacheCreationTokens
	cost := decimal.NewFromInt(int64(freshPromptTokens)).Mul(price.PromptPerToken)
	cost = cost.Add(decimal.NewFromInt(int64(cacheReadTokens)).Mul(resolveCacheRate(price.CacheReadPerToken, price.PromptPerToken)))
	cost = cost.Add(decimal.NewFromInt(int64(cacheCreationTokens)).Mul(resolveCacheRate(price.CacheCreationPerToken, price.PromptPerToken)))
	cost = cost.Add(decimal.NewFromInt(int64(usage.CompletionTokens)).Mul(price.CompletionPerToken))
	return cost
}

// resolveCacheRate returns *rate, or fallbackRate when rate is nil
// (unset). Per the confirmed decision, every caller in this package
// passes price.PromptPerToken as fallbackRate: an operator who hasn't
// configured an explicit cache rate for a model has that model's cache
// tokens priced as ordinary prompt tokens, closing the undercounting
// defect (cache tokens previously contributed $0 to cost) immediately,
// with zero config change required.
func resolveCacheRate(rate *decimal.Decimal, fallbackRate decimal.Decimal) decimal.Decimal {
	if rate != nil {
		return *rate
	}
	return fallbackRate
}
