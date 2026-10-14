package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// tierPriceTable prices gpt-6.1-sol at its Standard rates with the published
// per-tier multipliers: flex 0.5x, priority (fast) 2x, ultrafast 6x.
func tierPriceTable() costaccounting.PriceTable {
	d := func(s string) decimal.Decimal { return decimal.RequireFromString(s) }
	return costaccounting.PriceTable{
		"gpt-6.1-sol": {
			PromptPerToken: d("0.000002"), CompletionPerToken: d("0.00001"),
			ServiceTierMultipliers: map[string]decimal.Decimal{"flex": d("0.5"), "priority": d("2"), "ultrafast": d("6")},
		},
		// A Claude row an operator mistakenly gave a multiplier: it must never
		// apply (Anthropic's tier is a commitment, not a price).
		"claude-haiku-4-5": {
			PromptPerToken: d("0.000002"), CompletionPerToken: d("0.00001"),
			ServiceTierMultipliers: map[string]decimal.Decimal{"priority": d("2")},
		},
	}
}

// tierPipeline is memberPipeline with the tier price table, a buffered AND a
// streaming upstream, and an optional JSON log sink.
func tierPipeline(t *testing.T, deployments []Deployment, adapters adapter.Registry, upstream UpstreamCaller, upstreamStream UpstreamStreamCaller, logBuf *bytes.Buffer) (*Pipeline, *budget.Tracker) {
	t.Helper()
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	tracker := budget.NewTracker()
	if upstream == nil {
		upstream = func(context.Context, Deployment, any) (any, error) {
			t.Fatal("the buffered Upstream must not be called on a streaming test")
			return nil, nil
		}
	}
	logger := discardLogger()
	if logBuf != nil {
		logger = slog.New(slog.NewJSONHandler(logBuf, nil))
	}
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
		CostCalculator: costaccounting.NewCalculator(tierPriceTable()),
		Upstream:       upstream,
		UpstreamStream: upstreamStream,
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p, tracker
}

// openaiServed returns an OpenAI response (decoded from JSON, so the tag is
// exercised) of 1000 prompt / 100 completion tokens served at tier.
func openaiServed(t *testing.T, tier string) *openai.Response {
	t.Helper()
	body := `{"id":"chatcmpl-tier","model":"gpt-6.1-sol","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100,"total_tokens":1100}`
	if tier != "" {
		body += `,"service_tier":"` + tier + `"`
	}
	var r openai.Response
	if err := json.Unmarshal([]byte(body+"}"), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &r
}

func openaiDeployments(policy string) []Deployment {
	return []Deployment{{Name: "d1", Model: "gpt-6.1-sol", Provider: "openai", UpstreamModel: "gpt-6.1-sol", BaseURL: "http://unused", ServiceTierPolicy: policy}}
}

// Standard: 1000 x 0.000002 + 100 x 0.00001 = 0.003. The served tier decides:
// fast 2x, a ramp-downgraded default 1x even though fast was asked, flex 0.5x.
func TestHandleChatCompletionPricesTheServedOpenAIServiceTier(t *testing.T) {
	for name, tc := range map[string]struct{ requested, served, want string }{
		"served fast":                       {"fast", "fast", "0.006"},
		"requested fast, served default":    {"fast", "default", "0.003"},
		"requested auto, served flex":       {"auto", "flex", "0.0015"},
		"nothing requested, served default": {"", "default", "0.003"},
	} {
		t.Run(name, func(t *testing.T) {
			var captured *openai.Request
			p, tracker := tierPipeline(t, openaiDeployments(""), adapter.Registry{"openai": openai.New()}, func(_ context.Context, _ Deployment, req any) (any, error) {
				captured = req.(*openai.Request)
				return openaiServed(t, tc.served), nil
			}, nil, nil)
			req := adapter.ChatRequest{Model: "gpt-6.1-sol", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, ServiceTier: tc.requested}
			resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, "")
			if err != nil {
				t.Fatalf("HandleChatCompletion: %v", err)
			}
			if captured == nil || captured.ServiceTier != tc.requested {
				t.Errorf("forwarded service_tier = %q, want %q", captured.ServiceTier, tc.requested)
			}
			if resp.Usage.ServiceTier != tc.served {
				t.Errorf("Usage.ServiceTier = %q, want the served %q", resp.Usage.ServiceTier, tc.served)
			}
			if got, want := tracker.SpentUSD(context.Background(), "test-key", 0), decimal.RequireFromString(tc.want); !got.Equal(want) {
				t.Errorf("SpentUSD = %v, want %v", got, want)
			}
		})
	}
}

