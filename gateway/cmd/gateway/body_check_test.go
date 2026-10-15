package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/ingress/anthropicmsgs"
)

// The two OpenAI decode paths run anthropicmsgs.CheckBody before
// encoding/json (round-5 item 7, the third leg of Tier B item 7): a member
// that appears twice in one object, nesting past 64 levels and invalid UTF-8
// are 400 `invalid_json` before any pipeline work, in the route's own
// `invalid request body: …` form and never the Anthropic package's sentinel
// text. Before, encoding/json kept the last duplicate, put U+FFFD where the
// UTF-8 was invalid and nested up to its own 10,000-level limit, so these
// bodies reached the pipeline -- and `/v1/messages` had refused the same
// shapes since its parser shipped.

// rawResponse is a fully read, closed response: status, Content-Type, body.
type rawResponse struct {
	status      int
	contentType string
	body        []byte
}

// postBody posts raw bytes with the integration key and reads the whole
// response, closing its body.
func postBody(t *testing.T, url string, body []byte) rawResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-gateway-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("POST %s: read body: %v", url, err)
	}
	return rawResponse{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: raw}
}

func assertInvalidJSON(t *testing.T, name string, r rawResponse, wantDetail string) {
	t.Helper()
	if r.status != http.StatusBadRequest {
		t.Errorf("%s: status %d, want 400; body %s", name, r.status, r.body)
		return
	}
	if !strings.HasPrefix(r.contentType, "application/json") {
		t.Errorf("%s: Content-Type %q, want application/json", name, r.contentType)
	}
	var body apiErrorBody
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatalf("%s: decode envelope: %v (%s)", name, err, r.body)
	}
	if body.Error.Type != "invalid_request_error" || body.Error.Code == nil || *body.Error.Code != "invalid_json" {
		t.Errorf("%s: type/code = %q/%v, want invalid_request_error/invalid_json", name, body.Error.Type, body.Error.Code)
	}
	if want := "invalid request body: " + wantDetail; body.Error.Message != want {
		t.Errorf("%s: message %q, want %q", name, body.Error.Message, want)
	}
	if strings.Contains(body.Error.Message, "anthropicmsgs") {
		t.Errorf("%s: message %q leaks the Anthropic parser's sentinel text", name, body.Error.Message)
	}
}

// TestOpenAIRoutesRejectDuplicateMembersAsInvalidJSON: a repeated member at
// the top level and inside a nested object, on both routes, with the JSON
// pointer the Anthropic ingress already reports; the upstream is never called.
func TestOpenAIRoutesRejectDuplicateMembersAsInvalidJSON(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_BODYCHECK")
	cases := []struct{ name, path, body, detail string }{
		{"chat, top-level model twice", "/v1/chat/completions", `{"model":"gpt-4o","model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, `the body has a duplicate member "model"`},
		{"chat, content twice in a message", "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"a","content":"b"}]}`, `/messages/0 has a duplicate member "content"`},
		{"embeddings, input twice", "/v1/embeddings", `{"model":"gpt-4o","input":"x","input":"y"}`, `the body has a duplicate member "input"`},
	}
	for _, c := range cases {
		assertInvalidJSON(t, c.name, postBody(t, gw.URL+c.path, []byte(c.body)), c.detail)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("mock upstream calls = %d, want 0 (a duplicate member is refused before the pipeline)", got)
	}
}

