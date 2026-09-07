package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// TestRoundTrip proves the two hazards shared with anthropic: system-prompt
// placement and tool-call argument re-encoding, plus Gemini's own
// "assistant" -> "model" role mapping.
func TestRoundTrip(t *testing.T) {
	original := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful weather assistant."},
			{Role: "user", Content: "What's the weather in Boston?"},
		},
		Tools: []adapter.ToolDef{
			{
				Name:           "get_weather",
				Description:    "Get the current weather for a city",
				ParametersJSON: `{"type":"object","properties":{"city":{"type":"string"}}}`,
			},
		},
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

	if native.SystemInstruction == nil || len(native.SystemInstruction.Parts) != 1 ||
		native.SystemInstruction.Parts[0].Text != "You are a helpful weather assistant." {
		t.Errorf("native.SystemInstruction = %+v, want the system message content", native.SystemInstruction)
	}
	for _, c := range native.Contents {
		if c.Role == "system" {
			t.Errorf("native.Contents contains a role:system content; it must be pulled into SystemInstruction")
		}
	}
	if len(native.Contents) != 1 || native.Contents[0].Role != "user" {
		t.Fatalf("native.Contents = %+v, want exactly one user content", native.Contents)
	}

	toolCallArgs := `{"city":"Boston"}`
	var parsedArgs map[string]any
	if err := json.Unmarshal([]byte(toolCallArgs), &parsedArgs); err != nil {
		t.Fatalf("test setup: %v", err)
	}

	nativeResp := &Response{
		Candidates: []Candidate{
			{
				Content: Content{
					Role: "model",
					Parts: []Part{
						{FunctionCall: &FunctionCall{ID: "call_1", Name: "get_weather", Args: parsedArgs}},
					},
				},
				FinishReason: "STOP",
			},
		},
		UsageMetadata: UsageMetadata{PromptTokenCount: 20, CandidatesTokenCount: 8, TotalTokenCount: 28},
	}

	got, err := a.FromProvider(nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}

	if len(got.Choices) != 1 {
		t.Fatalf("Choices len = %d, want 1", len(got.Choices))
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want %q (STOP+functionCall must map to tool_calls)", got.Choices[0].FinishReason, "tool_calls")
	}
	gotToolCalls := got.Choices[0].Message.ToolCalls
	if len(gotToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(gotToolCalls))
	}

	if !json.Valid([]byte(gotToolCalls[0].ArgumentsJSON)) {
		t.Fatalf("ArgumentsJSON is not valid JSON: %q", gotToolCalls[0].ArgumentsJSON)
	}
	var roundTripped map[string]any
	if err := json.Unmarshal([]byte(gotToolCalls[0].ArgumentsJSON), &roundTripped); err != nil {
		t.Fatalf("unmarshaling round-tripped ArgumentsJSON: %v", err)
	}
	if roundTripped["city"] != "Boston" {
		t.Errorf("round-tripped arguments = %v, want city=Boston", roundTripped)
	}

	if got.Usage.PromptTokens != 20 || got.Usage.CompletionTokens != 8 || got.Usage.TotalTokens != 28 {
		t.Errorf("Usage = %+v, want {20 8 28}", got.Usage)
	}
}

// TestToProviderToolResultMessageResolvesFunctionResponseName proves the
// real hazard this adapter's own grounding research found by direct schema
// inspection: FunctionResponse.name is required, but the canonical
// role:"tool" message only carries ToolCallID — ToProvider must resolve
// the originating call's Name from message history.
func TestToProviderToolResultMessageResolvesFunctionResponseName(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "user", Content: "call the tool"},
			{Role: "assistant", ToolCalls: []adapter.ToolCall{
				{ID: "call_1", Name: "get_weather", ArgumentsJSON: `{"city":"Boston"}`},
			}},
			{Role: "tool", Content: `{"temp_f":72}`, ToolCallID: "call_1"},
		},
	}

	a := New()
	nativeAny, err := a.ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if len(native.Contents) != 3 {
		t.Fatalf("native.Contents len = %d, want 3", len(native.Contents))
	}
	toolResultContent := native.Contents[2]
	if toolResultContent.Role != "user" {
		t.Errorf("tool-result content Role = %q, want %q", toolResultContent.Role, "user")
	}
	if len(toolResultContent.Parts) != 1 || toolResultContent.Parts[0].FunctionResponse == nil {
		t.Fatalf("tool-result content Parts = %+v", toolResultContent.Parts)
	}
	fr := toolResultContent.Parts[0].FunctionResponse
	if fr.Name != "get_weather" {
		t.Errorf("FunctionResponse.Name = %q, want %q (resolved from message history)", fr.Name, "get_weather")
	}
	if fr.ID != "call_1" {
		t.Errorf("FunctionResponse.ID = %q, want %q", fr.ID, "call_1")
	}
	if fr.Response["result"] != `{"temp_f":72}` {
		t.Errorf("FunctionResponse.Response = %+v, want result=%q", fr.Response, `{"temp_f":72}`)
	}
}

