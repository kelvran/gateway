package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToolChoiceUnmarshalAcceptsOpenAIStrings: OpenAI's bare-string forms
// are what every OpenAI SDK sends by default ("auto" is the SDK default
// whenever tools are present). Live defect F4 (2026-10-08): these were a
// 400 "invalid request body" because ToolChoice had no UnmarshalJSON.
func TestToolChoiceUnmarshalAcceptsOpenAIStrings(t *testing.T) {
	for wire, mode := range map[string]string{`"auto"`: "auto", `"required"`: "required", `"none"`: "none"} {
		var tc ToolChoice
		if err := json.Unmarshal([]byte(wire), &tc); err != nil {
			t.Fatalf("%s: %v", wire, err)
		}
		if tc.Mode != mode || tc.ToolName != "" || tc.DisableParallelToolUse {
			t.Errorf("%s -> %+v, want Mode %q and nothing else", wire, tc, mode)
		}
	}
}

// TestToolChoiceUnmarshalAcceptsOpenAIFunctionObject: the object form that
// forces one named function. Today it unmarshalled into an empty canonical
// struct and surfaced as a 502 `unknown tool_choice mode ""` from the adapter.
func TestToolChoiceUnmarshalAcceptsOpenAIFunctionObject(t *testing.T) {
	var tc ToolChoice
	if err := json.Unmarshal([]byte(`{"type":"function","function":{"name":"get_weather"}}`), &tc); err != nil {
		t.Fatal(err)
	}
	if tc.Mode != "tool" || tc.ToolName != "get_weather" {
		t.Errorf("got %+v, want Mode tool, ToolName get_weather", tc)
	}
}

// TestToolChoiceUnmarshalAcceptsCanonicalObject: the documented Kelvran
// shape keeps working unchanged, including the Anthropic-only flag.
func TestToolChoiceUnmarshalAcceptsCanonicalObject(t *testing.T) {
	var tc ToolChoice
	if err := json.Unmarshal([]byte(`{"mode":"tool","tool_name":"lookup","disable_parallel_tool_use":true}`), &tc); err != nil {
		t.Fatal(err)
	}
	if tc.Mode != "tool" || tc.ToolName != "lookup" || !tc.DisableParallelToolUse {
		t.Errorf("got %+v", tc)
	}
}

// TestToolChoiceUnmarshalRejectsUnknownShapes: everything else is a loud
// error that names the accepted forms, so a client sees one 400 with a
// useful message instead of a 502 from the provider adapter.
func TestToolChoiceUnmarshalRejectsUnknownShapes(t *testing.T) {
	cases := map[string]string{
		"anthropic string any":      `"any"`,
		"anthropic object auto":     `{"type":"auto"}`,
		"openai allowed_tools":      `{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[]}}`,
		"mixed mode and type":       `{"mode":"auto","type":"function","function":{"name":"x"}}`,
		"function without a name":   `{"type":"function","function":{}}`,
		"function with empty name":  `{"type":"function","function":{"name":""}}`,
		"number":                    `3`,
		"canonical with bad mode":   `{"mode":"sometimes"}`,
		"canonical tool w/o a name": `{"mode":"tool"}`,
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			var tc ToolChoice
			err := json.Unmarshal([]byte(wire), &tc)
			if err == nil {
				t.Fatalf("%s unmarshalled to %+v, want an error", wire, tc)
			}
			if !strings.Contains(err.Error(), "tool_choice") {
				t.Errorf("error %q should name tool_choice", err)
			}
		})
	}
}

// TestToolChoiceNullLeavesPointerNil: JSON null on the *ToolChoice field
// must keep the pointer nil (no-op), exactly as before.
func TestToolChoiceNullLeavesPointerNil(t *testing.T) {
	var req ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[],"tool_choice":null}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.ToolChoice != nil {
		t.Errorf("tool_choice null -> %+v, want nil", req.ToolChoice)
	}
}

