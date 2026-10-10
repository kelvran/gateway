package anthropic

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// topLevelKeys returns an object's keys in wire order.
func topLevelKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %v %v", tok, err)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// On the passthrough path (RFC-1 §5, slice S11a) ToProvider returns the
// client's body with exactly two rewrites -- model → the deployment's upstream
// id, stream → the pipeline's value -- performed on the top level only, so
// field order, every unknown member at every depth and even the whitespace
// inside nested values survive byte-for-byte.
func TestToProviderPassthroughRelaysRawBodyWithModelAndStreamRewritten(t *testing.T) {
	raw := `{"model":"claude-sonnet-5","max_tokens":5,"stream":true,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u","custom":{ "deep" : [1, 2] }},"service_tier":"auto"}`
	req := adapter.ChatRequest{
		Model:    "claude-upstream-id",
		Stream:   false,
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
		Passthrough: &adapter.Passthrough{
			Format:         "anthropic-messages",
			RawBody:        json.RawMessage(raw),
			ForwardHeaders: http.Header{"Anthropic-Beta": {"beta-a,beta-b"}},
		},
	}
	out, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	pr, ok := out.(*PassthroughRequest)
	if !ok {
		t.Fatalf("ToProvider returned %T, want *PassthroughRequest on the passthrough path", out)
	}
	want := `{"model":"claude-upstream-id","max_tokens":5,"stream":false,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u","custom":{ "deep" : [1, 2] }},"service_tier":"auto"}`
	if got := string(pr.Body()); got != want {
		t.Fatalf("Body() =\n%s\nwant\n%s", got, want)
	}
	if got := topLevelKeys(t, pr.Body()); len(got) != 6 || got[0] != "model" || got[2] != "stream" || got[5] != "service_tier" {
		t.Errorf("key order = %v, want the client's order kept", got)
	}
	if pr.Headers.Get("Anthropic-Beta") != "beta-a,beta-b" {
		t.Errorf("Headers = %v, want the forward headers carried", pr.Headers)
	}
	// json.Marshal callers see the same bytes (compacted by encoding/json),
	// never a re-encoded Request.
	marshalled, err := json.Marshal(pr)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var compactWant bytes.Buffer
	if err := json.Compact(&compactWant, []byte(want)); err != nil {
		t.Fatal(err)
	}
	if string(marshalled) != compactWant.String() {
		t.Errorf("json.Marshal(pr) = %s, want the compacted raw body", marshalled)
	}
}

func TestToProviderPassthroughAppendsStreamAndModelWhenAbsent(t *testing.T) {
	req := adapter.ChatRequest{
		Model:       "claude-upstream-id",
		Stream:      true,
		Messages:    []adapter.Message{{Role: "user", Content: "hi"}},
		Passthrough: &adapter.Passthrough{Format: "anthropic-messages", RawBody: json.RawMessage(`{"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)},
	}
	out, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	pr := out.(*PassthroughRequest)
	want := `{"max_tokens":5,"messages":[{"role":"user","content":"hi"}],"model":"claude-upstream-id","stream":true}`
	if got := string(pr.Body()); got != want {
		t.Fatalf("Body() = %s, want %s", got, want)
	}
}

func TestToProviderPassthroughRejectsANonObjectBody(t *testing.T) {
	req := adapter.ChatRequest{
		Model:       "claude-upstream-id",
		Messages:    []adapter.Message{{Role: "user", Content: "hi"}},
		Passthrough: &adapter.Passthrough{Format: "anthropic-messages", RawBody: json.RawMessage(`[1,2]`)},
	}
	if _, err := New().ToProvider(req); err == nil {
		t.Fatal("ToProvider accepted a non-object raw body, want an error")
	}
}

// Without a passthrough envelope (the OpenAI route, a cache replay, a
// translate hop) the adapter encodes the canonical shadow exactly as before.
func TestToProviderWithoutPassthroughEncodesTheShadow(t *testing.T) {
	for name, pt := range map[string]*adapter.Passthrough{
		"nil":            nil,
		"other format":   {Format: "something-else", RawBody: json.RawMessage(`{"model":"x"}`)},
		"empty raw body": {Format: "anthropic-messages"},
	} {
		out, err := New().ToProvider(adapter.ChatRequest{Model: "claude-upstream-id", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, Passthrough: pt})
		if err != nil {
			t.Fatalf("%s: ToProvider: %v", name, err)
		}
		if _, ok := out.(*Request); !ok {
			t.Errorf("%s: ToProvider returned %T, want *Request", name, out)
		}
	}
}
