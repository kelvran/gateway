package dataplane

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// TestChatSpanNameIsSanitisedFromTheClientsModelString: the span is named
// from req.Model before any resolution, so an unknown model name reaches the
// tracer as typed; control characters must be gone and the name must be valid
// UTF-8 even when the 256-byte bound falls inside a multi-byte rune.
func TestChatSpanNameIsSanitisedFromTheClientsModelString(t *testing.T) {
	before := len(spanRecorder.Ended())
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	model := "gpt-4o\x1b[31m\u0085X Y" + strings.Repeat("a", 243) + "étail"
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{
		Model: model, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, "")
	if err == nil {
		t.Fatal("an unknown model must fail; the span is what is under test")
	}
	var name string
	for _, span := range spansSince(before) {
		if strings.HasPrefix(span.Name(), "chat ") {
			name = span.Name()
		}
	}
	if name == "" {
		t.Fatal("no chat span recorded")
	}
	if !utf8.ValidString(name) {
		t.Fatalf("span name is not valid UTF-8: %q", name)
	}
	for _, r := range name {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == ' ' || r == ' ' {
			t.Fatalf("span name carries control rune %U: %q", r, name)
		}
	}
	if !strings.HasPrefix(name, "chat gpt-4o[31mXY"+strings.Repeat("a", 243)) {
		t.Errorf("span name = %q, want the sanitised model with the controls dropped", name[:40])
	}
	if got := len(name) - len("chat "); got > maxModelForTelemetry {
		t.Errorf("model part is %d bytes, want <= %d", got, maxModelForTelemetry)
	}
	if strings.HasSuffix(name, "\xc3") || strings.Contains(name, "�") {
		t.Errorf("span name ends in a partial rune or a replacement char: %q", name[len(name)-6:])
	}
}
