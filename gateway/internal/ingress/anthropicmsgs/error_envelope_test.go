package anthropicmsgs

import "testing"

// ParseErrorEnvelope recognises exactly Anthropic's error object -- the shape
// Claude Code's recovery matches on -- and nothing looser: the verbatim
// exception of RFC-1 §9 (slice S11b) must never pass an arbitrary upstream
// body to a tenant.
func TestParseErrorEnvelope(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantType      string
		wantMessage   string
		wantRecognise bool
	}{
		{"anthropic error", `{"type":"error","error":{"type":"invalid_request_error","message":"thinking.signature: bound to a different conversation"}}`, "invalid_request_error", "thinking.signature: bound to a different conversation", true},
		{"extra members allowed", `{"type":"error","request_id":"req_1","error":{"type":"invalid_request_error","message":"m","details":{"x":1}}}`, "invalid_request_error", "m", true},
		{"wrong top-level type", `{"type":"message","error":{"type":"invalid_request_error","message":"m"}}`, "", "", false},
		{"missing message", `{"type":"error","error":{"type":"invalid_request_error"}}`, "", "", false},
		{"empty message", `{"type":"error","error":{"type":"invalid_request_error","message":""}}`, "", "", false},
		{"missing error type", `{"type":"error","error":{"message":"m"}}`, "", "", false},
		{"bedrock shape", `{"message":"The system field can't be null."}`, "", "", false},
		{"openai shape", `{"error":{"message":"m","type":"invalid_request_error","code":null}}`, "", "", false},
		{"not json", `<html>502</html>`, "", "", false},
		{"array", `[{"type":"error"}]`, "", "", false},
	}
	for _, tc := range cases {
		gotType, gotMessage, ok := ParseErrorEnvelope([]byte(tc.body))
		if ok != tc.wantRecognise || gotType != tc.wantType || gotMessage != tc.wantMessage {
			t.Errorf("%s: ParseErrorEnvelope = (%q, %q, %v), want (%q, %q, %v)", tc.name, gotType, gotMessage, ok, tc.wantType, tc.wantMessage, tc.wantRecognise)
		}
	}
}