// TestOpenAIRoutesRejectInvalidUTF8AndDeepNestingAsInvalidJSON: the other
// two shapes CheckBody refuses, one per route; an otherwise identical valid
// body is the control and reaches the mock upstream.
func TestOpenAIRoutesRejectInvalidUTF8AndDeepNestingAsInvalidJSON(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_BODYCHECK2")
	invalidUTF8 := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"caf` + "\xff" + `"}]}`)
	assertInvalidJSON(t, "chat, invalid UTF-8 in content", postBody(t, gw.URL+"/v1/chat/completions", invalidUTF8), "not valid UTF-8")
	// The body object is level 1; 64 nested arrays inside an ignored member make 65.
	deep := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"extra":` + strings.Repeat("[", 64) + strings.Repeat("]", 64) + `}`)
	assertInvalidJSON(t, "chat, 65 levels deep", postBody(t, gw.URL+"/v1/chat/completions", deep), "nests deeper than 64 levels")
	deepEmbed := []byte(`{"model":"gpt-4o","input":"x","extra":` + strings.Repeat("[", 64) + strings.Repeat("]", 64) + `}`)
	assertInvalidJSON(t, "embeddings, 65 levels deep", postBody(t, gw.URL+"/v1/embeddings", deepEmbed), "nests deeper than 64 levels")
	if got := calls.Load(); got != 0 {
		t.Errorf("mock upstream calls = %d, want 0 before the control", got)
	}
	// Control: 64 levels (the body object plus 63 arrays) and valid UTF-8 pass the check and reach the upstream.
	control := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"café"}],"extra":` + strings.Repeat("[", 63) + strings.Repeat("]", 63) + `}`)
	if r := postBody(t, gw.URL+"/v1/chat/completions", control); r.status != http.StatusOK {
		t.Errorf("control: status %d, want 200; body %s", r.status, r.body)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("mock upstream calls after the control = %d, want 1", got)
	}
	// An out-of-range number literal in an ignored member is not the walk's
	// business (UseNumber): it reaches encoding/json, which skips the member.
	outOfRange := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"extra":1e999}`)
	if r := postBody(t, gw.URL+"/v1/chat/completions", outOfRange); r.status != http.StatusOK {
		t.Errorf("1e999 in an ignored member: status %d, want 200; body %s", r.status, r.body)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("mock upstream calls after both controls = %d, want 2", got)
	}
}

// TestInvalidBodyMessageNeverRelaysTheSentinel: the helper renders a BodyError's
// detail behind the route's prefix, and its fallback (unreachable today, since
// CheckBody returns only *BodyError) is fixed text, so the Anthropic package's
// sentinel can never reach an OpenAI envelope through it.
func TestInvalidBodyMessageNeverRelaysTheSentinel(t *testing.T) {
	if got := invalidBodyMessage(anthropicmsgs.CheckBody([]byte(`{"a":1,"a":2}`))); got != `invalid request body: the body has a duplicate member "a"` {
		t.Errorf("BodyError rendered as %q", got)
	}
	if got := invalidBodyMessage(errors.New("anthropicmsgs: request body is not a JSON object: x")); got != "invalid request body" {
		t.Errorf("fallback = %q, want the fixed text", got)
	}
}

// TestIntegrationMessagesDuplicateMemberStaysInvalidJSON pins the Anthropic
// route's side of the shared check: a duplicate member is still `invalid_json`
// in the Anthropic envelope (parseErrorCode maps ErrInvalidBody, which the
// typed BodyError must keep satisfying) with the message the route has always
// relayed -- the sentinel text followed by the detail. The OpenAI routes'
// tests above prove the same detail reaches their clients without it.
func TestIntegrationMessagesDuplicateMemberStaysInvalidJSON(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)
	body := `{"model":"gpt-4o","model":"gpt-4o","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	status, _, raw := postMessages(t, gw.URL, nil, body)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", status, raw)
	}
	e := decodeAnthropicError(t, raw)
	if e.Error.Type != "invalid_request_error" || e.Error.Code != "invalid_json" || e.Error.Param != "" {
		t.Errorf("envelope = %+v, want invalid_request_error/invalid_json with no param", e.Error)
	}
	if want := `anthropicmsgs: request body is not a JSON object: the body has a duplicate member "model"`; e.Error.Message != want {
		t.Errorf("message %q, want %q (unchanged on this route)", e.Error.Message, want)
	}
	if calls.Load() != 0 {
		t.Errorf("upstream calls = %d, want 0", calls.Load())
	}
}
