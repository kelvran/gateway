package guardrail

import (
	"context"
	"testing"
)

func TestEmailDetectorTruePositive(t *testing.T) {
	findings, err := EmailDetector{}.Detect(context.Background(), "contact me at jane.doe@example.com please")
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

func TestEmailDetectorTrueNegative(t *testing.T) {
	findings, err := EmailDetector{}.Detect(context.Background(), "this message has no email address in it at all")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none", findings)
	}
}
