package guardrail

import (
	"context"
	"regexp"
)

// ssnPattern matches US Social Security Numbers (###-##-####, or a
// spaceless/space-separated variant). Deliberately US-only in v1 — other
// countries' national-ID formats are out of scope, not silently implied.
// Excludes the officially-invalid ranges (000/666/900-999 area, 00 group,
// 0000 serial) that Presidio's own US_SSN recognizer also excludes, to
// keep the false-positive rate down on genuinely impossible numbers.
var ssnPattern = regexp.MustCompile(`\b(?:[0-7][0-9]{2}|8[0-8][0-9])[-\s]?(?:[1-9][0-9])[-\s]?(?:[1-9][0-9]{3})\b`)

// SSNDetector detects US Social Security Numbers — CategoryGovernmentID
// (Block tier): a real, litigatable identifier per 45 CFR §164.514's
// HIPAA Safe Harbor list.
type SSNDetector struct{}

func (SSNDetector) Name() string       { return "ssn" }
func (SSNDetector) Category() Category { return CategoryGovernmentID }

// Detect matches against stripHiddenUnicode(text)'s stripped copy, not text
// directly — ssnPattern's three digit-groups are each a contiguous \d-run
// with no tolerance for an injected zero-width character mid-group, the same
// evasion class documented against CreditCardDetector by regcorpus-guardrail-
// 23. Every reported Finding.Start/End is remapped back to the ORIGINAL
// text via remapMatch, per that field's own documented offset contract.
func (SSNDetector) Detect(_ context.Context, text string) ([]Finding, error) {
	stripped, origOffsets := stripHiddenUnicode(text)
	var findings []Finding
	for _, loc := range ssnPattern.FindAllStringIndex(stripped, -1) {
		start, end := remapMatch(origOffsets, loc[0], loc[1])
		findings = append(findings, Finding{Category: CategoryGovernmentID, Detector: "ssn", Start: start, End: end})
	}
	return findings, nil
}
