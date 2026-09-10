package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIntegrationOversizedRequestBodyIsRejected proves
// maxRequestBodyBytes (docs/rfcs/2026-09-06-gateway-multimodal-content.md's
// own named DoS gap) is enforced end-to-end via the real HTTP handler, not
// just as an in-package unit test of http.MaxBytesReader's own behavior.
func TestIntegrationOversizedRequestBodyIsRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should never be called for an oversized request")
	}))
	t.Cleanup(upstream.Close)

	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_OVERSIZED_BODY")

	oversizedFiller := strings.Repeat("a", maxRequestBodyBytes+1)
	reqBody := `{"model": "gpt-4o", "messages": [{"role": "user", "content": "` + oversizedFiller + `"}]}`

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

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusRequestEntityTooLarge, body)
	}
}

// TestIntegrationMIMEMismatchedContentPartIsRejected proves a declared
// MediaType that doesn't match the inline Data's actually-detected content
// type is rejected at the real HTTP handler, per
// docs/upgrade-research/gateway-2026-09-06.md Finding 1's OWASP File
// Upload Cheat Sheet grounding: a client-declared Content-Type cannot be
// trusted as a security control.
func TestIntegrationMIMEMismatchedContentPartIsRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should never be called for a MIME-mismatched request")
	}))
	t.Cleanup(upstream.Close)

	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_MIME_MISMATCH")

	reqBody := `{
		"model": "gpt-4o",
		"messages": [{
			"role": "user",
			"content": "what's in this image?",
			"parts": [{"type": "image", "media_type": "image/png", "data": "dGhpcyBpcyBqdXN0IHBsYWluIHRleHQsIG5vdCBhbiBpbWFnZQ=="}]
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

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusBadRequest, body)
	}
}

// deeplyNestedJSONSchema builds a JSON document nested exactly depth
// levels deep (depth counts "{" opens), e.g. depth=2 produces
// `{"a":{"a":1}}` -- mirrors
// internal/adapter/validate_test.go's own deeplyNestedObjectSchema
// helper, duplicated here rather than exported since this is the only
// place outside that package's own tests needing one.
func deeplyNestedJSONSchema(depth int) string {
	var b strings.Builder
	for i := 0; i < depth; i++ {
		b.WriteString(`{"a":`)
	}
	b.WriteString("1")
	for i := 0; i < depth; i++ {
		b.WriteString("}")
	}
	return b.String()
}

// TestIntegrationDeeplyNestedResponseFormatSchemaIsRejected proves
// adapter.ValidateResponseFormatSchema's structural-complexity bound
// (THREAT_MODEL.md's Gateway Denial-of-Service row, 2026-09-15) is
// enforced at the real HTTP handler -- a response_format.json_schema.schema
// nested deeper than the gateway's own bound must be rejected with a 400
// before ever reaching the dataplane pipeline, on both the buffered and
// streaming paths (this check runs before the req.Stream branch in
// chatCompletionsHandler).
func TestIntegrationDeeplyNestedResponseFormatSchemaIsRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should never be called for a schema exceeding the complexity bound")
	}))
	t.Cleanup(upstream.Close)

	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_SCHEMA_TOO_DEEP")

	// 200 levels is far beyond any realistic schema and far beyond
	// adapter.maxJSONSchemaDepth (32), so this test doesn't need to know
	// that unexported constant's exact value to prove the bound fires.
	reqBody := `{
		"model": "gpt-4o",
		"messages": [{"role": "user", "content": "give me JSON"}],
		"response_format": {"type": "json_schema", "json_schema": {"name": "too_deep", "schema": ` + deeplyNestedJSONSchema(200) + `}}
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

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusBadRequest, body)
	}
}
