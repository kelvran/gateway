package guardrail

import (
	"context"
	"regexp"
)

// emailPattern is a pragmatic, RFC-5322-shaped email regex — not a full
// grammar implementation (no comments, no quoted-string local parts),
// matching the same "pattern-matchable, not exhaustive" scope Microsoft
// Presidio's own EMAIL_ADDRESS recognizer uses.
var emailPattern = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)

// EmailDetector detects email addresses — CategoryContactInfo (Warn
// tier): common in ordinary conversation, low-precision-cost false
// positives are cheap.
type EmailDetector struct{}

func (EmailDetector) Name() string       { return "email" }
func (EmailDetector) Category() Category { return CategoryContactInfo }

// Detect matches against stripHiddenUnicode(text)'s stripped copy, not text
// directly — emailPattern's local-part/domain character classes are each a
// contiguous run with no tolerance for an injected zero-width character
// mid-run, the same evasion class documented against CreditCardDetector by
// regcorpus-guardrail-23. Every reported Finding.Start/End is remapped back
// to the ORIGINAL text via remapMatch, per that field's own documented
// offset contract.
func (EmailDetector) Detect(_ context.Context, text string) ([]Finding, error) {
	stripped, origOffsets := stripHiddenUnicode(text)
	var findings []Finding
	for _, loc := range emailPattern.FindAllStringIndex(stripped, -1) {
		start, end := remapMatch(origOffsets, loc[0], loc[1])
		findings = append(findings, Finding{Category: CategoryContactInfo, Detector: "email", Start: start, End: end})
	}
	return findings, nil
}
