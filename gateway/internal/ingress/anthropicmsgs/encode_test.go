package anthropicmsgs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

func decodeResponseFixture(t *testing.T, name string) adapter.ChatResponse {
	t.Helper()
	var resp adapter.ChatResponse
	if err := json.Unmarshal(mustReadTestdata(t, name), &resp); err != nil {
		t.Fatalf("decoding %s: %v", name, err)
	}
	return resp
}

// TestEncodeResponseToolUseWithThinkingMatchesGolden: the canonical response
// becomes an Anthropic Message -- content in replay order (thinking with
// Sequence 0, the text, tool_use 0, the redacted thinking with Sequence 1
// after it), the native stop_reason, usage with input_tokens = PromptTokens
// minus the cache tokens (the inverse of the adapter's inclusive sum),
// output_tokens_details.thinking_tokens from ReasoningTokens, and the
// input_transformations array verbatim. Signatures and redacted data are
// byte-for-byte.
func TestEncodeResponseToolUseWithThinkingMatchesGolden(t *testing.T) {
	resp := decodeResponseFixture(t, "response_tool_use_with_thinking.json")
	out := EncodeResponse(resp)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("EncodeResponse output is not JSON: %v\n%s", err, out)
	}
	if m["type"] != "message" || m["role"] != "assistant" || m["stop_reason"] != "tool_use" {
		t.Errorf("envelope = %v", m)
	}
	usage, _ := m["usage"].(map[string]any)
	if usage["input_tokens"] != float64(30) || usage["cache_read_input_tokens"] != float64(80) || usage["cache_creation_input_tokens"] != float64(10) || usage["output_tokens"] != float64(30) {
		t.Errorf("usage = %v, want input_tokens 30 (120-80-10), cache 80/10, output 30", usage)
	}
	content, _ := m["content"].([]any)
	kinds := make([]string, 0, len(content))
	for _, c := range content {
		kinds = append(kinds, c.(map[string]any)["type"].(string))
	}
	if strings.Join(kinds, ",") != "thinking,text,tool_use,redacted_thinking" {
		t.Errorf("content order = %v, want thinking,text,tool_use,redacted_thinking", kinds)
	}
	checkGolden(t, "anthropic_tool_use_with_thinking.golden.json", prettyJSON(t, out))
}

// TestEncodeResponseStopReasonFallbackTable: without a native stop_reason the
// canonical finish_reason is mapped forward -- stop→end_turn, length→
// max_tokens, tool_calls→tool_use, content_filter→refusal, anything else
// verbatim -- and stop_sequence is null.
func TestEncodeResponseStopReasonFallbackTable(t *testing.T) {
	base := decodeResponseFixture(t, "response_stop_fallback.json")
	for finish, want := range map[string]string{"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use", "content_filter": "refusal", "pause_turn": "pause_turn"} {
		resp := base
		resp.Choices = []adapter.Choice{{Index: 0, Message: base.Choices[0].Message, FinishReason: finish}}
		var m map[string]any
		if err := json.Unmarshal(EncodeResponse(resp), &m); err != nil {
			t.Fatal(err)
		}
		if m["stop_reason"] != want {
			t.Errorf("finish_reason %q → stop_reason %v, want %q", finish, m["stop_reason"], want)
		}
		if v, present := m["stop_sequence"]; !present || v != nil {
			t.Errorf("finish_reason %q → stop_sequence %v, want an explicit null", finish, v)
		}
	}
	checkGolden(t, "anthropic_stop_fallback.golden.json", prettyJSON(t, EncodeResponse(base)))
}

// TestEncodeResponseNativeStopSequence: the S3 native fields win over the
// table, and the matched sequence is carried.
func TestEncodeResponseNativeStopSequence(t *testing.T) {
	out := EncodeResponse(decodeResponseFixture(t, "response_stop_sequence_native.json"))
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["stop_reason"] != "stop_sequence" || m["stop_sequence"] != " 5" {
		t.Errorf("stop_reason/stop_sequence = %v/%v, want stop_sequence/\" 5\"", m["stop_reason"], m["stop_sequence"])
	}
	usage, _ := m["usage"].(map[string]any)
	if _, has := usage["output_tokens_details"]; has {
		t.Error("output_tokens_details present with zero ReasoningTokens; want absent")
	}
	if _, has := m["input_transformations"]; has {
		t.Error("input_transformations present with none; want absent")
	}
	checkGolden(t, "anthropic_stop_sequence_native.golden.json", prettyJSON(t, out))
}

// TestEncodeErrorKeepsCodeAndParam: Anthropic's envelope has no code member;
// Kelvran's keeps `code` (kelvran connect's model_not_found probe reads it)
// and `param`, both omitted when empty.
func TestEncodeErrorKeepsCodeAndParam(t *testing.T) {
	got := string(EncodeError("invalid_request_error", "no deployment serves model \"m\"", "model_not_found", "model"))
	want := `{"type":"error","error":{"type":"invalid_request_error","message":"no deployment serves model \"m\"","code":"model_not_found","param":"model"}}`
	if got != want {
		t.Errorf("EncodeError = %s\nwant %s", got, want)
	}
	bare := string(EncodeError("api_error", "boom", "", ""))
	if bare != `{"type":"error","error":{"type":"api_error","message":"boom"}}` {
		t.Errorf("EncodeError without code/param = %s", bare)
	}
}

// TestEncodeResponseInvalidToolInputBecomesEmptyObject: a tool_use input
// must be a JSON value; an adapter that surfaced a truncated arguments
// string would otherwise produce a malformed document.
func TestEncodeResponseInvalidToolInputBecomesEmptyObject(t *testing.T) {
	resp := adapter.ChatResponse{ID: "x", Model: "m", Choices: []adapter.Choice{{Message: adapter.Message{
		Role: "assistant", ToolCalls: []adapter.ToolCall{{ID: "toolu_1", Name: "Read", ArgumentsJSON: `{"file_path":`}},
	}, FinishReason: "tool_calls"}}}
	out := EncodeResponse(resp)
	if !json.Valid(out) {
		t.Fatalf("EncodeResponse produced invalid JSON: %s", out)
	}
	if !strings.Contains(string(out), `"input":{}`) || !strings.Contains(string(out), `"name":"Read"`) {
		t.Errorf("EncodeResponse = %s, want a tool_use named Read with input {}", out)
	}
}
