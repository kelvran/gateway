package guardrail

import (
	"context"
	"testing"
)

func TestIPAddressDetectorTruePositiveIPv4(t *testing.T) {
	findings, err := IPAddressDetector{}.Detect(context.Background(), "the server is at 192.168.1.42 right now")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", findings)
	}
	if findings[0].Category != CategoryNetworkID {
		t.Errorf("Category = %v, want %v", findings[0].Category, CategoryNetworkID)
	}
}

func TestIPAddressDetectorTruePositiveIPv6(t *testing.T) {
	findings, err := IPAddressDetector{}.Detect(context.Background(), "connect to 2001:0db8:85a3:0000:0000:8a2e:0370:7334 please")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", findings)
	}
}

func TestIPAddressDetectorRejectsInvalidOctets(t *testing.T) {
	// 999.999.999.999 matches the coarse dotted-quad pre-filter shape
	// but is not a real IP address — net.ParseIP must reject it.
	findings, err := IPAddressDetector{}.Detect(context.Background(), "the value 999.999.999.999 is not a real address")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none (999.999.999.999 is not a valid IP)", findings)
	}
}

func TestIPAddressDetectorTrueNegative(t *testing.T) {
	findings, err := IPAddressDetector{}.Detect(context.Background(), "there is no network identifier in this sentence")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none", findings)
	}
}
