package anthropic

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// NewCountTokensPassthrough carries a count_tokens body to the deployment as
// received, with model rewritten to the upstream id and NOTHING else added:
// count_tokens takes no stream member, so unlike the Messages rewrite none is
// appended. Member order, unknown members and nested whitespace survive; the
// forwarded headers ride along (item 11 slice S11c).
func TestNewCountTokensPassthroughRewritesModelOnly(t *testing.T) {
	raw := json.RawMessage(`{"model":"claude-sys","messages":[{"role":"user","content":"hi"}],"system":[{ "type" : "text","text":"s"}],"future_member":{"deep":[1, 2]}}`)
	headers := http.Header{"Anthropic-Version": {"2024-06-01"}, "Anthropic-Beta": {"beta-a"}}
	pr, err := NewCountTokensPassthrough(raw, "claude-upstream-id", headers)
	if err != nil {
		t.Fatalf("NewCountTokensPassthrough: %v", err)
	}
	want := `{"model":"claude-upstream-id","messages":[{"role":"user","content":"hi"}],"system":[{ "type" : "text","text":"s"}],"future_member":{"deep":[1, 2]}}`
	if string(pr.Body()) != want {
		t.Errorf("Body() = %s\nwant      %s", pr.Body(), want)
	}
	if strings.Contains(string(pr.Body()), `"stream"`) {
		t.Errorf("a stream member was added to a count_tokens body: %s", pr.Body())
	}
	if pr.Headers.Get("Anthropic-Version") != "2024-06-01" || pr.Headers.Get("Anthropic-Beta") != "beta-a" {
		t.Errorf("forwarded headers = %v", pr.Headers)
	}
	if _, err := NewCountTokensPassthrough(json.RawMessage(`[1]`), "m", nil); err == nil {
		t.Error("a non-object body was accepted")
	}
}
