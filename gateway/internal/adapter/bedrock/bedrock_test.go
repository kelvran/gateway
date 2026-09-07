package bedrock

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// TestRoundTrip proves the two hazards shared with anthropic/gemini:
// system-prompt placement and tool-call argument re-encoding.
func TestRoundTrip(t *testing.T) {
	original := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
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

	if len(native.System) != 1 || native.System[0].Text != "You are a helpful weather assistant." {
		t.Errorf("native.System = %+v, want the system message content", native.System)
	}
	for _, m := range native.Messages {
		if m.Role == "system" {
			t.Errorf("native.Messages contains a role:system message; it must be pulled into System")
		}
	}
	if len(native.Messages) != 1 || native.Messages[0].Role != "user" {
		t.Fatalf("native.Messages = %+v, want exactly one user message", native.Messages)
	}

	toolCallArgs := `{"city":"Boston"}`
	var parsedArgs map[string]any
	if err := json.Unmarshal([]byte(toolCallArgs), &parsedArgs); err != nil {
		t.Fatalf("test setup: %v", err)
	}

	nativeResp := &Response{
		Output: Output{
			Message: Message{
				Role: "assistant",
				Content: []ContentBlock{
					{ToolUse: &ToolUse{ToolUseID: "tooluse_1", Name: "get_weather", Input: parsedArgs}},
				},
			},
		},
		StopReason: "tool_use",
		Usage:      Usage{InputTokens: 20, OutputTokens: 8, TotalTokens: 28},
	}

	got, err := a.FromProvider(nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}

	if len(got.Choices) != 1 {
		t.Fatalf("Choices len = %d, want 1", len(got.Choices))
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want %q", got.Choices[0].FinishReason, "tool_calls")
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

	if got.ID != "" {
		t.Errorf("ID = %q, want empty -- Converse has no native response-ID field", got.ID)
	}

	if got.Usage.PromptTokens != 20 || got.Usage.CompletionTokens != 8 || got.Usage.TotalTokens != 28 {
		t.Errorf("Usage = %+v, want {20 8 28}", got.Usage)
	}
}

// TestToProviderToolResultMessageNeedsNoNameLookup proves the real,
// named simplification vs. Gemini's functionResponse.name hazard: a
// Bedrock toolResult correlates purely by ToolUseID, with no "name"
// field to resolve from message history at all.
func TestToProviderToolResultMessageNeedsNoNameLookup(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			// Deliberately no preceding assistant tool-call message --
			// unlike gemini's equivalent test, this must still succeed,
			// since no name-lookup is needed at all.
			{Role: "tool", Content: `{"temp_f":72}`, ToolCallID: "tooluse_1"},
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
	toolResultMsg := native.Messages[0]
	if toolResultMsg.Role != "user" {
		t.Errorf("tool-result message Role = %q, want %q", toolResultMsg.Role, "user")
	}
	if len(toolResultMsg.Content) != 1 || toolResultMsg.Content[0].ToolResult == nil {
		t.Fatalf("tool-result message Content = %+v", toolResultMsg.Content)
	}
	tr := toolResultMsg.Content[0].ToolResult
	if tr.ToolUseID != "tooluse_1" {
		t.Errorf("ToolResult.ToolUseID = %q, want %q", tr.ToolUseID, "tooluse_1")
	}
	if len(tr.Content) != 1 || tr.Content[0].Text != `{"temp_f":72}` {
		t.Errorf("ToolResult.Content = %+v", tr.Content)
	}
	if tr.Status != "success" {
		t.Errorf("ToolResult.Status = %q, want %q", tr.Status, "success")
	}
}

func TestToProviderInvalidToolArguments(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			{Role: "assistant", ToolCalls: []adapter.ToolCall{
				{ID: "tooluse_1", Name: "get_weather", ArgumentsJSON: "{not valid json"},
			}},
		},
	}

	_, err := New().ToProvider(req)
	if err == nil {
		t.Fatal("ToProvider: want error for invalid ArgumentsJSON, got nil")
	}
}

// TestFromProviderMalformedToolUseReturnsError proves the model's own
// broken tool-use machinery surfaces as a real, typed error rather than
// a fake successful Choice.
func TestFromProviderMalformedToolUseReturnsError(t *testing.T) {
	resp := &Response{StopReason: "malformed_tool_use"}

	_, err := New().FromProvider(resp)
	if err == nil {
		t.Fatal("FromProvider: want error for malformed_tool_use, got nil")
	}
}

