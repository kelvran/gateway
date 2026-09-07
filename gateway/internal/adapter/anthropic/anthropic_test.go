package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// TestRoundTrip proves both documented hazards this adapter exists to
// handle: system-prompt placement and tool-call argument re-encoding.
// A request with a system message AND a tool call must survive
// ToProvider -> FromProvider with the system content preserved (pulled
// into the native System field, never left inside Messages) and the
// tool-call arguments still valid, semantically-equal JSON on the way
// back through FromProvider.
func TestRoundTrip(t *testing.T) {
	original := adapter.ChatRequest{
		Model: "claude-opus-4",
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

	// Hazard 1: system-prompt placement. The system message must be
	// pulled out of Messages into the top-level System field.
	if len(native.System) != 1 || native.System[0].Text != "You are a helpful weather assistant." {
		t.Errorf("native.System = %+v, want one block with the system message content", native.System)
	}
	for _, m := range native.Messages {
		if m.Role == "system" {
			t.Errorf("native.Messages contains a role:system message; it must be pulled into System")
		}
	}
	if len(native.Messages) != 1 || native.Messages[0].Role != "user" {
		t.Fatalf("native.Messages = %+v, want exactly one user message", native.Messages)
	}

	// Simulate the model responding with a tool call, using the tool
	// definition that survived ToProvider, to exercise Hazard 2 on the
	// way back through FromProvider.
	toolCallArgs := `{"city":"Boston"}`
	var parsedArgs map[string]any
	if err := json.Unmarshal([]byte(toolCallArgs), &parsedArgs); err != nil {
		t.Fatalf("test setup: %v", err)
	}

	nativeResp := &Response{
		ID:    "msg_test",
		Model: native.Model,
		Role:  "assistant",
		Content: []ContentBlock{
			{Type: "tool_use", ID: "toolu_1", Name: "get_weather", Input: parsedArgs},
		},
		StopReason: "tool_use",
		Usage:      Usage{InputTokens: 20, OutputTokens: 8},
	}

	got, err := a.FromProvider(nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}

	if len(got.Choices) != 1 {
		t.Fatalf("Choices len = %d, want 1", len(got.Choices))
	}
	gotToolCalls := got.Choices[0].Message.ToolCalls
	if len(gotToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(gotToolCalls))
	}

	// Hazard 2: tool-call arguments must be valid JSON, semantically
	// equal to the original (already-parsed-object) input.
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

// TestToProviderToolResultMessage covers the canonical role:"tool" ->
// native role:"user"/tool_result-block translation this adapter also
// performs, since Anthropic has no native "tool" role.
func TestToProviderToolResultMessage(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{Role: "user", Content: "call the tool"},
			{Role: "assistant", ToolCalls: []adapter.ToolCall{
				{ID: "toolu_1", Name: "get_weather", ArgumentsJSON: `{"city":"Boston"}`},
			}},
			{Role: "tool", Content: `{"temp_f":72}`, ToolCallID: "toolu_1"},
		},
	}

	a := New()
	nativeAny, err := a.ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if len(native.Messages) != 3 {
		t.Fatalf("native.Messages len = %d, want 3", len(native.Messages))
	}
	toolResultMsg := native.Messages[2]
	if toolResultMsg.Role != "user" {
		t.Errorf("tool-result message Role = %q, want %q", toolResultMsg.Role, "user")
	}
	if len(toolResultMsg.Content) != 1 || toolResultMsg.Content[0].Type != "tool_result" {
		t.Fatalf("tool-result message Content = %+v", toolResultMsg.Content)
	}
	if toolResultMsg.Content[0].ToolUseID != "toolu_1" {
		t.Errorf("ToolUseID = %q, want %q", toolResultMsg.Content[0].ToolUseID, "toolu_1")
	}
}

func TestName(t *testing.T) {
	if got := New().Name(); got != "anthropic" {
		t.Errorf("Name() = %q, want %q", got, "anthropic")
	}
}

