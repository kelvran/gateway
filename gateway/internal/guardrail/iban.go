package guardrail

import (
	"context"
	"regexp"
	"strings"
)

// ibanPattern matches an IBAN-shaped string: 2 letters (country), 2
// digits (check digits), then 11-30 alphanumeric characters — the
// pattern alone is loose; mod97Valid does the real filtering, per
// Presidio's own IBAN_CODE recognizer (pattern match + ISO 7064 mod-97
// checksum).
var ibanPattern = regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{11,30}\b`)

// mod97Valid reports whether an IBAN candidate (no spaces, uppercase)
// passes the real ISO 7064 mod-97 checksum: move the first 4 characters
// to the end, replace each letter with its two-digit alphabetic value
// (A=10 ... Z=35), and check the resulting number mod 97 == 1. Computed
// incrementally (digit by digit) rather than via a big-integer
// conversion, since IBAN candidates can be up to 34 characters —
// converting to decimal digit-by-digit and reducing mod 97 at each step
// is mathematically equivalent and avoids importing math/big for this.
func mod97Valid(candidate string) bool {
	if len(candidate) < 15 || len(candidate) > 34 {
		return false
	}
	rearranged := candidate[4:] + candidate[:4]
	remainder := 0
	for _, r := range rearranged {
		var value int
		switch {
		case r >= '0' && r <= '9':
			value = int(r - '0')
			remainder = (remainder*10 + value) % 97
		case r >= 'A' && r <= 'Z':
			value = int(r-'A') + 10 // A=10 ... Z=35
			remainder = (remainder*100 + value) % 97
		default:
			return false
		}
	}
	return remainder == 1
}

// IBANDetector detects IBAN-shaped strings that ALSO pass the mod-97
// checksum — CategoryFinancialID (Block tier), same reasoning as
// CreditCardDetector: a real financial identifier, checksum-gated to
// keep the false-positive rate low enough to justify a hard block.
type IBANDetector struct{}

func (IBANDetector) Name() string       { return "iban" }
func (IBANDetector) Category() Category { return CategoryFinancialID }
func (IBANDetector) Detect(_ context.Context, text string) ([]Finding, error) {
	var findings []Finding
	for _, loc := range ibanPattern.FindAllStringIndex(text, -1) {
		candidate := strings.ToUpper(text[loc[0]:loc[1]])
		if mod97Valid(candidate) {
			findings = append(findings, Finding{Category: CategoryFinancialID, Detector: "iban", Start: loc[0], End: loc[1]})
		}
	}
	return findings, nil
}
