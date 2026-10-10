package bedrock

import (
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// A system message whose text lives in Parts (the canonical shape a
// mid-conversation role:"system" entry could take) must hoist every text part
// into Converse's system[] -- Content first, then each part, a cachePoint
// after a part that carries cache_control and one after the message when the
// message does -- and never an empty {} block, which Bedrock rejects with
// "The system field can't be null" (live, 2026-10-11).
func TestToProviderSystemMessagePartsHoistEveryTextPart(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			{Role: "system", Content: "Prelude."},
			{Role: "system", Parts: []adapter.ContentPart{
				{Type: "text", Text: "Reminder A", CacheControl: &adapter.CacheControl{}},
				{Type: "text", Text: "Reminder B"},
			}},
			{Role: "system", Content: "Lead", Parts: []adapter.ContentPart{{Type: "text", Text: "Trail"}}, CacheControl: &adapter.CacheControl{}},
			{Role: "user", Content: "hi"},
		},
		DisableCacheControlAutoPopulate: true,
	}
	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)
	var got []string
	for _, b := range native.System {
		switch {
		case b.CachePoint != nil && b.Text == "":
			got = append(got, "<cachePoint>")
		case b.Text != "" && b.CachePoint == nil:
			got = append(got, b.Text)
		default:
			t.Fatalf("system block %+v is neither a text block nor a standalone cachePoint", b)
		}
	}
	want := []string{"Prelude.", "Reminder A", "<cachePoint>", "Reminder B", "Lead", "Trail", "<cachePoint>"}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("system blocks = %v, want %v", got, want)
	}
}

func TestToProviderSystemMessageWithoutTextEmitsNoBlock(t *testing.T) {
	req := adapter.ChatRequest{
		Model:                           "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages:                        []adapter.Message{{Role: "system"}, {Role: "user", Content: "hi"}},
		DisableCacheControlAutoPopulate: true,
	}
	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	if native := nativeAny.(*Request); len(native.System) != 0 {
		t.Fatalf("system = %+v, want no block at all: an empty block is exactly what Bedrock rejects", native.System)
	}
}

func TestToProviderSystemMessageNonTextPartIsAnError(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			{Role: "system", Parts: []adapter.ContentPart{{Type: "image", MediaType: "image/png", Data: "AAAA"}}},
			{Role: "user", Content: "hi"},
		},
	}
	if _, err := New().ToProvider(req); err == nil || !strings.Contains(err.Error(), "system message") {
		t.Fatalf("err = %v, want an error naming the system message's non-text part", err)
	}
}
