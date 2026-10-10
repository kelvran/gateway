package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
)

// newCapturingMockUpstream is newMockUpstream with the raw upstream body
// kept, so a test can assert what the gateway actually forwarded -- the
// item 11 slice S5 fields are exactly the ones the gateway used to drop
// between the client and the provider.
func newCapturingMockUpstream(t *testing.T) (*httptest.Server, func() []byte) {
	t.Helper()
	var mu sync.Mutex
	var last []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		last = body
		mu.Unlock()
		var req openai.Request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid upstream request body: %v", err), http.StatusBadRequest)
			return
		}
		resp := openai.Response{
			ID:      "chatcmpl-capturing-mock",
			Model:   req.Model,
			Choices: []openai.Choice{{Index: 0, Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"ok"`)}, FinishReason: "stop"}},
			Usage:   openai.Usage{PromptTokens: 7, CompletionTokens: 1, TotalTokens: 8},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []byte { mu.Lock(); defer mu.Unlock(); return last }
}

// TestChatCompletionsForwardsStopStringAndArray is the handler-level proof
// for item 11 slice S5's first field: OpenAI's `stop`, as a bare string
// or an array, decodes through the real handler and reaches the upstream
// as the array form; a request without it sends no `stop` key at all.
// Before this slice the field was an ignored unknown
// (docs/reference/compatibility.md listed it among the silently dropped
// fields), so the string form in particular must keep being accepted.
func TestChatCompletionsForwardsStopStringAndArray(t *testing.T) {
	upstream, lastBody := newCapturingMockUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "sampling-key", "SAMPLING_TEST_UPSTREAM_KEY")

	cases := []struct {
		name, body string
		wantStop   []string
	}{
		{"string", `{"model":"gpt-4o","messages":[{"role":"user","content":"count to ten"}],"stop":"END"}`, []string{"END"}},
		{"array", `{"model":"gpt-4o","messages":[{"role":"user","content":"count to twenty"}],"stop":["a","b"]}`, []string{"a", "b"}},
		{"absent", `{"model":"gpt-4o","messages":[{"role":"user","content":"count to thirty"}]}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postChatWithHeaders(t, gw, "sampling-key", tc.body, nil)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, body %s", resp.StatusCode, b)
			}
			var forwarded map[string]any
			if err := json.Unmarshal(lastBody(), &forwarded); err != nil {
				t.Fatalf("upstream body: %v", err)
			}
			got, present := forwarded["stop"]
			if tc.wantStop == nil {
				if present {
					t.Fatalf("upstream body carries stop %v, want no stop key", got)
				}
				return
			}
			arr, ok := got.([]any)
			if !ok || len(arr) != len(tc.wantStop) {
				t.Fatalf("upstream stop = %v (%T), want %v", got, got, tc.wantStop)
			}
			for i, want := range tc.wantStop {
				if arr[i] != want {
					t.Errorf("upstream stop[%d] = %v, want %q", i, arr[i], want)
				}
			}
		})
	}
}

// TestChatCompletionsRejectsNonStringStop: a `stop` that is neither a
// string nor an array of strings is a 400 like any other wrong-type field.
func TestChatCompletionsRejectsNonStringStop(t *testing.T) {
	upstream, _ := newCapturingMockUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "sampling-key", "SAMPLING_TEST_UPSTREAM_KEY")
	resp := postChatWithHeaders(t, gw, "sampling-key", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stop":5}`, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body %s", resp.StatusCode, b)
	}
}

// TestChatCompletionsForwardsTopP is the handler-level proof for slice S5's
// second field: `top_p` reaches the upstream as sent, and an absent field
// sends no key.
func TestChatCompletionsForwardsTopP(t *testing.T) {
	upstream, lastBody := newCapturingMockUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "sampling-key", "SAMPLING_TEST_UPSTREAM_KEY")

	resp := postChatWithHeaders(t, gw, "sampling-key", `{"model":"gpt-4o","messages":[{"role":"user","content":"top-p please"}],"top_p":0.9}`, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body %s", resp.StatusCode, b)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(lastBody(), &forwarded); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if got, ok := forwarded["top_p"].(float64); !ok || got != 0.9 {
		t.Errorf("upstream top_p = %v, want 0.9", forwarded["top_p"])
	}

	resp2 := postChatWithHeaders(t, gw, "sampling-key", `{"model":"gpt-4o","messages":[{"role":"user","content":"no top-p"}]}`, nil)
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp2.StatusCode)
	}
	forwarded = nil
	if err := json.Unmarshal(lastBody(), &forwarded); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if _, present := forwarded["top_p"]; present {
		t.Errorf("upstream body carries top_p for a request without one: %s", lastBody())
	}
}