// A silent response (no service_tier member: an older model, a backend that
// never echoes it) bills a requested PREMIUM tier at its multiplier -- never
// undercount a lever the client pulled -- and never applies a requested
// discount the provider did not confirm.
func TestHandleChatCompletionBillsTheRequestedPremiumTierWhenTheResponseIsSilent(t *testing.T) {
	for name, tc := range map[string]struct{ requested, want string }{
		"requested fast":      {"fast", "0.006"},
		"requested ultrafast": {"ultrafast", "0.018"},
		"requested flex":      {"flex", "0.003"},
	} {
		t.Run(name, func(t *testing.T) {
			p, tracker := tierPipeline(t, openaiDeployments(""), adapter.Registry{"openai": openai.New()}, func(context.Context, Deployment, any) (any, error) {
				return openaiServed(t, ""), nil
			}, nil, nil)
			req := adapter.ChatRequest{Model: "gpt-6.1-sol", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, ServiceTier: tc.requested}
			if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
				t.Fatalf("HandleChatCompletion: %v", err)
			}
			if got, want := tracker.SpentUSD(context.Background(), "test-key", 0), decimal.RequireFromString(tc.want); !got.Equal(want) {
				t.Errorf("SpentUSD = %v, want %v", got, want)
			}
		})
	}
}

// A served tier the price table does not carry bills at Standard and is
// announced once, so an operator can add the multiplier.
func TestFinalizeWarnsWhenTheServedServiceTierIsUnpriced(t *testing.T) {
	var logBuf bytes.Buffer
	p, tracker := tierPipeline(t, openaiDeployments(""), adapter.Registry{"openai": openai.New()}, func(context.Context, Deployment, any) (any, error) {
		return openaiServed(t, "scale"), nil
	}, nil, &logBuf)
	req := adapter.ChatRequest{Model: "gpt-6.1-sol", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, ServiceTier: "scale"}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if got := tracker.SpentUSD(context.Background(), "test-key", 0); !got.Equal(decimal.RequireFromString("0.003")) {
		t.Errorf("SpentUSD = %v, want the Standard 0.003", got)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, `"msg":"service_tier_unpriced"`) || !strings.Contains(logs, `"service_tier":"scale"`) || !strings.Contains(logs, `"virtual_key_id":"`) {
		t.Errorf("want one service_tier_unpriced line naming the tier and the key; logs:\n%s", logs)
	}
	if strings.Count(logs, `"msg":"service_tier_unpriced"`) != 1 {
		t.Errorf("service_tier_unpriced logged %d times, want once", strings.Count(logs, `"msg":"service_tier_unpriced"`))
	}
}

// The streamed path: the tier latched from the chunks reaches finalize, so a
// flex stream bills 0.5x.
func TestHandleChatCompletionStreamPricesTheServedOpenAIServiceTier(t *testing.T) {
	wire := `data: {"id":"c","object":"chat.completion.chunk","model":"gpt-6.1-sol","service_tier":"flex","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c","object":"chat.completion.chunk","model":"gpt-6.1-sol","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"c","object":"chat.completion.chunk","model":"gpt-6.1-sol","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":100,"total_tokens":1100}}` + "\n\n" +
		"data: [DONE]\n\n"
	p, tracker := tierPipeline(t, openaiDeployments(""), adapter.Registry{"openai": openai.New()}, nil, func(context.Context, Deployment, any) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(wire)), nil
	}, nil)
	req := adapter.ChatRequest{Model: "gpt-6.1-sol", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}}, ServiceTier: "flex"}
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, httptest.NewRecorder(), ""); err != nil {
		t.Fatalf("HandleChatCompletionStream: %v", err)
	}
	if got, want := tracker.SpentUSD(context.Background(), "test-key", 0), decimal.RequireFromString("0.0015"); !got.Equal(want) {
		t.Errorf("SpentUSD = %v, want %v (flex 0.5x)", got, want)
	}
}