// TestToProviderMultiModalContentPartsMapToNativeBlocks is the
// load-bearing proof for docs/rfcs/2026-09-06-gateway-multimodal-
// content.md: a canonical message with an inline-base64 image part and
// a URL-referenced document part must map to Anthropic's real
// {"type":"image"/"document","source":{...}} block shape, alongside the
// existing text block.
func TestToProviderMultiModalContentPartsMapToNativeBlocks(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
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

	if len(native.Messages) != 1 {
		t.Fatalf("native.Messages len = %d, want 1", len(native.Messages))
	}
	blocks := native.Messages[0].Content
	if len(blocks) != 3 {
		t.Fatalf("blocks len = %d, want 3 (text, image, document)", len(blocks))
	}

	if blocks[0].Type != "text" || blocks[0].Text != "what's in this image and document?" {
		t.Errorf("blocks[0] = %+v, want the text block", blocks[0])
	}

	imgBlock := blocks[1]
	if imgBlock.Type != "image" {
		t.Fatalf("blocks[1].Type = %q, want %q", imgBlock.Type, "image")
	}
	if imgBlock.Source == nil || imgBlock.Source.Type != "base64" || imgBlock.Source.MediaType != "image/png" || imgBlock.Source.Data != "aW1hZ2ViYXNlNjQ=" {
		t.Errorf("blocks[1].Source = %+v, want a base64 source with the image's MediaType/Data", imgBlock.Source)
	}

	docBlock := blocks[2]
	if docBlock.Type != "document" {
		t.Fatalf("blocks[2].Type = %q, want %q", docBlock.Type, "document")
	}
	if docBlock.Source == nil || docBlock.Source.Type != "url" || docBlock.Source.URL != "https://example.com/doc.pdf" {
		t.Errorf("blocks[2].Source = %+v, want a url source with the document's URL", docBlock.Source)
	}
}

// TestToProviderContentPartWithNeitherDataNorURLFailsLoudly proves a
// malformed image/document part (missing both Data and URL) returns a
// real, typed error rather than silently producing an empty/invalid
// source block.
func TestToProviderContentPartWithNeitherDataNorURLFailsLoudly(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
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
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{Role: "user", Parts: []adapter.ContentPart{{Type: "video", MediaType: "video/mp4", Data: "x"}}},
		},
	}

	if _, err := New().ToProvider(req); err == nil {
		t.Fatal("ToProvider with an unsupported content part type returned nil error, want an error")
	}
}

// TestToProviderSystemMessageCacheControlSetsPerBlockCacheControl is the
// load-bearing proof for docs/rfcs/2026-09-07-gateway-provider-prompt-
// caching.md's restructuring claim: two canonical system messages, only
// one with CacheControl set, must become two independent SystemBlocks
// where exactly one carries cache_control -- proving System is no
// longer flattened into a single joined string that would have made
// per-message marking impossible.
func TestToProviderSystemMessageCacheControlSetsPerBlockCacheControl(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{Role: "system", Content: "First system block."},
			{Role: "system", Content: "Second system block.", CacheControl: &adapter.CacheControl{TTL: "1h"}},
			{Role: "user", Content: "hi"},
		},
	}

	native := New()
	nativeAny, err := native.ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	reqNative := nativeAny.(*Request)

	if len(reqNative.System) != 2 {
		t.Fatalf("native.System len = %d, want 2 independent blocks", len(reqNative.System))
	}
	if reqNative.System[0].Text != "First system block." || reqNative.System[0].CacheControl != nil {
		t.Errorf("System[0] = %+v, want the first block with no cache_control", reqNative.System[0])
	}
	if reqNative.System[1].Text != "Second system block." {
		t.Errorf("System[1].Text = %q, want %q", reqNative.System[1].Text, "Second system block.")
	}
	if reqNative.System[1].CacheControl == nil || reqNative.System[1].CacheControl.Type != "ephemeral" || reqNative.System[1].CacheControl.TTL != "1h" {
		t.Errorf("System[1].CacheControl = %+v, want {ephemeral 1h}", reqNative.System[1].CacheControl)
	}
}

// TestToProviderMessageCacheControlAttachesToLastBlock proves a
// message-level CacheControl marks "cache everything through this
// message" by attaching to that message's own last generated block.
func TestToProviderMessageCacheControlAttachesToLastBlock(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{Role: "user", Content: "cache this whole message", CacheControl: &adapter.CacheControl{}},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if len(native.Messages) != 1 || len(native.Messages[0].Content) != 1 {
		t.Fatalf("native.Messages = %+v, want one message with one block", native.Messages)
	}
	block := native.Messages[0].Content[0]
	if block.CacheControl == nil || block.CacheControl.Type != "ephemeral" || block.CacheControl.TTL != "" {
		t.Errorf("block.CacheControl = %+v, want {ephemeral \"\"} (Anthropic's own 5-minute default)", block.CacheControl)
	}
}

