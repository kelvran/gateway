package guardrail

import (
	"context"
	"testing"
)

func TestSSNDetectorTruePositive(t *testing.T) {
	findings, err := SSNDetector{}.Detect(context.Background(), "my ssn is 123-45-6789 for the form")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", findings)
	}
	if findings[0].Category != CategoryGovernmentID {
		t.Errorf("Category = %v, want %v", findings[0].Category, CategoryGovernmentID)
	}
}

func TestSSNDetectorTrueNegative(t *testing.T) {
	findings, err := SSNDetector{}.Detect(context.Background(), "no government identifier appears in this text")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none", findings)
	}
}
