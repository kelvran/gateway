package identity

import (
	"errors"
	"testing"
)

func TestVerifyCorrectKeyPasses(t *testing.T) {
	v, err := NewVerifier("secret-key-123")
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if err := v.Verify("Bearer secret-key-123"); err != nil {
		t.Errorf("Verify with correct key returned error: %v", err)
	}
}

func TestVerifyWrongKeyRejected(t *testing.T) {
	v, err := NewVerifier("secret-key-123")
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	err = v.Verify("Bearer wrong-key")
	if !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Verify with wrong key = %v, want ErrInvalidKey", err)
	}
}

func TestVerifyMissingHeaderRejected(t *testing.T) {
	v, err := NewVerifier("secret-key-123")
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	err = v.Verify("")
	if !errors.Is(err, ErrMissingHeader) {
		t.Errorf("Verify with empty header = %v, want ErrMissingHeader", err)
	}
}

func TestVerifyMalformedHeaderRejected(t *testing.T) {
	v, err := NewVerifier("secret-key-123")
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	err = v.Verify("secret-key-123") // missing "Bearer " prefix
	if !errors.Is(err, ErrMissingHeader) {
		t.Errorf("Verify with malformed header = %v, want ErrMissingHeader", err)
	}
}

func TestNewVerifierRejectsEmptyKey(t *testing.T) {
	if _, err := NewVerifier(""); err == nil {
		t.Fatal("NewVerifier(\"\") returned nil error, want an error")
	}
}
