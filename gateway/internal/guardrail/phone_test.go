package guardrail

import (
	"context"
	"testing"
)

func TestPhoneDetectorTruePositive(t *testing.T) {
	findings, err := PhoneDetector{}.Detect(context.Background(), "call me at 415-555-0132 tomorrow")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", findings)
	}
	if findings[0].Category != CategoryContactInfo {
		t.Errorf("Category = %v, want %v", findings[0].Category, CategoryContactInfo)
	}
}

func TestPhoneDetectorTrueNegative(t *testing.T) {
	findings, err := PhoneDetector{}.Detect(context.Background(), "there is no phone number in this sentence")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none", findings)
	}
}
