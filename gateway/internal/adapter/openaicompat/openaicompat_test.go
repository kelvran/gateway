package openaicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// TestRoundTrip proves the openaicompat adapter's ToProvider -> FromProvider
// round-trip is lossless for every field the adapter claims to handle,
// per docs/testing/TESTING.md §3's explicit requirement. Stream is
// intentionally excluded from the equality check: the native response
// payload never echoes the request's Stream flag back.
func TestRoundTrip(t *testing.T) {
	temp := 0.7
	maxTokens := 512

	original := adapter.ChatRequest{
		Model: "llama-3.1-70b-instruct",
		Messages: []adapter.Message{
			{Role: "user", Content: "What's the weather in Austin?"},
			{
				Role: "assistant",
				ToolCalls: []adapter.ToolCall{
					{ID: "call_1", Name: "get_weather", ArgumentsJSON: `{"city":"Austin"}`},
				},
			},
			{Role: "tool", Content: `{"temp_f":95}`, ToolCallID: "call_1"},
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
	// back, as an OpenAI-compatible server's Chat Completions API would
	// (choices wrap messages).
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
	if native.Messages[1].ToolCalls[0].Function.Arguments != `{"city":"Austin"}` {
		t.Errorf("native tool call arguments = %q, want %q", native.Messages[1].ToolCalls[0].Function.Arguments, `{"city":"Austin"}`)
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
// is populated from a prompt_tokens_details.cached_tokens field, when a
// self-hosted runtime sends one (mirrors real OpenAI's own field --
// whether a given runtime actually populates it is runtime-dependent
// and NOT independently verified here; see Usage's own doc comment).
func TestFromProviderExtractsRealCachedTokens(t *testing.T) {
	raw := []byte(`{
		"id": "cmpl-test",
		"model": "llama-3-70b",
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

	if got.Usage.CacheReadTokens != 896 {
		t.Errorf("Usage.CacheReadTokens = %d, want 896", got.Usage.CacheReadTokens)
	}
	if got.Usage.CacheCreationTokens != 0 {
		t.Errorf("Usage.CacheCreationTokens = %d, want 0", got.Usage.CacheCreationTokens)
	}
}

// TestFromProviderSurfacesRefusalMessage mirrors
// internal/adapter/openai's identically named test -- see that copy's
// doc comment for the real, confirmed OpenAI field shape this near-
// verbatim adapter targets. Whether a given self-hosted runtime actually
// populates this field is runtime-dependent (same disclosed caveat as
// the cache-token tests above), but this proves the wire-shape
// acceptance at least: when a runtime DOES send one, it must not be
// silently discarded.
func TestFromProviderSurfacesRefusalMessage(t *testing.T) {
	raw := []byte(`{
		"id": "cmpl-test",
		"model": "llama-3-70b",
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
}

// TestFromProviderMissingPromptTokensDetailsDefaultsToZeroCacheRead proves
// the overwhelming-majority case (a runtime that never sends
// prompt_tokens_details at all) stays exactly as it behaved before this
// field existed -- no nil-pointer panic, CacheReadTokens simply 0.
func TestFromProviderMissingPromptTokensDetailsDefaultsToZeroCacheRead(t *testing.T) {
	nativeResp := &Response{
		ID:      "cmpl-test",
		Model:   "llama-3-70b",
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
	if got := New().Name(); got != "openaicompat" {
		t.Errorf("Name() = %q, want %q", got, "openaicompat")
	}
}

// TestToProviderTextOnlyContentIsByteIdenticalToPlainString mirrors
// internal/adapter/openai's own test of the same name — see that
// package's doc comment for the full rationale.
func TestToProviderTextOnlyContentIsByteIdenticalToPlainString(t *testing.T) {
	req := adapter.ChatRequest{
		Model:    "llama-3-70b",
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

// TestToProviderMultiModalContentPartsMapToImageURL mirrors
// internal/adapter/openai's own test of the same name.
func TestToProviderMultiModalContentPartsMapToImageURL(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "llama-3-70b",
		Messages: []adapter.Message{
			{
				Role:    "user",
				Content: "what's in this image?",
				Parts: []adapter.ContentPart{
					{Type: "image", MediaType: "image/png", Data: "aW1hZ2ViYXNlNjQ="},
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

	var parts []nativeContentPart
	if err := json.Unmarshal(native.Messages[0].Content, &parts); err != nil {
		t.Fatalf("Content is not a JSON array: %v (%s)", err, native.Messages[0].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("parts len = %d, want 2 (text, image)", len(parts))
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,aW1hZ2ViYXNlNjQ=" {
		t.Errorf("parts[1] = %+v, want an image_url part with a data: URI", parts[1])
	}
}

// TestToProviderDocumentContentPartFailsLoudly mirrors
// internal/adapter/openai's own test of the same name.
func TestToProviderDocumentContentPartFailsLoudly(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "llama-3-70b",
		Messages: []adapter.Message{
			{Role: "user", Parts: []adapter.ContentPart{{Type: "document", MediaType: "application/pdf", Data: "x"}}},
		},
	}

	if _, err := New().ToProvider(req); err == nil {
		t.Fatal("ToProvider with a document content part returned nil error, want an error")
	}
}

// TestFromProviderMultiModalResponseContentFailsLoudly mirrors
// internal/adapter/openai's own test of the same name.
func TestFromProviderMultiModalResponseContentFailsLoudly(t *testing.T) {
	resp := &Response{
		ID:    "cmpl-test",
		Model: "llama-3-70b",
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
		Model: "llama-3.1-70b-instruct",
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

// TestToProviderResponseFormatMapsToNativeShape proves this package
// mirrors internal/adapter/openai's response_format wiring exactly, per
// its own "near-verbatim copy" convention.
func TestToProviderResponseFormatMapsToNativeShape(t *testing.T) {
	req := adapter.ChatRequest{
		Model:    "llama-3-70b",
		Messages: []adapter.Message{{Role: "user", Content: "give me JSON"}},
		ResponseFormat: &adapter.ResponseFormat{
			Type: "json_schema",
			JSONSchema: &adapter.JSONSchema{
				Name:   "weather_response",
				Strict: true,
				Schema: json.RawMessage(`{"type":"object","properties":{"temp_f":{"type":"number"}}}`),
			},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if native.ResponseFormat == nil || native.ResponseFormat.JSONSchema == nil {
		t.Fatal("native.ResponseFormat/.JSONSchema = nil, want populated")
	}
	if native.ResponseFormat.Type != "json_schema" {
		t.Errorf("ResponseFormat.Type = %q, want json_schema", native.ResponseFormat.Type)
	}
	if native.ResponseFormat.JSONSchema.Name != "weather_response" || !native.ResponseFormat.JSONSchema.Strict {
		t.Errorf("JSONSchema = %+v, want Name=weather_response Strict=true", native.ResponseFormat.JSONSchema)
	}
}

// TestToProviderNilResponseFormatOmitsField proves the unset (nil, the
// default) case never emits response_format at all -- byte-identical to
// every ChatRequest built before this field existed.
func TestToProviderNilResponseFormatOmitsField(t *testing.T) {
	req := adapter.ChatRequest{
		Model:    "llama-3-70b",
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

// TestFromProviderCapturesReasoningContentField proves a native response
// using llama.cpp's wire name ("reasoning_content") round-trips into a
// canonical ReasoningBlock.
func TestFromProviderCapturesReasoningContentField(t *testing.T) {
	raw := []byte(`{
		"id": "cmpl-test",
		"model": "llama-3-70b",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "hi", "reasoning_content": "thinking it through"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)
	var nativeResp Response
	if err := json.Unmarshal(raw, &nativeResp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	got, err := New().FromProvider(&nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	want := []adapter.ReasoningBlock{{Sequence: 0, Text: "thinking it through"}}
	if diff := cmpReasoningBlocks(got.Choices[0].Message.ReasoningBlocks, want); diff != "" {
		t.Errorf("ReasoningBlocks mismatch: %s", diff)
	}
}

// TestFromProviderCapturesReasoningField proves a native response using
// vLLM/Ollama's wire name ("reasoning") round-trips identically to
// TestFromProviderCapturesReasoningContentField's llama.cpp shape.
func TestFromProviderCapturesReasoningField(t *testing.T) {
	raw := []byte(`{
		"id": "cmpl-test",
		"model": "llama-3-70b",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "hi", "reasoning": "thinking it through"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)
	var nativeResp Response
	if err := json.Unmarshal(raw, &nativeResp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	got, err := New().FromProvider(&nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	want := []adapter.ReasoningBlock{{Sequence: 0, Text: "thinking it through"}}
	if diff := cmpReasoningBlocks(got.Choices[0].Message.ReasoningBlocks, want); diff != "" {
		t.Errorf("ReasoningBlocks mismatch: %s", diff)
	}
}

// TestFromProviderPrefersReasoningOverReasoningContentWhenBothPresent
// proves the documented preference order: "reasoning" (the name every
// runtime except llama.cpp has migrated to) wins when a response
// somehow carries both.
func TestFromProviderPrefersReasoningOverReasoningContentWhenBothPresent(t *testing.T) {
	nativeResp := &Response{
		ID:    "cmpl-test",
		Model: "llama-3-70b",
		Choices: []Choice{{
			Index:        0,
			Message:      Message{Role: "assistant", Content: json.RawMessage(`"hi"`), Reasoning: "new-name", ReasoningContent: "old-name"},
			FinishReason: "stop",
		}},
	}

	got, err := New().FromProvider(nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	want := []adapter.ReasoningBlock{{Sequence: 0, Text: "new-name"}}
	if diff := cmpReasoningBlocks(got.Choices[0].Message.ReasoningBlocks, want); diff != "" {
		t.Errorf("ReasoningBlocks mismatch: %s", diff)
	}
}

// TestFromProviderMissingReasoningFieldsProducesNilReasoningBlocks proves
// the overwhelming-majority case (a runtime that never sends reasoning at
// all, e.g. TGI) stays exactly as it behaved before this field existed.
func TestFromProviderMissingReasoningFieldsProducesNilReasoningBlocks(t *testing.T) {
	nativeResp := &Response{
		ID:      "cmpl-test",
		Model:   "llama-3-70b",
		Choices: []Choice{{Index: 0, Message: Message{Role: "assistant", Content: json.RawMessage(`"hi"`)}, FinishReason: "stop"}},
	}

	got, err := New().FromProvider(nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if got.Choices[0].Message.ReasoningBlocks != nil {
		t.Errorf("ReasoningBlocks = %+v, want nil when neither wire field is present", got.Choices[0].Message.ReasoningBlocks)
	}
}

// TestToProviderEmitsReasoningUnderBothWireFieldNames proves a canonical
// ReasoningBlocks slice is written under BOTH "reasoning" and
// "reasoning_content" on the outgoing native request, so this adapter
// works regardless of which name the target runtime actually reads.
func TestToProviderEmitsReasoningUnderBothWireFieldNames(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "llama-3-70b",
		Messages: []adapter.Message{
			{Role: "assistant", Content: "the answer", ReasoningBlocks: []adapter.ReasoningBlock{{Sequence: 0, Text: "step one"}}},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	b, err := json.Marshal(nativeAny)
	if err != nil {
		t.Fatalf("marshaling native request: %v", err)
	}
	if !strings.Contains(string(b), `"reasoning":"step one"`) {
		t.Errorf("marshaled request = %s, want a \"reasoning\" field", b)
	}
	if !strings.Contains(string(b), `"reasoning_content":"step one"`) {
		t.Errorf("marshaled request = %s, want a \"reasoning_content\" field", b)
	}
}

// TestToProviderSkipsRedactedReasoningBlocks proves a Redacted block's
// (opaque, provider-encrypted) Text is never surfaced onto the wire — no
// self-hosted runtime this package targets produces or accepts ciphertext
// reasoning, so a Redacted block simply contributes nothing.
func TestToProviderSkipsRedactedReasoningBlocks(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "llama-3-70b",
		Messages: []adapter.Message{
			{
				Role:    "assistant",
				Content: "the answer",
				ReasoningBlocks: []adapter.ReasoningBlock{
					{Sequence: 0, Redacted: true, Data: "opaque-ciphertext"},
					{Sequence: 1, Text: "visible step"},
				},
			},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)
	if native.Messages[0].Reasoning != "visible step" {
		t.Errorf("Reasoning = %q, want only the non-redacted block's text", native.Messages[0].Reasoning)
	}
	if strings.Contains(native.Messages[0].Reasoning, "opaque-ciphertext") {
		t.Errorf("Reasoning = %q, must never surface a Redacted block's Data", native.Messages[0].Reasoning)
	}
}

// cmpReasoningBlocks reports a human-readable diff, or "" if equal --
// avoids pulling in a third-party diff dependency for this small struct.
func cmpReasoningBlocks(got, want []adapter.ReasoningBlock) string {
	if len(got) != len(want) {
		return fmtReasoningBlocks(got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			return fmtReasoningBlocks(got, want)
		}
	}
	return ""
}

func fmtReasoningBlocks(got, want []adapter.ReasoningBlock) string {
	return "got=" + fmtBlockSlice(got) + " want=" + fmtBlockSlice(want)
}

func fmtBlockSlice(bs []adapter.ReasoningBlock) string {
	b, _ := json.Marshal(bs)
	return string(b)
}
