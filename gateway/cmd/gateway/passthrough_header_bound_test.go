package main

import (
	"net/http"
	"strings"
	"testing"
)

// headerBytes sums every name and value, so a bound on it is a bound on what
// the passthrough hop forwards.
func TestHeaderBytesSumsNamesAndValues(t *testing.T) {
	h := http.Header{}
	h.Add("Anthropic-Beta", "ab")      // 14 + 2
	h.Add("Anthropic-Beta", "cde")     // 14 + 3
	h.Add("Anthropic-Version", "2023") // 17 + 4
	if got := headerBytes(h); got != 54 {
		t.Fatalf("headerBytes = %d, want 54", got)
	}
	if got := headerBytes(nil); got != 0 {
		t.Fatalf("headerBytes(nil) = %d, want 0", got)
	}
}

// A client whose anthropic-* headers exceed the bound is refused before the
// pipeline runs -- 400 invalid_request in the Anthropic envelope, no upstream
// call -- while a set the size Claude Code sends passes (slice S11a; the
// names are an open list, so their size is what the gateway bounds).
func TestIntegrationMessagesForwardedHeadersAreBounded(t *testing.T) {
	upstream, seen := newCapturingAnthropicUpstream(t)
	gw := newPassthroughIntegrationServer(t, upstream.URL, nil)

	oversized := map[string]string{
		"Authorization":  "Bearer all-secret",
		"anthropic-beta": strings.Repeat("beta-value-padding,", maxForwardedAnthropicHeaderBytes/19+1),
	}
	status, _, raw := postMessages(t, gw.URL, oversized, `{"model":"claude-sys","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", status, raw)
	}
	e := decodeAnthropicError(t, raw)
	if e.Error.Type != "invalid_request_error" || e.Error.Code != "invalid_request" || !strings.Contains(e.Error.Message, "anthropic-* request headers") {
		t.Errorf("error = %+v, want invalid_request_error / invalid_request naming the headers", e.Error)
	}
	if n := len(seen()); n != 0 {
		t.Errorf("upstream requests = %d, want 0: the refusal precedes the pipeline", n)
	}

	status, _, raw = postMessages(t, gw.URL, passthroughClientHeaders, `{"model":"claude-sys","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("a normal header set: status = %d, want 200; body %s", status, raw)
	}
}
