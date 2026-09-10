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

// TestFromProviderIncludesCacheTokensInPromptAndTotal proves the
// cache-token cost-accounting fix: Anthropic's real cache_read_input_tokens
// /cache_creation_input_tokens response fields are read (not silently
// dropped), folded into PromptTokens/TotalTokens per Anthropic's own
// confirmed total_input_tokens formula, and surfaced separately via
// CacheReadTokens/CacheCreationTokens for cost-accounting to price.
func TestFromProviderIncludesCacheTokensInPromptAndTotal(t *testing.T) {
	a := New()
	nativeResp := &Response{
		ID:         "msg_cached",
		Model:      "claude-opus-4",
		Role:       "assistant",
		Content:    []ContentBlock{{Type: "text", Text: "hello"}},
		StopReason: "end_turn",
		Usage: Usage{
			InputTokens:              50,
			OutputTokens:             10,
			CacheReadInputTokens:     1800,
			CacheCreationInputTokens: 248,
		},
	}

	got, err := a.FromProvider(nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}

	wantPrompt := 50 + 1800 + 248
	wantTotal := wantPrompt + 10
	if got.Usage.PromptTokens != wantPrompt {
		t.Errorf("Usage.PromptTokens = %d, want %d (cache-inclusive)", got.Usage.PromptTokens, wantPrompt)
	}
	if got.Usage.TotalTokens != wantTotal {
		t.Errorf("Usage.TotalTokens = %d, want %d", got.Usage.TotalTokens, wantTotal)
	}
	if got.Usage.CacheReadTokens != 1800 {
		t.Errorf("Usage.CacheReadTokens = %d, want 1800", got.Usage.CacheReadTokens)
	}
	if got.Usage.CacheCreationTokens != 248 {
		t.Errorf("Usage.CacheCreationTokens = %d, want 248", got.Usage.CacheCreationTokens)
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
// per-message marking impossible. DisableCacheControlAutoPopulate is
// deliberately set here, per docs/rfcs/2026-09-07-gateway-cache-
// control-auto-populate.md: this test's own claim is about explicit,
// caller-supplied per-block marking, not the later auto-populate
// default, so the first (unmarked) block's "no cache_control" assertion
// stays isolated from that feature.
func TestToProviderSystemMessageCacheControlSetsPerBlockCacheControl(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{Role: "system", Content: "First system block."},
			{Role: "system", Content: "Second system block.", CacheControl: &adapter.CacheControl{TTL: "1h"}},
			{Role: "user", Content: "hi"},
		},
		DisableCacheControlAutoPopulate: true,
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
// DisableCacheControlAutoPopulate is deliberately set here, per
// docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md: without
// it, the request's own unmarked system message would now get the new
// default auto-populated marker, which is exactly the later feature this
// test predates and isn't about -- see
// TestToProviderSystemPromptAutoPopulatesCacheControlByDefault below for
// that proof.
func TestToProviderUnsetCacheControlIsNoOp(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "hi", Parts: []adapter.ContentPart{
				{Type: "image", MediaType: "image/png", Data: "aW1n"},
			}},
		},
		Tools: []adapter.ToolDef{
			{Name: "get_weather", Description: "Get the weather", ParametersJSON: `{"type":"object"}`},
		},
		DisableCacheControlAutoPopulate: true,
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

// TestToProviderToolDefCacheControlSetsInlineCacheControl is the
// load-bearing proof for docs/rfcs/2026-09-07-gateway-provider-prompt-
// caching.md's tool-definition-level addendum: among two canonical
// ToolDefs, only the second carrying CacheControl, the produced native
// Tool for that second definition must carry an inline cache_control
// sibling key -- mirroring ContentBlock's own inline placement, not a
// separate wrapper -- while the first, unmarked tool's native Tool
// carries none.
func TestToProviderToolDefCacheControlSetsInlineCacheControl(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{Role: "user", Content: "what's the weather and time?"},
		},
		Tools: []adapter.ToolDef{
			{Name: "get_weather", Description: "Get the weather", ParametersJSON: `{"type":"object"}`},
			{
				Name:           "get_time",
				Description:    "Get the time",
				ParametersJSON: `{"type":"object"}`,
				CacheControl:   &adapter.CacheControl{TTL: "1h"},
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

	if len(native.Tools) != 2 {
		t.Fatalf("native.Tools len = %d, want 2", len(native.Tools))
	}
	if native.Tools[0].CacheControl != nil {
		t.Errorf("Tools[0] (get_weather, unmarked) CacheControl = %+v, want nil", native.Tools[0].CacheControl)
	}
	if native.Tools[1].CacheControl == nil || native.Tools[1].CacheControl.Type != "ephemeral" || native.Tools[1].CacheControl.TTL != "1h" {
		t.Errorf("Tools[1] (get_time, marked) CacheControl = %+v, want {ephemeral 1h}", native.Tools[1].CacheControl)
	}
}

// TestToProviderSystemPromptAutoPopulatesCacheControlByDefault is the
// load-bearing proof for docs/rfcs/2026-09-07-gateway-cache-control-
// auto-populate.md's core policy: a system message with CacheControl
// left completely unset, on a deployment that has not opted out (the
// zero-value default), must still get a real cache_control marker --
// Kelvran's own default (Anthropic's own 5-minute TTL), not the
// caller-explicit-only behavior every deployment had before this RFC.
func TestToProviderSystemPromptAutoPopulatesCacheControlByDefault(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "hi"},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if len(native.System) != 1 {
		t.Fatalf("native.System len = %d, want 1", len(native.System))
	}
	cc := native.System[0].CacheControl
	if cc == nil || cc.Type != "ephemeral" || cc.TTL != "" {
		t.Errorf("System[0].CacheControl = %+v, want the auto-populated {ephemeral \"\"} default", cc)
	}
}

// TestToProviderExplicitSystemCacheControlNotOverriddenByAutoPopulate
// proves precedence rule (c): a caller-supplied, explicit CacheControl on
// a system message always wins outright over auto-populate's own default
// -- the caller's exact TTL survives untouched, never replaced.
func TestToProviderExplicitSystemCacheControlNotOverriddenByAutoPopulate(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
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

	cc := native.System[0].CacheControl
	if cc == nil || cc.TTL != "1h" {
		t.Errorf("System[0].CacheControl = %+v, want the caller's own explicit {ephemeral 1h}, not the auto-populate default", cc)
	}
}

// TestToProviderSystemPromptAutoPopulateSuppressedByDeploymentOptOut is
// the load-bearing proof for the per-deployment opt-out (b): with
// DisableCacheControlAutoPopulate set (as dataplane sets it from a
// deployment's own config field, per that RFC), an unmarked system
// message gets NO cache_control at all -- byte-identical to this whole
// feature not existing.
func TestToProviderSystemPromptAutoPopulateSuppressedByDeploymentOptOut(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "hi"},
		},
		DisableCacheControlAutoPopulate: true,
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if native.System[0].CacheControl != nil {
		t.Errorf("System[0].CacheControl = %+v, want nil -- the deployment opted out of auto-populate", native.System[0].CacheControl)
	}

	b, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("marshaling native request: %v", err)
	}
	if strings.Contains(string(b), "cache_control") {
		t.Errorf("marshaled request contains cache_control despite the deployment opting out of auto-populate: %s", b)
	}
}

