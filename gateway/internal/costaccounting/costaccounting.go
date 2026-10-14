// Package costaccounting calculates the dollar cost of a completion's
// token usage against a static, per-model price table.
//
// Decimal arithmetic (github.com/shopspring/decimal), not float64, per
// docs/rfcs/2026-09-02-decimal-cost-accounting.md — this fulfills the
// "Phase 1 upgrade" PRD.md's Decimal-precision requirement was deferred
// to since the initial scaffolding pass.
package costaccounting

import (
	"strings"

	"github.com/shopspring/decimal"
)

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
	// ServiceTier is the processing tier the provider reports it served
	// (adapter.Usage.ServiceTier; OpenAI's response service_tier) and
	// RequestedServiceTier the tier the request asked for, as forwarded
	// (empty when the deployment stripped it or has no such member). The
	// served tier is billed when present; when the response is silent a
	// requested PREMIUM tier is billed at its multiplier -- never undercount a
	// lever the client pulled -- and a requested discount is never applied
	// unconfirmed. See ServiceTierMultipliers and billedTierMultiplier.
	ServiceTier          string
	RequestedServiceTier string
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
	// ServiceTierMultipliers scales the token cost (not the search fees) by
	// the tier a request was served at (price_table.<model>.
	// service_tier_multipliers; round-5 Tier A item 4): OpenAI prices every
	// tier as a per-model multiple of Standard across input, cached input,
	// cache write and output (flex 0.5x, priority/fast 2x on the GPT-6 and
	// GPT-5.6 families, ultrafast 6x where offered). Keys are the lower-case
	// tier names; fast and priority are one price column, so either key
	// serves both (OpenAI accepts both and bills them alike; which literal it
	// echoes depends on the model generation). nil, or a tier the map does
	// not carry, applies no multiplier.
	ServiceTierMultipliers map[string]decimal.Decimal
	// LongContext is price_table.<model>.long_context (round-5 Tier A item
	// 5): the price tier a request pays once its cache-inclusive prompt
	// exceeds AboveTokens. nil means the model has one price schedule for
	// every prompt length (Claude 4.6+ bill the 1M window at Standard;
	// gpt-4o and gemini-2.5-flash are flat).
	LongContext *LongContextTier
}

// LongContextTier is one model's long-context price tier. Both pricing
// pages (read 2026-10-14) express it the same way: above a prompt-length
// threshold the WHOLE request -- not the excess -- pays higher rates, with
// one ratio for the input side and one for the output. Anthropic: Claude
// Haiku 5.5, "prompts over 100,000 tokens" counting cache reads and cache
// writes, 5x on every category. OpenAI: the GPT-6, GPT-5.6 and GPT-5.5
// lines, "> 272K input tokens", 2x input, cached input and cache writes,
// 1.5x output. Gemini 2.5-pro/3.1-pro: 200k, 2x input and caching, 1.5x
// output. Ratios of Standard rather than absolute rates because every
// published tier is an exact multiple, and a ratio stays true when the
// base rates change (`kelvran init --price` keeps it, like the
// service-tier multipliers).
type LongContextTier struct {
	// AboveTokens is the cache-inclusive prompt count (Usage.PromptTokens:
	// fresh + cache reads + cache writes) a request must EXCEED -- strictly
	// greater -- to pay the tier. The pricing pages' "over 100,000" and
	// "> 272K" are both strict; OpenAI's K is taken as 1,000 (its 128K
	// max output is 128,000), an assumption the docs record.
	AboveTokens int
	// InputMultiplier scales the fresh prompt, the cache reads and both
	// cache-write slices; OutputMultiplier scales the completion. Both at
	// least 1 (the loader refuses less: a long prompt never bills under
	// Standard). The US and service-tier multipliers and the search fee
	// compose on top exactly as without the tier.
	InputMultiplier  decimal.Decimal
	OutputMultiplier decimal.Decimal
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
//
// The long-context tier (round-5 Tier A item 5): when the row carries
// LongContext and the cache-inclusive PromptTokens exceeds AboveTokens, the
// input side (fresh prompt, cache reads, both cache-write slices) is scaled
// by InputMultiplier and the completion by OutputMultiplier BEFORE the US
// and service-tier multipliers -- selecting the tier's rates, which both
// pricing pages then multiply further; the search fee stays outside.
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
	inputCost := decimal.NewFromInt(int64(freshPromptTokens)).Mul(price.PromptPerToken)
	inputCost = inputCost.Add(decimal.NewFromInt(int64(cacheReadTokens)).Mul(resolveCacheRate(price.CacheReadPerToken, price.PromptPerToken)))
	oneHourTokens := clampOneHourTokens(usage.CacheCreation1hTokens, cacheCreationTokens)
	fiveMinuteRate := resolveCacheRate(price.CacheCreationPerToken, price.PromptPerToken)
	inputCost = inputCost.Add(decimal.NewFromInt(int64(cacheCreationTokens - oneHourTokens)).Mul(fiveMinuteRate))
	inputCost = inputCost.Add(decimal.NewFromInt(int64(oneHourTokens)).Mul(resolveCacheRate(price.CacheCreation1hPerToken, fiveMinuteRate)))
	outputCost := decimal.NewFromInt(int64(usage.CompletionTokens)).Mul(price.CompletionPerToken)
	if price.longContextEngaged(usage.PromptTokens) {
		inputCost = inputCost.Mul(price.LongContext.InputMultiplier)
		outputCost = outputCost.Mul(price.LongContext.OutputMultiplier)
	}
	cost := inputCost.Add(outputCost)
	if usage.InferenceGeo == InferenceGeoUS && price.USInferenceMultiplier != nil {
		cost = cost.Mul(*price.USInferenceMultiplier)
	}
	if m := billedTierMultiplier(price, usage); !m.Equal(decimalOne) {
		cost = cost.Mul(m)
	}
	if price.WebSearchPerRequest != nil && usage.WebSearchRequests > 0 {
		cost = cost.Add(decimal.NewFromInt(int64(usage.WebSearchRequests)).Mul(*price.WebSearchPerRequest))
	}
	return cost
}

