package guardrail

import (
	"context"
	"strings"
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

// TestSSNDetectorZWSPEvasionCaught proves SSNDetector is vulnerable to
// (and, with this fix, no longer defeated by) the identical zero-width-
// space-inside-digit-run evasion documented against CreditCardDetector by
// evals/tests/fixtures/regression_corpus_guardrail.json's regcorpus-
// guardrail-23 case: ssnPattern's three digit-groups are each a
// contiguous \d-run with no tolerance for an injected zero-width
// character in place of a separator, the same structural gap. A real,
// otherwise-valid SSN with a single U+200B substituted for its first
// hyphen must still be caught at the same government_id Block tier.
func TestSSNDetectorZWSPEvasionCaught(t *testing.T) {
	zwsp := string(rune(0x200B))
	text := "my ssn is 123" + zwsp + "45-6789 for the form"
	findings, err := SSNDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1 (ZWSP-inside-digit-run evasion must still be caught)", findings)
	}
	if findings[0].Category != CategoryGovernmentID {
		t.Errorf("Category = %v, want %v", findings[0].Category, CategoryGovernmentID)
	}
	if findings[0].Detector != "ssn" {
		t.Errorf("Detector = %v, want ssn", findings[0].Detector)
	}
}

// TestSSNDetectorCleanBaselineUnaffectedByZWSPFix proves the fix causes
// zero regression on the common, no-evasion case: an ordinary SSN with no
// injected character still produces exactly the same single finding, at
// exactly the same byte offsets, as it did before this fix.
func TestSSNDetectorCleanBaselineUnaffectedByZWSPFix(t *testing.T) {
	text := "my ssn is 123-45-6789 for the form"
	findings, err := SSNDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", findings)
	}
	wantStart := strings.Index(text, "123-45-6789")
	wantEnd := wantStart + len("123-45-6789")
	if findings[0].Start != wantStart || findings[0].End != wantEnd {
		t.Errorf("Start/End = %d/%d, want %d/%d (unchanged from pre-fix behavior)", findings[0].Start, findings[0].End, wantStart, wantEnd)
	}
}

// TestSSNDetectorZWSPEvasionOffsetIndexesOriginalText proves
// Finding.Start/End index into the ORIGINAL (unstripped) text, not the
// internal ZWSP-stripped copy used for matching. The injected character
// replaces the SECOND hyphen (well inside the match, not its very first
// byte), so a naive fix that forgot to remap the match's end would
// produce a visibly wrong End (short by the ZWSP's own 3-byte UTF-8
// width), not a coincidentally correct one.
func TestSSNDetectorZWSPEvasionOffsetIndexesOriginalText(t *testing.T) {
	zwsp := string(rune(0x200B))
	prefix := "Please file this SSN: "
	suffix := " on the paperwork."
	text := prefix + "123-45" + zwsp + "6789" + suffix

	findings, err := SSNDetector{}.Detect(context.Background(), text)
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
	want := "123-45" + zwsp + "6789"
	if got != want {
		t.Errorf("text[Start:End] = %q, want %q (the ZWSP itself must still be inside the reported original-text span)", got, want)
	}
}
