package dataplane

import (
	"errors"
	"fmt"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/idempotency"
)

// TestRetryStormEligibilityExcludesRequestFaults pins the G16 consequence
// that follows from outcomeFor: a reused Idempotency-Key, an unenforceable
// response_format, a non-text system part, a lossy Anthropic body and the
// four invalid_prompt_reference causes are the client's own request faults,
// so they carry no Retry-After and never bump the key's backoff — before G16
// the first two classified as upstream errors and did; the rest fell through
// the same way until 2026-10-11.
func TestRetryStormEligibilityExcludesRequestFaults(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("dataplane: idempotency: %w", idempotency.ErrFingerprintMismatch),
		fmt.Errorf("%w: model m", adapter.ErrStructuredOutputUnsupported),
		fmt.Errorf("dataplane: %w", adapter.ErrSystemPartsUnsupported),
		fmt.Errorf("dataplane: %w", &ErrLossyIngressRejected{TopLevelFields: 1, Pointers: []string{"/service_tier"}}),
		ErrPromptAndMessagesBothSet,
		ErrPromptLabelAndVersionBothSet,
		fmt.Errorf("%w: %w", ErrPromptResolutionFailed, errors.New("no such prompt")),
		fmt.Errorf("%w: %w", ErrResolvedPromptContentInvalid, errors.New("bad mime")),
	} {
		if isRetryStormEligible(err) {
			t.Errorf("isRetryStormEligible(%v) = true, want false", err)
		}
	}
	if !isRetryStormEligible(ErrRateLimited) {
		t.Error("isRetryStormEligible(ErrRateLimited) = false, want true")
	}
}
