package dataplane

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// testCred builds a placeholder credential-shaped string for tests --
// computed, not a literal, so nothing resembling a real secret ever
// appears as source text. Only this test's own signer math cares about
// the value; no real AWS account or key format is implied.
func testCred(label string) string {
	return "not-a-real-" + label + "-" + strings.Repeat("x", 12)
}

// TestSetUpstreamAuthHeadersBedrockSignsRealSigV4Headers proves a real
// AWS SigV4 signature is genuinely computed and attached -- not just
// that the function returns nil error. Confirmed against real
// aws-sdk-go-v2 source that Signer.SignHTTP sets the Authorization/
// X-Amz-Date headers (and X-Amz-Security-Token when a session token is
// present) itself; this test proves that behavior is reachable through
// this codebase's own call site, per
// docs/rfcs/2026-09-04-bedrock-adapter.md.
func TestSetUpstreamAuthHeadersBedrockSignsRealSigV4Headers(t *testing.T) {
	dep := Deployment{
		Name:            "bedrock-primary",
		Provider:        "bedrock",
		AccessKeyID:     testCred("access-key"),
		SecretAccessKey: testCred("access-value"),
		Region:          "us-east-1",
	}
	body := []byte(`{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`)

	httpReq, err := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	if err := setUpstreamAuthHeaders(context.Background(), httpReq, dep, body); err != nil {
		t.Fatalf("setUpstreamAuthHeaders: %v", err)
	}

	auth := httpReq.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256") {
		t.Errorf("Authorization = %q, want it to start with AWS4-HMAC-SHA256", auth)
	}
	if !strings.Contains(auth, bedrockSigningName) {
		t.Errorf("Authorization = %q, want it to reference the real signing name %q", auth, bedrockSigningName)
	}
	if httpReq.Header.Get("X-Amz-Date") == "" {
		t.Error("X-Amz-Date header is empty, want a real signing timestamp")
	}
}

// TestBedrockSigningNameIsTheRealLiveVerifiedValue pins the literal
// string, not just a reference to the constant -- a prior value
// ("amazonbedrockfrontendservice") was confirmed only against static SDK
// source reading and passed every existing test (which only checks
// self-consistency against the constant, never against a real signature)
// while failing every real, credentialed call with a live AWS 403
// ("Credential should be scoped to correct service: 'bedrock'."), caught
// only by an actual end-to-end call against real AWS Bedrock
// (2026-09-11). This test cannot re-verify the live claim itself (no
// credentials in CI), but it stops the exact silent-regression shape
// that let the wrong value survive an entire test suite once already.
func TestBedrockSigningNameIsTheRealLiveVerifiedValue(t *testing.T) {
	const wantLiveVerified = "bedrock"
	if bedrockSigningName != wantLiveVerified {
		t.Errorf("bedrockSigningName = %q, want %q (live-verified against a real AWS Bedrock Converse call -- see dataplane.go's doc comment)", bedrockSigningName, wantLiveVerified)
	}
}

// TestSetUpstreamAuthHeadersBedrockIncludesSessionToken proves a session
// value, when present, is genuinely signed in (X-Amz-Security-Token) --
// not silently dropped.
func TestSetUpstreamAuthHeadersBedrockIncludesSessionToken(t *testing.T) {
	sessionValue := testCred("session-value")
	dep := Deployment{
		Name:            "bedrock-primary",
		Provider:        "bedrock",
		AccessKeyID:     testCred("access-key"),
		SecretAccessKey: testCred("access-value"),
		SessionToken:    sessionValue,
		Region:          "us-east-1",
	}
	body := []byte(`{}`)

	httpReq, err := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	if err := setUpstreamAuthHeaders(context.Background(), httpReq, dep, body); err != nil {
		t.Fatalf("setUpstreamAuthHeaders: %v", err)
	}

	if got := httpReq.Header.Get("X-Amz-Security-Token"); got != sessionValue {
		t.Errorf("X-Amz-Security-Token = %q, want the real session value", got)
	}
}

// TestSetUpstreamAuthHeadersNonBedrockProvidersUnchanged is the decisive
// backward-compatibility proof: every provider that isn't bedrock keeps
// its exact pre-existing auth-header behavior, and setUpstreamAuthHeaders
// never errors for them.
func TestSetUpstreamAuthHeadersNonBedrockProvidersUnchanged(t *testing.T) {
	cases := []struct {
		provider   string
		headerName string
		wantPrefix string
	}{
		{"anthropic", "x-api-key", ""},
		{"gemini", "x-goog-api-key", ""},
		{"openai", "Authorization", "Bearer "},
		{"openaicompat", "Authorization", "Bearer "},
	}

	for _, c := range cases {
		key := testCred(c.provider)
		dep := Deployment{Name: "d", Provider: c.provider, APIKey: key}
		httpReq, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
		if err != nil {
			t.Fatalf("NewRequest(%s): %v", c.provider, err)
		}

		if err := setUpstreamAuthHeaders(context.Background(), httpReq, dep, nil); err != nil {
			t.Fatalf("setUpstreamAuthHeaders(%s): %v", c.provider, err)
		}

		want := c.wantPrefix + key
		if got := httpReq.Header.Get(c.headerName); got != want {
			t.Errorf("%s: header %q = %q, want %q", c.provider, c.headerName, got, want)
		}
	}
}
