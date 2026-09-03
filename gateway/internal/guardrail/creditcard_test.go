package guardrail

import (
	"context"
	"testing"
)

func TestLuhnValid(t *testing.T) {
	if !luhnValid("4111111111111111") {
		t.Error("4111111111111111 (the standard Visa test number) should pass Luhn")
	}
	if luhnValid("4111111111111112") {
		t.Error("4111111111111112 (last digit changed) should fail Luhn")
	}
}

func TestCreditCardDetectorTruePositive(t *testing.T) {
	findings, err := CreditCardDetector{}.Detect(context.Background(), "my card number is 4111111111111111 thanks")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", findings)
	}
	if findings[0].Category != CategoryFinancialID {
		t.Errorf("Category = %v, want %v", findings[0].Category, CategoryFinancialID)
	}
}

// TestCreditCardDetectorRejectsPatternMatchButChecksumFail is the
// load-bearing proof that the detector is checksum-gated, not
// pattern-only: a 16-digit run that matches the shape but fails Luhn
// must NOT be reported.
func TestCreditCardDetectorRejectsPatternMatchButChecksumFail(t *testing.T) {
	findings, err := CreditCardDetector{}.Detect(context.Background(), "random number 4111111111111112 appears here")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none (pattern matches but Luhn checksum fails)", findings)
	}
}

func TestCreditCardDetectorTrueNegative(t *testing.T) {
	findings, err := CreditCardDetector{}.Detect(context.Background(), "there is no financial identifier in this sentence")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none", findings)
	}
}
