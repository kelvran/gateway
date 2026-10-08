package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
)

// TestWriteErrorResponseRedactsTransportFailureBehindUpstreamCallFailed pins
// the 2026-10-08 fix for an information leak found while writing
// docs/operations/FAILURE-MODES.md: a non-HTTP upstream failure (refused dial,
// timeout) has no *UpstreamHTTPError in its chain, so clientSafeMessage
// returned err.Error() verbatim -- including the deployment's internal
// BaseURL and host:port. Every upstream-call failure now wraps
// dataplane.ErrUpstreamCallFailed, and that alone must redact the message.
func TestWriteErrorResponseRedactsTransportFailureBehindUpstreamCallFailed(t *testing.T) {
	transport := fmt.Errorf("upstream call to deployment %q: calling upstream %q: Post %q: dial tcp 10.0.0.5:8000: connect: connection refused",
		"internal-vllm", "http://10.0.0.5:8000", "http://10.0.0.5:8000/v1/chat/completions")
	for _, streaming := range []bool{false, true} {
		err := dataplane.WrapUpstreamCallFailed("gpt-4o", streaming, transport)
		if !errors.Is(err, dataplane.ErrUpstreamCallFailed) {
			t.Fatalf("WrapUpstreamCallFailed result does not satisfy errors.Is(ErrUpstreamCallFailed)")
		}
		rec := httptest.NewRecorder()
		writeErrorResponse(rec, err)
		if rec.Code != http.StatusBadGateway {
			t.Errorf("streaming=%v: status = %d, want 502", streaming, rec.Code)
		}
		body := rec.Body.String()
		for _, leak := range []string{"10.0.0.5", "8000", "dial tcp", "http://", "internal-vllm", "connection refused"} {
			if strings.Contains(body, leak) {
				t.Errorf("streaming=%v: response body leaks %q: %s", streaming, leak, body)
			}
		}
		if !strings.Contains(body, `"code":"upstream_error"`) || !strings.Contains(body, "gpt-4o") {
			t.Errorf("streaming=%v: body = %s, want code upstream_error and the client-supplied model name", streaming, body)
		}
	}
}

// TestClientSafeMessageKeepsTypedUpstreamMessagesInsideTheWrapper: the wrapper
// must not hide the two messages that were already client-safe.
func TestClientSafeMessageKeepsTypedUpstreamMessagesInsideTheWrapper(t *testing.T) {
	httpErr := &dataplane.UpstreamHTTPError{StatusCode: 429, Body: `{"error":"arn:aws:bedrock:us-east-1:123456789012:inference-profile/x throttled"}`}
	got := clientSafeMessage(dataplane.WrapUpstreamCallFailed("gpt-4o", false, httpErr))
	if got != httpErr.ClientSafeMessage() {
		t.Errorf("clientSafeMessage = %q, want the UpstreamHTTPError's own client-safe message %q", got, httpErr.ClientSafeMessage())
	}
	streamErr := &adapter.UpstreamStreamError{Provider: "openaicompat", Raw: "upstream stream error: connection refused to internal-vllm-07.corp"}
	got = clientSafeMessage(dataplane.WrapUpstreamCallFailed("gpt-4o", true, fmt.Errorf("decoding stream from deployment %q: %w", "self-hosted", streamErr)))
	if got != streamErr.ClientSafeMessage() {
		t.Errorf("clientSafeMessage = %q, want the UpstreamStreamError's own client-safe message %q", got, streamErr.ClientSafeMessage())
	}
	// Kelvran's own capacity decision keeps its reason but, like every other
	// client-facing message, no longer names the operator's deployment.
	capErr := &dataplane.DeploymentCapacityError{Deployment: "shared-internal", Reason: "rate_limit"}
	got = clientSafeMessage(dataplane.WrapUpstreamCallFailed("gpt-4o", false, capErr))
	if !strings.Contains(got, "capacity") || !strings.Contains(got, "rate_limit") || strings.Contains(got, "shared-internal") {
		t.Errorf("clientSafeMessage for a wrapped DeploymentCapacityError = %q, want the capacity reason without the deployment name", got)
	}
	// Sentinels raised inside the upstream stage map to their own status, so
	// the generic redaction (reserved for the 502 default) must not touch them.
	for _, tc := range []struct {
		name   string
		err    error
		keep   string
		status int
	}{
		{"embeddings not configured", fmt.Errorf("%w: provider %q", dataplane.ErrEmbeddingsNotConfigured, "openai"), "not configured", http.StatusNotImplemented},
		{"streaming not supported", fmt.Errorf("%w: provider %q", dataplane.ErrStreamingNotSupported, "gemini"), "streaming", http.StatusBadRequest},
	} {
		wrapped := dataplane.WrapUpstreamCallFailed("gpt-4o", false, tc.err)
		if got := clientSafeMessage(wrapped); !strings.Contains(got, tc.keep) {
			t.Errorf("%s: clientSafeMessage = %q, want the sentinel's own text kept (containing %q)", tc.name, got, tc.keep)
		}
		if got := errorStatus(wrapped); got != tc.status {
			t.Errorf("%s: errorStatus = %d, want %d", tc.name, got, tc.status)
		}
	}
}
