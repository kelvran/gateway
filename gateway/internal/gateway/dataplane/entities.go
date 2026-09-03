package dataplane

import (
	"regexp"
	"strings"

	"github.com/kelvran/gateway/internal/adapter"
)

// Fingerprint extracts a query's entity/number/date signature for Cache
// L3-lite's hard-gate, per
// docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md. Deliberately
// simple and auditable — regex-based numbers/dates, capitalized
// multi-token sequences as a coarse proper-noun proxy, no gazetteer, no
// NER model. This is schema-aware work (it reads adapter.Message), so it
// stays here in dataplane, never in internal/cache — see
// cache/key.go's own documented "never imports internal/adapter"
// boundary, already established for serializeMessages/normalizeMessages.
//
// The returned set is deliberately biased toward OVER-inclusion: for a
// security-critical hard gate, a false positive (treating a common word
// as an "entity," costing only hit rate) is the safe failure mode: a
// false negative (missing a real entity/number mismatch and serving a
// wrong cached answer) is the dangerous one, per THREAT_MODEL.md's own
// framing of this exact class of risk.
func Fingerprint(messages []adapter.Message) map[string]struct{} {
	fingerprint := map[string]struct{}{}
	for _, m := range messages {
		for _, match := range numberPattern.FindAllString(m.Content, -1) {
			fingerprint[match] = struct{}{}
		}
		for _, match := range datePattern.FindAllString(m.Content, -1) {
			fingerprint[match] = struct{}{}
		}
		for _, match := range capitalizedSequences(m.Content) {
			fingerprint[match] = struct{}{}
		}
	}
	return fingerprint
}

// numberPattern matches integers, decimals, currency-prefixed, and
// percentage-suffixed numbers — e.g. "92", "92.50", "$92.50", "15%".
var numberPattern = regexp.MustCompile(`\$?\d+(?:\.\d+)?%?`)

// datePattern matches a deliberately bounded, explicit set of common
// date formats — ISO 8601, slash-separated, and "Month Day[, Year]".
// NOT exhaustive (e.g. no support for "the 15th of January" or
// non-English month names) — a known, documented gap, not a silent one.
var datePattern = regexp.MustCompile(
	`\d{4}-\d{2}-\d{2}` + // ISO 8601: 2024-01-15
		`|\d{1,2}/\d{1,2}/\d{2,4}` + // 01/15/2024
		`|(?:January|February|March|April|May|June|July|August|September|October|November|December|Jan|Feb|Mar|Apr|Jun|Jul|Aug|Sept?|Oct|Nov|Dec)\.?\s+\d{1,2}(?:st|nd|rd|th)?(?:,?\s+\d{4})?`,
)

// capitalizedSequencePattern matches one or more consecutive
// Title-Case words — e.g. "Paris", "New York", "Golden Gate Bridge".
var capitalizedSequencePattern = regexp.MustCompile(`\b[A-Z][a-zA-Z]*(?:\s+[A-Z][a-zA-Z]*)*\b`)

// alwaysCapitalizedWords are standalone English words capitalized
// regardless of sentence position — structurally indistinguishable from
// a real entity by capitalization alone, unlike "The"/"What"/etc., which
// are only capitalized sentence-initially and are already excluded by
// capitalizedSequences' own first-word skip below. "I" is the one word
// in this category; deliberately not a broader stoplist (a broader list
// would start reintroducing the same "which common words don't count"
// judgment call this function's whole design avoids by using position
// instead of a dictionary).
var alwaysCapitalizedWords = map[string]struct{}{"I": {}}

// capitalizedSequences extracts capitalized multi-token sequences from
// content, EXCLUDING the message's own first word — sentence-initial
// capitalization is a grammar artifact ("How do I...", "What is...."),
// not an entity signal, and including it would make ordinary paraphrases
// that merely start with a different question word (a real, safe,
// lexically-near-duplicate rephrasing) incorrectly fail the hard gate.
// This is a deliberate, documented simplification: a capitalized word
// starting a LATER sentence within a multi-sentence message is not
// similarly excluded, and so may still be over-included — accepted per
// this function's own "over-inclusion is the safe failure mode" doc
// comment above.
func capitalizedSequences(content string) []string {
	firstSpace := strings.IndexAny(content, " \t\n")
	if firstSpace < 0 {
		return nil // the entire content is one word — already excluded as "first word"
	}
	rest := content[firstSpace+1:]
	matches := capitalizedSequencePattern.FindAllString(rest, -1)
	filtered := make([]string, 0, len(matches))
	for _, m := range matches {
		if _, always := alwaysCapitalizedWords[m]; !always {
			filtered = append(filtered, m)
		}
	}
	return filtered
}
