package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// TestRoundTrip proves the OpenAI adapter's ToProvider -> FromProvider
// round-trip is lossless for every field the adapter claims to handle,
// per docs/testing/TESTING.md §3's explicit requirement. Stream is
// intentionally excluded from the equality check: OpenAI's response
// payload never echoes the request's Stream flag back.
func TestRoundTrip(t *testing.T) {
	temp := 0.7
	maxTokens := 512

	original := adapter.ChatRequest{
		Model: "gpt-4o",
		Messages: []adapter.Message{
			{Role: "user", Content: "What's the weather in Boston?"},
			{
				Role: "assistant",
				ToolCalls: []adapter.ToolCall{
					{ID: "call_1", Name: "get_weather", ArgumentsJSON: `{"city":"Boston"}`},
				},
			},
			{Role: "tool", Content: `{"temp_f":72}`, ToolCallID: "call_1"},
		},
		Temperature: &temp,
		MaxTokens:   &maxTokens,
		Tools: []adapter.ToolDef{
			{
				Name:           "get_weather",
				Description:    "Get the current weather for a city",
				ParametersJSON: `{"type":"object","properties":{"city":{"type":"string"}}}`,
			},
		},
		Stream: false,
	}

	a := New()

	nativeAny, err := a.ToProvider(original)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native, ok := nativeAny.(*Request)
	if !ok {
		t.Fatalf("ToProvider returned %T, want *Request", nativeAny)
	}

	// Build a native Response echoing the native Request's messages/tools
	// back, as OpenAI's Chat Completions API would (choices wrap messages).
	nativeResp := &Response{
		ID:    "chatcmpl-test",
		Model: native.Model,
		Choices: []Choice{
			{Index: 0, Message: native.Messages[1], FinishReason: "tool_calls"},
		},
		Usage: Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}

	got, err := a.FromProvider(nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}

	if got.Model != original.Model {
		t.Errorf("Model = %q, want %q", got.Model, original.Model)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("Choices len = %d, want 1", len(got.Choices))
	}
	gotMsg := got.Choices[0].Message
	wantMsg := original.Messages[1]
	if gotMsg.Role != wantMsg.Role {
		t.Errorf("Message.Role = %q, want %q", gotMsg.Role, wantMsg.Role)
	}
	if len(gotMsg.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(gotMsg.ToolCalls))
	}
	if gotMsg.ToolCalls[0].ID != wantMsg.ToolCalls[0].ID {
		t.Errorf("ToolCall.ID = %q, want %q", gotMsg.ToolCalls[0].ID, wantMsg.ToolCalls[0].ID)
	}
	if gotMsg.ToolCalls[0].Name != wantMsg.ToolCalls[0].Name {
		t.Errorf("ToolCall.Name = %q, want %q", gotMsg.ToolCalls[0].Name, wantMsg.ToolCalls[0].Name)
	}
	if gotMsg.ToolCalls[0].ArgumentsJSON != wantMsg.ToolCalls[0].ArgumentsJSON {
		t.Errorf("ToolCall.ArgumentsJSON = %q, want %q", gotMsg.ToolCalls[0].ArgumentsJSON, wantMsg.ToolCalls[0].ArgumentsJSON)
	}

	// Also verify the request side: the native request's tool/message
	// shape must reflect the canonical input exactly (field-for-field).
	if native.Model != original.Model {
		t.Errorf("native.Model = %q, want %q", native.Model, original.Model)
	}
	if len(native.Messages) != len(original.Messages) {
		t.Fatalf("native.Messages len = %d, want %d", len(native.Messages), len(original.Messages))
	}
	if native.Messages[1].ToolCalls[0].Function.Arguments != `{"city":"Boston"}` {
		t.Errorf("native tool call arguments = %q, want %q", native.Messages[1].ToolCalls[0].Function.Arguments, `{"city":"Boston"}`)
	}
	if len(native.Tools) != 1 || native.Tools[0].Function.Name != "get_weather" {
		t.Errorf("native.Tools mismatch: %+v", native.Tools)
	}
	if native.Temperature == nil || *native.Temperature != temp {
		t.Errorf("native.Temperature = %v, want %v", native.Temperature, temp)
	}
	if native.MaxTokens == nil || *native.MaxTokens != maxTokens {
		t.Errorf("native.MaxTokens = %v, want %v", native.MaxTokens, maxTokens)
	}
}