func TestFromProviderStopWithoutToolUseMapsToStop(t *testing.T) {
	resp := &Response{
		Output:     Output{Message: Message{Role: "assistant", Content: []ContentBlock{{Text: "hello"}}}},
		StopReason: "end_turn",
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
	resp := &Response{StopReason: "max_tokens"}
	got, err := New().FromProvider(resp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if got.Choices[0].FinishReason != "length" {
		t.Errorf("FinishReason = %q, want %q", got.Choices[0].FinishReason, "length")
	}
}

func TestFromProviderGuardrailInterventionMapsToContentFilter(t *testing.T) {
	resp := &Response{StopReason: "guardrail_intervened"}
	got, err := New().FromProvider(resp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if got.Choices[0].FinishReason != "content_filter" {
		t.Errorf("FinishReason = %q, want %q", got.Choices[0].FinishReason, "content_filter")
	}
}

func TestFromProviderResponseIDIsEmptyNotFabricated(t *testing.T) {
	resp := &Response{
		Output:     Output{Message: Message{Role: "assistant", Content: []ContentBlock{{Text: "hi"}}}},
		StopReason: "end_turn",
	}
	got, err := New().FromProvider(resp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if got.ID != "" {
		t.Errorf("ID = %q, want empty", got.ID)
	}
	if got.Model != "" {
		t.Errorf("Model = %q, want empty", got.Model)
	}
}

func TestName(t *testing.T) {
	if got := New().Name(); got != "bedrock" {
		t.Errorf("Name() = %q, want %q", got, "bedrock")
	}
}

// TestToProviderMultiModalContentPartsMapToImageAndDocumentBlocks is
// the load-bearing proof for docs/rfcs/2026-09-06-gateway-multimodal-
// content.md: an inline-base64 image part must map to Converse's real
// {"image":{"format","source":{"bytes"}}} shape, and likewise for a
// document part.
func TestToProviderMultiModalContentPartsMapToImageAndDocumentBlocks(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			{
				Role:    "user",
				Content: "what's in this image and document?",
				Parts: []adapter.ContentPart{
					{Type: "image", MediaType: "image/png", Data: "aW1hZ2ViYXNlNjQ="},
					{Type: "document", MediaType: "application/pdf", Data: "ZG9jYmFzZTY0"},
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
	blocks := native.Messages[0].Content
	if len(blocks) != 3 {
		t.Fatalf("blocks len = %d, want 3 (text, image, document)", len(blocks))
	}

	if blocks[0].Text != "what's in this image and document?" {
		t.Errorf("blocks[0].Text = %q, want the message content", blocks[0].Text)
	}

	imgBlock := blocks[1]
	if imgBlock.Image == nil || imgBlock.Image.Format != "png" || imgBlock.Image.Source.Bytes != "aW1hZ2ViYXNlNjQ=" {
		t.Errorf("blocks[1].Image = %+v, want format=png and the image's Data as Source.Bytes", imgBlock.Image)
	}

	docBlock := blocks[2]
	if docBlock.Document == nil || docBlock.Document.Format != "pdf" || docBlock.Document.Source.Bytes != "ZG9jYmFzZTY0" {
		t.Errorf("blocks[2].Document = %+v, want format=pdf and the document's Data as Source.Bytes", docBlock.Document)
	}
	if docBlock.Document.Name == "" {
		t.Error("blocks[2].Document.Name is empty, want a non-empty placeholder (Converse requires a name)")
	}
}

// TestToProviderURLBasedContentPartFailsLoudly proves the real,
// deliberate scope limit: Converse has no generic-URL image/document
// source (only inline bytes or an s3Location), so a URL-based part must
// return a real, typed error rather than a silently wrong mapping.
func TestToProviderURLBasedContentPartFailsLoudly(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			{Role: "user", Parts: []adapter.ContentPart{{Type: "image", MediaType: "image/png", URL: "https://example.com/image.png"}}},
		},
	}

	if _, err := New().ToProvider(req); err == nil {
		t.Fatal("ToProvider with a URL-based image part returned nil error, want an error")
	}
}

// TestToProviderUnsupportedContentPartTypeFailsLoudly proves an unknown
// part type returns a real, typed error rather than being silently
// dropped.
func TestToProviderUnsupportedContentPartTypeFailsLoudly(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			{Role: "user", Parts: []adapter.ContentPart{{Type: "video", MediaType: "video/mp4", Data: "x"}}},
		},
	}

	if _, err := New().ToProvider(req); err == nil {
		t.Fatal("ToProvider with an unsupported content part type returned nil error, want an error")
	}
}

// TestToProviderSystemMessageCacheControlAppendsCachePointBlock is the
// load-bearing proof for docs/rfcs/2026-09-07-gateway-provider-prompt-
// caching.md's Bedrock wiring: two canonical system messages, only one
// with CacheControl set, must produce a system[] array where a
// standalone cachePoint block follows only the marked message's own
// text block -- not a property of that block itself (unlike
// Anthropic), and not present after the unmarked block.
func TestToProviderSystemMessageCacheControlAppendsCachePointBlock(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			{Role: "system", Content: "First system block."},
			{Role: "system", Content: "Second system block.", CacheControl: &adapter.CacheControl{}},
			{Role: "user", Content: "hi"},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if len(native.System) != 3 {
		t.Fatalf("native.System len = %d, want 3 (text, text, cachePoint)", len(native.System))
	}
	if native.System[0].Text != "First system block." || native.System[0].CachePoint != nil {
		t.Errorf("System[0] = %+v, want the first block with no cachePoint", native.System[0])
	}
	if native.System[1].Text != "Second system block." {
		t.Errorf("System[1].Text = %q, want %q", native.System[1].Text, "Second system block.")
	}
	if native.System[2].Text != "" || native.System[2].CachePoint == nil || native.System[2].CachePoint.Type != "default" {
		t.Errorf("System[2] = %+v, want a standalone cachePoint block", native.System[2])
	}
}

// TestToProviderMessageCacheControlAppendsTrailingCachePoint proves a
// message-level CacheControl appends a trailing cachePoint block after
// that message's own last content block.
func TestToProviderMessageCacheControlAppendsTrailingCachePoint(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			{Role: "user", Content: "cache this whole message", CacheControl: &adapter.CacheControl{}},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	blocks := native.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("blocks len = %d, want 2 (text, cachePoint)", len(blocks))
	}
	if blocks[0].Text != "cache this whole message" {
		t.Errorf("blocks[0].Text = %q, want the message content", blocks[0].Text)
	}
	if blocks[1].CachePoint == nil || blocks[1].CachePoint.Type != "default" {
		t.Errorf("blocks[1].CachePoint = %+v, want {default}", blocks[1].CachePoint)
	}
}

// TestToProviderContentPartCacheControlAppendsCachePointAfterThatPartOnly
// proves the part-level marker's independence from the message-level
// marker: among a text lead-in and two parts, only the part that
// carries its own CacheControl gets a trailing cachePoint immediately
// after it -- the second, unmarked part (this message's own last
// block) gets none, since no message-level marker is set here.
func TestToProviderContentPartCacheControlAppendsCachePointAfterThatPartOnly(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			{
				Role:    "user",
				Content: "see attached",
				Parts: []adapter.ContentPart{
					{Type: "document", MediaType: "application/pdf", Data: "ZG9j", CacheControl: &adapter.CacheControl{}},
					{Type: "image", MediaType: "image/png", Data: "aW1n"},
				},
			},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	blocks := native.Messages[0].Content
	// text, document, cachePoint, image -- exactly one cachePoint,
	// immediately after the marked document part.
	if len(blocks) != 4 {
		t.Fatalf("blocks len = %d, want 4 (text, document, cachePoint, image): %+v", len(blocks), blocks)
	}
	if blocks[1].Document == nil {
		t.Fatalf("blocks[1] = %+v, want the document part", blocks[1])
	}
	if blocks[2].CachePoint == nil || blocks[2].CachePoint.Type != "default" {
		t.Errorf("blocks[2] = %+v, want the standalone cachePoint block", blocks[2])
	}
	if blocks[3].Image == nil {
		t.Fatalf("blocks[3] = %+v, want the (unmarked) image part", blocks[3])
	}
}

// TestToProviderMessageCacheControlDoesNotDuplicateCachePoint proves
// that when a message's own last part already appended its own
// cachePoint block, a message-level CacheControl set at the same time
// never appends a second, redundant, empty-content checkpoint right
// after it.
func TestToProviderMessageCacheControlDoesNotDuplicateCachePoint(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			{
				Role:         "user",
				CacheControl: &adapter.CacheControl{},
				Parts: []adapter.ContentPart{
					{Type: "text", Text: "attached", CacheControl: &adapter.CacheControl{}},
				},
			},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	blocks := native.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("blocks len = %d, want exactly 2 (text, one cachePoint) -- not a duplicate second cachePoint: %+v", len(blocks), blocks)
	}
	if blocks[1].CachePoint == nil {
		t.Errorf("blocks[1] = %+v, want the single cachePoint block", blocks[1])
	}
}

