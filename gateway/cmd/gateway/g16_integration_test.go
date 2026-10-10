package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// postChatWithHeaders sends one chat request with the integration key and the given
// extra headers and returns the response; the caller closes the body.
func postChatWithHeaders(t *testing.T, gw *httptest.Server, key, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

// TestIntegrationIdempotencyKeyReusedWithADifferentBodyIs422 drives G16's
// first correction through the real handler: the same Idempotency-Key with a
// different body is the client's own fault, so it is 422
// idempotency_key_reused with param Idempotency-Key and no Retry-After —
// not the 502 upstream_error with Retry-After it was before.
func TestIntegrationIdempotencyKeyReusedWithADifferentBodyIs422(t *testing.T) {
	upstream, calls := newMockUpstream(t)
	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key-g16", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_G16")
	headers := map[string]string{"Idempotency-Key": "g16-reuse-7d1c"}

	first := postChatWithHeaders(t, gw, "test-gateway-key-g16", `{"model":"gpt-4o","messages":[{"role":"user","content":"first body"}]}`, headers)
	_ = first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", first.StatusCode)
	}
	second := postChatWithHeaders(t, gw, "test-gateway-key-g16", `{"model":"gpt-4o","messages":[{"role":"user","content":"a different body"}]}`, headers)
	defer func() { _ = second.Body.Close() }()
	if second.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("second request: status = %d, want 422", second.StatusCode)
	}
	body := decodeAPIErrorResponse(t, second)
	if body.Error.Type != "invalid_request_error" || body.Error.Code == nil || *body.Error.Code != "idempotency_key_reused" {
		t.Errorf("envelope type/code = %q/%v, want invalid_request_error/idempotency_key_reused", body.Error.Type, body.Error.Code)
	}
	if body.Error.Param == nil || *body.Error.Param != "Idempotency-Key" {
		t.Errorf("param = %v, want Idempotency-Key", body.Error.Param)
	}
	if ra := second.Header.Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q, want none: a reused key is not a transient condition", ra)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("mock upstream calls = %d, want 1 (the second request must be rejected before any upstream call)", got)
	}
}

// TestIntegrationResponseFormatWithNoCapableDeploymentIs400 drives G16's
// second correction: response_format on a pool whose only deployment is a
// Bedrock model outside adapter.SupportsStructuredOutput's whitelist is a
// request-shape fault, 400 response_format_unsupported with param
// response_format and no Retry-After, decided before any upstream call.
func TestIntegrationResponseFormatWithNoCapableDeploymentIs400(t *testing.T) {
	t.Setenv("KELVRAN_INTEGRATION_TEST_G16_AKID", "fake-access-key-id-not-a-real-secret")
	t.Setenv("KELVRAN_INTEGRATION_TEST_G16_SAK", "fake-secret-access-key-not-a-real-secret")
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash("test-gateway-key-g16b"), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:               "claude-unsupported",
				Model:              "claude-bedrock",
				Provider:           "bedrock",
				UpstreamModel:      "anthropic.claude-3-5-sonnet-20241022-v2:0", // outside the structured-output whitelist
				BaseURL:            "http://127.0.0.1:1/never-called",
				Region:             "us-east-1",
				AccessKeyIDEnv:     "KELVRAN_INTEGRATION_TEST_G16_AKID",
				SecretAccessKeyEnv: "KELVRAN_INTEGRATION_TEST_G16_SAK",
			},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"claude-bedrock": {PromptPerToken: decimal.RequireFromString("0.000003"), CompletionPerToken: decimal.RequireFromString("0.000015")},
		},
	}
	gw := newIntegrationServerFromConfig(t, cfg)
	resp := postChatWithHeaders(t, gw, "test-gateway-key-g16b", `{"model":"claude-bedrock","messages":[{"role":"user","content":"give me JSON"}],"response_format":{"type":"json_schema","json_schema":{"name":"weather","schema":{"type":"object"}}}}`, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body := decodeAPIErrorResponse(t, resp)
	if body.Error.Type != "invalid_request_error" || body.Error.Code == nil || *body.Error.Code != "response_format_unsupported" {
		t.Errorf("envelope type/code = %q/%v, want invalid_request_error/response_format_unsupported", body.Error.Type, body.Error.Code)
	}
	if body.Error.Param == nil || *body.Error.Param != "response_format" {
		t.Errorf("param = %v, want response_format", body.Error.Param)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After = %q, want none", ra)
	}
}

// decodeAPIErrorResponse is decodeAPIError for a live *http.Response.
func decodeAPIErrorResponse(t *testing.T, resp *http.Response) apiErrorBody {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body apiErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return body
}
