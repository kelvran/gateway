package guardrail

import (
	"context"
	"strings"
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

// TestIBANDetectorZWSPEvasionCaught proves IBANDetector is vulnerable to
// (and, with this fix, no longer defeated by) the identical zero-width-
// space-inside-run evasion documented against CreditCardDetector by
// evals/tests/fixtures/regression_corpus_guardrail.json's regcorpus-
// guardrail-23 case: ibanPattern's country/check-digit/BBAN run is one
// contiguous character class with no tolerance for an injected zero-
// width character, the same structural gap. A real, mod-97-valid IBAN
// with a single U+200B inserted mid-run must still be caught at the same
// financial_id Block tier.
func TestIBANDetectorZWSPEvasionCaught(t *testing.T) {
	zwsp := string(rune(0x200B))
	text := "please wire to GB29NWBK6016" + zwsp + "1331926819 today"
	findings, err := IBANDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1 (ZWSP-inside-run evasion must still be caught)", findings)
	}
	if findings[0].Category != CategoryFinancialID {
		t.Errorf("Category = %v, want %v", findings[0].Category, CategoryFinancialID)
	}
	if findings[0].Detector != "iban" {
		t.Errorf("Detector = %v, want iban", findings[0].Detector)
	}
}

// TestIBANDetectorCleanBaselineUnaffectedByZWSPFix proves the fix causes
// zero regression on the common, no-evasion case: an ordinary IBAN with
// no injected character still produces exactly the same single finding,
// at exactly the same byte offsets, as it did before this fix.
func TestIBANDetectorCleanBaselineUnaffectedByZWSPFix(t *testing.T) {
	text := "please wire to GB29NWBK60161331926819 today"
	findings, err := IBANDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", findings)
	}
	wantStart := strings.Index(text, "GB29NWBK60161331926819")
	wantEnd := wantStart + len("GB29NWBK60161331926819")
	if findings[0].Start != wantStart || findings[0].End != wantEnd {
		t.Errorf("Start/End = %d/%d, want %d/%d (unchanged from pre-fix behavior)", findings[0].Start, findings[0].End, wantStart, wantEnd)
	}
}

// TestIBANDetectorZWSPEvasionOffsetIndexesOriginalText proves
// Finding.Start/End index into the ORIGINAL (unstripped) text, not the
// internal ZWSP-stripped copy used for matching. The injected character
// is placed well inside the BBAN portion of the run, not its very first
// byte, so a naive fix that forgot to remap the match's end would
// produce a visibly wrong End (short by the ZWSP's own 3-byte UTF-8
// width), not a coincidentally correct one.
func TestIBANDetectorZWSPEvasionOffsetIndexesOriginalText(t *testing.T) {
	zwsp := string(rune(0x200B))
	prefix := "Please wire the funds to "
	suffix := " by end of day."
	text := prefix + "GB29NWBK6016" + zwsp + "1331926819" + suffix

	findings, err := IBANDetector{}.Detect(context.Background(), text)
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
	want := "GB29NWBK6016" + zwsp + "1331926819"
	if got != want {
		t.Errorf("text[Start:End] = %q, want %q (the ZWSP itself must still be inside the reported original-text span)", got, want)
	}
}