// service_tier_policy strip on a deployment: the member is not forwarded and
// the drop is logged on the request_field_dropped line; forward (the default)
// forwards it with no line.
func TestServiceTierPolicyStripDropsAndLogs(t *testing.T) {
	run := func(t *testing.T, policy string) (*openai.Request, string) {
		t.Helper()
		var captured *openai.Request
		var logBuf bytes.Buffer
		p, _ := tierPipeline(t, openaiDeployments(policy), adapter.Registry{"openai": openai.New()}, func(_ context.Context, _ Deployment, req any) (any, error) {
			captured = req.(*openai.Request)
			return openaiServed(t, "default"), nil
		}, nil, &logBuf)
		req := adapter.ChatRequest{Model: "gpt-6.1-sol", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, ServiceTier: "ultrafast"}
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
			t.Fatalf("HandleChatCompletion(%s): %v", policy, err)
		}
		return captured, logBuf.String()
	}
	captured, logs := run(t, "strip")
	if captured == nil || captured.ServiceTier != "" {
		t.Errorf("strip: forwarded service_tier = %q, want empty", captured.ServiceTier)
	}
	if !strings.Contains(logs, `"msg":"request_field_dropped"`) || !strings.Contains(logs, `"field":"service_tier"`) || !strings.Contains(logs, `"value":"ultrafast"`) || !strings.Contains(logs, "service_tier_policy") {
		t.Errorf("strip: want a request_field_dropped line for service_tier naming the policy; logs:\n%s", logs)
	}
	captured, logs = run(t, "forward")
	if captured == nil || captured.ServiceTier != "ultrafast" {
		t.Errorf("forward: forwarded service_tier = %q, want ultrafast", captured.ServiceTier)
	}
	if strings.Contains(logs, `"field":"service_tier"`) {
		t.Errorf("forward: a drop was logged:\n%s", logs)
	}
}

// Providers with no OpenAI service_tier (anthropic's own takes a different
// vocabulary on /v1/messages; Converse has none) drop it and say so.
func TestCallDeploymentLogsRequestFieldDroppedForServiceTierOffOpenAI(t *testing.T) {
	run := func(t *testing.T, provider, upstreamModel string, upstream UpstreamCaller) string {
		t.Helper()
		var logBuf bytes.Buffer
		p := newTestPipeline(t, upstream, []Deployment{{Name: "d-1", Model: "m", Provider: provider, UpstreamModel: upstreamModel, BaseURL: "http://unused"}})
		p.logger = slog.New(slog.NewJSONHandler(&logBuf, nil))
		p.adapters["anthropic"] = anthropic.New()
		p.adapters["bedrock"] = bedrock.New()
		req := adapter.ChatRequest{Model: "m", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, ServiceTier: "fast"}
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
			t.Fatalf("HandleChatCompletion(%s): %v", provider, err)
		}
		return logBuf.String()
	}
	anthropicUp := func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeAnthropicResponse(dep.UpstreamModel), nil
	}
	bedrockUp := func(ctx context.Context, dep Deployment, req any) (any, error) { return fakeBedrockResponse("ok"), nil }
	for provider, model := range map[string]string{"anthropic": "claude-haiku-4-5", "bedrock": "global.anthropic.claude-haiku-4-5-20251001-v1:0"} {
		up := anthropicUp
		if provider == "bedrock" {
			up = bedrockUp
		}
		logs := run(t, provider, model, up)
		if !strings.Contains(logs, `"msg":"request_field_dropped"`) || !strings.Contains(logs, `"field":"service_tier"`) || !strings.Contains(logs, `"value":"fast"`) || !strings.Contains(logs, provider) {
			t.Errorf("%s: want a request_field_dropped line for service_tier naming the provider; logs:\n%s", provider, logs)
		}
	}
}

// The served tier reaches the chat span as kelvran.usage.service_tier, which
// until now carried only Anthropic's telemetry-only value.
func TestServedOpenAIServiceTierReachesTheSpan(t *testing.T) {
	before := len(spanRecorder.Ended())
	p, _ := tierPipeline(t, openaiDeployments(""), adapter.Registry{"openai": openai.New()}, func(context.Context, Deployment, any) (any, error) {
		return openaiServed(t, "fast"), nil
	}, nil, nil)
	req := adapter.ChatRequest{Model: "gpt-6.1-sol", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, ServiceTier: "fast"}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	spans := spansSince(before)
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}
	if v, ok := spanAttr(t, spans[0].Attributes(), telemetry.AttrKelvranUsageServiceTier); !ok || v.AsString() != "fast" {
		t.Errorf("%s = %v, ok=%v, want fast", telemetry.AttrKelvranUsageServiceTier, v, ok)
	}
}

