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
func (EmailDetector) Detect(_ context.Context, text string) ([]Finding, error) {
	var findings []Finding
	for _, loc := range emailPattern.FindAllStringIndex(text, -1) {
		findings = append(findings, Finding{Category: CategoryContactInfo, Detector: "email", Start: loc[0], End: loc[1]})
	}
	return findings, nil
}
