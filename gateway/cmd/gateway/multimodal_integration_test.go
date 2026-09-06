package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIntegrationMultiModalRequestReachesRealUpstreamAsImageURLPart is
// the real end-to-end proof for docs/rfcs/2026-09-06-gateway-
// multimodal-content.md: a client-sent request with an image content
// part must reach the real upstream HTTP call as OpenAI's actual
// content-array wire shape — not just something an adapter unit test
// asserts on a Go struct in isolation. The mock upstream here captures
// and asserts on the raw request body it received, unlike
// newMockUpstream (which only decodes it far enough to echo the model
// back).
func TestIntegrationMultiModalRequestReachesRealUpstreamAsImageURLPart(t *testing.T) {
	var capturedBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}
		capturedBody = body

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "chatcmpl-multimodal-integration-test",
			"model": "gpt-4o",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]any{"role": "assistant", "content": "I see an image"}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 4, "total_tokens": 24},
		})
	}))
	t.Cleanup(upstream.Close)

	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_MULTIMODAL")

	reqBody := `{
		"model": "gpt-4o",
		"messages": [{
			"role": "user",
			"content": "what's in this image?",
			"parts": [{"type": "image", "media_type": "image/png", "data": "iVBORw0KGgoAAAAAAAAAAA=="}]
		}]
	}`
	httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
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

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusOK, body)
	}

	if capturedBody == nil {
		t.Fatal("mock upstream never received a request")
	}

	var upstreamReq struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text,omitempty"`
				ImageURL *struct {
					URL string `json:"url"`
				} `json:"image_url,omitempty"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(capturedBody, &upstreamReq); err != nil {
		t.Fatalf("upstream request content was not the expected array shape: %v\nbody: %s", err, capturedBody)
	}

	if len(upstreamReq.Messages) != 1 {
		t.Fatalf("upstream request Messages len = %d, want 1", len(upstreamReq.Messages))
	}
	parts := upstreamReq.Messages[0].Content
	if len(parts) != 2 {
		t.Fatalf("upstream request content parts len = %d, want 2 (text, image_url)", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "what's in this image?" {
		t.Errorf("parts[0] = %+v, want the text lead-in", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,iVBORw0KGgoAAAAAAAAAAA==" {
		t.Errorf("parts[1] = %+v, want an image_url part with a data: URI built from the request's media_type/data", parts[1])
	}
}