// Under strip the provider was never asked for the tier, so a silent response
// bills Standard even though the client requested fast: the fallback prices
// only a lever that actually reached the provider.
func TestStrippedServiceTierIsNeverBilledFromTheRequest(t *testing.T) {
	p, tracker := tierPipeline(t, openaiDeployments("strip"), adapter.Registry{"openai": openai.New()}, func(context.Context, Deployment, any) (any, error) {
		return openaiServed(t, ""), nil
	}, nil, nil)
	req := adapter.ChatRequest{Model: "gpt-6.1-sol", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, ServiceTier: "fast"}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if got, want := tracker.SpentUSD(context.Background(), "test-key", 0), decimal.RequireFromString("0.003"); !got.Equal(want) {
		t.Errorf("SpentUSD = %v, want the Standard %v (the stripped tier never reached the provider)", got, want)
	}
}

// Anthropic's usage.service_tier is a capacity commitment, never a price: a
// Claude response served "priority" bills Standard even when an operator put
// a multiplier on the Claude row, and no service_tier_unpriced line is
// written for it. The span still carries the raw value (resp.Usage).
func TestAnthropicServiceTierIsNeverPricedOrWarned(t *testing.T) {
	var logBuf bytes.Buffer
	deployments := []Deployment{{Name: "a1", Model: "claude-haiku-4-5", Provider: "anthropic", UpstreamModel: "claude-haiku-4-5", BaseURL: "http://unused"}}
	p, tracker := tierPipeline(t, deployments, adapter.Registry{"anthropic": anthropic.New()}, func(_ context.Context, dep Deployment, _ any) (any, error) {
		r := fakeAnthropicResponse(dep.UpstreamModel)
		r.Usage = anthropic.Usage{InputTokens: 1000, OutputTokens: 100, ServiceTier: "priority"}
		return r, nil
	}, nil, &logBuf)
	req := adapter.ChatRequest{Model: "claude-haiku-4-5", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, "")
	if err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if resp.Usage.ServiceTier != "priority" {
		t.Errorf("Usage.ServiceTier = %q, want the raw priority for telemetry", resp.Usage.ServiceTier)
	}
	if got, want := tracker.SpentUSD(context.Background(), "test-key", 0), decimal.RequireFromString("0.003"); !got.Equal(want) {
		t.Errorf("SpentUSD = %v, want the Standard %v (Anthropic's tier is never priced)", got, want)
	}
	if strings.Contains(logBuf.String(), "service_tier_unpriced") {
		t.Errorf("service_tier_unpriced logged for an Anthropic response:\n%s", logBuf.String())
	}
}

// The common echo -- "default" on an openai model with no price row -- is not
// an unpriced tier: the model is unpriced (kelvran doctor's business), and a
// warning per response would be noise. Same for a premium tier on that model:
// there is no row to add the multiplier to.
func TestUnpricedOpenAIModelNeverWarnsAboutItsTier(t *testing.T) {
	for _, served := range []string{"default", "fast"} {
		var logBuf bytes.Buffer
		deployments := []Deployment{{Name: "d1", Model: "gpt-unpriced", Provider: "openai", UpstreamModel: "gpt-unpriced", BaseURL: "http://unused"}}
		p, _ := tierPipeline(t, deployments, adapter.Registry{"openai": openai.New()}, func(context.Context, Deployment, any) (any, error) {
			return openaiServed(t, served), nil
		}, nil, &logBuf)
		req := adapter.ChatRequest{Model: "gpt-unpriced", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
			t.Fatalf("HandleChatCompletion(%s): %v", served, err)
		}
		if strings.Contains(logBuf.String(), "service_tier_unpriced") {
			t.Errorf("served %s: service_tier_unpriced logged for an unpriced model:\n%s", served, logBuf.String())
		}
	}
}