// TestToProviderContentPartCacheControlAttachesToThatPartOnly proves the
// part-level marker's independence from the message-level marker: among
// a text-lead-in block and two content-part blocks, only the part that
// actually carries its own CacheControl gets one -- neither the
// unmarked lead-in text block nor the unmarked second part does, even
// though the second part is this message's own last block (so a
// message-level marker, which is deliberately NOT set here, would have
// landed there instead).
func TestToProviderContentPartCacheControlAttachesToThatPartOnly(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{
				Role:    "user",
				Content: "see attached",
				Parts: []adapter.ContentPart{
					{Type: "document", MediaType: "application/pdf", Data: "ZG9j", CacheControl: &adapter.CacheControl{TTL: "1h"}},
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
	if len(blocks) != 3 {
		t.Fatalf("blocks len = %d, want 3 (text, document, image)", len(blocks))
	}
	if blocks[0].CacheControl != nil {
		t.Errorf("blocks[0] (text lead-in) CacheControl = %+v, want nil", blocks[0].CacheControl)
	}
	if blocks[1].CacheControl == nil || blocks[1].CacheControl.TTL != "1h" {
		t.Errorf("blocks[1] (document part) CacheControl = %+v, want {ephemeral 1h}", blocks[1].CacheControl)
	}
	if blocks[2].CacheControl != nil {
		t.Errorf("blocks[2] (image part, unmarked, and this message's own last block) CacheControl = %+v, want nil -- no message-level marker was set", blocks[2].CacheControl)
	}
}

// TestToProviderMessageCacheControlDoesNotOverwritePartLevelMarker
// proves that when a message's own last block already carries a more
// specific part-level CacheControl, a message-level CacheControl set at
// the same time never overwrites it -- the part's own TTL must survive
// untouched.
func TestToProviderMessageCacheControlDoesNotOverwritePartLevelMarker(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{
				Role:         "user",
				CacheControl: &adapter.CacheControl{TTL: "message-level-should-not-win"},
				Parts: []adapter.ContentPart{
					{Type: "text", Text: "attached", CacheControl: &adapter.CacheControl{TTL: "1h"}},
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
	if len(blocks) != 1 {
		t.Fatalf("blocks len = %d, want 1", len(blocks))
	}
	if blocks[0].CacheControl == nil || blocks[0].CacheControl.TTL != "1h" {
		t.Errorf("blocks[0].CacheControl = %+v, want the part's own {ephemeral 1h}, not the message-level marker", blocks[0].CacheControl)
	}
}

// TestToProviderToolResultCacheControlAttachesToBlock proves a
// message-level CacheControl on a role:"tool" message attaches to the
// single tool_result block that message produces.
func TestToProviderToolResultCacheControlAttachesToBlock(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{Role: "user", Content: "call the tool"},
			{Role: "assistant", ToolCalls: []adapter.ToolCall{
				{ID: "toolu_1", Name: "get_weather", ArgumentsJSON: `{"city":"Boston"}`},
			}},
			{Role: "tool", Content: `{"temp_f":72}`, ToolCallID: "toolu_1", CacheControl: &adapter.CacheControl{}},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	toolResultMsg := native.Messages[2]
	if len(toolResultMsg.Content) != 1 || toolResultMsg.Content[0].Type != "tool_result" {
		t.Fatalf("tool-result message Content = %+v", toolResultMsg.Content)
	}
	if toolResultMsg.Content[0].CacheControl == nil || toolResultMsg.Content[0].CacheControl.Type != "ephemeral" {
		t.Errorf("tool_result block CacheControl = %+v, want {ephemeral \"\"}", toolResultMsg.Content[0].CacheControl)
	}
}

// TestToProviderUnsetCacheControlIsNoOp proves the unset (nil, the
// default) case never emits any cache_control field anywhere in the
// marshaled request -- matching this schema's existing optional-field
// convention (ContentPart/Parts's own additive, no-op-when-empty
// precedent) rather than emitting a zero-value cache_control block.
func TestToProviderUnsetCacheControlIsNoOp(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "hi", Parts: []adapter.ContentPart{
				{Type: "image", MediaType: "image/png", Data: "aW1n"},
			}},
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
	if strings.Contains(string(b), "cache_control") {
		t.Errorf("marshaled request contains a cache_control field despite no CacheControl being set anywhere: %s", b)
	}
}
