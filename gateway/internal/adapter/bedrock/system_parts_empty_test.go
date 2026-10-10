package bedrock

import (
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// A text part with no text contributes neither a block nor its cache marker:
// a cachePoint with nothing before it would be meaningless to Converse, and
// an empty text block is what Bedrock rejects. The message's other parts are
// unaffected.
func TestToProviderSystemMessageEmptyTextPartWithCacheControlAddsNothing(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
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
	if len(native.System) != 1 || native.System[0].Text != "Kept." || native.System[0].CachePoint != nil {
		t.Fatalf("system = %+v, want exactly one text block \"Kept.\" and no cachePoint", native.System)
	}
}
