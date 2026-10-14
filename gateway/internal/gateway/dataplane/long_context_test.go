package dataplane

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// longContextPriceTable is Claude Haiku 5.5's published row (2026-10-14):
// Standard rates plus the tier a prompt over 100,000 cache-inclusive tokens
// pays -- 5x on every category for the whole request. The tier is a property
// of the price row, so an anthropic and a bedrock deployment of the same
// model share it; nothing is gated by provider.
func longContextPriceTable() costaccounting.PriceTable {
	d := func(s string) decimal.Decimal { return decimal.RequireFromString(s) }
	read, write := d("0.00000001"), d("0.000000125")
	return costaccounting.PriceTable{"claude-haiku-5-5": {
		PromptPerToken: d("0.0000001"), CompletionPerToken: d("0.0000005"),
		CacheReadPerToken: &read, CacheCreationPerToken: &write,
		LongContext: &costaccounting.LongContextTier{AboveTokens: 100000, InputMultiplier: d("5"), OutputMultiplier: d("5")},
	}}
}

// The adapter's PromptTokens is input + cache reads + cache writes, which is
// exactly the count the pricing page compares ("a request's prompt length
// counts all of its input tokens, including cache reads and cache writes").
// Over: 50,000 fresh x 0.0000001 + 100,000 reads x 0.00000001 + 100 out x
// 0.0000005 = 0.00605, x5 = 0.03025. At exactly 100,000: Standard 0.00555.
// Under: 0.00455. The span carries kelvran.pricing.long_context_tier only
// when the tier engaged.
func TestHandleChatCompletionPricesTheLongContextTierAndMarksTheSpan(t *testing.T) {
	for name, tc := range map[string]struct {
		input, cacheRead int
		want             string
		engaged          bool
	}{
		"over the threshold":       {50000, 100000, "0.03025", true},
		"exactly at the threshold": {50000, 50000, "0.00555", false},
		"under the threshold":      {40000, 50000, "0.00455", false},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(spanRecorder.Ended())
			deployments := []Deployment{{Name: "a1", Model: "claude-haiku-5-5", Provider: "anthropic", UpstreamModel: "claude-haiku-5-5", BaseURL: "http://unused"}}
			p, tracker := pricedPipeline(t, longContextPriceTable(), deployments, adapter.Registry{"anthropic": anthropic.New()}, func(_ context.Context, dep Deployment, _ any) (any, error) {
				r := fakeAnthropicResponse(dep.UpstreamModel)
				r.Usage = anthropic.Usage{InputTokens: tc.input, CacheReadInputTokens: tc.cacheRead, OutputTokens: 100}
				return r, nil
			}, nil, nil)
			req := adapter.ChatRequest{Model: "claude-haiku-5-5", Messages: []adapter.Message{{Role: "user", Content: name}}}
			resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, "")
			if err != nil {
				t.Fatalf("HandleChatCompletion: %v", err)
			}
			if resp.Usage.PromptTokens != tc.input+tc.cacheRead {
				t.Fatalf("Usage.PromptTokens = %d, want the cache-inclusive %d", resp.Usage.PromptTokens, tc.input+tc.cacheRead)
			}
			if got, want := tracker.SpentUSD(context.Background(), "test-key", 0), decimal.RequireFromString(tc.want); !got.Equal(want) {
				t.Errorf("SpentUSD = %v, want %v", got, want)
			}
			spans := spansSince(before)
			if len(spans) != 1 {
				t.Fatalf("len(spans) = %d, want 1", len(spans))
			}
			v, ok := spanAttr(t, spans[0].Attributes(), telemetry.AttrKelvranPricingLongContextTier)
			if tc.engaged && (!ok || !v.AsBool()) {
				t.Errorf("%s = %v, ok=%v, want true", telemetry.AttrKelvranPricingLongContextTier, v, ok)
			}
			if !tc.engaged && ok {
				t.Errorf("%s = %v emitted on a Standard-priced request", telemetry.AttrKelvranPricingLongContextTier, v)
			}
		})
	}
}
