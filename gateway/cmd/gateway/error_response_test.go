// Direct proof for writeErrorResponse's own DeploymentCapacityError case,
// per docs/upgrade-research/gateway-per-deployment-concurrency-2026-09-09.md:
// a backend-capacity rejection must map to 503, never the 429 bucket
// ErrRateLimited/ErrBudgetExceeded/ErrConcurrencyLimitExceeded already
// share — the caller may be nowhere near its OWN rate limit or
// concurrency cap.
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
)

func TestWriteErrorResponseMapsDeploymentCapacityErrorTo503(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErrorResponse(rec, &dataplane.DeploymentCapacityError{Deployment: "shared", Reason: "concurrency"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestWriteErrorResponseMapsWrappedDeploymentCapacityErrorTo503 proves the
// mapping survives a fmt.Errorf("...: %w", ...) wrap — the shape
// runMissPath's own error return actually produces — not just a bare,
// unwrapped *DeploymentCapacityError.
func TestWriteErrorResponseMapsWrappedDeploymentCapacityErrorTo503(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapped := fmt.Errorf("dataplane: upstream call failed for model %q: %w", "gpt-4o", &dataplane.DeploymentCapacityError{Deployment: "shared", Reason: "rate_limit"})
	writeErrorResponse(rec, wrapped)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestWriteErrorResponseDeploymentCapacityErrorNeverMapsTo429 is the
// negative-space proof: a DeploymentCapacityError must be distinguishable
// from a real client-facing rate-limit rejection at the status-code
// level, since a client polling for a 429/Retry-After signal would
// otherwise misinterpret a backend-capacity condition as its own fault.
func TestWriteErrorResponseDeploymentCapacityErrorNeverMapsTo429(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErrorResponse(rec, &dataplane.DeploymentCapacityError{Deployment: "shared", Reason: "concurrency"})
	if rec.Code == http.StatusTooManyRequests {
		t.Error("status = 429, want anything but 429 — a deployment-capacity rejection is a backend condition, not a per-caller rate-limit decision")
	}
}

// TestWriteErrorResponseRedactsUpstreamHTTPErrorBody is the direct,
// function-level proof for the information-disclosure fix: a bare
// *dataplane.UpstreamHTTPError carrying a realistic Bedrock
// AccessDeniedException body (a real-shaped AWS account ID and IAM role
// ARN) must never appear in the client-facing response body
// writeErrorResponse writes, even though the status code it maps to
// (the http.StatusBadGateway default -- UpstreamHTTPError has no
// dedicated case in writeErrorResponse's switch) is unchanged by this
// fix.
func TestWriteErrorResponseRedactsUpstreamHTTPErrorBody(t *testing.T) {
	arn := "arn:aws:iam::123456789012:role/kelvran-prod-gateway-role"
	upstreamErr := &dataplane.UpstreamHTTPError{
		StatusCode: 403,
		Body:       fmt.Sprintf(`{"message":"User: %s is not authorized to perform: bedrock:InvokeModel on resource: ..."}`, arn),
	}

	rec := httptest.NewRecorder()
	writeErrorResponse(rec, upstreamErr)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d (the status mapping itself must be untouched by this fix)", rec.Code, http.StatusBadGateway)
	}
	body := rec.Body.String()
	if strings.Contains(body, arn) {
		t.Errorf("response body %q contains the raw upstream ARN %q -- the exact information-disclosure bug this fix closes", body, arn)
	}
	if strings.Contains(body, "123456789012") {
		t.Errorf("response body %q contains the raw AWS account ID -- the exact information-disclosure bug this fix closes", body)
	}
	if !strings.Contains(body, "403") {
		t.Errorf("response body %q, want it to still name status 403 -- the status code is safe to disclose, only the body is redacted", body)
	}
}

// TestWriteErrorResponseRedactsWrappedUpstreamHTTPErrorBody proves the
// same redaction survives the real wrap shape runMissPath/
// streamDeploymentWithFallback actually produce
// (fmt.Errorf("...: %w", ...), same convention
// TestWriteErrorResponseMapsWrappedDeploymentCapacityErrorTo503 above
// already proves for DeploymentCapacityError) -- not just a bare,
// unwrapped *UpstreamHTTPError.
func TestWriteErrorResponseRedactsWrappedUpstreamHTTPErrorBody(t *testing.T) {
	arn := "arn:aws:iam::123456789012:role/kelvran-prod-gateway-role"
	upstreamErr := &dataplane.UpstreamHTTPError{
		StatusCode: 403,
		Body:       fmt.Sprintf(`{"message":"User: %s is not authorized to perform: bedrock:InvokeModel on resource: ..."}`, arn),
	}
	wrapped := fmt.Errorf("dataplane: upstream call failed for model %q: %w", "claude-3-sonnet", upstreamErr)

	rec := httptest.NewRecorder()
	writeErrorResponse(rec, wrapped)

	body := rec.Body.String()
	if strings.Contains(body, arn) {
		t.Errorf("response body %q contains the raw upstream ARN %q through a wrapped error -- errors.As must still find the *UpstreamHTTPError and redact it", body, arn)
	}
}

// TestWriteErrorResponseRedactsUpstreamStreamErrorBody is the direct,
// function-level proof that the mid-stream-decode information-disclosure
// gap (UpstreamHTTPError only covers a non-2xx HTTP response; it never
// fires for an in-band error arriving on an already-2xx streaming
// connection) is closed: a bare *adapter.UpstreamStreamError carrying a
// realistic Bedrock throttling-exception payload (a real-shaped internal
// detail) must never appear in the client-facing response body
// writeErrorResponse writes.
func TestWriteErrorResponseRedactsUpstreamStreamErrorBody(t *testing.T) {
	secret := "internal-shard-XYZZY123"
	streamErr := &adapter.UpstreamStreamError{
		Provider: "bedrock",
		Raw:      fmt.Sprintf(`{"message":"...%s..."}`, secret),
	}

	rec := httptest.NewRecorder()
	writeErrorResponse(rec, streamErr)

	body := rec.Body.String()
	if strings.Contains(body, secret) {
		t.Errorf("response body %q contains the raw upstream payload %q -- the exact information-disclosure bug this fix closes", body, secret)
	}
	if !strings.Contains(body, "bedrock") {
		t.Errorf("response body %q, want it to still name the provider -- the provider name is safe to disclose, only the raw payload is redacted", body)
	}
}

// TestWriteErrorResponseRedactsWrappedUpstreamStreamErrorBody proves the
// same redaction survives the real wrap shape this codebase's own
// dataplane package actually produces for this error class:
// streamDeployment/streamDeploymentBedrock wrap the decoder's returned
// error with fmt.Errorf("decoding stream from deployment %q: %w", ...),
// and HandleChatCompletionStream wraps that again with
// fmt.Errorf("dataplane: streaming upstream call failed for model %q:
// %w", ...) before it ever reaches writeErrorResponse -- not just a
// bare, unwrapped *UpstreamStreamError.
func TestWriteErrorResponseRedactsWrappedUpstreamStreamErrorBody(t *testing.T) {
	hostname := "internal-vllm-shard-07.corp.internal"
	streamErr := &adapter.UpstreamStreamError{
		Provider: "openaicompat",
		Raw:      fmt.Sprintf(`upstream stream error (server_error): connection refused to %s`, hostname),
	}
	decodeWrapped := fmt.Errorf("decoding stream from deployment %q: %w", "self-hosted-vllm", streamErr)
	wrapped := fmt.Errorf("dataplane: streaming upstream call failed for model %q: %w", "llama-3.1-70b", decodeWrapped)

	rec := httptest.NewRecorder()
	writeErrorResponse(rec, wrapped)

	body := rec.Body.String()
	if strings.Contains(body, hostname) {
		t.Errorf("response body %q contains the raw internal hostname %q through a wrapped mid-stream decode error -- errors.As must still find the *UpstreamStreamError and redact it", body, hostname)
	}
}