// TestToProviderAutoPopulateNeverAppliesToNonSystemContent is the
// load-bearing proof for heuristic (a): auto-populate is scoped to
// system messages ONLY. A request with no CacheControl set anywhere, on
// a deployment that has NOT opted out, must still leave every
// user/assistant message, content part, and tool definition completely
// unmarked -- proving auto-populate doesn't quietly widen its own scope
// to "any large or first content," only the one structurally-guaranteed
// case the RFC names.
func TestToProviderAutoPopulateNeverAppliesToNonSystemContent(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{
				Role:    "user",
				Content: "see attached",
				Parts: []adapter.ContentPart{
					{Type: "document", MediaType: "application/pdf", Data: "ZG9j"},
				},
			},
			{Role: "assistant", Content: "sure, one moment"},
		},
		Tools: []adapter.ToolDef{
			{Name: "get_weather", Description: "Get the weather", ParametersJSON: `{"type":"object"}`},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	// The system message DOES get auto-populated (proven by the test
	// above) -- strip it out of the marshaled check below by asserting
	// on the non-system parts of the output directly instead.
	if len(native.Messages) != 2 {
		t.Fatalf("native.Messages len = %d, want 2 (user, assistant)", len(native.Messages))
	}
	for _, m := range native.Messages {
		for _, block := range m.Content {
			if block.CacheControl != nil {
				t.Errorf("role %q block %+v carries an auto-populated CacheControl, want nil -- auto-populate must never apply outside system messages", m.Role, block)
			}
		}
	}
	if len(native.Tools) != 1 || native.Tools[0].CacheControl != nil {
		t.Errorf("native.Tools = %+v, want the one tool def with no CacheControl", native.Tools)
	}
}

// TestToProviderToolDefAndMessageCacheControlBothApply proves the
// task-named interference case: a request carrying both a cached
// ToolDef AND a cached Message must produce correct, independent
// markers for both in the same ToProvider output, not just one or the
// other.
func TestToProviderToolDefAndMessageCacheControlBothApply(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "claude-opus-4",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful assistant.", CacheControl: &adapter.CacheControl{TTL: "1h"}},
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

	if len(native.Tools) != 1 || native.Tools[0].CacheControl == nil {
		t.Fatalf("native.Tools = %+v, want 1 tool with CacheControl set", native.Tools)
	}
	if len(native.System) != 1 || native.System[0].CacheControl == nil || native.System[0].CacheControl.TTL != "1h" {
		t.Fatalf("native.System = %+v, want 1 system block with CacheControl {ephemeral 1h}", native.System)
	}
	if len(native.Messages) != 1 || len(native.Messages[0].Content) != 1 || native.Messages[0].Content[0].CacheControl == nil {
		t.Fatalf("native.Messages = %+v, want 1 message with a cache_control-marked last block", native.Messages)
	}
}

