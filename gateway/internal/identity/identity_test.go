package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

// hashOf is the test-only equivalent of what an operator does once with
// `openssl rand -hex 32 | sha256sum` per docs/rfcs/2026-09-02-virtual-keys-budgets.md.
func hashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func TestVerifyResolvesCorrectKeyAmongMultiple(t *testing.T) {
	v, err := NewVerifier([]VirtualKey{
		{ID: "team-alpha", KeyHash: hashOf("alpha-secret")},
		{ID: "team-beta", KeyHash: hashOf("beta-secret")},
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	vk, err := v.Verify("Bearer alpha-secret")
	if err != nil {
		t.Fatalf("Verify(alpha-secret) error = %v", err)
	}
	if vk.ID != "team-alpha" {
		t.Errorf("Verify(alpha-secret).ID = %q, want %q", vk.ID, "team-alpha")
	}

	vk, err = v.Verify("Bearer beta-secret")
	if err != nil {
		t.Fatalf("Verify(beta-secret) error = %v", err)
	}
	if vk.ID != "team-beta" {
		t.Errorf("Verify(beta-secret).ID = %q, want %q", vk.ID, "team-beta")
	}
}

func TestVerifyPreservesConfiguredFields(t *testing.T) {
	v, err := NewVerifier([]VirtualKey{
		{
			ID:              "team-alpha",
			KeyHash:         hashOf("alpha-secret"),
			BudgetUSD:       50,
			AllowedModels:   map[string]struct{}{"gpt-4o": {}},
			RateLimitBurst:  10,
			RateLimitRefill: 5,
		},
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	vk, err := v.Verify("Bearer alpha-secret")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if vk.BudgetUSD != 50 {
		t.Errorf("BudgetUSD = %v, want 50", vk.BudgetUSD)
	}
	if _, ok := vk.AllowedModels["gpt-4o"]; !ok {
		t.Errorf("AllowedModels = %v, want it to contain gpt-4o", vk.AllowedModels)
	}
	if vk.RateLimitBurst != 10 || vk.RateLimitRefill != 5 {
		t.Errorf("RateLimitBurst/Refill = %v/%v, want 10/5", vk.RateLimitBurst, vk.RateLimitRefill)
	}
}

func TestVerifyUnknownTokenRejected(t *testing.T) {
	v, err := NewVerifier([]VirtualKey{{ID: "team-alpha", KeyHash: hashOf("alpha-secret")}})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	_, err = v.Verify("Bearer some-other-token")
	if !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Verify with unknown token = %v, want ErrInvalidKey", err)
	}
}

func TestVerifyMissingHeaderRejected(t *testing.T) {
	v, err := NewVerifier([]VirtualKey{{ID: "team-alpha", KeyHash: hashOf("alpha-secret")}})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	_, err = v.Verify("")
	if !errors.Is(err, ErrMissingHeader) {
		t.Errorf("Verify with empty header = %v, want ErrMissingHeader", err)
	}
}

func TestVerifyMalformedHeaderRejected(t *testing.T) {
	v, err := NewVerifier([]VirtualKey{{ID: "team-alpha", KeyHash: hashOf("alpha-secret")}})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	_, err = v.Verify("alpha-secret") // missing "Bearer " prefix
	if !errors.Is(err, ErrMissingHeader) {
		t.Errorf("Verify with malformed header = %v, want ErrMissingHeader", err)
	}
}

func TestNewVerifierRejectsEmptySlice(t *testing.T) {
	if _, err := NewVerifier(nil); err == nil {
		t.Fatal("NewVerifier(nil) returned nil error, want an error")
	}
	if _, err := NewVerifier([]VirtualKey{}); err == nil {
		t.Fatal("NewVerifier([]VirtualKey{}) returned nil error, want an error")
	}
}

func TestNewVerifierRejectsEmptyID(t *testing.T) {
	_, err := NewVerifier([]VirtualKey{{ID: "", KeyHash: hashOf("secret")}})
	if err == nil {
		t.Fatal("NewVerifier with empty ID returned nil error, want an error")
	}
}

func TestNewVerifierRejectsMalformedHash(t *testing.T) {
	tests := []struct {
		name string
		hash string
	}{
		{"not hex", "not-valid-hex-at-all!!"},
		{"wrong length", "abcd"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewVerifier([]VirtualKey{{ID: "team-alpha", KeyHash: tt.hash}})
			if err == nil {
				t.Fatalf("NewVerifier with hash %q returned nil error, want an error", tt.hash)
			}
		})
	}
}

func TestNewVerifierRejectsDuplicateID(t *testing.T) {
	_, err := NewVerifier([]VirtualKey{
		{ID: "team-alpha", KeyHash: hashOf("secret-one")},
		{ID: "team-alpha", KeyHash: hashOf("secret-two")},
	})
	if err == nil {
		t.Fatal("NewVerifier with duplicate ID returned nil error, want an error")
	}
}

func TestNewVerifierRejectsDuplicateHash(t *testing.T) {
	_, err := NewVerifier([]VirtualKey{
		{ID: "team-alpha", KeyHash: hashOf("same-secret")},
		{ID: "team-beta", KeyHash: hashOf("same-secret")},
	})
	if !errors.Is(err, ErrDuplicateKeyHash) {
		t.Errorf("NewVerifier with duplicate hash = %v, want ErrDuplicateKeyHash", err)
	}
}

func TestNewVerifierNormalizesHashCasing(t *testing.T) {
	// A hash written in uppercase in config must still resolve correctly
	// and must still collide-detect against a lowercase duplicate — hash
	// comparison must never depend on the config file's own casing.
	lower := hashOf("alpha-secret")
	upper := ""
	for _, r := range lower {
		if r >= 'a' && r <= 'f' {
			upper += string(r - 'a' + 'A')
		} else {
			upper += string(r)
		}
	}

	v, err := NewVerifier([]VirtualKey{{ID: "team-alpha", KeyHash: upper}})
	if err != nil {
		t.Fatalf("NewVerifier with uppercase hash: %v", err)
	}
	if _, err := v.Verify("Bearer alpha-secret"); err != nil {
		t.Errorf("Verify against an uppercase-configured hash failed: %v", err)
	}

	_, err = NewVerifier([]VirtualKey{
		{ID: "team-alpha", KeyHash: lower},
		{ID: "team-beta", KeyHash: upper},
	})
	if !errors.Is(err, ErrDuplicateKeyHash) {
		t.Errorf("NewVerifier with same hash in different casing = %v, want ErrDuplicateKeyHash", err)
	}
}
