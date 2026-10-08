package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var integrationCompletionIDPattern = regexp.MustCompile(`^chatcmpl-[0-9a-f]{32}$`)

type integrationEnvelope struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
}

func postChatRaw(t *testing.T, gwURL, body string) (int, []byte) {
	t.Helper()
	httpReq, err := http.NewRequest(http.MethodPost, gwURL+"/v1/chat/completions", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer test-gateway-key")
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return resp.StatusCode, raw
}

func assertCreatedWithin(t *testing.T, created int64, before, after time.Time) {
	t.Helper()
	if created < before.Unix()-1 || created > after.Unix()+1 {
		t.Errorf("created = %d, want within [%d, %d]", created, before.Unix()-1, after.Unix()+1)
	}
}

// TestIntegrationBedrockResponseCarriesGatewayIssuedEnvelope is the F7
// reproduction over real HTTP on both sides: a Bedrock Converse response has
// no id, so before 2026-10-08 the client body was `"id":""` with no
// object/created. Now the gateway issues the id and the identical request
// replays it from cache unchanged.
func TestIntegrationBedrockResponseCarriesGatewayIssuedEnvelope(t *testing.T) {
	upstream, calls := newMockBedrockUpstream(t)
	gw := newIntegrationServerBedrock(t, upstream.URL, "test-gateway-key",
		"KELVRAN_INTEGRATION_TEST_BEDROCK_ACCESS_KEY_8C", "KELVRAN_INTEGRATION_TEST_BEDROCK_SECRET_KEY_8C")

	reqBody := `{"model":"anthropic.claude-3-5-sonnet-20241022-v2:0","messages":[{"role":"user","content":"envelope hello, bedrock"}]}`
	before := time.Now()
	status, raw := postChatRaw(t, gw.URL, reqBody)
	after := time.Now()
	if status != http.StatusOK {
		t.Fatalf("status = %d; body: %s", status, raw)
	}
	var first integrationEnvelope
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, raw)
	}
	if !integrationCompletionIDPattern.MatchString(first.ID) {
		t.Errorf("id = %q, want a gateway-issued chatcmpl- id for a Bedrock completion", first.ID)
	}
	if first.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", first.Object)
	}
	assertCreatedWithin(t, first.Created, before, after)
	if first.Model != "anthropic.claude-3-5-sonnet-20241022-v2:0" {
		t.Errorf("model = %q, want the canonical model", first.Model)
	}

	status, raw = postChatRaw(t, gw.URL, reqBody)
	if status != http.StatusOK {
		t.Fatalf("second status = %d; body: %s", status, raw)
	}
	var second integrationEnvelope
	if err := json.Unmarshal(raw, &second); err != nil {
		t.Fatalf("second body is not JSON: %v\n%s", err, raw)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (second request is a cache hit)", got)
	}
	if second.ID != first.ID || second.Created != first.Created {
		t.Errorf("cache replay envelope = (%q, %d), want the original (%q, %d)", second.ID, second.Created, first.ID, first.Created)
	}
}

// newMockStreamingUpstreamWithoutEnvelope streams OpenAI-shaped chunks that
// carry neither id nor model -- what the gateway must complete itself.
func newMockStreamingUpstreamWithoutEnvelope(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"content":"hello "},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"content":"stream"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":6,"completion_tokens":2,"total_tokens":8}}`,
		} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", frame); err != nil {
				return
			}
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func integrationSSEEnvelopes(t *testing.T, body string) []integrationEnvelope {
	t.Helper()
	var frames []integrationEnvelope
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.TrimPrefix(line, "data: ") == "[DONE]" {
			continue
		}
		var f integrationEnvelope
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &f); err != nil {
			t.Fatalf("frame is not JSON: %v\n%s", err, line)
		}
		frames = append(frames, f)
	}
	if len(frames) == 0 {
		t.Fatalf("no data frames:\n%s", body)
	}
	return frames
}

// TestIntegrationStreamingChunksWithoutIDsAreStamped: every SSE frame of a
// stream whose upstream chunks carry no id/model gets one gateway-issued id,
// "chat.completion.chunk", the canonical model and one created; the
// identical request replays the same id from cache.
func TestIntegrationStreamingChunksWithoutIDsAreStamped(t *testing.T) {
	upstream, calls := newMockStreamingUpstreamWithoutEnvelope(t)
	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_8C")

	reqBody := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"envelope streaming hello"}]}`
	before := time.Now()
	status, raw := postChatRaw(t, gw.URL, reqBody)
	after := time.Now()
	if status != http.StatusOK {
		t.Fatalf("status = %d; body: %s", status, raw)
	}
	frames := integrationSSEEnvelopes(t, string(raw))
	id := frames[0].ID
	if !integrationCompletionIDPattern.MatchString(id) {
		t.Fatalf("first frame id = %q, want gateway-issued", id)
	}
	for i, f := range frames {
		if f.ID != id || f.Object != "chat.completion.chunk" || f.Model != "gpt-4o" || f.Created != frames[0].Created {
			t.Errorf("frame %d = %+v, want id %q, chat.completion.chunk, model gpt-4o, created %d", i, f, id, frames[0].Created)
		}
	}
	assertCreatedWithin(t, frames[0].Created, before, after)

	status, raw = postChatRaw(t, gw.URL, reqBody)
	if status != http.StatusOK {
		t.Fatalf("second status = %d; body: %s", status, raw)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (second stream is a cache hit)", got)
	}
	for i, f := range integrationSSEEnvelopes(t, string(raw)) {
		if f.ID != id || f.Created != frames[0].Created {
			t.Errorf("replayed frame %d = %+v, want the original id %q and created %d", i, f, id, frames[0].Created)
		}
	}
}
