package anthropic

import (
	"encoding/json"
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
	if native.System != "You are a helpful weather assistant." {
		t.Errorf("native.System = %q, want the system message content", native.System)
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
