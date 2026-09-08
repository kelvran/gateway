package guardrail

import (
	"context"
	"regexp"
)

// phonePattern covers NANP (North American Numbering Plan) formats and a
// generic E.164-shaped international pattern — a regional regex set, not
// an exhaustive per-country grammar, matching PhoneDetector's own
// documented, deliberately-bounded scope.
var phonePattern = regexp.MustCompile(`(?:\+?1[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}\b|\+[1-9]\d{7,14}\b`)

// PhoneDetector detects phone numbers — CategoryContactInfo (Warn tier).
type PhoneDetector struct{}

func (PhoneDetector) Name() string       { return "phone" }
func (PhoneDetector) Category() Category { return CategoryContactInfo }

// Detect matches against stripHiddenUnicode(text)'s stripped copy, not text
// directly — phonePattern's digit-groups are each a contiguous \d-run with
// no tolerance for an injected zero-width character mid-group, the same
// evasion class documented against CreditCardDetector by regcorpus-
// guardrail-23. Every reported Finding.Start/End is remapped back to the
// ORIGINAL text via remapMatch, per that field's own documented offset
// contract.
func (PhoneDetector) Detect(_ context.Context, text string) ([]Finding, error) {
	stripped, origOffsets := stripHiddenUnicode(text)
	var findings []Finding
	for _, loc := range phonePattern.FindAllStringIndex(stripped, -1) {
		start, end := remapMatch(origOffsets, loc[0], loc[1])
		findings = append(findings, Finding{Category: CategoryContactInfo, Detector: "phone", Start: start, End: end})
	}
	return findings, nil
}
