package dataplane

import (
	"fmt"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/idempotency"
)

// TestRetryStormEligibilityExcludesRequestFaults pins the G16 consequence
// that follows from outcomeFor: a reused Idempotency-Key and an unenforceable
// response_format are the client's own request faults, so they carry no
// Retry-After and never bump the key's backoff — before G16 both classified as
// upstream errors and did.
func TestRetryStormEligibilityExcludesRequestFaults(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("dataplane: idempotency: %w", idempotency.ErrFingerprintMismatch),
		fmt.Errorf("%w: model m", adapter.ErrStructuredOutputUnsupported),
	} {
		if isRetryStormEligible(err) {
			t.Errorf("isRetryStormEligible(%v) = true, want false", err)
		}
	}
	if !isRetryStormEligible(ErrRateLimited) {
		t.Error("isRetryStormEligible(ErrRateLimited) = false, want true")
	}
}