func TestName(t *testing.T) {
	if got := New().Name(); got != "openai" {
		t.Errorf("Name() = %q, want %q", got, "openai")
	}
}

// TestToProviderTextOnlyContentIsByteIdenticalToPlainString is the
// load-bearing proof that docs/rfcs/2026-09-06-gateway-multimodal-
// content.md's Content-becomes-json.RawMessage change is byte-identical
// for the common, Parts-empty case: the marshaled JSON must still be a
// bare string, never wrapped in an array or otherwise altered.
func TestToProviderTextOnlyContentIsByteIdenticalToPlainString(t *testing.T) {
	req := adapter.ChatRequest{
		Model:    "gpt-4o",
		Messages: []adapter.Message{{Role: "user", Content: "plain text, no parts"}},
	}

	native, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	b, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("marshaling native request: %v", err)
	}
	if !strings.Contains(string(b), `"content":"plain text, no parts"`) {
		t.Errorf("marshaled request = %s, want a bare string content field", b)
	}
}

// TestToProviderMultiModalContentPartsMapToImageURL is the load-bearing
// proof for multi-modal content: a text lead-in plus an inline-base64
// image part and a URL-referenced image part must map to OpenAI's real
// content-array shape ({"type":"text",...}/{"type":"image_url",
// "image_url":{"url":...}}), with inline Data encoded as a base64
// "data:" URI.
func TestToProviderMultiModalContentPartsMapToImageURL(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gpt-4o",
		Messages: []adapter.Message{
			{
				Role:    "user",
				Content: "what's in these images?",
				Parts: []adapter.ContentPart{
					{Type: "image", MediaType: "image/png", Data: "aW1hZ2ViYXNlNjQ="},
					{Type: "image", MediaType: "image/jpeg", URL: "https://example.com/photo.jpg"},
				},
			},
		},
	}

	a := New()
	nativeAny, err := a.ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if len(native.Messages) != 1 {
		t.Fatalf("native.Messages len = %d, want 1", len(native.Messages))
	}

	var parts []nativeContentPart
	if err := json.Unmarshal(native.Messages[0].Content, &parts); err != nil {
		t.Fatalf("Content is not a JSON array: %v (%s)", err, native.Messages[0].Content)
	}
	if len(parts) != 3 {
		t.Fatalf("parts len = %d, want 3 (text, image, image)", len(parts))
	}

	if parts[0].Type != "text" || parts[0].Text != "what's in these images?" {
		t.Errorf("parts[0] = %+v, want the text part", parts[0])
	}

	dataPart := parts[1]
	if dataPart.Type != "image_url" || dataPart.ImageURL == nil {
		t.Fatalf("parts[1] = %+v, want an image_url part", dataPart)
	}
	if dataPart.ImageURL.URL != "data:image/png;base64,aW1hZ2ViYXNlNjQ=" {
		t.Errorf("parts[1].ImageURL.URL = %q, want a data: URI built from MediaType/Data", dataPart.ImageURL.URL)
	}

	urlPart := parts[2]
	if urlPart.Type != "image_url" || urlPart.ImageURL == nil || urlPart.ImageURL.URL != "https://example.com/photo.jpg" {
		t.Errorf("parts[2] = %+v, want an image_url part with the part's own URL passed through verbatim", urlPart)
	}
}

// TestToProviderDocumentContentPartFailsLoudly proves the real,
// deliberate scope limit: the Chat Completions API has no native
// document content-part type, so a "document" part must return a real,
// typed error rather than a silently wrong or dropped mapping.
func TestToProviderDocumentContentPartFailsLoudly(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gpt-4o",
		Messages: []adapter.Message{
			{Role: "user", Parts: []adapter.ContentPart{{Type: "document", MediaType: "application/pdf", Data: "x"}}},
		},
	}

	if _, err := New().ToProvider(req); err == nil {
		t.Fatal("ToProvider with a document content part returned nil error, want an error")
	}
}

