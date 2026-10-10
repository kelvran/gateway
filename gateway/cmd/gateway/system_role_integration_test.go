package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
)

// A role:"system" entry inside messages[] on /v1/messages -- Claude Code's
// mid-conversation system message, sent under its mid-conversation-system
// beta -- must reach the provider with its text: one system message per text
// block, none empty. Before the fix the parser kept the text in Parts and
// every adapter's system hoist read Content only, so Bedrock received an
// empty {} system block and answered 400 "The system field can't be null"
// (live, 2026-10-11). The OpenAI mock stands in for the provider here; the
// adapter-level parity tests cover bedrock, anthropic and gemini.
func TestIntegrationMessagesSystemRoleEntryKeepsItsText(t *testing.T) {
	var mu sync.Mutex
	var seen []openai.Message
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}
		var req openai.Request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid upstream request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		seen = append(seen[:0], req.Messages...)
		mu.Unlock()
		resp := openai.Response{
			ID:    "chatcmpl-system-role-test",
			Model: req.Model,
			Choices: []openai.Choice{
				{Index: 0, Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"ok"`)}, FinishReason: "stop"},
			},
			Usage: openai.Usage{PromptTokens: 9, CompletionTokens: 1, TotalTokens: 10},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(upstream.Close)
	gw := newMessagesIntegrationServer(t, upstream.URL, false, nil)

	body := `{"model":"gpt-4o","max_tokens":16,"system":[{"type":"text","text":"Be terse.","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"first"}]},` +
		`{"role":"system","content":[{"type":"text","text":"Reminder A","cache_control":{"type":"ephemeral"}},{"type":"text","text":"Reminder B"}]},` +
		`{"role":"user","content":"second"}]}`
	status, _, raw := postMessages(t, gw.URL, nil, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d; body %s", status, raw)
	}

	mu.Lock()
	defer mu.Unlock()
	var got []string
	for _, m := range seen {
		var text string
		if err := json.Unmarshal(m.Content, &text); err != nil {
			t.Fatalf("upstream message %+v: content is not a plain string (%v); a system message must never be hoisted without its text", m, err)
		}
		if text == "" {
			t.Fatalf("upstream message %+v has empty content: this is the empty block Bedrock rejects", m)
		}
		got = append(got, m.Role+":"+text)
	}
	want := []string{"system:Be terse.", "user:first", "system:Reminder A", "system:Reminder B", "user:second"}
	if len(got) != len(want) {
		t.Fatalf("upstream messages = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("upstream messages[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}