// TestToProviderToolResultCacheControlAppendsCachePoint proves a
// message-level CacheControl on a role:"tool" message appends a
// trailing cachePoint block after the single toolResult block that
// message produces.
func TestToProviderToolResultCacheControlAppendsCachePoint(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			{Role: "tool", Content: `{"temp_f":72}`, ToolCallID: "tooluse_1", CacheControl: &adapter.CacheControl{}},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	blocks := native.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("blocks len = %d, want 2 (toolResult, cachePoint): %+v", len(blocks), blocks)
	}
	if blocks[0].ToolResult == nil {
		t.Fatalf("blocks[0] = %+v, want the toolResult block", blocks[0])
	}
	if blocks[1].CachePoint == nil || blocks[1].CachePoint.Type != "default" {
		t.Errorf("blocks[1] = %+v, want the standalone cachePoint block", blocks[1])
	}
}

// TestToProviderUnsetCacheControlIsNoOp proves the unset (nil, the
// default) case never emits any cachePoint field anywhere in the
// marshaled request, matching this schema's existing optional-field
// convention.
func TestToProviderUnsetCacheControlIsNoOp(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "hi", Parts: []adapter.ContentPart{
				{Type: "image", MediaType: "image/png", Data: "aW1n"},
			}},
		},
		Tools: []adapter.ToolDef{
			{Name: "get_weather", Description: "Get the weather", ParametersJSON: `{"type":"object"}`},
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
	if strings.Contains(string(b), "cachePoint") {
		t.Errorf("marshaled request contains a cachePoint field despite no CacheControl being set anywhere: %s", b)
	}
}

