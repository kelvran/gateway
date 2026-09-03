package guardrail

import (
	"context"
	"testing"
)

func TestMod97Valid(t *testing.T) {
	if !mod97Valid("GB29NWBK60161331926819") {
		t.Error("GB29NWBK60161331926819 (a real, well-known valid IBAN) should pass mod-97")
	}
	if mod97Valid("GB29NWBK60161331926810") {
		t.Error("GB29NWBK60161331926810 (last digit changed) should fail mod-97")
	}
}

func TestIBANDetectorTruePositive(t *testing.T) {
	findings, err := IBANDetector{}.Detect(context.Background(), "please wire to GB29NWBK60161331926819 today")
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

// TestIBANDetectorRejectsPatternMatchButChecksumFail is the load-bearing
// proof that the detector is checksum-gated, not pattern-only.
func TestIBANDetectorRejectsPatternMatchButChecksumFail(t *testing.T) {
	findings, err := IBANDetector{}.Detect(context.Background(), "a code like GB29NWBK60161331926810 appears here")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none (pattern matches but mod-97 checksum fails)", findings)
	}
}

func TestIBANDetectorTrueNegative(t *testing.T) {
	findings, err := IBANDetector{}.Detect(context.Background(), "there is no financial identifier in this sentence")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none", findings)
	}
}
