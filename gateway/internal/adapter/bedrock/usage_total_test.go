package bedrock

import "testing"

// Converse's totalTokens already includes the cache tokens. A live turn on
// 2026-10-11 reported inputTokens 2, outputTokens 4, cacheWriteInputTokens
// 35932, totalTokens 35938 -- and the adapter answered total_tokens 71870,
// counting the cache tokens twice, so the TPM limiter debited double the
// real size. The canonical total is PromptTokens + CompletionTokens (the
// data-plane contract), derived from the adapter's own cache-inclusive prompt
// count and never read from native totalTokens: a fixture whose native total
// EXCLUDES the cache tokens (TestFromProviderIncludesCacheTokensInPromptAndTotal)
// and this one, whose native total INCLUDES them, must both come out right.
func TestFromProviderTotalTokensIsPromptPlusCompletionNotNativeTotalPlusCache(t *testing.T) {
	nativeResp := &Response{
		Output:     Output{Message: Message{Role: "assistant", Content: []ContentBlock{{Text: "ok"}}}},
		StopReason: "end_turn",
		Usage:      Usage{InputTokens: 2, OutputTokens: 4, TotalTokens: 35938, CacheWriteInputTokens: 35932},
	}
	got, err := New().FromProvider(nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if got.Usage.PromptTokens != 35934 || got.Usage.CompletionTokens != 4 {
		t.Fatalf("Usage prompt/completion = %d/%d, want 35934/4", got.Usage.PromptTokens, got.Usage.CompletionTokens)
	}
	if got.Usage.TotalTokens != 35938 {
		t.Fatalf("Usage.TotalTokens = %d, want 35938 (prompt 35934 + completion 4); 71870 counts the cache tokens twice", got.Usage.TotalTokens)
	}
}

// The streaming twin, with a cache READ: the metadata event's totalTokens
// already includes cacheReadInputTokens too.
func TestDecodeMetadataTotalTokensIsPromptPlusCompletion(t *testing.T) {
	msg := newEventMessage("metadata", `{"usage":{"inputTokens":2,"outputTokens":4,"totalTokens":35938,"cacheReadInputTokens":35932,"cacheWriteInputTokens":0}}`)
	_, usage, err := NewStreamDecoder().Decode(msg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if usage == nil {
		t.Fatal("usage = nil, want non-nil")
	}
	if usage.PromptTokens != 35934 || usage.CompletionTokens != 4 || usage.CacheReadTokens != 35932 {
		t.Fatalf("usage = %+v, want prompt 35934, completion 4, cache read 35932", usage)
	}
	if usage.TotalTokens != 35938 {
		t.Fatalf("usage.TotalTokens = %d, want 35938 (prompt + completion); 71870 counts the cache tokens twice", usage.TotalTokens)
	}
}
