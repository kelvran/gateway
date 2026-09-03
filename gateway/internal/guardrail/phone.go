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
func (PhoneDetector) Detect(_ context.Context, text string) ([]Finding, error) {
	var findings []Finding
	for _, loc := range phonePattern.FindAllStringIndex(text, -1) {
		findings = append(findings, Finding{Category: CategoryContactInfo, Detector: "phone", Start: loc[0], End: loc[1]})
	}
	return findings, nil
}