// TestToProviderToolDefCacheControlAppendsCachePointElement is the
// load-bearing proof for docs/rfcs/2026-09-07-gateway-provider-prompt-
// caching.md's tool-definition-level addendum, Bedrock section: among
// two canonical ToolDefs, only the second carrying CacheControl, the
// produced native tools[] array must contain a standalone
// {"cachePoint":{"type":"default"}} element immediately after that
// second tool's own {"toolSpec":{...}} element -- confirmed against
// AWS's real Tool union type (cachePoint is a sibling array element,
// never an inline property of a toolSpec object, unlike Anthropic's
// tool-level cache_control).
func TestToProviderToolDefCacheControlAppendsCachePointElement(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			{Role: "user", Content: "what's the weather and time?"},
		},
		Tools: []adapter.ToolDef{
			{Name: "get_weather", Description: "Get the weather", ParametersJSON: `{"type":"object"}`},
			{
				Name:           "get_time",
				Description:    "Get the time",
				ParametersJSON: `{"type":"object"}`,
				CacheControl:   &adapter.CacheControl{},
			},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native, ok := nativeAny.(*Request)
	if !ok {
		t.Fatalf("ToProvider returned %T, want *Request", nativeAny)
	}

	tools := native.ToolConfig.Tools
	if len(tools) != 3 {
		t.Fatalf("tools len = %d, want 3 (get_weather, get_time, cachePoint)", len(tools))
	}
	if tools[0].ToolSpec == nil || tools[0].ToolSpec.Name != "get_weather" || tools[0].CachePoint != nil {
		t.Errorf("tools[0] = %+v, want get_weather's toolSpec with no cachePoint", tools[0])
	}
	if tools[1].ToolSpec == nil || tools[1].ToolSpec.Name != "get_time" || tools[1].CachePoint != nil {
		t.Errorf("tools[1] = %+v, want get_time's toolSpec with no cachePoint of its own", tools[1])
	}
	if tools[2].ToolSpec != nil || tools[2].CachePoint == nil || tools[2].CachePoint.Type != "default" {
		t.Errorf("tools[2] = %+v, want a standalone {cachePoint:{default}} element with no toolSpec", tools[2])
	}
}

// TestToProviderToolDefAndMessageCacheControlBothApply proves the
// task-named interference case: a request carrying both a cached
// ToolDef AND a cached Message must produce correct, independent
// checkpoints for both in the same ToProvider output.
func TestToProviderToolDefAndMessageCacheControlBothApply(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful assistant.", CacheControl: &adapter.CacheControl{}},
			{Role: "user", Content: "what's the weather?", CacheControl: &adapter.CacheControl{}},
		},
		Tools: []adapter.ToolDef{
			{Name: "get_weather", Description: "Get the weather", ParametersJSON: `{"type":"object"}`, CacheControl: &adapter.CacheControl{}},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native, ok := nativeAny.(*Request)
	if !ok {
		t.Fatalf("ToProvider returned %T, want *Request", nativeAny)
	}

	tools := native.ToolConfig.Tools
	if len(tools) != 2 || tools[1].CachePoint == nil {
		t.Fatalf("tools = %+v, want [toolSpec, cachePoint]", tools)
	}
	if len(native.System) != 2 || native.System[1].CachePoint == nil {
		t.Fatalf("native.System = %+v, want [text, cachePoint]", native.System)
	}
	if len(native.Messages) != 1 {
		t.Fatalf("native.Messages len = %d, want 1", len(native.Messages))
	}
	blocks := native.Messages[0].Content
	if len(blocks) != 2 || blocks[1].CachePoint == nil {
		t.Fatalf("native.Messages[0].Content = %+v, want [text, cachePoint]", blocks)
	}
}
