package main

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// TestChatCompletionsForwardsServiceTier is the handler-level proof for
// round-5 Tier A item 4: OpenAI's service_tier decodes through the real
// handler and reaches the upstream verbatim; a request without it sends no
// service_tier key at all. Before this slice the member was an ignored unknown
// (docs/reference/compatibility.md listed it among the silently dropped
// fields).
func TestChatCompletionsForwardsServiceTier(t *testing.T) {
	upstream, lastBody := newCapturingMockUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "sampling-key", "SAMPLING_TEST_UPSTREAM_KEY")

	resp := postChatWithHeaders(t, gw, "sampling-key", `{"model":"gpt-4o","messages":[{"role":"user","content":"flex please"}],"service_tier":"flex"}`, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body %s", resp.StatusCode, b)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(lastBody(), &forwarded); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if got, ok := forwarded["service_tier"].(string); !ok || got != "flex" {
		t.Errorf("upstream service_tier = %v, want flex", forwarded["service_tier"])
	}

	resp2 := postChatWithHeaders(t, gw, "sampling-key", `{"model":"gpt-4o","messages":[{"role":"user","content":"no tier"}]}`, nil)
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp2.StatusCode)
	}
	forwarded = nil
	if err := json.Unmarshal(lastBody(), &forwarded); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if _, present := forwarded["service_tier"]; present {
		t.Errorf("upstream body carries service_tier for a request without one: %s", lastBody())
	}
}