// longContextEngaged reports whether promptTokens (cache-inclusive) puts a
// request into the row's long-context tier: strictly above AboveTokens, on
// a row that has one. The raw prompt count is compared, not the
// invariant-corrected cache split -- the threshold is about prompt length,
// which the provider reported whole even when its cache breakdown is off.
func (p ModelPrice) longContextEngaged(promptTokens int) bool {
	return p.LongContext != nil && promptTokens > p.LongContext.AboveTokens
}

// LongContextEngaged reports whether a request of promptTokens
// (cache-inclusive) to model was priced at its long-context tier -- the
// dataplane's input for the kelvran.pricing.long_context_tier span
// attribute. False for an unpriced model or a row without the tier.
func (c *Calculator) LongContextEngaged(model string, promptTokens int) bool {
	price, ok := c.prices[model]
	return ok && price.longContextEngaged(promptTokens)
}

var decimalOne = decimal.NewFromInt(1)

// isBaseServiceTier reports whether tier names the Standard rate itself:
// empty (not reported), OpenAI's default, Anthropic's standard, or auto (a
// request-side routing preference, never a served tier).
func isBaseServiceTier(tier string) bool {
	switch tier {
	case "", "default", "standard", "auto":
		return true
	}
	return false
}

// tierMultiplier resolves one tier against price.ServiceTierMultipliers: 1
// for a base tier, the configured multiple for a priced tier (fast and
// priority serve as each other's alias), and (1, false) for a non-base tier
// the map does not carry -- the caller bills Standard and may warn.
func tierMultiplier(price ModelPrice, tier string) (decimal.Decimal, bool) {
	// Lower-cased and trimmed: OpenAI emits lower case, but a self-hosted
	// backend's echo is its own say-so and a stray space must not turn a
	// priced tier into an "unknown" one.
	tier = strings.TrimSpace(strings.ToLower(tier))
	if isBaseServiceTier(tier) {
		return decimalOne, true
	}
	if m, ok := price.ServiceTierMultipliers[tier]; ok {
		return m, true
	}
	switch tier {
	case "fast":
		if m, ok := price.ServiceTierMultipliers["priority"]; ok {
			return m, true
		}
	case "priority":
		if m, ok := price.ServiceTierMultipliers["fast"]; ok {
			return m, true
		}
	}
	return decimalOne, false
}

// billedTierMultiplier is the multiple Calculate applies for usage: the
// served tier when the response names one (the provider bills what it ran --
// a fast request ramp-downgraded to default is Standard), else the requested
// tier only when its multiple is at least 1 (never an unconfirmed discount),
// else 1.
func billedTierMultiplier(price ModelPrice, usage Usage) decimal.Decimal {
	if usage.ServiceTier != "" {
		m, _ := tierMultiplier(price, usage.ServiceTier)
		return m
	}
	if m, known := tierMultiplier(price, usage.RequestedServiceTier); known && m.GreaterThanOrEqual(decimalOne) {
		return m
	}
	return decimalOne
}

// ServiceTierMultiplier reports the multiple model's price table applies to
// a served tier and whether the table accounts for it -- the dataplane's
// input for the service_tier_unpriced warning. A base tier is 1 and
// accounted for on every model (tierMultiplier says so for a priced model;
// the common "default" echo must never warn);
// a model with no price row is 1 and accounted for too (it bills $0 by
// design and kelvran doctor already names it -- there is no multiplier to
// add before the row exists); only a priced model served at a non-base tier
// its map lacks is (1, false).
func (c *Calculator) ServiceTierMultiplier(model, tier string) (decimal.Decimal, bool) {
	price, ok := c.prices[model]
	if !ok {
		return decimalOne, true
	}
	return tierMultiplier(price, tier)
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
