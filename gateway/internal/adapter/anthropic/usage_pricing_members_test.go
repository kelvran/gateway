package anthropic

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// pricingMembersResponse is a real Messages response shape (anthropic-sdk-go
// v1.80.0's Usage: cache_creation breakdown, server_tool_use, service_tier,
// inference_geo) decoded through the wire struct, so the test proves the
// JSON tags, not just the Go field mapping.
const pricingMembersResponse = `{"id":"msg_01Members00000000000","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":50,"output_tokens":10,"cache_creation_input_tokens":500,"cache_read_input_tokens":400,"cache_creation":{"ephemeral_5m_input_tokens":200,"ephemeral_1h_input_tokens":300},"server_tool_use":{"web_search_requests":3,"web_fetch_requests":1,"code_execution_requests":0},"service_tier":"standard","inference_geo":"us"}}`

func TestFromProviderLiftsThePricingMembersOutOfUsage(t *testing.T) {
	var native Response
	if err := json.Unmarshal([]byte(pricingMembersResponse), &native); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := New().FromProvider(&native)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	want := adapter.Usage{
		PromptTokens:          50 + 400 + 500,
		CompletionTokens:      10,
		TotalTokens:           50 + 400 + 500 + 10,
		CacheReadTokens:       400,
		CacheCreationTokens:   500,
		CacheCreation1hTokens: 300,
		WebSearchRequests:     3,
		ServiceTier:           "standard",
		InferenceGeo:          "us",
	}
	if got.Usage != want {
		t.Errorf("Usage = %+v, want %+v", got.Usage, want)
	}
}

func TestFromProviderWithoutPricingMembersLeavesThemZero(t *testing.T) {
	got, err := New().FromProvider(&Response{
		ID: "msg_plain", Model: "claude-sonnet-5-5", Role: "assistant",
		Content: []ContentBlock{{Type: "text", Text: "ok"}}, StopReason: "end_turn",
		Usage: Usage{InputTokens: 5, OutputTokens: 1, CacheCreationInputTokens: 2},
	})
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if got.Usage.CacheCreation1hTokens != 0 || got.Usage.WebSearchRequests != 0 || got.Usage.ServiceTier != "" || got.Usage.InferenceGeo != "" {
		t.Errorf("pricing members without wire data = %+v, want all zero", got.Usage)
	}
}

// The streamed shape: message_start carries the cache_creation breakdown,
// service_tier and inference_geo; the final message_delta carries the
// cumulative server_tool_use.
func TestStreamDecoder_UsageCarriesThePricingMembers(t *testing.T) {
	f, err := os.Open("testdata/stream_usage_with_pricing_members.txt")
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	decoder := New().NewStreamDecoder()
	reader := streaming.NewReader(f)
	var usage *adapter.Usage
	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reader.Next(): %v", err)
		}
		_, _, gotUsage, decErr := decoder.Decode(ev)
		if decErr != nil {
			t.Fatalf("Decode(%+v) error = %v", ev, decErr)
		}
		if gotUsage != nil {
			usage = gotUsage
		}
	}
	if usage == nil {
		t.Fatal("usage = nil")
	}
	want := adapter.Usage{
		PromptTokens:          50 + 1800 + 248,
		CompletionTokens:      12,
		TotalTokens:           50 + 1800 + 248 + 12,
		CacheReadTokens:       1800,
		CacheCreationTokens:   248,
		CacheCreation1hTokens: 48,
		WebSearchRequests:     2,
		ServiceTier:           "standard",
		InferenceGeo:          "us",
	}
	if *usage != want {
		t.Errorf("usage = %+v, want %+v", *usage, want)
	}
}
