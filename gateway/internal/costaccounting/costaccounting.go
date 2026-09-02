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
	PromptPerToken     decimal.Decimal
	CompletionPerToken decimal.Decimal
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
func (c *Calculator) Calculate(model string, usage Usage) decimal.Decimal {
	price, ok := c.prices[model]
	if !ok {
		return decimal.Zero
	}
	promptCost := decimal.NewFromInt(int64(usage.PromptTokens)).Mul(price.PromptPerToken)
	completionCost := decimal.NewFromInt(int64(usage.CompletionTokens)).Mul(price.CompletionPerToken)
	return promptCost.Add(completionCost)
}
