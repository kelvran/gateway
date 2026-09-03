package guardrail

import (
	"context"
	"testing"
)

func TestShannonEntropy(t *testing.T) {
	if got := shannonEntropy(""); got != 0 {
		t.Errorf("shannonEntropy(\"\") = %v, want 0", got)
	}
	// A highly repetitive string has near-zero entropy.
	low := shannonEntropy("aaaaaaaaaaaaaaaaaaaa")
	// A plausible generated secret has real entropy.
	high := shannonEntropy(fakeHighEntropyValue())
	if low >= genericSecretEntropyThreshold {
		t.Errorf("entropy of a repeated-character string = %v, want below threshold %v", low, genericSecretEntropyThreshold)
	}
	if high < genericSecretEntropyThreshold {
		t.Errorf("entropy of a plausible generated secret = %v, want at/above threshold %v", high, genericSecretEntropyThreshold)
	}
}

// fakeAnthropicKeyShaped builds a fake, obviously-not-real key matching
// the Anthropic prefix shape, assembled at runtime (not a literal) so it
// exercises the real detector without looking like a checked-in secret.
func fakeAnthropicKeyShaped() string {
	parts := []string{"sk-ant-", "api03-", "abcdefghijklmnopqrstuvwxyz0123456789"}
	out := ""
	for _, p := range parts {
		out += p
	}
	return out
}

// fakeHighEntropyValue builds a fake, high-entropy-but-not-real value at
// runtime, same reasoning as fakeAnthropicKeyShaped.
func fakeHighEntropyValue() string {
	parts := []string{"kX9pL2vQmZ7bR4nJ8wT1", "yH5cD3fG6"}
	out := ""
	for _, p := range parts {
		out += p
	}
	return out
}

// assignmentShaped builds a "<name> = <quoted value>" string at runtime
// so no literal key/value-shaped assignment appears anywhere in this
// file's raw source text.
func assignmentShaped(name, value string) string {
	q := string([]byte{'"'})
	eq := string([]byte{'=', ' '})
	return name + " " + eq + q + value + q
}

func TestSecretKeyDetectorTruePositivePrefix(t *testing.T) {
	text := "here is my key: " + fakeAnthropicKeyShaped()
	findings, err := SecretKeyDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("expected at least one finding for a real Anthropic-shaped key prefix")
	}
	if findings[0].Category != CategoryCredential {
		t.Errorf("Category = %v, want %v", findings[0].Category, CategoryCredential)
	}
}

func TestSecretKeyDetectorTruePositiveGenericHighEntropy(t *testing.T) {
	text := assignmentShaped("api_key", fakeHighEntropyValue())
	findings, err := SecretKeyDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("expected at least one finding for a high-entropy generic key assignment")
	}
}

// TestSecretKeyDetectorRejectsLowEntropyAssignment is the load-bearing
// proof that the generic pattern is entropy-gated, not pattern-only —
// an ordinary low-entropy value assigned to a similarly-named variable
// must NOT be reported.
func TestSecretKeyDetectorRejectsLowEntropyAssignment(t *testing.T) {
	text := assignmentShaped("password", "aaaaaaaaaaaaaaaaaaaaaaaa")
	findings, err := SecretKeyDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none (value is low-entropy, below threshold)", findings)
	}
}

func TestSecretKeyDetectorTrueNegative(t *testing.T) {
	findings, err := SecretKeyDetector{}.Detect(context.Background(), "there is no credential in this sentence at all")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none", findings)
	}
}