// TestToProviderToolMessageWithUnknownToolCallIDFails proves ToProvider
// never sends Gemini a functionResponse with an empty/guessed name — a
// tool message referencing a tool_call_id with no matching prior ToolCall
// must fail loudly, not silently.
func TestToProviderToolMessageWithUnknownToolCallIDFails(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "tool", Content: "result", ToolCallID: "no-such-call"},
		},
	}

	_, err := New().ToProvider(req)
	if err == nil {
		t.Fatal("ToProvider: want error for unknown tool_call_id, got nil")
	}
	if !strings.Contains(err.Error(), "no-such-call") {
		t.Errorf("error = %v, want it to mention the unknown tool_call_id", err)
	}
}

func TestToProviderInvalidToolArguments(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "assistant", ToolCalls: []adapter.ToolCall{
				{ID: "call_1", Name: "get_weather", ArgumentsJSON: "{not valid json"},
			}},
		},
	}

	_, err := New().ToProvider(req)
	if err == nil {
		t.Fatal("ToProvider: want error for invalid ArgumentsJSON, got nil")
	}
}

// TestFromProviderMalformedFunctionCallReturnsError proves the model's own
// broken tool-call machinery surfaces as a real, typed error rather than a
// fake successful Choice.
func TestFromProviderMalformedFunctionCallReturnsError(t *testing.T) {
	resp := &Response{
		Candidates: []Candidate{
			{Content: Content{Role: "model"}, FinishReason: "MALFORMED_FUNCTION_CALL"},
		},
	}

	_, err := New().FromProvider(resp)
	if err == nil {
		t.Fatal("FromProvider: want error for MALFORMED_FUNCTION_CALL, got nil")
	}
}

func TestFromProviderNoCandidatesReturnsError(t *testing.T) {
	resp := &Response{Candidates: []Candidate{}}

	_, err := New().FromProvider(resp)
	if err == nil {
		t.Fatal("FromProvider: want error for zero candidates, got nil")
	}
}

func TestFromProviderStopWithoutFunctionCallMapsToStop(t *testing.T) {
	resp := &Response{
		Candidates: []Candidate{
			{
				Content:      Content{Role: "model", Parts: []Part{{Text: "hello"}}},
				FinishReason: "STOP",
			},
		},
	}

	got, err := New().FromProvider(resp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", got.Choices[0].FinishReason, "stop")
	}
	if got.Choices[0].Message.Content != "hello" {
		t.Errorf("Content = %q, want %q", got.Choices[0].Message.Content, "hello")
	}
}

func TestFromProviderMaxTokensMapsToLength(t *testing.T) {
	resp := &Response{
		Candidates: []Candidate{{Content: Content{Role: "model"}, FinishReason: "MAX_TOKENS"}},
	}
	got, err := New().FromProvider(resp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if got.Choices[0].FinishReason != "length" {
		t.Errorf("FinishReason = %q, want %q", got.Choices[0].FinishReason, "length")
	}
}

func TestFromProviderSafetyMapsToContentFilter(t *testing.T) {
	resp := &Response{
		Candidates: []Candidate{{Content: Content{Role: "model"}, FinishReason: "SAFETY"}},
	}
	got, err := New().FromProvider(resp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if got.Choices[0].FinishReason != "content_filter" {
		t.Errorf("FinishReason = %q, want %q", got.Choices[0].FinishReason, "content_filter")
	}
}

func TestName(t *testing.T) {
	if got := New().Name(); got != "gemini" {
		t.Errorf("Name() = %q, want %q", got, "gemini")
	}
}

// TestToProviderMultiModalContentPartsMapToInlineDataAndFileData is the
// load-bearing proof for docs/rfcs/2026-09-06-gateway-multimodal-
// content.md: an inline-base64 image part must map to Gemini's real
// inlineData shape, and a URL-referenced document part to fileData —
// Gemini has no distinct "document" wire type, so a document part
// reuses the identical shape with a document MediaType.
func TestToProviderMultiModalContentPartsMapToInlineDataAndFileData(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{
				Role:    "user",
				Content: "what's in this image and document?",
				Parts: []adapter.ContentPart{
					{Type: "image", MediaType: "image/png", Data: "aW1hZ2ViYXNlNjQ="},
					{Type: "document", MediaType: "application/pdf", URL: "https://example.com/doc.pdf"},
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

	if len(native.Contents) != 1 {
		t.Fatalf("native.Contents len = %d, want 1", len(native.Contents))
	}
	parts := native.Contents[0].Parts
	if len(parts) != 3 {
		t.Fatalf("parts len = %d, want 3 (text, image, document)", len(parts))
	}

	if parts[0].Text != "what's in this image and document?" {
		t.Errorf("parts[0].Text = %q, want the message content", parts[0].Text)
	}

	imgPart := parts[1]
	if imgPart.InlineData == nil || imgPart.InlineData.MimeType != "image/png" || imgPart.InlineData.Data != "aW1hZ2ViYXNlNjQ=" {
		t.Errorf("parts[1].InlineData = %+v, want a populated InlineData with the image's MediaType/Data", imgPart.InlineData)
	}
	if imgPart.FileData != nil {
		t.Errorf("parts[1].FileData = %+v, want nil (this part used inline Data, not URL)", imgPart.FileData)
	}

	docPart := parts[2]
	if docPart.FileData == nil || docPart.FileData.MimeType != "application/pdf" || docPart.FileData.FileURI != "https://example.com/doc.pdf" {
		t.Errorf("parts[2].FileData = %+v, want a populated FileData with the document's MediaType/URL", docPart.FileData)
	}
	if docPart.InlineData != nil {
		t.Errorf("parts[2].InlineData = %+v, want nil (this part used URL, not inline Data)", docPart.InlineData)
	}
}

// TestToProviderContentPartWithNeitherDataNorURLFailsLoudly proves a
// malformed image/document part returns a real, typed error rather than
// silently producing an empty part.
func TestToProviderContentPartWithNeitherDataNorURLFailsLoudly(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "user", Parts: []adapter.ContentPart{{Type: "image", MediaType: "image/png"}}},
		},
	}

	if _, err := New().ToProvider(req); err == nil {
		t.Fatal("ToProvider with a Data-less, URL-less image part returned nil error, want an error")
	}
}

// TestToProviderUnsupportedContentPartTypeFailsLoudly proves an unknown
// part type returns a real, typed error rather than being silently
// dropped.
func TestToProviderUnsupportedContentPartTypeFailsLoudly(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "user", Parts: []adapter.ContentPart{{Type: "video", MediaType: "video/mp4", Data: "x"}}},
		},
	}

	if _, err := New().ToProvider(req); err == nil {
		t.Fatal("ToProvider with an unsupported content part type returned nil error, want an error")
	}
}

