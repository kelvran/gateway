package anthropic

import (
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// bedrock's TestToProviderSystemMessageEmptyTextPartWithCacheControlAddsNothing
// in this adapter's shape: an empty text part contributes no block and its
// cache_control is not transplanted onto a neighbour.
func TestToProviderSystemMessageEmptyTextPartWithCacheControlAddsNothing(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{Role: "system", Parts: []adapter.ContentPart{
				{Type: "text", Text: "", CacheControl: &adapter.CacheControl{}},
				{Type: "text", Text: "Kept."},
			}},
			{Role: "user", Content: "hi"},
		},
		DisableCacheControlAutoPopulate: true,
	}
	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)
	if len(native.System) != 1 || native.System[0].Text != "Kept." || native.System[0].CacheControl != nil {
		t.Fatalf("system = %+v, want exactly one text block \"Kept.\" with no cache_control", native.System)
	}
}
