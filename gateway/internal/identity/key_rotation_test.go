package identity

import (
	"errors"
	"testing"
	"time"
)

// TestVerifyAcceptsBothHashesDuringGracePeriod proves the core dual-hash
// mechanism: a VirtualKey with a non-empty PreviousKeyHash authenticates
// via EITHER the current credential (KeyHash) or the prior one
// (PreviousKeyHash), as long as the grace period hasn't elapsed.
func TestVerifyAcceptsBothHashesDuringGracePeriod(t *testing.T) {
	v, err := NewVerifier([]VirtualKey{
		{
			ID:                       "team-alpha",
			KeyHash:                  hashOf("credential-v2"),
			PreviousKeyHash:          hashOf("credential-v1"),
			PreviousKeyHashExpiresAt: time.Now().Add(time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	if vk, err := v.Verify("Bearer credential-v2"); err != nil || vk.ID != "team-alpha" {
		t.Errorf("Verify(credential-v2) = (%v, %v), want (team-alpha, nil)", vk, err)
	}
	if vk, err := v.Verify("Bearer credential-v1"); err != nil || vk.ID != "team-alpha" {
		t.Errorf("Verify(credential-v1), still within grace period = (%v, %v), want (team-alpha, nil)", vk, err)
	}
}

// TestVerifyRejectsPreviousHashOnceGracePeriodElapses proves the prior
// credential fails closed once PreviousKeyHashExpiresAt has passed, with
// no separate cleanup sweep required.
func TestVerifyRejectsPreviousHashOnceGracePeriodElapses(t *testing.T) {
	v, err := NewVerifier([]VirtualKey{
		{
			ID:                       "team-alpha",
			KeyHash:                  hashOf("credential-v2"),
			PreviousKeyHash:          hashOf("credential-v1"),
			PreviousKeyHashExpiresAt: time.Now().Add(-time.Hour), // already elapsed
		},
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	if _, err := v.Verify("Bearer credential-v1"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Verify(credential-v1), grace period elapsed, error = %v, want ErrInvalidKey", err)
	}
	// The current credential must be entirely unaffected by the prior
	// one's expiry.
	if vk, err := v.Verify("Bearer credential-v2"); err != nil || vk.ID != "team-alpha" {
		t.Errorf("Verify(credential-v2) = (%v, %v), want (team-alpha, nil)", vk, err)
	}
}

// TestVerifyWithNoPreviousKeyHashBehavesExactlyAsBefore proves the
// dual-hash mechanism is fully additive -- a VirtualKey that never
// rotated (PreviousKeyHash empty, the zero value) is unaffected.
func TestVerifyWithNoPreviousKeyHashBehavesExactlyAsBefore(t *testing.T) {
	v, err := NewVerifier([]VirtualKey{
		{ID: "team-alpha", KeyHash: hashOf("solo-credential")},
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	if vk, err := v.Verify("Bearer solo-credential"); err != nil || vk.ID != "team-alpha" {
		t.Errorf("Verify(solo-credential) = (%v, %v), want (team-alpha, nil)", vk, err)
	}
	if _, err := v.Verify("Bearer never-configured"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Verify of an unconfigured value, error = %v, want ErrInvalidKey", err)
	}
}

// TestNewVerifierRejectsPreviousKeyHashCollidingWithAnotherKey proves
// PreviousKeyHash is duplicate-checked exactly like KeyHash -- a stale
// prior hash must never be reassignable to a different virtual key while
// its grace period is still running.
func TestNewVerifierRejectsPreviousKeyHashCollidingWithAnotherKey(t *testing.T) {
	_, err := NewVerifier([]VirtualKey{
		{ID: "team-alpha", KeyHash: hashOf("alpha-v2"), PreviousKeyHash: hashOf("shared-value"), PreviousKeyHashExpiresAt: time.Now().Add(time.Hour)},
		{ID: "team-beta", KeyHash: hashOf("shared-value")},
	})
	if !errors.Is(err, ErrDuplicateKeyHash) {
		t.Fatalf("NewVerifier with a PreviousKeyHash colliding with another key's KeyHash, error = %v, want ErrDuplicateKeyHash", err)
	}
}