// TestToProviderCacheControlIsUnaffected is the load-bearing proof for
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md's "why Gemini
// is excluded" claim: setting adapter.CacheControl on a message, on a
// system message, on a content part, and (per that RFC's
// tool-definition-level addendum) on a ToolDef, must produce a native
// Gemini request byte-identical to the same request with no
// CacheControl set at all -- not just "gemini.go has no
// CacheControl-reading code" (true by inspection) but a real, executed
// proof that the marker has zero observable effect on this adapter's
// actual output, across the whole feature's scope, not just its
// original message/part-level part. Also extended, per
// docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md, to set
// DisableCacheControlAutoPopulate on withMarker too -- that field only
// ever changes Anthropic's/Bedrock's own system-message handling;
// gemini.go's ToProvider never reads it, so it must be an equally
// zero-effect no-op here, proven in the same single test rather than a
// new standalone one.
func TestToProviderCacheControlIsUnaffected(t *testing.T) {
	withoutMarker := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{
				Role:    "user",
				Content: "what's in this document?",
				Parts: []adapter.ContentPart{
					{Type: "document", MediaType: "application/pdf", Data: "ZG9jYmFzZTY0"},
				},
			},
		},
		Tools: []adapter.ToolDef{
			{Name: "get_weather", Description: "Get the weather", ParametersJSON: `{"type":"object"}`},
		},
	}

	withMarker := adapter.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful assistant.", CacheControl: &adapter.CacheControl{TTL: "1h"}},
			{
				Role:         "user",
				Content:      "what's in this document?",
				CacheControl: &adapter.CacheControl{Key: "session-123"},
				Parts: []adapter.ContentPart{
					{
						Type:         "document",
						MediaType:    "application/pdf",
						Data:         "ZG9jYmFzZTY0",
						CacheControl: &adapter.CacheControl{TTL: "1h", Key: "session-123"},
					},
				},
			},
		},
		Tools: []adapter.ToolDef{
			{
				Name:           "get_weather",
				Description:    "Get the weather",
				ParametersJSON: `{"type":"object"}`,
				CacheControl:   &adapter.CacheControl{TTL: "1h", Key: "session-123"},
			},
		},
		DisableCacheControlAutoPopulate: true,
	}

	a := New()

	gotWithout, err := a.ToProvider(withoutMarker)
	if err != nil {
		t.Fatalf("ToProvider(withoutMarker): %v", err)
	}
	gotWith, err := a.ToProvider(withMarker)
	if err != nil {
		t.Fatalf("ToProvider(withMarker): %v", err)
	}

	jsonWithout, err := json.Marshal(gotWithout)
	if err != nil {
		t.Fatalf("marshaling withoutMarker result: %v", err)
	}
	jsonWith, err := json.Marshal(gotWith)
	if err != nil {
		t.Fatalf("marshaling withMarker result: %v", err)
	}

	if string(jsonWithout) != string(jsonWith) {
		t.Errorf("CacheControl changed Gemini's native request output:\nwithout marker: %s\nwith marker:    %s", jsonWithout, jsonWith)
	}
}
