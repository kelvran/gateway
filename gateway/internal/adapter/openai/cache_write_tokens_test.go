package openai

import (
	"encoding/json"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// OpenAI's CompletionUsage carries prompt_tokens_details.cache_write_tokens
// (openai-openapi spec, info.version 2.3.0, line 43551; the prompt-caching
// guide: "For GPT-5.6 and later, cache writes cost 1.25x the standard,
// uncached input-token rate"). It is the subset of prompt_tokens written to
// the cache, so it maps onto adapter.Usage.CacheCreationTokens exactly as
// Anthropic's cache_creation_input_tokens does.
func TestFromProviderLiftsCacheWriteTokensIntoCacheCreation(t *testing.T) {
	raw := []byte(`{
		"id": "chatcmpl-cw", "model": "gpt-6.1-sol",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 1024, "completion_tokens": 10, "total_tokens": 1034,
			"prompt_tokens_details": {"cached_tokens": 640, "cache_write_tokens": 256}}
	}`)
	var nativeResp Response
	if err := json.Unmarshal(raw, &nativeResp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	got, err := New().FromProvider(&nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	want := adapter.Usage{PromptTokens: 1024, CompletionTokens: 10, TotalTokens: 1034, CacheReadTokens: 640, CacheCreationTokens: 256}
	if got.Usage != want {
		t.Errorf("Usage = %+v, want %+v", got.Usage, want)
	}
}

// The final include_usage stream chunk carries the same object.
func TestDecodeFinalUsageChunkLiftsCacheWriteTokens(t *testing.T) {
	dec := New().NewStreamDecoder()
	_, _, finalUsage, err := dec.Decode(streaming.SSEEvent{
		Data: `{"id":"x","model":"gpt-6.1-sol","choices":[],"usage":{"prompt_tokens":1024,"completion_tokens":10,"total_tokens":1034,"prompt_tokens_details":{"cached_tokens":640,"cache_write_tokens":256}}}`,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if finalUsage == nil {
		t.Fatal("finalUsage = nil")
	}
	want := adapter.Usage{PromptTokens: 1024, CompletionTokens: 10, TotalTokens: 1034, CacheReadTokens: 640, CacheCreationTokens: 256}
	if *finalUsage != want {
		t.Errorf("finalUsage = %+v, want %+v", *finalUsage, want)
	}
}

// A details object without the member (GPT-5.5 and earlier, which carry no
// cache-write charge) keeps CacheCreationTokens at 0; the cache-inclusive
// invariant CacheRead + CacheCreation <= PromptTokens is never violated by
// the lift.
func TestFromProviderWithoutCacheWriteTokensKeepsCacheCreationZero(t *testing.T) {
	native := &Response{ID: "c", Model: "gpt-4o", Choices: []Choice{{Message: Message{Role: "assistant", Content: json.RawMessage(`"hi"`)}, FinishReason: "stop"}},
		Usage: Usage{PromptTokens: 1024, CompletionTokens: 10, TotalTokens: 1034, PromptTokensDetails: &PromptTokensDetails{CachedTokens: 896}}}
	got, err := New().FromProvider(native)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if got.Usage.CacheCreationTokens != 0 || got.Usage.CacheReadTokens != 896 {
		t.Errorf("Usage = %+v, want CacheReadTokens 896 and CacheCreationTokens 0", got.Usage)
	}
}
