package guardrail

import (
	"context"
	"strings"
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

// TestPhoneDetectorZWSPEvasionCaught proves PhoneDetector is vulnerable to
// (and, with this fix, no longer defeated by) the identical zero-width-
// space-inside-digit-run evasion documented against CreditCardDetector by
// evals/tests/fixtures/regression_corpus_guardrail.json's regcorpus-
// guardrail-23 case: phonePattern's digit-groups are each a contiguous
// \d-run with no tolerance for an injected zero-width character in place
// of a separator, the same structural gap. A real phone number with a
// single U+200B substituted for its first hyphen must still be caught at
// the same contact_info Warn tier.
func TestPhoneDetectorZWSPEvasionCaught(t *testing.T) {
	zwsp := string(rune(0x200B))
	text := "call me at 415" + zwsp + "555-0132 tomorrow"
	findings, err := PhoneDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1 (ZWSP-inside-digit-run evasion must still be caught)", findings)
	}
	if findings[0].Category != CategoryContactInfo {
		t.Errorf("Category = %v, want %v", findings[0].Category, CategoryContactInfo)
	}
	if findings[0].Detector != "phone" {
		t.Errorf("Detector = %v, want phone", findings[0].Detector)
	}
}

// TestPhoneDetectorCleanBaselineUnaffectedByZWSPFix proves the fix causes
// zero regression on the common, no-evasion case: an ordinary phone
// number with no injected character still produces exactly the same
// single finding, at exactly the same byte offsets, as it did before
// this fix.
func TestPhoneDetectorCleanBaselineUnaffectedByZWSPFix(t *testing.T) {
	text := "call me at 415-555-0132 tomorrow"
	findings, err := PhoneDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", findings)
	}
	wantStart := strings.Index(text, "415-555-0132")
	wantEnd := wantStart + len("415-555-0132")
	if findings[0].Start != wantStart || findings[0].End != wantEnd {
		t.Errorf("Start/End = %d/%d, want %d/%d (unchanged from pre-fix behavior)", findings[0].Start, findings[0].End, wantStart, wantEnd)
	}
}

// TestPhoneDetectorZWSPEvasionOffsetIndexesOriginalText proves
// Finding.Start/End index into the ORIGINAL (unstripped) text, not the
// internal ZWSP-stripped copy used for matching. The injected character
// replaces the SECOND hyphen (well inside the match, not its very first
// byte), so a naive fix that forgot to remap the match's end would
// produce a visibly wrong End (short by the ZWSP's own 3-byte UTF-8
// width), not a coincidentally correct one.
func TestPhoneDetectorZWSPEvasionOffsetIndexesOriginalText(t *testing.T) {
	zwsp := string(rune(0x200B))
	prefix := "You can reach me at "
	suffix := " after 5pm."
	text := prefix + "415-555" + zwsp + "0132" + suffix

	findings, err := PhoneDetector{}.Detect(context.Background(), text)
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
	want := "415-555" + zwsp + "0132"
	if got != want {
		t.Errorf("text[Start:End] = %q, want %q (the ZWSP itself must still be inside the reported original-text span)", got, want)
	}
}
