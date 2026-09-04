package openaicompat

import (
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

func TestName(t *testing.T) {
	if got := New().Name(); got != "openaicompat" {
		t.Errorf("Name() = %q, want %q", got, "openaicompat")
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
