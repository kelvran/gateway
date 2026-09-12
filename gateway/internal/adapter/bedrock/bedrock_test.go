package bedrock

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// TestRoundTrip proves the two hazards shared with anthropic/gemini:
// system-prompt placement and tool-call argument re-encoding.
// DisableCacheControlAutoPopulate is deliberately set here, per
// docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md: this
// test's own assertions (len(native.System) != 1) are about hazard 1/2,
// not caching, so auto-populate's own trailing cachePoint element (which
// would otherwise make len(native.System) == 2) is turned off to keep
// this test's original, still-valid claim isolated from that later
// feature.
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
		DisableCacheControlAutoPopulate: true,
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

// TestFromProviderIncludesCacheTokensInPromptAndTotal proves the
// cache-token cost-accounting fix: Bedrock's real cacheReadInputTokens/
// cacheWriteInputTokens response fields are read (not silently dropped)
// and folded into PromptTokens/TotalTokens -- Converse's own totalTokens
// is documented as inputTokens+outputTokens ONLY, so cache tokens must be
// added on top here, unlike Anthropic's native totalTokens which already
// includes them.
func TestFromProviderIncludesCacheTokensInPromptAndTotal(t *testing.T) {
	a := New()
	nativeResp := &Response{
		Output: Output{
			Message: Message{
				Role:    "assistant",
				Content: []ContentBlock{{Text: "hello"}},
			},
		},
		StopReason: "end_turn",
		Usage: Usage{
			InputTokens:           8,
			OutputTokens:          10,
			TotalTokens:           18,
			CacheReadInputTokens:  0,
			CacheWriteInputTokens: 5120,
		},
	}

	got, err := a.FromProvider(nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}

	wantPrompt := 8 + 5120
	wantTotal := 18 + 5120
	if got.Usage.PromptTokens != wantPrompt {
		t.Errorf("Usage.PromptTokens = %d, want %d (cache-inclusive)", got.Usage.PromptTokens, wantPrompt)
	}
	if got.Usage.TotalTokens != wantTotal {
		t.Errorf("Usage.TotalTokens = %d, want %d", got.Usage.TotalTokens, wantTotal)
	}
	if got.Usage.CacheReadTokens != 0 {
		t.Errorf("Usage.CacheReadTokens = %d, want 0", got.Usage.CacheReadTokens)
	}
	if got.Usage.CacheCreationTokens != 5120 {
		t.Errorf("Usage.CacheCreationTokens = %d, want 5120", got.Usage.CacheCreationTokens)
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

// TestFromProviderCapturesReasoningAndRedactedReasoningBlocks proves the
// live-breaking bug fixed by docs/rfcs/2026-09-12-gateway-reasoning-
// content-canonical-schema.md's Phase 3: before this fix, ContentBlock
// had no field for a reasoningContent block, so FromProvider's switch
// matched neither the ToolUse nor Text case and silently dropped the
// entire block. A response with a reasoning block followed by a toolUse
// block, followed by a second (redacted) reasoning block, must now
// surface all of it as ReasoningBlocks with Sequence values that
// correctly record each block's position relative to ToolCalls.
func TestFromProviderCapturesReasoningAndRedactedReasoningBlocks(t *testing.T) {
	a := New()
	nativeResp := &Response{
		Output: Output{
			Message: Message{
				Role: "assistant",
				Content: []ContentBlock{
					{ReasoningContent: &ReasoningContentBlock{ReasoningText: &ReasoningTextBlock{
						Text: "Let me check the weather API first.", Signature: "sig_abc123",
					}}},
					{ToolUse: &ToolUse{ToolUseID: "tooluse_1", Name: "get_weather", Input: map[string]any{"city": "Boston"}}},
					{ReasoningContent: &ReasoningContentBlock{RedactedContent: "opaque_ciphertext_xyz"}},
				},
			},
		},
		StopReason: "tool_use",
		Usage:      Usage{InputTokens: 30, OutputTokens: 12, TotalTokens: 42},
	}

	got, err := a.FromProvider(nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("Choices len = %d, want 1", len(got.Choices))
	}
	msg := got.Choices[0].Message

	if len(msg.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1 (reasoning blocks must not disturb tool-call capture)", len(msg.ToolCalls))
	}
	if len(msg.ReasoningBlocks) != 2 {
		t.Fatalf("ReasoningBlocks len = %d, want 2 -- the block(s) were silently dropped", len(msg.ReasoningBlocks))
	}

	first, second := msg.ReasoningBlocks[0], msg.ReasoningBlocks[1]
	if first.Redacted || first.Text != "Let me check the weather API first." || first.Signature != "sig_abc123" {
		t.Errorf("ReasoningBlocks[0] = %+v, want plaintext reasoning block with the original text+signature", first)
	}
	if first.Sequence != 0 {
		t.Errorf("ReasoningBlocks[0].Sequence = %d, want 0 (appears before ToolCalls[0])", first.Sequence)
	}
	if !second.Redacted || second.Data != "opaque_ciphertext_xyz" || second.Text != "" {
		t.Errorf("ReasoningBlocks[1] = %+v, want a redacted block carrying only the opaque Data payload", second)
	}
	if second.Sequence != 1 {
		t.Errorf("ReasoningBlocks[1].Sequence = %d, want 1 (appears after ToolCalls[0], the only tool call)", second.Sequence)
	}
}

// blockKindOf classifies one native Bedrock ContentBlock for the
// interleave-order assertions below, since Converse's ContentBlock is a
// union with no repeated "type" discriminator field of its own.
func blockKindOf(b ContentBlock) string {
	switch {
	case b.ReasoningContent != nil:
		return "reasoningContent"
	case b.ToolUse != nil:
		return "toolUse"
	case b.Text != "":
		return "text"
	default:
		return "other"
	}
}

// TestToProviderReplaysReasoningBlocksInOriginalInterleavedOrder proves
// the request-side half of the Phase 3 fix: a canonical Message
// carrying ReasoningBlocks (as a caller would echo back from a prior
// FromProvider response, per the RFC's replay contract) must be
// serialized with the reasoning blocks placed back at their exact
// original position relative to ToolCalls -- not bunched before or
// after every tool call.
func TestToProviderReplaysReasoningBlocksInOriginalInterleavedOrder(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-opus-4-6",
		Messages: []adapter.Message{
			{Role: "user", Content: "What's the weather in Boston and Tokyo?"},
			{
				Role: "assistant",
				ReasoningBlocks: []adapter.ReasoningBlock{
					{Sequence: 0, Text: "First I'll check Boston.", Signature: "sig_1"},
					{Sequence: 1, Redacted: true, Data: "opaque_between_calls"},
					{Sequence: 2, Text: "Both results are in, I can answer now.", Signature: "sig_3"},
				},
				ToolCalls: []adapter.ToolCall{
					{ID: "tooluse_1", Name: "get_weather", ArgumentsJSON: `{"city":"Boston"}`},
					{ID: "tooluse_2", Name: "get_weather", ArgumentsJSON: `{"city":"Tokyo"}`},
				},
			},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)
	if len(native.Messages) != 2 {
		t.Fatalf("native.Messages len = %d, want 2", len(native.Messages))
	}

	blocks := native.Messages[1].Content
	gotKinds := make([]string, len(blocks))
	for i, b := range blocks {
		gotKinds[i] = blockKindOf(b)
	}
	wantKinds := []string{"reasoningContent", "toolUse", "reasoningContent", "toolUse", "reasoningContent"}
	if len(gotKinds) != len(wantKinds) {
		t.Fatalf("assistant message block kinds = %v, want %v", gotKinds, wantKinds)
	}
	for i, want := range wantKinds {
		if gotKinds[i] != want {
			t.Errorf("block[%d] kind = %q, want %q (full sequence: %v)", i, gotKinds[i], want, gotKinds)
		}
	}

	if blocks[0].ReasoningContent.ReasoningText == nil ||
		blocks[0].ReasoningContent.ReasoningText.Text != "First I'll check Boston." ||
		blocks[0].ReasoningContent.ReasoningText.Signature != "sig_1" {
		t.Errorf("blocks[0] = %+v, want the first plaintext reasoning block replayed verbatim", blocks[0])
	}
	if blocks[2].ReasoningContent.RedactedContent != "opaque_between_calls" {
		t.Errorf("blocks[2] = %+v, want the redacted block's opaque content replayed verbatim", blocks[2])
	}
	if blocks[4].ReasoningContent.ReasoningText == nil ||
		blocks[4].ReasoningContent.ReasoningText.Text != "Both results are in, I can answer now." ||
		blocks[4].ReasoningContent.ReasoningText.Signature != "sig_3" {
		t.Errorf("blocks[4] = %+v, want the trailing reasoning block replayed verbatim after the last tool call", blocks[4])
	}
}

// TestReasoningBlockRoundTripPreservesExactOriginalOrder is an
// end-to-end proof: capture an interleaved reasoning+toolUse response
// via FromProvider, then feed the resulting canonical Message straight
// back through ToProvider as conversation history (exactly what a real
// caller does on the next turn) and assert the re-serialized block
// order is byte-for-byte identical in kind to what Converse originally
// sent.
func TestReasoningBlockRoundTripPreservesExactOriginalOrder(t *testing.T) {
	a := New()
	originalContent := []ContentBlock{
		{ReasoningContent: &ReasoningContentBlock{ReasoningText: &ReasoningTextBlock{Text: "Step one.", Signature: "sig_a"}}},
		{ToolUse: &ToolUse{ToolUseID: "tooluse_1", Name: "step_one", Input: map[string]any{}}},
		{ReasoningContent: &ReasoningContentBlock{ReasoningText: &ReasoningTextBlock{Text: "Step two.", Signature: "sig_b"}}},
		{ToolUse: &ToolUse{ToolUseID: "tooluse_2", Name: "step_two", Input: map[string]any{}}},
	}
	nativeResp := &Response{
		Output:     Output{Message: Message{Role: "assistant", Content: originalContent}},
		StopReason: "tool_use",
	}

	got, err := a.FromProvider(nativeResp)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	assistantMsg := got.Choices[0].Message
	assistantMsg.Role = "assistant" // FromProvider already sets this; explicit for clarity

	replayed, err := a.ToProvider(adapter.ChatRequest{
		Model:    "anthropic.claude-opus-4-6",
		Messages: []adapter.Message{{Role: "user", Content: "go"}, assistantMsg},
	})
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := replayed.(*Request)
	replayedContent := native.Messages[1].Content

	if len(replayedContent) != len(originalContent) {
		t.Fatalf("replayed block count = %d, want %d", len(replayedContent), len(originalContent))
	}
	for i := range originalContent {
		if blockKindOf(replayedContent[i]) != blockKindOf(originalContent[i]) {
			t.Errorf("block[%d] kind = %q, want %q -- reasoning/toolUse order was not preserved through a full FromProvider->ToProvider round trip", i, blockKindOf(replayedContent[i]), blockKindOf(originalContent[i]))
		}
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
// DisableCacheControlAutoPopulate is deliberately set here, per
// docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md: this
// test's own claim is about explicit, caller-supplied per-message
// checkpoints, not the later auto-populate default, so the first
// (unmarked) block's "no cachePoint" assertion stays isolated from that
// feature.
func TestToProviderSystemMessageCacheControlAppendsCachePointBlock(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			{Role: "system", Content: "First system block."},
			{Role: "system", Content: "Second system block.", CacheControl: &adapter.CacheControl{}},
			{Role: "user", Content: "hi"},
		},
		DisableCacheControlAutoPopulate: true,
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
// convention. DisableCacheControlAutoPopulate is deliberately set here,
// per docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md:
// without it, the request's own unmarked system message would now get
// the new default auto-populated marker -- see
// TestToProviderSystemPromptAutoPopulatesCachePointByDefault below for
// that proof.
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

// TestToProviderSystemPromptAutoPopulatesCachePointByDefault is the
// load-bearing proof for docs/rfcs/2026-09-07-gateway-cache-control-
// auto-populate.md's core policy: a system message with CacheControl
// left completely unset, on a deployment that has not opted out (the
// zero-value default), must still get a real, standalone cachePoint
// checkpoint block -- not the caller-explicit-only behavior every
// deployment had before this RFC.
func TestToProviderSystemPromptAutoPopulatesCachePointByDefault(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
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

	if len(native.System) != 2 {
		t.Fatalf("native.System len = %d, want 2 (text, cachePoint)", len(native.System))
	}
	if native.System[0].Text != "You are a helpful assistant." || native.System[0].CachePoint != nil {
		t.Errorf("System[0] = %+v, want the text block with no cachePoint of its own", native.System[0])
	}
	if native.System[1].CachePoint == nil || native.System[1].CachePoint.Type != "default" {
		t.Errorf("System[1] = %+v, want the auto-populated standalone cachePoint block", native.System[1])
	}
}

// TestToProviderExplicitSystemCacheControlNotOverriddenByAutoPopulate
// proves precedence rule (c): a caller-supplied, explicit CacheControl on
// a system message still produces the checkpoint (auto-populate never
// suppresses or double-appends over an explicit marker's own effect).
func TestToProviderExplicitSystemCacheControlNotOverriddenByAutoPopulate(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{
			{Role: "system", Content: "You are a helpful assistant.", CacheControl: &adapter.CacheControl{}},
			{Role: "user", Content: "hi"},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if len(native.System) != 2 || native.System[1].CachePoint == nil {
		t.Fatalf("native.System = %+v, want exactly one cachePoint following the explicitly-marked text block", native.System)
	}
}

// TestToProviderSystemPromptAutoPopulateSuppressedByDeploymentOptOut is
// the load-bearing proof for the per-deployment opt-out (b): with
// DisableCacheControlAutoPopulate set (as dataplane sets it from a
// deployment's own config field, per that RFC), an unmarked system
// message gets NO cachePoint at all -- byte-identical to this whole
// feature not existing.
func TestToProviderSystemPromptAutoPopulateSuppressedByDeploymentOptOut(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
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

	if len(native.System) != 1 {
		t.Fatalf("native.System len = %d, want 1 (text only, no cachePoint) -- the deployment opted out of auto-populate", len(native.System))
	}

	b, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("marshaling native request: %v", err)
	}
	if strings.Contains(string(b), "cachePoint") {
		t.Errorf("marshaled request contains cachePoint despite the deployment opting out of auto-populate: %s", b)
	}
}

// TestToProviderAutoPopulateNeverAppliesToNonSystemContent is the
// load-bearing proof for heuristic (a): auto-populate is scoped to
// system messages ONLY. A request with no CacheControl set anywhere, on
// a deployment that has NOT opted out, must still leave every
// user/assistant message, content part, and tool definition completely
// unmarked.
func TestToProviderAutoPopulateNeverAppliesToNonSystemContent(t *testing.T) {
	req := adapter.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
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

	// The system message DOES get an auto-populated cachePoint (proven
	// above) -- check only the non-system parts of the output here.
	if len(native.Messages) != 2 {
		t.Fatalf("native.Messages len = %d, want 2 (user, assistant)", len(native.Messages))
	}
	for _, m := range native.Messages {
		for _, block := range m.Content {
			if block.CachePoint != nil {
				t.Errorf("role %q carries an auto-populated cachePoint block, want none -- auto-populate must never apply outside system messages", m.Role)
			}
		}
	}
	if native.ToolConfig == nil || len(native.ToolConfig.Tools) != 1 || native.ToolConfig.Tools[0].CachePoint != nil {
		t.Errorf("native.ToolConfig.Tools = %+v, want the one tool def with no cachePoint", native.ToolConfig)
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

// TestToProviderResponseFormatSetsAdditionalModelRequestFieldsOnWhitelistedModel
// proves a canonical ResponseFormat/JSONSchema maps onto Converse's real
// additionalModelRequestFields.output_config.format.{type,schema} escape
// hatch, but ONLY when req.Model is on
// adapter.SupportsStructuredOutput's Bedrock whitelist.
func TestToProviderResponseFormatSetsAdditionalModelRequestFieldsOnWhitelistedModel(t *testing.T) {
	req := adapter.ChatRequest{
		Model:    "global.anthropic.claude-haiku-4-5-20251001-v1:0",
		Messages: []adapter.Message{{Role: "user", Content: "give me JSON"}},
		ResponseFormat: &adapter.ResponseFormat{
			Type: "json_schema",
			JSONSchema: &adapter.JSONSchema{
				Name:   "weather_response",
				Schema: json.RawMessage(`{"type":"object","properties":{"temp_f":{"type":"number"}}}`),
			},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if native.AdditionalModelRequestFields == nil {
		t.Fatal("native.AdditionalModelRequestFields = nil, want a populated map for a whitelisted model")
	}
	outputConfig, ok := native.AdditionalModelRequestFields["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("AdditionalModelRequestFields = %v, want an output_config key", native.AdditionalModelRequestFields)
	}
	format, ok := outputConfig["format"].(map[string]any)
	if !ok {
		t.Fatalf("output_config = %v, want a format key", outputConfig)
	}
	if format["type"] != "json_schema" {
		t.Errorf("output_config.format.type = %v, want json_schema", format["type"])
	}
	schema, ok := format["schema"].(map[string]any)
	if !ok || schema["type"] != "object" {
		t.Errorf("output_config.format.schema = %v, want the parsed schema object", format["schema"])
	}
}

// TestToProviderResponseFormatOmitsAdditionalModelRequestFieldsOnUnsupportedModel
// is the named, accepted-gap proof: a request with ResponseFormat set
// against a Bedrock model NOT on the structured-output whitelist (e.g.
// Claude 3.5 Sonnet) must NOT send additionalModelRequestFields at all --
// AWS itself rejects output_config.format for such models with a real
// ValidationException, so this adapter never sends that combination.
// Capability-gating THIS specific gap for a first-attempt (non-fallback)
// call is a named, accepted scope limit -- see
// additionalModelRequestFieldsFor's own doc comment.
func TestToProviderResponseFormatOmitsAdditionalModelRequestFieldsOnUnsupportedModel(t *testing.T) {
	req := adapter.ChatRequest{
		Model:    "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []adapter.Message{{Role: "user", Content: "give me JSON"}},
		ResponseFormat: &adapter.ResponseFormat{
			Type: "json_schema",
			JSONSchema: &adapter.JSONSchema{
				Name:   "weather_response",
				Schema: json.RawMessage(`{"type":"object"}`),
			},
		},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)

	if native.AdditionalModelRequestFields != nil {
		t.Errorf("native.AdditionalModelRequestFields = %v, want nil for a model not on the structured-output whitelist", native.AdditionalModelRequestFields)
	}
}

// TestToProviderNilResponseFormatOmitsAdditionalModelRequestFields proves
// the unset (nil, the default) case never emits
// additionalModelRequestFields at all -- byte-identical to every
// ChatRequest built before this field existed.
func TestToProviderNilResponseFormatOmitsAdditionalModelRequestFields(t *testing.T) {
	req := adapter.ChatRequest{
		Model:    "global.anthropic.claude-haiku-4-5-20251001-v1:0",
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}

	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)
	if native.AdditionalModelRequestFields != nil {
		t.Errorf("native.AdditionalModelRequestFields = %v, want nil", native.AdditionalModelRequestFields)
	}

	b, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("marshaling native request: %v", err)
	}
	if strings.Contains(string(b), "additionalModelRequestFields") {
		t.Errorf("marshaled request contains additionalModelRequestFields despite ResponseFormat being nil: %s", b)
	}
}
