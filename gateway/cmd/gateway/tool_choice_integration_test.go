package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// newMockUpstreamRecordingBody answers a minimal valid OpenAI chat completion
// and records every request body it receives, so a test can assert what the
// gateway forwarded on the wire.
func newMockUpstreamRecordingBody(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-tool-choice-integration","model":"` + req.Model + `","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

func postChat(t *testing.T, gwURL, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, gwURL+"/v1/chat/completions", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-gateway-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	return resp.StatusCode, respBody
}

const toolDefsJSON = `"tools":[{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]`

// TestIntegrationToolChoiceOpenAIFormsAreForwarded: live defect F4 was that
// the OpenAI SDK's default tool_choice "auto" produced a 400 and the function
// object a 502. Both must now reach the upstream in OpenAI's own wire form.
func TestIntegrationToolChoiceOpenAIFormsAreForwarded(t *testing.T) {
	upstream, bodies := newMockUpstreamRecordingBody(t)
	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_TOOLCHOICE")

	cases := []struct {
		name       string
		toolChoice string
		wantWire   string
	}{
		{"string auto", `"auto"`, `"tool_choice":"auto"`},
		{"string required", `"required"`, `"tool_choice":"required"`},
		{"string none", `"none"`, `"tool_choice":"none"`},
		{"function object", `{"type":"function","function":{"name":"get_weather"}}`, `"tool_choice":{"type":"function","function":{"name":"get_weather"}}`},
		{"canonical object", `{"mode":"none"}`, `"tool_choice":"none"`},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Distinct message per subtest: cache keys do not include tool_choice
			// (plan item 18), so identical messages would be served from L1 after
			// the first call and never reach the upstream again.
			body := `{"model":"gpt-4o","messages":[{"role":"user","content":"weather in Oslo, ` + tc.name + `"}],` + toolDefsJSON + `,"tool_choice":` + tc.toolChoice + `}`
			status, respBody := postChat(t, gw.URL, body)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", status, respBody)
			}
			got := bodies()
			if len(got) != i+1 {
				t.Fatalf("upstream bodies = %d, want %d", len(got), i+1)
			}
			if !strings.Contains(got[i], tc.wantWire) {
				t.Errorf("forwarded body lacks %s:\n%s", tc.wantWire, got[i])
			}
		})
	}
}

// TestIntegrationToolChoiceUnknownShapeIs400Envelope: an unsupported
// tool_choice shape is rejected before any upstream call, as a JSON envelope
// that names the field.
func TestIntegrationToolChoiceUnknownShapeIs400Envelope(t *testing.T) {
	upstream, bodies := newMockUpstreamRecordingBody(t)
	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_TOOLCHOICE_BAD")

	for name, toolChoice := range map[string]string{
		"anthropic object":    `{"type":"auto"}`,
		"unknown string":      `"any"`,
		"forced unknown tool": `{"type":"function","function":{"name":"not_offered"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],` + toolDefsJSON + `,"tool_choice":` + toolChoice + `}`
			status, respBody := postChat(t, gw.URL, body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", status, respBody)
			}
			var env apiErrorBody
			if err := json.Unmarshal(respBody, &env); err != nil {
				t.Fatalf("body is not the error envelope: %v", err)
			}
			if env.Error.Type != "invalid_request_error" || env.Error.Code == nil || *env.Error.Code != "invalid_tool_choice" {
				t.Errorf("type/code = %q/%v, want invalid_request_error/invalid_tool_choice", env.Error.Type, env.Error.Code)
			}
			if env.Error.Param == nil || *env.Error.Param != "tool_choice" {
				t.Errorf("param = %v, want tool_choice", env.Error.Param)
			}
		})
	}
	if got := bodies(); len(got) != 0 {
		t.Errorf("rejected requests must never reach the upstream; it saw %d", len(got))
	}
}
