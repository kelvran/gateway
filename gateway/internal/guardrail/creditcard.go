package guardrail

import (
	"context"
	"regexp"
	"strings"
)

// creditCardPattern matches a plausible 13-19 digit run, optionally
// grouped by spaces/hyphens in 4s — the pattern alone is deliberately
// loose; luhnValid does the real filtering, per Presidio's own
// CREDIT_CARD recognizer (pattern match + Luhn checksum).
var creditCardPattern = regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`)

// luhnValid reports whether digits (with no separators) passes the Luhn
// checksum — the standard mod-10 algorithm every real card issuer uses.
func luhnValid(digits string) bool {
	if len(digits) < 12 {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// CreditCardDetector detects credit-card-shaped numbers that ALSO pass
// the Luhn checksum — CategoryFinancialID (Block tier): PCI DSS
// Requirement 3.4/3.5.1 treats PAN as always-sensitive, and the checksum
// gate keeps this detector's false-positive rate low enough to justify a
// hard block.
type CreditCardDetector struct{}

func (CreditCardDetector) Name() string       { return "creditcard" }
func (CreditCardDetector) Category() Category { return CategoryFinancialID }

// Detect matches against stripHiddenUnicode(text)'s stripped copy, not text
// directly — creditCardPattern's contiguous \d-run has no tolerance for an
// injected zero-width character (or any other hiddenUnicodeRanges member)
// mid-run, so a single one breaks the match entirely (regcorpus-guardrail-23).
// Every reported Finding.Start/End is remapped back to the ORIGINAL text via
// remapMatch, per that field's own documented original-text-offset contract.
func (CreditCardDetector) Detect(_ context.Context, text string) ([]Finding, error) {
	stripped, origOffsets := stripHiddenUnicode(text)
	var findings []Finding
	for _, loc := range creditCardPattern.FindAllStringIndex(stripped, -1) {
		digits := strings.Map(func(r rune) rune {
			if r == ' ' || r == '-' {
				return -1
			}
			return r
		}, stripped[loc[0]:loc[1]])
		if luhnValid(digits) {
			start, end := remapMatch(origOffsets, loc[0], loc[1])
			findings = append(findings, Finding{Category: CategoryFinancialID, Detector: "creditcard", Start: start, End: end})
		}
	}
	return findings, nil
}
