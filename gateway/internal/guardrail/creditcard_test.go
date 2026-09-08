package guardrail

import (
	"context"
	"strings"
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

// TestCreditCardDetectorZWSPEvasionCaught proves the fix for the real gap
// documented by evals/tests/fixtures/regression_corpus_guardrail.json's
// regcorpus-guardrail-23-creditcard-zwsp-evasion-downgrades-block-to-warn:
// the exact same clean, Luhn-valid card number used by that case's
// sibling clean-baseline case (regcorpus-guardrail-22), with a single
// zero-width space (U+200B) inserted mid-digit-run, must now be caught --
// the same financial_id Block-tier verdict the clean baseline gets, not
// silently missed via the unrelated hidden-Unicode prompt-injection scan.
// Sanity-checked by breaking (see docs/agents/LOGS.md's matching entry):
// with Detect() temporarily reverted to match against text directly
// instead of stripHiddenUnicode(text), this test failed with zero
// findings -- the right reason -- before the fix was restored.
func TestCreditCardDetectorZWSPEvasionCaught(t *testing.T) {
	zwsp := string(rune(0x200B))
	text := "Charge my card 4111" + zwsp + "1111 1111 1111 for the invoice."
	findings, err := CreditCardDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1 (ZWSP-inside-digit-run evasion must still be caught)", findings)
	}
	if findings[0].Category != CategoryFinancialID {
		t.Errorf("Category = %v, want %v", findings[0].Category, CategoryFinancialID)
	}
	if findings[0].Detector != "creditcard" {
		t.Errorf("Detector = %v, want creditcard", findings[0].Detector)
	}
}

// TestCreditCardDetectorCleanBaselineUnaffectedByZWSPFix proves the fix
// causes zero regression on the common, no-evasion case: regcorpus-
// guardrail-22's exact clean input still produces exactly the same single
// finding, at exactly the same byte offsets, as it did before this fix.
func TestCreditCardDetectorCleanBaselineUnaffectedByZWSPFix(t *testing.T) {
	text := "Charge my card 4111 1111 1111 1111 for the invoice."
	findings, err := CreditCardDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", findings)
	}
	wantStart := strings.Index(text, "4111")
	wantEnd := wantStart + len("4111 1111 1111 1111")
	if findings[0].Start != wantStart || findings[0].End != wantEnd {
		t.Errorf("Start/End = %d/%d, want %d/%d (unchanged from pre-fix behavior)", findings[0].Start, findings[0].End, wantStart, wantEnd)
	}
}

// TestCreditCardDetectorZWSPEvasionOffsetIndexesOriginalText proves
// Finding.Start/End index into the ORIGINAL (unstripped) text, not the
// internal ZWSP-stripped copy used for matching. The injected character
// is placed well INSIDE the match, not at its very first byte, so a
// naive fix that forgot to remap the match's end through stripHiddenUnicode's
// offset mapping would produce a visibly wrong End (short by the ZWSP's
// own 3-byte UTF-8 width), not a coincidentally correct one.
func TestCreditCardDetectorZWSPEvasionOffsetIndexesOriginalText(t *testing.T) {
	zwsp := string(rune(0x200B))
	prefix := "Please charge card "
	suffix := " today, thanks."
	text := prefix + "4111 1111" + zwsp + "1111 1111" + suffix

	findings, err := CreditCardDetector{}.Detect(context.Background(), text)
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
	want := "4111 1111" + zwsp + "1111 1111"
	if got != want {
		t.Errorf("text[Start:End] = %q, want %q (the ZWSP itself must still be inside the reported original-text span)", got, want)
	}
}