// TestToProviderResponseFormatMapsToOutputConfig proves a canonical
// ResponseFormat/JSONSchema maps onto Anthropic's real top-level
// output_config.format.{type,schema} shape -- confirmed live-verified:
// no beta header required, and (unlike OpenAI) no "name" field exists on
// Anthropic's own real wire shape at all.
func TestToProviderResponseFormatMapsToOutputConfig(t *testing.T) {
	req := adapter.ChatRequest{
		Model:    "claude-opus-4",
		Messages: []adapter.Message{{Role: "user", Content: "give me JSON"}},
		ResponseFormat: &adapter.ResponseFormat{
			Type: "json_schema",
			JSONSchema: &adapter.JSONSchema{
				Name:   "weather_response",
				Schema: json.RawMessage(`{"type":"object","properties":{"temp_f":{"type":"number"}},"required":["temp_f"]}`),
			},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if native.OutputConfig == nil || native.OutputConfig.Format == nil {
		t.Fatal("native.OutputConfig.Format = nil, want a populated OutputFormat")
	}
	if native.OutputConfig.Format.Type != "json_schema" {
		t.Errorf("OutputConfig.Format.Type = %q, want json_schema", native.OutputConfig.Format.Type)
	}
	if native.OutputConfig.Format.Schema["type"] != "object" {
		t.Errorf("OutputConfig.Format.Schema = %v, want the parsed schema object", native.OutputConfig.Format.Schema)
	}

	b, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("marshaling native request: %v", err)
	}
	var wireTree map[string]any
	if err := json.Unmarshal(b, &wireTree); err != nil {
		t.Fatalf("unmarshaling marshaled request: %v", err)
	}
	oc, ok := wireTree["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("marshaled request has no output_config object: %s", b)
	}
	format, ok := oc["format"].(map[string]any)
	if !ok {
		t.Fatalf("output_config has no format object: %v", oc)
	}
	if format["type"] != "json_schema" {
		t.Errorf("output_config.format.type = %v, want json_schema", format["type"])
	}
	if _, hasName := format["name"]; hasName {
		t.Errorf("output_config.format carries a name field, want none -- Anthropic's real shape has no name key")
	}
}

// TestToProviderNilResponseFormatOmitsOutputConfig proves the unset (nil,
// the default) case never emits output_config at all -- byte-identical
// to every ChatRequest built before this field existed.
func TestToProviderNilResponseFormatOmitsOutputConfig(t *testing.T) {
	req := adapter.ChatRequest{
		Model:    "claude-opus-4",
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)
	if native.OutputConfig != nil {
		t.Errorf("native.OutputConfig = %+v, want nil", native.OutputConfig)
	}

	b, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("marshaling native request: %v", err)
	}
	if strings.Contains(string(b), "output_config") {
		t.Errorf("marshaled request contains output_config despite ResponseFormat being nil: %s", b)
	}
}

// TestToProviderToolStrictMapsToWireStrictTrueOnly proves ToolDef.Strict
// maps onto Anthropic's real per-tool "strict" sibling key, and that an
// unset (false) ToolDef.Strict never emits a spurious "strict":false --
// only an explicit true is ever placed on the wire.
func TestToProviderToolStrictMapsToWireStrictTrueOnly(t *testing.T) {
	req := adapter.ChatRequest{
		Model:    "claude-opus-4",
		Messages: []adapter.Message{{Role: "user", Content: "what's the weather?"}},
		Tools: []adapter.ToolDef{
			{Name: "get_weather", ParametersJSON: `{"type":"object"}`, Strict: true},
			{Name: "get_time", ParametersJSON: `{"type":"object"}`},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)
	if len(native.Tools) != 2 {
		t.Fatalf("native.Tools len = %d, want 2", len(native.Tools))
	}
	if native.Tools[0].Strict == nil || !*native.Tools[0].Strict {
		t.Errorf("Tools[0] (get_weather, Strict:true) native Strict = %v, want a pointer to true", native.Tools[0].Strict)
	}
	if native.Tools[1].Strict != nil {
		t.Errorf("Tools[1] (get_time, Strict unset) native Strict = %v, want nil", native.Tools[1].Strict)
	}

	b, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("marshaling native request: %v", err)
	}
	if strings.Contains(string(b), `"strict":false`) {
		t.Errorf("marshaled request contains a spurious \"strict\":false: %s", b)
	}
	if !strings.Contains(string(b), `"strict":true`) {
		t.Errorf("marshaled request is missing the expected \"strict\":true: %s", b)
	}
}
