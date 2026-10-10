package gemini

import (
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// bedrock's TestToProviderSystemMessagePartsHoistEveryTextPart, in this
// adapter's shape: every text part joins the single systemInstruction text
// (Gemini has no per-block cache markers), in order, and a system message
// without text contributes nothing.
func TestToProviderSystemMessagePartsJoinTheSystemInstruction(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "system", Content: "Prelude."},
			{Role: "system", Parts: []adapter.ContentPart{
				{Type: "text", Text: "Reminder A", CacheControl: &adapter.CacheControl{}},
				{Type: "text", Text: "Reminder B"},
			}},
			{Role: "system", Content: "Lead", Parts: []adapter.ContentPart{{Type: "text", Text: "Trail"}}},
			{Role: "system"},
			{Role: "user", Content: "hi"},
		},
	}
	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)
	if native.SystemInstruction == nil || len(native.SystemInstruction.Parts) != 1 {
		t.Fatalf("systemInstruction = %+v, want one text part", native.SystemInstruction)
	}
	want := strings.Join([]string{"Prelude.", "Reminder A", "Reminder B", "Lead", "Trail"}, "\n\n")
	if got := native.SystemInstruction.Parts[0].Text; got != want {
		t.Fatalf("systemInstruction text = %q, want %q", got, want)
	}
}

func TestToProviderSystemMessageNonTextPartIsAnError(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "system", Parts: []adapter.ContentPart{{Type: "image", MediaType: "image/png", Data: "AAAA"}}},
			{Role: "user", Content: "hi"},
		},
	}
	if _, err := New().ToProvider(req); err == nil || !strings.Contains(err.Error(), "system message") {
		t.Fatalf("err = %v, want an error naming the system message's non-text part", err)
	}
}
