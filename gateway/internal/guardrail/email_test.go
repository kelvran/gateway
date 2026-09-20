package guardrail

import (
	"context"
	"strings"
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

// TestEmailDetectorZWSPEvasionCaught proves EmailDetector is vulnerable to
// (and, with this fix, no longer defeated by) the identical zero-width-
// space-inside-run evasion documented against CreditCardDetector by
// evals/tests/fixtures/regression_corpus_guardrail.json's regcorpus-
// guardrail-23 case, applied here to the email-address PII detector:
// unlike every sibling detector in this package (creditcard/iban/
// ipaddress/phone/secretkey/ssn), EmailDetector never adopted
// stripHiddenUnicode, so a real email address with a single U+200B
// substituted right after the "@" must still be caught at the same
// contact_info Warn tier. The ZWSP is placed immediately after "@"
// (rather than mid-local-part) deliberately: emailPattern's local part
// and domain are both variable-length (+), so a ZWSP elsewhere still
// leaves a truncated substring (e.g. "doe@example.com") that the
// pre-fix, unstripped code would coincidentally still match — placing it
// at the "@" boundary itself breaks every possible match, since "@"
// appears exactly once and is required immediately adjacent to the
// domain on one side.
func TestEmailDetectorZWSPEvasionCaught(t *testing.T) {
	zwsp := string(rune(0x200B))
	text := "contact me at jane.doe@" + zwsp + "example.com please"
	findings, err := EmailDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1 (ZWSP-inside-local-part evasion must still be caught)", findings)
	}
	if findings[0].Category != CategoryContactInfo {
		t.Errorf("Category = %v, want %v", findings[0].Category, CategoryContactInfo)
	}
	if findings[0].Detector != "email" {
		t.Errorf("Detector = %v, want email", findings[0].Detector)
	}
}

// TestEmailDetectorCleanBaselineUnaffectedByZWSPFix proves the fix causes
// zero regression on the common, no-evasion case: an ordinary email
// address with no injected character still produces exactly the same
// single finding, at exactly the same byte offsets, as it did before this
// fix.
func TestEmailDetectorCleanBaselineUnaffectedByZWSPFix(t *testing.T) {
	text := "contact me at jane.doe@example.com please"
	findings, err := EmailDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", findings)
	}
	wantStart := strings.Index(text, "jane.doe@example.com")
	wantEnd := wantStart + len("jane.doe@example.com")
	if findings[0].Start != wantStart || findings[0].End != wantEnd {
		t.Errorf("Start/End = %d/%d, want %d/%d (unchanged from pre-fix behavior)", findings[0].Start, findings[0].End, wantStart, wantEnd)
	}
}

// TestEmailDetectorZWSPEvasionOffsetIndexesOriginalText proves
// Finding.Start/End index into the ORIGINAL (unstripped) text, not the
// internal ZWSP-stripped copy used for matching. The injected character
// replaces the "@"-adjacent domain boundary (well inside the match, not
// its very first byte), so a naive fix that forgot to remap the match's
// end would produce a visibly wrong End (short by the ZWSP's own 3-byte
// UTF-8 width), not a coincidentally correct one.
func TestEmailDetectorZWSPEvasionOffsetIndexesOriginalText(t *testing.T) {
	zwsp := string(rune(0x200B))
	prefix := "You can reach me at "
	suffix := " after 5pm."
	text := prefix + "jane.doe@example" + zwsp + ".com" + suffix

	findings, err := EmailDetector{}.Detect(context.Background(), text)
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
	want := "jane.doe@example" + zwsp + ".com"
	if got != want {
		t.Errorf("text[Start:End] = %q, want %q (the ZWSP itself must still be inside the reported original-text span)", got, want)
	}
}
