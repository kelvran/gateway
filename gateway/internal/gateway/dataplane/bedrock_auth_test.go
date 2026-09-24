package dataplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
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

	if err := setUpstreamAuthHeaders(context.Background(), httpReq, dep, nil, body); err != nil {
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

	if err := setUpstreamAuthHeaders(context.Background(), httpReq, dep, nil, body); err != nil {
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

		if err := setUpstreamAuthHeaders(context.Background(), httpReq, dep, nil, nil); err != nil {
			t.Fatalf("setUpstreamAuthHeaders(%s): %v", c.provider, err)
		}

		want := c.wantPrefix + key
		if got := httpReq.Header.Get(c.headerName); got != want {
			t.Errorf("%s: header %q = %q, want %q", c.provider, c.headerName, got, want)
		}
	}
}

// TestSetUpstreamAuthHeadersSetsIdempotencyKeyForOpenAI proves OpenAI's
// own outbound calls get a real, content-derived Idempotency-Key, per
// docs/upgrade-research/request-lifecycle-reliability-2026-09-15.md.
func TestSetUpstreamAuthHeadersSetsIdempotencyKeyForOpenAI(t *testing.T) {
	dep := Deployment{Name: "d", Provider: "openai", APIKey: testCred("openai")}
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	httpReq, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	if err := setUpstreamAuthHeaders(context.Background(), httpReq, dep, nil, body); err != nil {
		t.Fatalf("setUpstreamAuthHeaders: %v", err)
	}

	wantHash := sha256.Sum256(body)
	want := hex.EncodeToString(wantHash[:])
	if got := httpReq.Header.Get("Idempotency-Key"); got != want {
		t.Errorf("Idempotency-Key = %q, want the hex-encoded sha256 of body (%q)", got, want)
	}
}

