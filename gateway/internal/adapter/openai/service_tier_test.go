package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// OpenAI's service_tier (round-5 Tier A item 4): the canonical request member
// is forwarded verbatim (never validated here -- the enum is the upstream's own
// 400), and the tier the response says was actually served is lifted into
// adapter.Usage.ServiceTier for pricing.

func TestToProviderServiceTierForwardedAndOmitted(t *testing.T) {
	req := adapter.ChatRequest{Model: "gpt-6.1-sol", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, ServiceTier: "flex"}
	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	wire, err := json.Marshal(nativeAny)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(wire), `"service_tier":"flex"`) {
		t.Errorf("wire = %s, want service_tier flex", wire)
	}
	req.ServiceTier = ""
	nativeAny, err = New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	wire, _ = json.Marshal(nativeAny)
	if strings.Contains(string(wire), "service_tier") {
		t.Errorf("wire carries service_tier for a request without one: %s", wire)
	}
}

// The response's top-level service_tier is the processing mode actually
// used, which may differ from the request (auto -> default, a ramp-downgraded
// fast -> default). Decoded from JSON so the tag is proven.
func TestFromProviderLiftsTheServedServiceTier(t *testing.T) {
	for tier, body := range map[string]string{
		"priority": `{"id":"c1","model":"gpt-5.6-sol","service_tier":"priority","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`,
		"default":  `{"id":"c2","model":"gpt-6.1-sol","service_tier":"default","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`,
		"":         `{"id":"c3","model":"gpt-4o","service_tier":null,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`,
	} {
		var native Response
		if err := json.Unmarshal([]byte(body), &native); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got, err := New().FromProvider(&native)
		if err != nil {
			t.Fatalf("FromProvider: %v", err)
		}
		if got.Usage.ServiceTier != tier {
			t.Errorf("served %q: Usage.ServiceTier = %q", tier, got.Usage.ServiceTier)
		}
	}
	var absent Response
	if err := json.Unmarshal([]byte(`{"id":"c4","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`), &absent); err != nil {
		t.Fatal(err)
	}
	if got, _ := New().FromProvider(&absent); got.Usage.ServiceTier != "" {
		t.Errorf("absent service_tier: Usage.ServiceTier = %q, want empty", got.Usage.ServiceTier)
	}
}

// Which chunk carries service_tier on a stream is undocumented (openai-go's
// accumulator latches it from any chunk), so the decoder remembers the last
// non-empty value it saw and puts it on the final usage: here the first
// content chunk carries it and the usage chunk does not.
func TestDecodeStreamLatchesServiceTierFromAnyChunk(t *testing.T) {
	result := decodeFixture(t, "stream_service_tier.txt")
	if result.finalUsage == nil {
		t.Fatal("finalUsage = nil")
	}
	want := adapter.Usage{PromptTokens: 19, CompletionTokens: 2, TotalTokens: 21, ServiceTier: "fast"}
	if *result.finalUsage != want {
		t.Errorf("finalUsage = %+v, want %+v", *result.finalUsage, want)
	}
}

// ... and when only the usage-bearing chunk carries it, that value wins.
func TestDecodeStreamServiceTierOnTheUsageChunk(t *testing.T) {
	dec := New().NewStreamDecoder()
	for _, data := range []string{
		`{"id":"c","object":"chat.completion.chunk","model":"gpt-6.1-sol","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
		`{"id":"c","object":"chat.completion.chunk","model":"gpt-6.1-sol","service_tier":"flex","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`,
	} {
		_, _, usage, err := dec.Decode(streaming.SSEEvent{Data: data})
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if usage != nil && usage.ServiceTier != "flex" {
			t.Errorf("finalUsage.ServiceTier = %q, want flex", usage.ServiceTier)
		}
	}
}
