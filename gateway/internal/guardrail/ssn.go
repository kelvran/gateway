package guardrail

import (
	"context"
	"regexp"
)

// ssnPattern matches every shape a US Social Security Number is written in
// (###-##-####, space-separated, one separator, or a bare nine-digit run).
// Deliberately US-only in v1 — other countries' national-ID formats are out
// of scope, not silently implied. Excludes the officially-invalid ranges
// (000/666/900-999 area, 00 group, 0000 serial), to keep the false-positive
// rate down on genuinely impossible numbers: Presidio's US_SSN recognizer
// invalidates the 000/666 areas, a 00 group and a 0000 serial the same way
// (presidio-analyzer, predefined_recognizers/country_specific/us/
// us_ssn_recognizer.py, invalidate_result; main branch as read 2026-10-12);
// the 900-999 exclusion is the SSA's own never-issued band, not Presidio's.
// Known over-exclusion, unchanged here: the group `[1-9][0-9]` and serial
// `[1-9][0-9]{3}` also reject the valid groups 01-09 and serials 0001-0999
// (a follow-up recorded in DECISIONS.md, 2026-10-12).
// The area alternation enumerates 001-899 minus 666 because Go's RE2 has no
// lookahead; until 2026-10-12 the shorter `[0-7][0-9]{2}|8[0-8][0-9]`
// quietly admitted 000 and 666 against this comment and excluded the valid
// 890-899. Since 2026-10-12 a match counts as a finding only when it is a
// separated form (ssnSeparatedPattern) or an SSN cue precedes it
// (ssnCuePattern within ssnCueWindow). That is Presidio's weighting made a
// gate: in the same source file its bare nine-digit pattern "SSN4 (very
// weak)" scores 0.05 and the separated "SSN5 (medium)" 0.5, with context
// words lifting a weak match. A bare run is what a build number or a
// timestamp in a working-directory path looks like — Claude Code writes such
// paths into its system prompt, and the default policy blocked the turn
// (docs/how-to/clients/claude-code.md).
var ssnPattern = regexp.MustCompile(`\b(?:00[1-9]|0[1-9][0-9]|[1-5][0-9]{2}|6[0-5][0-9]|66[0-57-9]|6[7-9][0-9]|[78][0-9]{2})[-\s]?(?:[1-9][0-9])[-\s]?(?:[1-9][0-9]{3})\b`)

// ssnSeparatedPattern is the ###-##-#### / ### ## #### form, both
// separators present (dash or whitespace, independently): the way an SSN
// is written on a form, a finding on its own.
var ssnSeparatedPattern = regexp.MustCompile(`^(?:00[1-9]|0[1-9][0-9]|[1-5][0-9]{2}|6[0-5][0-9]|66[0-57-9]|6[7-9][0-9]|[78][0-9]{2})[-\s](?:[1-9][0-9])[-\s](?:[1-9][0-9]{3})$`)

// ssnCuePattern is the context that lifts a bare or single-separator run
// to a finding. Presidio's US_SSN context words are social, security, ssn,
// ssns and ssid (ss#/ssn# sit commented out there because `#` breaks its
// lemma-based enhancer); here the cue is a hard gate rather than a score
// boost, so the lone words social, security and ssid are out — they would
// fire on "security build 349521234" — and the forms an SSN is actually
// introduced with are in: ssn, ss#, ss number, social security, soc sec.
// Case-insensitive, word-bounded where the word has a boundary to offer
// (`#` has none).
var ssnCuePattern = regexp.MustCompile(`(?i)(?:\bssn\b|\bss#|\bss\s+number\b|\bsocial\s+security\b|\bsoc\s+sec\b)`)

// ssnCueWindow is how many bytes before a bare run the cue may end: a
// cue word, a colon and a few connecting words ("the SSN on the form is"),
// never a cue a paragraph away.
const ssnCueWindow = 48

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
// 23. A match is a finding when it is the separated form or an SSN cue ends
// within ssnCueWindow bytes before it on the stripped text (the cue is
// stripped too, so a zero-width character inside "ssn" hides nothing).
// Every reported Finding.Start/End is remapped back to the ORIGINAL text via
// remapMatch, per that field's own documented offset contract; the span is
// the number alone, never the cue.
func (SSNDetector) Detect(_ context.Context, text string) ([]Finding, error) {
	stripped, origOffsets := stripHiddenUnicode(text)
	var findings []Finding
	for _, loc := range ssnPattern.FindAllStringIndex(stripped, -1) {
		if !ssnSeparatedPattern.MatchString(stripped[loc[0]:loc[1]]) && !ssnCuePattern.MatchString(stripped[max(0, loc[0]-ssnCueWindow):loc[0]]) {
			continue
		}
		start, end := remapMatch(origOffsets, loc[0], loc[1])
		findings = append(findings, Finding{Category: CategoryGovernmentID, Detector: "ssn", Start: start, End: end})
	}
	return findings, nil
}
