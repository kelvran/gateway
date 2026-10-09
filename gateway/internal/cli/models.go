package cli

import (
	"fmt"
	"sort"
	"strings"
)

// priceTableAsOf is the date the embedded prices below were read from the
// providers' own pricing pages; `kelvran init` writes it as a comment above
// the price_table it emits, because prices and Bedrock ids go stale.
//
// Sources, all fetched 2026-10-10:
//   - docs.anthropic.com/en/docs/about-claude/pricing (standard tier; the
//     Haiku 5.5 row is the "prompts up to 100,000 tokens" tier) and
//     docs.anthropic.com/en/docs/about-claude/models/overview (Claude API
//     aliases and the Amazon Bedrock ids beside them). The overview lists the
//     base id `anthropic.<alias>`; Bedrock serves these models through the
//     global inference profile `global.anthropic.<alias>`, which is what the
//     table carries: on 2026-10-10 a live Converse request through a gateway
//     configured by `kelvran init` returned 400 for the base id and 200 for the
//     global form (the regional `us.`/`eu.` profiles are a --upstream-model
//     override);
//   - ai.google.dev/gemini-api/docs/pricing (paid tier, text input, output
//     including thinking tokens);
//   - gpt-4o: the repository's own documented example
//     (gateway/config.example.yaml, docs/tutorials/quickstart.md) — the
//     OpenAI pricing page's model table does not render without a browser.
//
// Bedrock prices Anthropic models at Anthropic's first-party rates on its
// global endpoints (regional endpoints carry a 10 % premium the table does
// not apply); the price_table is keyed by canonical model, so a bedrock
// deployment of claude-sonnet-5-5 shares the entry below.
const priceTableAsOf = "2026-10-10"

// modelEntry is one row of the embedded table: a canonical (client-facing)
// model id, the vendor whose hosted endpoint the price describes, the four
// price_table rates in USD per token as decimal strings (written verbatim —
// the loader parses money from the source text, never through float64) and,
// for Anthropic models, the Amazon Bedrock model id that serves the same
// model.
type modelEntry struct {
	Canonical     string
	Vendor        string // anthropic | openai | gemini
	PromptUSD     string // price_table.<model>.prompt_per_token
	CompletionUSD string // price_table.<model>.completion_per_token
	CacheReadUSD  string // cache_read_per_token; "" = loader prices it at PromptUSD
	CacheWriteUSD string // cache_creation_per_token; "" = same fallback
	BedrockID     string // "" = not offered on Bedrock, or not in the embedded mapping
}

// models is the embedded table, keyed by canonical id.
var models = map[string]modelEntry{
	"claude-sonnet-5-5": {Canonical: "claude-sonnet-5-5", Vendor: "anthropic", PromptUSD: "0.000002", CompletionUSD: "0.00001", CacheReadUSD: "0.0000001", CacheWriteUSD: "0.0000025", BedrockID: "global.anthropic.claude-sonnet-5-5"},
	"claude-opus-5-5":   {Canonical: "claude-opus-5-5", Vendor: "anthropic", PromptUSD: "0.000004", CompletionUSD: "0.00002", CacheReadUSD: "0.0000002", CacheWriteUSD: "0.000005", BedrockID: "global.anthropic.claude-opus-5-5"},
	"claude-haiku-5-5":  {Canonical: "claude-haiku-5-5", Vendor: "anthropic", PromptUSD: "0.0000001", CompletionUSD: "0.0000005", CacheReadUSD: "0.00000001", CacheWriteUSD: "0.000000125", BedrockID: "global.anthropic.claude-haiku-5-5"},
	"claude-fable-5-1":  {Canonical: "claude-fable-5-1", Vendor: "anthropic", PromptUSD: "0.00001", CompletionUSD: "0.00005", CacheReadUSD: "0.00000025", CacheWriteUSD: "0.0000125", BedrockID: "global.anthropic.claude-fable-5-1"},
	"gpt-4o":            {Canonical: "gpt-4o", Vendor: "openai", PromptUSD: "0.0000025", CompletionUSD: "0.00001"},
	"gemini-2.5-flash":  {Canonical: "gemini-2.5-flash", Vendor: "gemini", PromptUSD: "0.0000003", CompletionUSD: "0.0000025"},
}

// defaultModel is the one deployment `kelvran init` writes per selected
// provider when --models is not given: the current general-purpose model
// each vendor's own SDK examples reach for, and for anthropic the id Claude
// Code offers by default. --models widens the set (RFC-3 decision 6).
var defaultModel = map[string]string{
	"anthropic": "claude-sonnet-5-5",
	"openai":    "gpt-4o",
	"gemini":    "gemini-2.5-flash",
	"bedrock":   "claude-sonnet-5-5",
}

// providerVendor maps a gateway provider to the vendor whose price applies:
// bedrock serves Anthropic models at Anthropic's rates (see priceTableAsOf).
var providerVendor = map[string]string{
	"anthropic": "anthropic",
	"openai":    "openai",
	"gemini":    "gemini",
	"bedrock":   "anthropic",
}

// lookupModel returns the embedded entry for canonical, if any.
func lookupModel(canonical string) (modelEntry, bool) {
	e, ok := models[canonical]
	return e, ok
}

// embeddedModelsFor lists the canonical ids the table prices for provider,
// sorted, for error messages ("the embedded table prices: …").
func embeddedModelsFor(provider string) []string {
	vendor := providerVendor[provider]
	var out []string
	for id, e := range models {
		if e.Vendor != vendor {
			continue
		}
		if provider == "bedrock" && e.BedrockID == "" {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// bedrockIDFor resolves the Bedrock model id for canonical: an explicit
// --upstream-model override first, then the embedded mapping.
func bedrockIDFor(canonical string, overrides map[string]string) (string, error) {
	if id, ok := overrides[canonical]; ok {
		return id, nil
	}
	if e, ok := models[canonical]; ok && e.BedrockID != "" {
		return e.BedrockID, nil
	}
	known := embeddedModelsFor("bedrock")
	return "", fmt.Errorf("no Bedrock model id is known for %q; pass --upstream-model %s=<bedrock-model-id> (the embedded mapping, as of %s, covers %s)",
		canonical, canonical, priceTableAsOf, strings.Join(known, ", "))
}
