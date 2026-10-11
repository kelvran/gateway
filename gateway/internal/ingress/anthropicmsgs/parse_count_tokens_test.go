package anthropicmsgs

import (
	"errors"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// ParseCountTokens is Parse for the token-counting body: the same members,
// the same canonical shadow and Passthrough, but max_tokens is not required
// (count_tokens has none) and stream is not a member it knows (item 11 slice
// S11c).
func TestParseCountTokensAcceptsABodyWithoutMaxTokens(t *testing.T) {
	body := []byte(`{"model":"claude-sys","system":"be brief","messages":[{"role":"user","content":"how many tokens"}],"tools":[{"name":"t","description":"d","input_schema":{"type":"object"}}]}`)
	req, pt, err := ParseCountTokens(body)
	if err != nil {
		t.Fatalf("ParseCountTokens: %v", err)
	}
	if req.Model != "claude-sys" || req.MaxTokens != nil || len(req.Messages) != 2 || len(req.Tools) != 1 {
		t.Errorf("req = model %q max_tokens %v messages %d tools %d, want claude-sys, nil, 2 (system + user), 1", req.Model, req.MaxTokens, len(req.Messages), len(req.Tools))
	}
	if pt == nil || pt.Format != adapter.IngressFormatAnthropicMessages || string(pt.RawBody) != string(body) || req.Passthrough != pt {
		t.Errorf("passthrough = %+v, want the raw body under the Messages format, attached to the request", pt)
	}
}

// The other required members stay required, and a Messages body with
// max_tokens still parses (Claude Code's /context sends the turn's body).
func TestParseCountTokensKeepsTheOtherRequiredMembers(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want error
	}{
		"missing model":    {`{"messages":[{"role":"user","content":"hi"}]}`, ErrMissingModel},
		"missing messages": {`{"model":"claude-sys"}`, ErrMissingMessages},
		"invalid json":     {`{"model":`, ErrInvalidBody},
	} {
		if _, _, err := ParseCountTokens([]byte(tc.body)); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	req, _, err := ParseCountTokens([]byte(`{"model":"claude-sys","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil || req.MaxTokens == nil || *req.MaxTokens != 16 {
		t.Errorf("a body with max_tokens: err %v max_tokens %v, want accepted with 16", err, req.MaxTokens)
	}
}
