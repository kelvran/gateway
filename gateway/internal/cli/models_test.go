package cli

import (
	"regexp"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// The embedded table is data `kelvran init` writes into a user's config, so
// its invariants are pinned: every default model is priced, every price is
// a positive decimal the loader will accept verbatim, every Anthropic model
// has a Bedrock id, and the as-of date is a real date.

func TestEveryDefaultModelIsPricedForItsProvider(t *testing.T) {
	for provider, id := range defaultModel {
		e, ok := lookupModel(id)
		if !ok {
			t.Errorf("default model %q for %s is not in the embedded table", id, provider)
			continue
		}
		if e.Vendor != providerVendor[provider] {
			t.Errorf("default model %q for %s is priced for vendor %q, want %q", id, provider, e.Vendor, providerVendor[provider])
		}
		if provider == "bedrock" && e.BedrockID == "" {
			t.Errorf("bedrock's default model %q has no Bedrock id", id)
		}
	}
	for _, provider := range []string{"anthropic", "openai", "gemini", "bedrock"} {
		if _, ok := defaultModel[provider]; !ok {
			t.Errorf("provider %s has no default model", provider)
		}
	}
	if _, ok := defaultModel["openaicompat"]; ok {
		t.Error("openaicompat is a protocol, not a vendor: it must have no default model (RFC-3 decision 6)")
	}
}

func TestEveryPriceIsAPositiveDecimalTheLoaderAccepts(t *testing.T) {
	for id, e := range models {
		if e.Canonical != id {
			t.Errorf("entry %q has Canonical %q", id, e.Canonical)
		}
		for name, raw := range map[string]string{"prompt_per_token": e.PromptUSD, "completion_per_token": e.CompletionUSD} {
			d, err := decimal.NewFromString(raw)
			if err != nil || !d.IsPositive() {
				t.Errorf("%s %s = %q, want a positive decimal (err %v)", id, name, raw, err)
			}
		}
		for name, raw := range map[string]string{"cache_read_per_token": e.CacheReadUSD, "cache_creation_per_token": e.CacheWriteUSD} {
			if raw == "" {
				continue
			}
			d, err := decimal.NewFromString(raw)
			if err != nil || !d.IsPositive() {
				t.Errorf("%s %s = %q, want a positive decimal (err %v)", id, name, raw, err)
			}
		}
		if (e.CacheReadUSD == "") != (e.CacheWriteUSD == "") {
			t.Errorf("%s prices only one of the two cache rates", id)
		}
		if e.Vendor == "anthropic" && !strings.HasPrefix(e.BedrockID, "global.anthropic.") {
			t.Errorf("anthropic model %s Bedrock id = %q, want the global inference-profile form (the base id is rejected by Converse; live-checked 2026-10-10)", id, e.BedrockID)
		}
		if e.Vendor != "anthropic" && e.BedrockID != "" {
			t.Errorf("non-anthropic model %s carries a Bedrock id %q", id, e.BedrockID)
		}
	}
}

func TestAnthropicCacheReadIsCheaperThanPromptForEveryModel(t *testing.T) {
	// Anthropic's pricing page: a cache hit costs 2.5 % to 10 % of the input
	// price depending on the model; a table that inverted that would make
	// prompt caching look like a cost, not a saving.
	for id, e := range models {
		if e.CacheReadUSD == "" {
			continue
		}
		read := decimal.RequireFromString(e.CacheReadUSD)
		prompt := decimal.RequireFromString(e.PromptUSD)
		if !read.LessThan(prompt) {
			t.Errorf("%s cache_read_per_token %s is not below prompt_per_token %s", id, read, prompt)
		}
	}
}

func TestPriceTableAsOfIsADate(t *testing.T) {
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`).MatchString(priceTableAsOf) {
		t.Errorf("priceTableAsOf = %q, want YYYY-MM-DD", priceTableAsOf)
	}
}

func TestBedrockIDForPrefersAnExplicitOverrideAndNamesTheFlagOtherwise(t *testing.T) {
	if id, err := bedrockIDFor("claude-sonnet-5-5", nil); err != nil || id != "global.anthropic.claude-sonnet-5-5" {
		t.Errorf("bedrockIDFor(claude-sonnet-5-5) = (%q, %v)", id, err)
	}
	if id, err := bedrockIDFor("claude-sonnet-5-5", map[string]string{"claude-sonnet-5-5": "us.anthropic.claude-sonnet-5-5"}); err != nil || id != "us.anthropic.claude-sonnet-5-5" {
		t.Errorf("override not honoured: (%q, %v)", id, err)
	}
	_, err := bedrockIDFor("gpt-4o", nil)
	if err == nil {
		t.Fatal("gpt-4o has no Bedrock id; want an error")
	}
	for _, want := range []string{"--upstream-model gpt-4o=", priceTableAsOf, "claude-sonnet-5-5"} {
		if !containsString(err.Error(), want) {
			t.Errorf("error %q must mention %q", err.Error(), want)
		}
	}
	if got := embeddedModelsFor("openai"); len(got) != 1 || got[0] != "gpt-4o" {
		t.Errorf("embeddedModelsFor(openai) = %v", got)
	}
}

func containsString(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})())
}
