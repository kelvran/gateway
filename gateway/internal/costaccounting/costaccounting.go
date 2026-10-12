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
	// CacheCreation1hTokens is the subset of CacheCreationTokens written with
	// the 1-hour TTL (adapter.Usage.CacheCreation1hTokens); a count outside
	// [0, CacheCreationTokens] is clamped -- below to 0, above to every write
	// (the conservative reading, never undercounting).
	CacheCreation1hTokens int
	// WebSearchRequests is the number of server-side web searches the
	// provider billed on this request (adapter.Usage.WebSearchRequests).
	WebSearchRequests int
	// InferenceGeo is the provider-reported inference geography
	// (adapter.Usage.InferenceGeo); InferenceGeoUS applies the model's
	// USInferenceMultiplier to every token category.
	InferenceGeo string
}

// InferenceGeoUS is the usage.inference_geo value Anthropic bills at a
// multiplier (1.1x on the pricing page, 2026-10-13).
const InferenceGeoUS = "us"

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
	// CacheCreation1hPerToken prices the 1-hour slice of cache writes
	// (price_table.<model>.cache_creation_1h_per_token). nil falls back to
	// the resolved CacheCreationPerToken -- the behaviour before this field
	// existed, so an operator who sets nothing sees no change -- which
	// undercounts a 1h write by the documented 2x-vs-1.25x gap until set.
	CacheCreation1hPerToken *decimal.Decimal
	// WebSearchPerRequest is the fee per server-side web search
	// (price_table.<model>.web_search_per_request). nil leaves searches
	// unpriced, as before this field existed.
	WebSearchPerRequest *decimal.Decimal
	// USInferenceMultiplier scales the token cost (not the search fees) when
	// the provider reports InferenceGeoUS (price_table.<model>.
	// us_inference_multiplier, at least 1). nil applies no multiplier.
	USInferenceMultiplier *decimal.Decimal
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
//
// Reasoning/thinking tokens (adapter.Usage.ReasoningTokens, tracked since
// 2026-10-07 for telemetry) deliberately have NO price here and no field on
// this package's Usage: every provider that reports them already counts
// them inside CompletionTokens and bills them at the plain output rate, so
// CompletionPerToken * CompletionTokens is already the right charge —
// pricing them separately would double-bill.
//
// The pricing members (item (i), 2026-10-13): cache writes split into the
// 5-minute slice at CacheCreationPerToken and the 1-hour slice at
// CacheCreation1hPerToken; every token category is then scaled by
// USInferenceMultiplier when InferenceGeo is InferenceGeoUS; the web-search
// fee (WebSearchRequests x WebSearchPerRequest) is added last, unscaled --
// the pricing page applies the US multiplier to token categories only.
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
	oneHourTokens := clampOneHourTokens(usage.CacheCreation1hTokens, cacheCreationTokens)
	fiveMinuteRate := resolveCacheRate(price.CacheCreationPerToken, price.PromptPerToken)
	cost = cost.Add(decimal.NewFromInt(int64(cacheCreationTokens - oneHourTokens)).Mul(fiveMinuteRate))
	cost = cost.Add(decimal.NewFromInt(int64(oneHourTokens)).Mul(resolveCacheRate(price.CacheCreation1hPerToken, fiveMinuteRate)))
	cost = cost.Add(decimal.NewFromInt(int64(usage.CompletionTokens)).Mul(price.CompletionPerToken))
	if usage.InferenceGeo == InferenceGeoUS && price.USInferenceMultiplier != nil {
		cost = cost.Mul(*price.USInferenceMultiplier)
	}
	if price.WebSearchPerRequest != nil && usage.WebSearchRequests > 0 {
		cost = cost.Add(decimal.NewFromInt(int64(usage.WebSearchRequests)).Mul(*price.WebSearchPerRequest))
	}
	return cost
}

// clampOneHourTokens bounds the 1-hour write count to [0, writes]: a count
// above the write total is an adapter/provider invariant violation read
// conservatively (every write at the 1-hour rate); a negative count is none.
func clampOneHourTokens(oneHour, writes int) int {
	if oneHour < 0 {
		return 0
	}
	if oneHour > writes {
		return writes
	}
	return oneHour
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