// TestSetUpstreamAuthHeadersIdempotencyKeyIsStableAcrossRetriesWithIdenticalBody
// proves the actual retry-safety property: calling this twice with the
// identical body (exactly what Kelvran's own retry/fallback machinery
// would do against the SAME deployment) produces the identical key.
func TestSetUpstreamAuthHeadersIdempotencyKeyIsStableAcrossRetriesWithIdenticalBody(t *testing.T) {
	dep := Deployment{Name: "d", Provider: "openai", APIKey: testCred("openai")}
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"retry me"}]}`)

	keys := make([]string, 2)
	for i := range keys {
		httpReq, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if err := setUpstreamAuthHeaders(context.Background(), httpReq, dep, nil, body); err != nil {
			t.Fatalf("setUpstreamAuthHeaders: %v", err)
		}
		keys[i] = httpReq.Header.Get("Idempotency-Key")
	}

	if keys[0] == "" || keys[0] != keys[1] {
		t.Errorf("Idempotency-Key across two calls with the identical body = %q, %q, want identical, non-empty values", keys[0], keys[1])
	}
}

// TestSetUpstreamAuthHeadersDoesNotSetIdempotencyKeyForAnthropicOrGeminiOrBedrock
// encodes the verified-live finding (platform.claude.com's own request-
// header reference, 2026-09-16) that Anthropic has no Idempotency-Key-
// shaped header at all, so a future contributor doesn't "helpfully" add
// an invented one without re-verifying — and confirms Gemini/Bedrock,
// which never claimed to support one, stay untouched too.
func TestSetUpstreamAuthHeadersDoesNotSetIdempotencyKeyForAnthropicOrGeminiOrBedrock(t *testing.T) {
	for _, provider := range []string{"anthropic", "gemini", "bedrock"} {
		dep := Deployment{Name: "d", Provider: provider, APIKey: testCred(provider), AccessKeyID: testCred("access-key"), SecretAccessKey: testCred("access-value"), Region: "us-east-1"}
		httpReq, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
		if err != nil {
			t.Fatalf("NewRequest(%s): %v", provider, err)
		}
		if err := setUpstreamAuthHeaders(context.Background(), httpReq, dep, nil, []byte(`{}`)); err != nil {
			t.Fatalf("setUpstreamAuthHeaders(%s): %v", provider, err)
		}
		if got := httpReq.Header.Get("Idempotency-Key"); got != "" {
			t.Errorf("%s: Idempotency-Key = %q, want empty — no verified support for this header", provider, got)
		}
	}
}

// nativeAnthropicRequest builds a real *anthropic.Request the exact way
// callDeployment does -- via the real Adapter.ToProvider, not a hand-
// built literal -- so this test exercises the genuine ToProvider ->
// setUpstreamAuthHeaders wiring end to end, not just setUpstreamAuthHeaders
// in isolation.
func nativeAnthropicRequest(t *testing.T, model, mode string) *anthropic.Request {
	t.Helper()
	req := adapter.ChatRequest{
		Model:               model,
		Messages:            []adapter.Message{{Role: "user", Content: "hi"}},
		ThinkingBindingMode: mode,
	}
	nativeAny, err := anthropic.New().ToProvider(req)
	if err != nil {
		t.Fatalf("anthropic ToProvider: %v", err)
	}
	native, ok := nativeAny.(*anthropic.Request)
	if !ok {
		t.Fatalf("anthropic ToProvider returned %T, want *anthropic.Request", nativeAny)
	}
	return native
}

// TestSetUpstreamAuthHeadersAnthropicBetaDefaultsNonStrictForQualifyingModel
// proves the default-value WIRING mechanism end to end: a ChatRequest
// with ThinkingBindingMode left unset -- the common case, and every
// ChatRequest built before this field existed -- must reach the REAL
// outgoing *http.Request as the thinking-binding-controls-2026-08-01
// anthropic-beta header, with the marshaled body's
// thinking.block_binding.prefix_mismatch_behavior set to Kelvran's own
// non-strict default ("drop_block"), for every model that runs
// Anthropic's preserved-thinking prefix check.
//
// Scope, stated precisely: this is the mechanism Finding 4's own
// live-prompt-mutation reachability scenario DEPENDS ON, but this test
// does not itself drive that scenario -- it never mutates a prompt or
// replays a stale-signed thinking block across two turns. No test in
// this diff does; see this RFC addendum's own "Unresolved Questions" for
// that explicitly-disclosed gap ("proven at the unit/wiring level ...
// but not against a live Anthropic account"). Without the wiring this
// test DOES cover, an admin's ordinary live prompt-template edit between
// the turn a thinking block was signed under and the turn it's replayed
// on would surface Anthropic's own strict-by-default 400 to a caller who
// did nothing wrong -- but proving that this wiring is correct is not
// the same claim as proving the end-to-end scenario was exercised.
func TestSetUpstreamAuthHeadersAnthropicBetaDefaultsNonStrictForQualifyingModel(t *testing.T) {
	for _, model := range []string{"claude-opus-5-5", "claude-fable-5-1"} {
		native := nativeAnthropicRequest(t, model, "")
		body, err := json.Marshal(native)
		if err != nil {
			t.Fatalf("marshaling native request: %v", err)
		}
		dep := Deployment{Name: "d", Provider: "anthropic", APIKey: testCred("anthropic")}
		httpReq, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}

		if err := setUpstreamAuthHeaders(context.Background(), httpReq, dep, native, body); err != nil {
			t.Fatalf("setUpstreamAuthHeaders(%s): %v", model, err)
		}

		if got := httpReq.Header.Get("anthropic-beta"); got != "thinking-binding-controls-2026-08-01" {
			t.Errorf("%s: anthropic-beta header = %q, want %q", model, got, "thinking-binding-controls-2026-08-01")
		}
		var wire struct {
			Thinking struct {
				Type         string `json:"type"`
				BlockBinding struct {
					PrefixMismatchBehavior string `json:"prefix_mismatch_behavior"`
				} `json:"block_binding"`
			} `json:"thinking"`
		}
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatalf("unmarshaling marshaled body: %v", err)
		}
		if wire.Thinking.Type != "adaptive" {
			t.Errorf("%s: thinking.type = %q, want %q", model, wire.Thinking.Type, "adaptive")
		}
		if got := wire.Thinking.BlockBinding.PrefixMismatchBehavior; got != "drop_block" {
			t.Errorf("%s: thinking.block_binding.prefix_mismatch_behavior = %q, want %q", model, got, "drop_block")
		}
	}
}

// TestSetUpstreamAuthHeadersAnthropicBetaStrictOptIn proves the explicit
// per-caller opt-in half of Finding 4's decision: ThinkingBindingMode
// "strict" still sends the beta header (both the field and the response
// array require it), but resolves the wire value to Anthropic's own
// default ("error") instead of Kelvran's non-strict override.
func TestSetUpstreamAuthHeadersAnthropicBetaStrictOptIn(t *testing.T) {
	native := nativeAnthropicRequest(t, "claude-opus-5-5", "strict")
	body, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("marshaling native request: %v", err)
	}
	dep := Deployment{Name: "d", Provider: "anthropic", APIKey: testCred("anthropic")}
	httpReq, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	if err := setUpstreamAuthHeaders(context.Background(), httpReq, dep, native, body); err != nil {
		t.Fatalf("setUpstreamAuthHeaders: %v", err)
	}

	if got := httpReq.Header.Get("anthropic-beta"); got != "thinking-binding-controls-2026-08-01" {
		t.Errorf("anthropic-beta header = %q, want %q (still required to set the strict value at all)", got, "thinking-binding-controls-2026-08-01")
	}
	if native.Thinking == nil || native.Thinking.BlockBinding == nil || native.Thinking.BlockBinding.PrefixMismatchBehavior != "error" {
		t.Errorf("Thinking = %+v, want BlockBinding.PrefixMismatchBehavior = %q", native.Thinking, "error")
	}
}

// TestSetUpstreamAuthHeadersAnthropicBetaOmittedForNonQualifyingModel is
// the regression guard: a model that doesn't run the preserved-thinking
// prefix check at all must get neither the beta header nor a thinking
// field on the wire -- exactly today's pre-existing behavior, unchanged.
func TestSetUpstreamAuthHeadersAnthropicBetaOmittedForNonQualifyingModel(t *testing.T) {
	native := nativeAnthropicRequest(t, "claude-sonnet-5", "")
	body, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("marshaling native request: %v", err)
	}
	dep := Deployment{Name: "d", Provider: "anthropic", APIKey: testCred("anthropic")}
	httpReq, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	if err := setUpstreamAuthHeaders(context.Background(), httpReq, dep, native, body); err != nil {
		t.Fatalf("setUpstreamAuthHeaders: %v", err)
	}

	if got := httpReq.Header.Get("anthropic-beta"); got != "" {
		t.Errorf("anthropic-beta header = %q, want empty for a model that doesn't run the prefix check", got)
	}
	if strings.Contains(string(body), `"thinking"`) {
		t.Errorf("marshaled body = %s, want no \"thinking\" field at all", body)
	}
}
