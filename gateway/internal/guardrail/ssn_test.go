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

// TestSSNDetectorBareNineDigitRunNeedsAnSSNCue pins the 2026-10-12 rule: a
// separated SSN (both separators present, dash or whitespace) matches on
// its own; a bare nine-digit run, or one with a single separator, matches
// only when an SSN cue -- ssn, ss#, ss number, social security, soc sec --
// ends within ssnCueWindow bytes before it. Presidio's own US_SSN
// recognizer scores the bare run "very weak" and lifts it only through
// such context words; before this rule a build number or a timestamp in
// a working-directory path (which Claude Code writes into its system
// prompt) blocked the turn at the government_id tier.
func TestSSNDetectorBareNineDigitRunNeedsAnSSNCue(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       int
	}{
		{"a nine-digit run in a working-directory path", "cwd is /tmp/kelvran-compat-claude-home-349521234/project", 0},
		{"a nine-digit run with no cue", "ticket 349521234 is still open", 0},
		{"a single separator with no cue", "build 349-521234 failed", 0},
		{"the cue right before the bare run", "SSN: 349521234", 1},
		{"the cue a few words back", "the social security number for the form is 349521234 today", 1},
		{"ss# as the cue", "ss# 349521234 on file", 1},
		{"the cue before a single-separator form", "ssn 349-521234", 1},
		{"the cue beyond the window", "ssn " + strings.Repeat("x", ssnCueWindow) + " 349521234", 0},
		{"a ten-digit run after the cue", "ssn 3495212345", 0},
		{"a word containing ssn is not a cue", "assn 349521234", 0},
		{"the dashed form needs no cue", "call 349-52-1234 back", 1},
		{"the spaced form needs no cue", "call 349 52 1234 back", 1},
		{"an excluded area stays excluded with the cue", "ssn 666521234", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings, err := SSNDetector{}.Detect(context.Background(), tc.text)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if len(findings) != tc.want {
				t.Fatalf("findings = %v, want %d", findings, tc.want)
			}
		})
	}
}

// TestSSNDetectorCueMatchReportsTheDigitsOnly: with the cue in play the
// reported span is still the number alone, indexed into the original text.
func TestSSNDetectorCueMatchReportsTheDigitsOnly(t *testing.T) {
	zwsp := string(rune(0x200B))
	text := "Please file this SSN: 349" + zwsp + "521234 today."
	findings, err := SSNDetector{}.Detect(context.Background(), text)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", findings)
	}
	if got, want := text[findings[0].Start:findings[0].End], "349"+zwsp+"521234"; got != want {
		t.Fatalf("text[Start:End] = %q, want %q", got, want)
	}
}
