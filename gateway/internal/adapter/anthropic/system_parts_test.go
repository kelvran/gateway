package anthropic

import (
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// bedrock's TestToProviderSystemMessagePartsHoistEveryTextPart, in this
// adapter's shape: one system block per text part, each part's cache_control
// on its own block, the message-level cache_control on the message's last
// block, and no empty block for a system message without text.
func TestToProviderSystemMessagePartsHoistEveryTextPart(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{Role: "system", Content: "Prelude."},
			{Role: "system", Parts: []adapter.ContentPart{
				{Type: "text", Text: "Reminder A", CacheControl: &adapter.CacheControl{}},
				{Type: "text", Text: "Reminder B"},
			}},
			{Role: "system", Content: "Lead", Parts: []adapter.ContentPart{{Type: "text", Text: "Trail"}}, CacheControl: &adapter.CacheControl{}},
			{Role: "system"},
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
		if b.Type != "text" || b.Text == "" {
			t.Fatalf("system block %+v, want a non-empty text block", b)
		}
		if b.CacheControl != nil {
			got = append(got, b.Text+"*")
		} else {
			got = append(got, b.Text)
		}
	}
	want := []string{"Prelude.", "Reminder A*", "Reminder B", "Lead", "Trail*"}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("system blocks = %v, want %v (* = cache_control)", got, want)
	}
}

func TestToProviderSystemMessageNonTextPartIsAnError(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{Role: "system", Parts: []adapter.ContentPart{{Type: "image", MediaType: "image/png", Data: "AAAA"}}},
			{Role: "user", Content: "hi"},
		},
	}
	if _, err := New().ToProvider(req); err == nil || !strings.Contains(err.Error(), "system message") {
		t.Fatalf("err = %v, want an error naming the system message's non-text part", err)
	}
}
