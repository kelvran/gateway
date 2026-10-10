package dataplane

import (
	"context"
	"net/http"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
)

// On the passthrough path (RFC-1 §5, slice S11a) an anthropic deployment
// receives every anthropic-* request header the client sent, as an open list
// by prefix -- the protocol page forbids allow-listing the values -- with the
// client's anthropic-version replacing the adapter's default; the
// deployment's own x-api-key is the credential, and nothing outside the
// prefix (the client's Authorization, Idempotency-Key, custom headers) is
// forwarded even if it reached the carrier.
func TestSetUpstreamAuthHeadersAnthropicPassthroughForwardsAnthropicHeadersOnly(t *testing.T) {
	dep := Deployment{Name: "claude-primary", Provider: "anthropic", APIKey: testCred("dep-credential")}
	pr := &anthropic.PassthroughRequest{Raw: []byte(`{"model":"m"}`), Headers: http.Header{
		"Anthropic-Beta":         {"beta-a,beta-b"},
		"Anthropic-Version":      {"2024-06-01"},
		"Anthropic-Workspace-Id": {"ws_1"},
		"Authorization":          {"Bearer client-side"},
		"Idempotency-Key":        {"pt-1"},
		"X-Custom":               {"nope"},
	}}
	httpReq, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := setUpstreamAuthHeaders(context.Background(), httpReq, dep, pr, pr.Body()); err != nil {
		t.Fatalf("setUpstreamAuthHeaders: %v", err)
	}
	h := httpReq.Header
	if got := h.Get("x-api-key"); got != testCred("dep-credential") {
		t.Errorf("x-api-key = %q, want the deployment's credential", got)
	}
	if got := h.Get("anthropic-version"); got != "2024-06-01" {
		t.Errorf("anthropic-version = %q, want the client's 2024-06-01 over the default", got)
	}
	if got := h.Get("anthropic-beta"); got != "beta-a,beta-b" {
		t.Errorf("anthropic-beta = %q, want the client's value verbatim", got)
	}
	if got := h.Get("anthropic-workspace-id"); got != "ws_1" {
		t.Errorf("anthropic-workspace-id = %q, want forwarded (open list by prefix)", got)
	}
	for _, name := range []string{"Authorization", "Idempotency-Key", "X-Custom"} {
		if got := h.Get(name); got != "" {
			t.Errorf("%s = %q, want absent upstream", name, got)
		}
	}
}

// A translate-hop request (a canonical *anthropic.Request, no passthrough)
// keeps the adapter's default anthropic-version and no forwarded headers.
func TestSetUpstreamAuthHeadersAnthropicTranslateHopKeepsTheDefaultVersion(t *testing.T) {
	dep := Deployment{Name: "claude-primary", Provider: "anthropic", APIKey: testCred("dep-credential")}
	httpReq, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := setUpstreamAuthHeaders(context.Background(), httpReq, dep, &anthropic.Request{Model: "m"}, []byte(`{}`)); err != nil {
		t.Fatalf("setUpstreamAuthHeaders: %v", err)
	}
	if got := httpReq.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want the default 2023-06-01", got)
	}
	if got := httpReq.Header.Get("anthropic-beta"); got != "" {
		t.Errorf("anthropic-beta = %q, want none on a translate hop without thinking binding", got)
	}
}
