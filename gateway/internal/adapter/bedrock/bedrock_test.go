package bedrock

import (
	"encoding/json"
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
