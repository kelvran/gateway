// Package costaccounting calculates the dollar cost of a completion's
// token usage against a static, per-model price table.
//
// float64 arithmetic is used for this pass. PRD.md's Decimal-precision
// requirement is a documented Phase 1 upgrade, not silently dropped — see
// docs/rfcs/2026-09-02-initial-code-scaffolding.md's scope boundary.
package costaccounting

// Usage is token accounting for a single completion. This is a local
// type (mirroring internal/adapter.Usage's shape) rather than a direct
// dependency on the adapter package, so costaccounting stays a leaf that
// doesn't need to know about the canonical request/response schema.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ModelPrice is the per-token price for one model's prompt and
// completion tokens, in USD.
type ModelPrice struct {
	PromptPerToken     float64
	CompletionPerToken float64
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
// (missing from the configured price table) returns 0 rather than
// guessing a price — this pass has no error-reporting path wired for
// pricing gaps yet, so a visibly-zero cost is the honest default, not a
// silently wrong estimate.
func (c *Calculator) Calculate(model string, usage Usage) float64 {
	price, ok := c.prices[model]
	if !ok {
		return 0
	}
	return float64(usage.PromptTokens)*price.PromptPerToken +
		float64(usage.CompletionTokens)*price.CompletionPerToken
}
