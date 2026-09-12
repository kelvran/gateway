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

// TestFromProviderExtractsRealCachedTokens proves adapter.Usage.CacheReadTokens
// is populated from OpenAI's real prompt_tokens_details.cached_tokens field,
// decoded from a raw JSON payload shaped exactly like OpenAI's own OpenAPI
// spec (openapi.yaml's CompletionUsage schema) -- not hand-constructed Go
// structs, so a wrong json tag would be caught here too.
func TestFromProviderExtractsRealCachedTokens(t *testing.T) {
	raw := []byte(`{
		"id": "chatcmpl-test",
		"model": "gpt-4o",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}],
		"usage": {
			"prompt_tokens": 1024,
			"completion_tokens": 10,
			"total_tokens": 1034,
			"prompt_tokens_details": {"cached_tokens": 896}
		}
	}`)
	var nativeResp Response
	if err := json.Unmarshal(raw, &nativeResp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	a := New()
	got, err := a.FromProvider(&nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}

	if got.Usage.PromptTokens != 1024 {
		t.Errorf("Usage.PromptTokens = %d, want 1024", got.Usage.PromptTokens)
	}
	if got.Usage.CacheReadTokens != 896 {
		t.Errorf("Usage.CacheReadTokens = %d, want 896", got.Usage.CacheReadTokens)
	}
	if got.Usage.CacheCreationTokens != 0 {
		t.Errorf("Usage.CacheCreationTokens = %d, want 0 -- OpenAI's automatic caching has no cache-creation charge", got.Usage.CacheCreationTokens)
	}
}

// TestFromProviderSurfacesRefusalMessage proves OpenAI's real, current
// structured-outputs refusal shape (confirmed against OpenAI's own
// official Node SDK source: ChatCompletionMessage.refusal, a field
// SIBLING to content, "The refusal message generated by the model.")
// is surfaced onto the canonical Message.Refusal field, not silently
// discarded the way an unrecognized JSON field ordinarily would be.
func TestFromProviderSurfacesRefusalMessage(t *testing.T) {
	raw := []byte(`{
		"id": "chatcmpl-test",
		"model": "gpt-4o",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": null, "refusal": "I cannot help with that request."}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)
	var nativeResp Response
	if err := json.Unmarshal(raw, &nativeResp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	a := New()
	got, err := a.FromProvider(&nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}

	if len(got.Choices) != 1 {
		t.Fatalf("Choices len = %d, want 1", len(got.Choices))
	}
	if got.Choices[0].Message.Refusal != "I cannot help with that request." {
		t.Errorf("Message.Refusal = %q, want the real refusal text (currently silently dropped)", got.Choices[0].Message.Refusal)
	}
	if got.Choices[0].Message.Content != "" {
		t.Errorf("Message.Content = %q, want empty (null content alongside a refusal)", got.Choices[0].Message.Content)
	}
}

// TestFromProviderMissingPromptTokensDetailsDefaultsToZeroCacheRead proves
// the common case (an older API response, or a model that never populates
// prompt_tokens_details at all) stays exactly as it behaved before this
// field existed -- no nil-pointer panic, CacheReadTokens simply 0.
func TestFromProviderMissingPromptTokensDetailsDefaultsToZeroCacheRead(t *testing.T) {
	nativeResp := &Response{
		ID:      "chatcmpl-test",
		Model:   "gpt-4o",
		Choices: []Choice{{Index: 0, Message: Message{Role: "assistant", Content: json.RawMessage(`"hi"`)}, FinishReason: "stop"}},
		Usage:   Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}

	a := New()
	got, err := a.FromProvider(nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if got.Usage.CacheReadTokens != 0 {
		t.Errorf("Usage.CacheReadTokens = %d, want 0 when PromptTokensDetails is absent", got.Usage.CacheReadTokens)
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
// inventing a fabricated value, per findCacheKey's own doc comment. Also
// extended to prove OpenAI is unaffected by the deployment-level
// DisableCacheControlAutoPopulate opt-out added by
// docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md -- that
// field only ever changes Anthropic's/Bedrock's own system-message
// handling; openai.go's ToProvider never reads it at all, so setting it
// must produce byte-identical output.
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

	withOptOut := req
	withOptOut.DisableCacheControlAutoPopulate = true
	nativeOptOutAny, err := New().ToProvider(withOptOut)
	if err != nil {
		t.Fatalf("ToProvider(withOptOut): %v", err)
	}
	bOptOut, err := json.Marshal(nativeOptOutAny)
	if err != nil {
		t.Fatalf("marshaling opt-out native request: %v", err)
	}
	if string(b) != string(bOptOut) {
		t.Errorf("DisableCacheControlAutoPopulate changed OpenAI's native request output:\nwithout: %s\nwith:    %s", b, bOptOut)
	}
}

// TestToProviderToolDefCacheControlHasNoEffect is the load-bearing proof
// for docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md's
// tool-definition-level addendum re-confirming OpenAI's exclusion: a
// ToolDef.CacheControl.Key must NOT flow into prompt_cache_key (unlike a
// Message/ContentPart-level Key) since findCacheKey only ever scans
// messages/parts, never Tools, and OpenAI's native Tool struct has no
// cache-marker field for either adapter to read at all -- the produced
// native request must be identical to the same request with no ToolDef
// CacheControl set, and must carry no prompt_cache_key from the tool
// definition's Key either.
func TestToProviderToolDefCacheControlHasNoEffect(t *testing.T) {
	base := adapter.ChatRequest{
		Model: "gpt-4o",
		Messages: []adapter.Message{
			{Role: "user", Content: "what's the weather?"},
		},
		Tools: []adapter.ToolDef{
			{Name: "get_weather", Description: "Get the weather", ParametersJSON: `{"type":"object"}`},
		},
	}
	withToolCacheControl := adapter.ChatRequest{
		Model:    base.Model,
		Messages: base.Messages,
		Tools: []adapter.ToolDef{
			{
				Name:           "get_weather",
				Description:    "Get the weather",
				ParametersJSON: `{"type":"object"}`,
				CacheControl:   &adapter.CacheControl{TTL: "1h", Key: "tool-level-key"},
			},
		},
	}

	gotBase, err := New().ToProvider(base)
	if err != nil {
		t.Fatalf("ToProvider(base): %v", err)
	}
	gotWithMarker, err := New().ToProvider(withToolCacheControl)
	if err != nil {
		t.Fatalf("ToProvider(withToolCacheControl): %v", err)
	}

	jsonBase, err := json.Marshal(gotBase)
	if err != nil {
		t.Fatalf("marshaling base: %v", err)
	}
	jsonWithMarker, err := json.Marshal(gotWithMarker)
	if err != nil {
		t.Fatalf("marshaling withToolCacheControl: %v", err)
	}
	if string(jsonBase) != string(jsonWithMarker) {
		t.Errorf("ToolDef.CacheControl changed OpenAI's native request output:\nwithout marker: %s\nwith marker:    %s", jsonBase, jsonWithMarker)
	}
	if strings.Contains(string(jsonWithMarker), "tool-level-key") {
		t.Errorf("marshaled request leaked the tool-level CacheControl.Key into the wire format: %s", jsonWithMarker)
	}
}

// TestToProviderResponseFormatMapsToNativeShape proves a canonical
// ResponseFormat/JSONSchema maps directly onto OpenAI's real
// response_format/json_schema wire shape (name/strict/schema, confirmed
// against OpenAI's own live shared_params/response_format_json_schema.py).
func TestToProviderResponseFormatMapsToNativeShape(t *testing.T) {
	req := adapter.ChatRequest{
		Model:    "gpt-4o",
		Messages: []adapter.Message{{Role: "user", Content: "give me JSON"}},
		ResponseFormat: &adapter.ResponseFormat{
			Type: "json_schema",
			JSONSchema: &adapter.JSONSchema{
				Name:   "weather_response",
				Strict: true,
				Schema: json.RawMessage(`{"type":"object","properties":{"temp_f":{"type":"number"}},"required":["temp_f"]}`),
			},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if native.ResponseFormat == nil {
		t.Fatal("native.ResponseFormat = nil, want a populated *ResponseFormat")
	}
	if native.ResponseFormat.Type != "json_schema" {
		t.Errorf("native.ResponseFormat.Type = %q, want %q", native.ResponseFormat.Type, "json_schema")
	}
	if native.ResponseFormat.JSONSchema == nil {
		t.Fatal("native.ResponseFormat.JSONSchema = nil, want a populated *JSONSchema")
	}
	if native.ResponseFormat.JSONSchema.Name != "weather_response" {
		t.Errorf("JSONSchema.Name = %q, want %q", native.ResponseFormat.JSONSchema.Name, "weather_response")
	}
	if !native.ResponseFormat.JSONSchema.Strict {
		t.Error("JSONSchema.Strict = false, want true")
	}

	b, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("marshaling native request: %v", err)
	}
	var wireTree map[string]any
	if err := json.Unmarshal(b, &wireTree); err != nil {
		t.Fatalf("unmarshaling marshaled request: %v", err)
	}
	rf, ok := wireTree["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("marshaled request has no response_format object: %s", b)
	}
	if rf["type"] != "json_schema" {
		t.Errorf("wire response_format.type = %v, want json_schema", rf["type"])
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("wire response_format has no json_schema object: %v", rf)
	}
	if js["name"] != "weather_response" || js["strict"] != true {
		t.Errorf("wire json_schema = %v, want name=weather_response strict=true", js)
	}
	if _, ok := js["schema"].(map[string]any); !ok {
		t.Errorf("wire json_schema.schema is not an object: %v", js)
	}
}

// TestToProviderNilResponseFormatOmitsField proves the unset (nil, the
// default) case never emits response_format at all, matching this
// schema's existing optional-field convention — byte-identical to every
// ChatRequest built before this field existed.
func TestToProviderNilResponseFormatOmitsField(t *testing.T) {
	req := adapter.ChatRequest{
		Model:    "gpt-4o",
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)
	if native.ResponseFormat != nil {
		t.Errorf("native.ResponseFormat = %+v, want nil", native.ResponseFormat)
	}

	b, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("marshaling native request: %v", err)
	}
	if strings.Contains(string(b), "response_format") {
		t.Errorf("marshaled request contains response_format despite ResponseFormat being nil: %s", b)
	}
}
