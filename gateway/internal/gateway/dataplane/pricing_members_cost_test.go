package dataplane

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// memberPriceTable prices claude-sonnet-5-5 with every optional rate set:
// 1h cache writes at 2x base input, $0.01 per web search, 1.1x US-only.
func memberPriceTable() costaccounting.PriceTable {
	d := func(s string) *decimal.Decimal { v := decimal.RequireFromString(s); return &v }
	return costaccounting.PriceTable{
		"claude-sonnet-5-5": {
			PromptPerToken: decimal.RequireFromString("0.00001"), CompletionPerToken: decimal.RequireFromString("0.00005"),
			CacheReadPerToken: d("0.00000025"), CacheCreationPerToken: d("0.0000125"),
			CacheCreation1hPerToken: d("0.00002"), WebSearchPerRequest: d("0.01"), USInferenceMultiplier: d("1.1"),
		},
	}
}

func memberPipeline(t *testing.T, deployments []Deployment, adapters adapter.Registry, upstream func(context.Context, Deployment, any) (any, error)) (*Pipeline, *budget.Tracker) {
	t.Helper()
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	tracker := budget.NewTracker()
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         tracker,
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapters,
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(memberPriceTable()),
		Upstream:       upstream,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p, tracker
}

// An anthropic response reporting the members the pricing page bills
// beyond flat tokens: 300 of the 500 cache writes at the 1-hour TTL, 3 web
// searches, US-only inference. Spend must be
// ((100 fresh + 400 reads + 200 5m writes + 300 1h writes + 10 output) x 1.1)
// + 3 searches = 0.0101 x 1.1 + 0.03 = 0.04111.
func TestHandleChatCompletionPricesTheProviderReportedPricingMembers(t *testing.T) {
	deployments := []Deployment{{Name: "d1", Model: "claude-sonnet-5-5", Provider: "anthropic", UpstreamModel: "claude-sonnet-5-5", BaseURL: "http://unused"}}
	p, tracker := memberPipeline(t, deployments, adapter.Registry{"anthropic": anthropic.New()}, func(_ context.Context, dep Deployment, _ any) (any, error) {
		return &anthropic.Response{
			ID: "msg_members", Model: dep.UpstreamModel, Role: "assistant",
			Content: []anthropic.ContentBlock{{Type: "text", Text: "ok"}}, StopReason: "end_turn",
			Usage: anthropic.Usage{
				InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 400, CacheCreationInputTokens: 500,
				CacheCreation: &anthropic.CacheCreationBreakdown{Ephemeral5mInputTokens: 200, Ephemeral1hInputTokens: 300},
				ServerToolUse: &anthropic.ServerToolUsage{WebSearchRequests: 3},
				ServiceTier:   "standard", InferenceGeo: "us",
			},
		}, nil
	})
	req := adapter.ChatRequest{Model: "claude-sonnet-5-5", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	want := decimal.RequireFromString("0.04111")
	if got := tracker.SpentUSD(context.Background(), "test-key", 0); !got.Equal(want) {
		t.Errorf("SpentUSD = %v, want %v (1h writes, web searches and US inference priced)", got, want)
	}
}

// The inference is a workaround for Bedrock's documented blind spot, not a
// general override: an anthropic deployment whose response carries cache
// writes but no cache_creation breakdown (not current Anthropic, which
// always reports it) is billed what it reports, 1h breakpoint or not.
func TestOneHourInferenceIsConfinedToBedrock(t *testing.T) {
	deployments := []Deployment{{Name: "d1", Model: "claude-sonnet-5-5", Provider: "anthropic", UpstreamModel: "claude-sonnet-5-5", BaseURL: "http://unused"}}
	p, tracker := memberPipeline(t, deployments, adapter.Registry{"anthropic": anthropic.New()}, func(_ context.Context, dep Deployment, _ any) (any, error) {
		return &anthropic.Response{
			ID: "msg_nobreakdown", Model: dep.UpstreamModel, Role: "assistant",
			Content: []anthropic.ContentBlock{{Type: "text", Text: "ok"}}, StopReason: "end_turn",
			Usage: anthropic.Usage{InputTokens: 100, OutputTokens: 10, CacheCreationInputTokens: 500},
		}, nil
	})
	req := adapter.ChatRequest{Model: "claude-sonnet-5-5", Messages: []adapter.Message{{Role: "user", Content: "hi", CacheControl: &adapter.CacheControl{TTL: "1h"}}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	// 100 fresh + 500 writes at the 5m rate + 10 output.
	want := decimal.RequireFromString("0.0015").Add(decimal.NewFromInt(500).Mul(decimal.RequireFromString("0.0000125")))
	if got := tracker.SpentUSD(context.Background(), "test-key", 0); !got.Equal(want) {
		t.Errorf("SpentUSD = %v, want %v (no 1h inference outside bedrock)", got, want)
	}
}

// Converse reports cacheWriteInputTokens without a TTL split, but the
// gateway knows what it sent: a request carrying a 1-hour breakpoint on a
// model Bedrock accepts the ttl for is billed 2x on every write, so every
// write is priced at the 1-hour rate. A default-TTL request, and a model the
// adapter never forwards the ttl to, keep the 5-minute rate.
func TestBedrockOneHourBreakpointPricesEveryCacheWriteAtTheOneHourRate(t *testing.T) {
	const ttlModel = "global.anthropic.claude-sonnet-5-5-20260301-v1:0"
	const noTTLModel = "global.anthropic.claude-opus-4-1-20250805-v1:0"
	// 100 fresh + 500 writes + 10 output: 0.001 + writes + 0.0005.
	fiveMinute := decimal.RequireFromString("0.0015").Add(decimal.NewFromInt(500).Mul(decimal.RequireFromString("0.0000125")))
	oneHour := decimal.RequireFromString("0.0015").Add(decimal.NewFromInt(500).Mul(decimal.RequireFromString("0.00002")))
	for _, tc := range []struct {
		name          string
		upstreamModel string
		ttl           string
		want          decimal.Decimal
	}{
		{"1h breakpoint on a ttl model", ttlModel, "1h", oneHour},
		{"default ttl on a ttl model", ttlModel, "", fiveMinute},
		{"1h breakpoint on a model the adapter never forwards ttl to", noTTLModel, "1h", fiveMinute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deployments := []Deployment{{Name: "b1", Model: "claude-sonnet-5-5", Provider: "bedrock", UpstreamModel: tc.upstreamModel, BaseURL: "http://unused"}}
			p, tracker := memberPipeline(t, deployments, adapter.Registry{"bedrock": bedrock.New()}, func(context.Context, Deployment, any) (any, error) {
				return &bedrock.Response{
					Output:     bedrock.Output{Message: bedrock.Message{Role: "assistant", Content: []bedrock.ContentBlock{{Text: "ok"}}}},
					StopReason: "end_turn",
					Usage:      bedrock.Usage{InputTokens: 100, OutputTokens: 10, TotalTokens: 610, CacheWriteInputTokens: 500},
				}, nil
			})
			var cc *adapter.CacheControl
			if tc.ttl != "" {
				cc = &adapter.CacheControl{TTL: tc.ttl}
			}
			req := adapter.ChatRequest{Model: "claude-sonnet-5-5", Messages: []adapter.Message{{Role: "user", Content: "hi", CacheControl: cc}}}
			if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
				t.Fatalf("HandleChatCompletion: %v", err)
			}
			if got := tracker.SpentUSD(context.Background(), "test-key", 0); !got.Equal(tc.want) {
				t.Errorf("SpentUSD = %v, want %v", got, tc.want)
			}
		})
	}
}

// The two strings the members add to the span are upstream-controlled, so
// they pass through the same bound every foreign string does before it
// reaches a span attribute (item (m)): no control characters, valid UTF-8,
// at most 256 bytes.
func TestPricingMemberStringsAreBoundedBeforeTheSpan(t *testing.T) {
	before := len(spanRecorder.Ended())
	deployments := []Deployment{{Name: "d1", Model: "claude-sonnet-5-5", Provider: "anthropic", UpstreamModel: "claude-sonnet-5-5", BaseURL: "http://unused"}}
	p, _ := memberPipeline(t, deployments, adapter.Registry{"anthropic": anthropic.New()}, func(_ context.Context, dep Deployment, _ any) (any, error) {
		return &anthropic.Response{
			ID: "msg_geo", Model: dep.UpstreamModel, Role: "assistant",
			Content: []anthropic.ContentBlock{{Type: "text", Text: "ok"}}, StopReason: "end_turn",
			Usage: anthropic.Usage{InputTokens: 10, OutputTokens: 1, InferenceGeo: "us\x1b[31m\u0085X", ServiceTier: "standard" + strings.Repeat("é", 200)},
		}, nil
	})
	req := adapter.ChatRequest{Model: "claude-sonnet-5-5", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	got := map[string]string{}
	for _, span := range spansSince(before) {
		for _, a := range span.Attributes() {
			if k := string(a.Key); k == "kelvran.usage.inference_geo" || k == "kelvran.usage.service_tier" {
				got[k] = a.Value.AsString()
			}
		}
	}
	for k, v := range got {
		if v == "" {
			t.Errorf("%s missing from the span", k)
		}
		if strings.ContainsAny(v, "\x1b\u0085") || !utf8.ValidString(v) || len(v) > 256 {
			t.Errorf("%s = %q: control characters, invalid UTF-8 or over 256 bytes reached the span", k, v)
		}
	}
	if len(got) != 2 {
		t.Errorf("span carried %d of the two member attributes: %v", len(got), got)
	}
}