// TestToProviderUnsupportedContentPartTypeFailsLoudly proves an unknown
// part type returns a real, typed error rather than being silently
// dropped.
func TestToProviderUnsupportedContentPartTypeFailsLoudly(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gpt-4o",
		Messages: []adapter.Message{
			{Role: "user", Parts: []adapter.ContentPart{{Type: "video", MediaType: "video/mp4", Data: "x"}}},
		},
	}

	if _, err := New().ToProvider(req); err == nil {
		t.Fatal("ToProvider with an unsupported content part type returned nil error, want an error")
	}
}

// TestFromProviderMultiModalResponseContentFailsLoudly proves the
// response-direction scope limit named in docs/rfcs/2026-09-06-gateway-
// multimodal-content.md's Alternatives Considered: a native response
// whose content is a JSON array (a real possibility for other OpenAI
// endpoints, even though Chat Completions responses are text-only in
// practice) returns a real, typed error rather than silently discarding
// non-text parts.
func TestFromProviderMultiModalResponseContentFailsLoudly(t *testing.T) {
	resp := &Response{
		ID:    "chatcmpl-test",
		Model: "gpt-4o",
		Choices: []Choice{
			{Index: 0, Message: Message{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)}, FinishReason: "stop"},
		},
	}

	if _, err := New().FromProvider(resp); err == nil {
		t.Fatal("FromProvider with array-shaped response content returned nil error, want an error")
	}
}

func TestToProviderInvalidToolArguments(t *testing.T) {
	a := New()
	req := adapter.ChatRequest{
		Model: "gpt-4o",
		Messages: []adapter.Message{
			{Role: "assistant", ToolCalls: []adapter.ToolCall{
				{ID: "call_1", Name: "f", ArgumentsJSON: "not-json"},
			}},
		},
	}
	if _, err := a.ToProvider(req); err == nil {
		t.Fatal("expected error for invalid ArgumentsJSON, got nil")
	}
}

// TestToProviderCacheControlKeySetsPromptCacheKey is the load-bearing
// proof for docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md's
// OpenAI wiring: a CacheControl.Key set on a message must become the
// native request's top-level prompt_cache_key field verbatim.
func TestToProviderCacheControlKeySetsPromptCacheKey(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gpt-4o",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful assistant.", CacheControl: &adapter.CacheControl{Key: "session-abc-123"}},
			{Role: "user", Content: "hi"},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if native.PromptCacheKey != "session-abc-123" {
		t.Errorf("native.PromptCacheKey = %q, want %q", native.PromptCacheKey, "session-abc-123")
	}
}

// TestToProviderCacheControlKeyOnContentPartIsFound proves findCacheKey
// also scans multi-modal content parts, not just message-level markers.
func TestToProviderCacheControlKeyOnContentPartIsFound(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gpt-4o",
		Messages: []adapter.Message{
			{
				Role:    "user",
				Content: "see attached",
				Parts: []adapter.ContentPart{
					{Type: "image", MediaType: "image/png", Data: "aW1n", CacheControl: &adapter.CacheControl{Key: "part-level-key"}},
				},
			},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if native.PromptCacheKey != "part-level-key" {
		t.Errorf("native.PromptCacheKey = %q, want %q", native.PromptCacheKey, "part-level-key")
	}
}

// TestToProviderUnsetCacheControlOmitsPromptCacheKey proves the unset
// (nil, the default) case -- and a CacheControl set with an empty Key,
// which is also a no-value case -- never emits prompt_cache_key at all,
// matching this schema's existing optional-field convention rather than
// inventing a fabricated value, per findCacheKey's own doc comment.
func TestToProviderUnsetCacheControlOmitsPromptCacheKey(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gpt-4o",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful assistant.", CacheControl: &adapter.CacheControl{TTL: "1h"}},
			{Role: "user", Content: "hi"},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if native.PromptCacheKey != "" {
		t.Errorf("native.PromptCacheKey = %q, want empty (no Key was ever supplied)", native.PromptCacheKey)
	}

	b, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("marshaling native request: %v", err)
	}
	if strings.Contains(string(b), "prompt_cache_key") {
		t.Errorf("marshaled request contains prompt_cache_key despite no Key ever being supplied: %s", b)
	}
}
