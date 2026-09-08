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
	"testing"

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
