package guardrail

import (
	"context"
	"strings"
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

// TestIPAddressDetectorZWSPEvasionCaught is a round-4 backlog-audit
// finding: net.ParseIP requires an exact, contiguous valid address, so
// a single zero-width space (U+200B) inserted mid-address means
// ipCandidatePattern's coarse pre-filter never even reaches the ParseIP
// step -- the identical evasion class already fixed for phone/
// creditcard/ssn/iban/secretkey (regcorpus-guardrail-23). Sanity-checked
// by breaking: with Detect() temporarily reverted to match against text
// directly instead of stripHiddenUnicode(text), this test failed with
// zero findings -- the right reason -- before the fix was restored.
func TestIPAddressDetectorZWSPEvasionCaught(t *testing.T) {
	zwsp := string(rune(0x200B))
	text := "the server is at 203.0" + zwsp + ".113.42 right now"

	findings, err := IPAddressDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1 (ZWSP-inside-address evasion must still be caught)", findings)
	}
	if findings[0].Category != CategoryNetworkID {
		t.Errorf("Category = %v, want %v", findings[0].Category, CategoryNetworkID)
	}
}

// TestIPAddressDetectorZWSPEvasionOffsetIndexesOriginalText proves
// Finding.Start/End index into the ORIGINAL (unstripped) text, not the
// internal ZWSP-stripped copy used for matching -- the ZWSP is placed
// well inside the match, not at its first byte, so a naive fix that
// forgot to remap the match's end would produce a visibly wrong (short)
// End, not a coincidentally correct one.
func TestIPAddressDetectorZWSPEvasionOffsetIndexesOriginalText(t *testing.T) {
	zwsp := string(rune(0x200B))
	prefix := "Please connect to "
	suffix := " today, thanks."
	text := prefix + "203.0" + zwsp + ".113.42" + suffix

	findings, err := IPAddressDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", findings)
	}

	wantStart := len(prefix)
	wantEnd := len(text) - len(suffix)
	if findings[0].Start != wantStart {
		t.Errorf("Start = %d, want %d (byte offset in the ORIGINAL text)", findings[0].Start, wantStart)
	}
	if findings[0].End != wantEnd {
		t.Errorf("End = %d, want %d (byte offset in the ORIGINAL text) -- a naive unmapped fix would report %d (short by the ZWSP's own byte width)", findings[0].End, wantEnd, wantEnd-len(zwsp))
	}
	got := text[findings[0].Start:findings[0].End]
	want := "203.0" + zwsp + ".113.42"
	if got != want {
		t.Errorf("text[Start:End] = %q, want %q (the ZWSP itself must still be inside the reported original-text span)", got, want)
	}
}

// TestIPAddressDetectorCleanBaselineUnaffectedByZWSPFix proves zero
// regression on the common, no-evasion case.
func TestIPAddressDetectorCleanBaselineUnaffectedByZWSPFix(t *testing.T) {
	text := "the server is at 192.168.1.42 right now"
	findings, err := IPAddressDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	wantStart := strings.Index(text, "192.168.1.42")
	wantEnd := wantStart + len("192.168.1.42")
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", findings)
	}
	if findings[0].Start != wantStart || findings[0].End != wantEnd {
		t.Errorf("Start/End = %d/%d, want %d/%d (unchanged from pre-fix behavior)", findings[0].Start, findings[0].End, wantStart, wantEnd)
	}
}
