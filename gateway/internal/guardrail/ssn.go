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
func (SSNDetector) Detect(_ context.Context, text string) ([]Finding, error) {
	var findings []Finding
	for _, loc := range ssnPattern.FindAllStringIndex(text, -1) {
		findings = append(findings, Finding{Category: CategoryGovernmentID, Detector: "ssn", Start: loc[0], End: loc[1]})
	}
	return findings, nil
}
