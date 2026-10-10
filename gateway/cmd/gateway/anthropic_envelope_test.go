package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
)

func TestAnthropicErrorTypeForStatus(t *testing.T) {
	for status, want := range map[int]string{
		400: "invalid_request_error", 422: "invalid_request_error", 405: "invalid_request_error",
		401: "authentication_error", 403: "permission_error", 404: "not_found_error",
		413: "request_too_large", 429: "rate_limit_error",
		500: "api_error", 501: "api_error", 502: "api_error", 503: "api_error",
	} {
		if got := anthropicErrorType(status); got != want {
			t.Errorf("anthropicErrorType(%d) = %q, want %q", status, got, want)
		}
	}
}

// TestWriteAnthropicErrorKeepsCodeParamAndRetryAfter: the Anthropic envelope
// is Kelvran's status and code vocabulary under Anthropic's type names, with
// param and Retry-After exactly as the OpenAI envelope carries them.
func TestWriteAnthropicErrorKeepsCodeParamAndRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAnthropicError(rec, &dataplane.ErrLossyIngressRejected{Pointers: []string{"/foo", "/thinking/display"}, TopLevelFields: 1, NestedCount: 1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type, Message, Code, Param string
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, rec.Body.String())
	}
	if env.Type != "error" || env.Error.Type != "invalid_request_error" || env.Error.Code != "lossy_ingress_rejected" || env.Error.Param != "/foo,/thinking/display" {
		t.Errorf("envelope = %+v", env)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	// Retry-After itself is proven through the real pipeline in
	// TestIntegrationMessagesRetryAfterRows (RetryAfterError's inner error is
	// unexported, so a bare literal here would have no message).
}

func TestWriteAnthropicStatusRequestTooLarge(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAnthropicStatus(rec, http.StatusRequestEntityTooLarge, "request_too_large", "", "request body too large")
	var env struct {
		Error struct{ Type, Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || rec.Code != 413 || env.Error.Type != "request_too_large" || env.Error.Code != "request_too_large" {
		t.Errorf("413 = %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
}

// TestAnthropicClientMessagePrefixesPromptTooLong: an upstream context-window
// rejection carries the capability_rejected token Claude Code matches on;
// everything else is the same redacted text the OpenAI envelope carries.
func TestAnthropicClientMessagePrefixesPromptTooLong(t *testing.T) {
	tooLong := &dataplane.UpstreamHTTPError{StatusCode: 400, Body: `{"error":{"message":"prompt is too long: 9000 tokens"}}`}
	if got := anthropicClientMessage(tooLong); got != "capability_rejected: prompt_too_long: "+clientSafeMessage(tooLong) {
		t.Errorf("message = %q", got)
	}
	other := &dataplane.UpstreamHTTPError{StatusCode: 500, Body: "boom"}
	if got := anthropicClientMessage(other); got != clientSafeMessage(other) {
		t.Errorf("message = %q, want the redacted text unchanged", got)
	}
	if got := anthropicClientMessage(errors.New("plain")); got != "plain" {
		t.Errorf("message = %q", got)
	}
}
