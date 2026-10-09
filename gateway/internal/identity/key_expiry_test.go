package identity

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Whole-key expiry (RFC-3 decision 4,
// docs/rfcs/2026-10-09-gateway-kelvran-cli-and-single-user-mode.md):
// Verify rejects a key from its ExpiresAt instant inclusive with a
// KeyExpiredError that matches ErrKeyExpired, carries the id and instant
// for the log line, and never names the key in its message. The clock is
// the Verifier's own now field, overridden here for determinism.

var expiryInstant = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func newExpiringVerifier(t *testing.T, now time.Time) *Verifier {
	t.Helper()
	v, err := NewVerifier([]VirtualKey{{ID: "team-a", KeyHash: HashSecret("pw-one"), ExpiresAt: expiryInstant}})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	v.now = func() time.Time { return now }
	return v
}

func TestVerifyAcceptsAKeyBeforeItsExpiry(t *testing.T) {
	v := newExpiringVerifier(t, expiryInstant.Add(-time.Hour))
	key, err := v.Verify("Bearer pw-one")
	if err != nil || key.ID != "team-a" {
		t.Fatalf("Verify one hour before expiry: key=%v err=%v, want team-a", key, err)
	}
}

func TestVerifyRejectsAKeyFromItsExpiryInstantInclusive(t *testing.T) {
	for name, now := range map[string]time.Time{
		"exactly at expires_at": expiryInstant,
		"one hour after":        expiryInstant.Add(time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			v := newExpiringVerifier(t, now)
			key, err := v.Verify("Bearer pw-one")
			if key != nil || err == nil {
				t.Fatalf("Verify = (%v, %v), want (nil, KeyExpiredError)", key, err)
			}
			if !errors.Is(err, ErrKeyExpired) {
				t.Errorf("errors.Is(err, ErrKeyExpired) = false for %v", err)
			}
			if errors.Is(err, ErrInvalidKey) {
				t.Errorf("an expired key must not read as ErrInvalidKey: %v", err)
			}
			var expired *KeyExpiredError
			if !errors.As(err, &expired) {
				t.Fatalf("errors.As(err, *KeyExpiredError) = false for %T", err)
			}
			if expired.ID != "team-a" || !expired.ExpiresAt.Equal(expiryInstant) {
				t.Errorf("KeyExpiredError = %+v, want id team-a and the configured instant", expired)
			}
			if strings.Contains(err.Error(), "team-a") {
				t.Errorf("the client-facing message names the key: %q", err.Error())
			}
			if err.Error() != ErrKeyExpired.Error() {
				t.Errorf("message = %q, want exactly ErrKeyExpired's text", err.Error())
			}
			wrapped := fmt.Errorf("dataplane: auth: %w", err)
			if !errors.Is(wrapped, ErrKeyExpired) || !errors.As(wrapped, &expired) {
				t.Errorf("wrapping must preserve both errors.Is and errors.As: %v", wrapped)
			}
		})
	}
}

func TestVerifyZeroExpiresAtNeverExpires(t *testing.T) {
	v, err := NewVerifier([]VirtualKey{{ID: "forever", KeyHash: HashSecret("pw-two")}})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	v.now = func() time.Time { return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC) }
	if key, err := v.Verify("Bearer pw-two"); err != nil || key.ID != "forever" {
		t.Fatalf("a key with a zero ExpiresAt must never expire: key=%v err=%v", key, err)
	}
}

// TestVerifyExpiryIsCheckedBeforeRotationGrace: an expired key that is
// mid-rotation rejects its previous secret as expired, not as invalid —
// the expiry check runs on the resolved key before the grace check.
func TestVerifyExpiryIsCheckedBeforeRotationGrace(t *testing.T) {
	v, err := NewVerifier([]VirtualKey{{
		ID:                       "team-b",
		KeyHash:                  HashSecret("v2-secret"),
		PreviousKeyHash:          HashSecret("v1-secret"),
		PreviousKeyHashExpiresAt: expiryInstant.Add(24 * time.Hour),
		ExpiresAt:                expiryInstant,
	}})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	v.now = func() time.Time { return expiryInstant.Add(time.Minute) }
	for _, secret := range []string{"v2-secret", "v1-secret"} {
		if _, err := v.Verify("Bearer " + secret); !errors.Is(err, ErrKeyExpired) {
			t.Errorf("Verify(%s) after expiry = %v, want ErrKeyExpired", secret, err)
		}
	}
}

// TestVerifyGraceCheckUsesTheVerifierClock pins that the pre-existing
// rotation-grace check moved onto the same clock as the expiry check (one
// clock per Verifier) and keeps its inclusive boundary.
func TestVerifyGraceCheckUsesTheVerifierClock(t *testing.T) {
	graceEnd := expiryInstant
	v, err := NewVerifier([]VirtualKey{{
		ID:                       "team-c",
		KeyHash:                  HashSecret("c-v2"),
		PreviousKeyHash:          HashSecret("c-v1"),
		PreviousKeyHashExpiresAt: graceEnd,
	}})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	v.now = func() time.Time { return graceEnd.Add(-time.Second) }
	if key, err := v.Verify("Bearer c-v1"); err != nil || key.ID != "team-c" {
		t.Fatalf("previous secret inside grace: key=%v err=%v", key, err)
	}
	v.now = func() time.Time { return graceEnd }
	if _, err := v.Verify("Bearer c-v1"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("previous secret at the grace instant = %v, want ErrInvalidKey (inclusive boundary)", err)
	}
	if key, err := v.Verify("Bearer c-v2"); err != nil || key.ID != "team-c" {
		t.Errorf("current secret is unaffected by grace expiry: key=%v err=%v", key, err)
	}
}