// TestToolChoiceMarshalJSONStaysCanonical is the load-bearing invariant:
// the idempotency fingerprint is sha256(json.Marshal(req)) (dataplane
// claimIdempotency), so re-encoding must still produce the canonical
// object, not the OpenAI wire form, or retries across a rolling deploy
// would see spurious fingerprint mismatches.
func TestToolChoiceMarshalJSONStaysCanonical(t *testing.T) {
	var tc ToolChoice
	if err := json.Unmarshal([]byte(`"required"`), &tc); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(tc)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"mode":"required"}` {
		t.Errorf("re-encoded %s, want {\"mode\":\"required\"}", out)
	}
}

// TestValidateToolChoiceRequiresKnownTool: mode "tool" must name a tool
// that is actually offered, otherwise the provider would 400 much later.
func TestValidateToolChoiceRequiresKnownTool(t *testing.T) {
	tools := []ToolDef{{Name: "get_weather"}}
	if err := ValidateToolChoice(&ToolChoice{Mode: "tool", ToolName: "get_weather"}, tools); err != nil {
		t.Errorf("known tool rejected: %v", err)
	}
	if err := ValidateToolChoice(&ToolChoice{Mode: "tool", ToolName: "nope"}, tools); err == nil {
		t.Error("unknown tool accepted")
	}
	if err := ValidateToolChoice(&ToolChoice{Mode: "auto"}, nil); err != nil {
		t.Errorf("auto without tools should be left to the provider, got %v", err)
	}
	if err := ValidateToolChoice(nil, tools); err != nil {
		t.Errorf("nil tool_choice must be a no-op, got %v", err)
	}
}

// TestToolChoiceErrorsCapEchoedInput: a hostile tool_choice value must not
// be repeated unbounded or with control characters in the error (which is
// also logged) — the same 128-rune house style as upstream_error.go.
func TestToolChoiceErrorsCapEchoedInput(t *testing.T) {
	huge := `"` + strings.Repeat("x", 10000) + `\n\u001b[31m"`
	var tc ToolChoice
	err := json.Unmarshal([]byte(huge), &tc)
	if err == nil {
		t.Fatal("huge unknown string accepted")
	}
	msg := err.Error()
	if len(msg) > 400 {
		t.Errorf("error message is %d bytes; the echoed value must be capped", len(msg))
	}
	if strings.ContainsAny(msg, "\n\x1b") {
		t.Errorf("error message carries raw control characters: %q", msg)
	}
	if !strings.Contains(msg, "…") {
		t.Errorf("truncated echo should be marked with an ellipsis: %q", msg)
	}
}

// TestToolChoiceNamesOpenAINewerTypesAsUnsupported: OpenAI's allowed_tools
// and custom tool_choice types exist today; rejecting them must say so
// rather than calling them unknown.
func TestToolChoiceNamesOpenAINewerTypesAsUnsupported(t *testing.T) {
	for _, wire := range []string{`{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[]}}`, `{"type":"custom","custom":{"name":"x"}}`} {
		var tc ToolChoice
		err := json.Unmarshal([]byte(wire), &tc)
		if err == nil || !strings.Contains(err.Error(), "not supported by this gateway yet") {
			t.Errorf("%s -> %v, want an explicit not-supported-yet error", wire, err)
		}
	}
}

// TestToolChoiceErrorsNeverLeakGoInternals: a type-mismatched object
// ({"function":"x"} where an object is expected) must produce the
// accepted-shapes message, not encoding/json's "Go struct field
// toolChoiceWire..." text, which would name the implementation.
func TestToolChoiceErrorsNeverLeakGoInternals(t *testing.T) {
	var tc ToolChoice
	err := json.Unmarshal([]byte(`{"type":"function","function":"get_weather"}`), &tc)
	if err == nil {
		t.Fatal("type-mismatched object accepted")
	}
	for _, leak := range []string{"Go struct", "toolChoiceWire", "struct {"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks implementation detail %q: %q", leak, err.Error())
		}
	}
	if !strings.Contains(err.Error(), `"function"`) {
		t.Errorf("error should name the accepted shapes: %q", err.Error())
	}
}
