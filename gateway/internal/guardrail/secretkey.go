package guardrail

import (
	"context"
	"math"
	"regexp"
)

// secretPrefixPattern covers common, real API-key/secret prefixes —
// lifted directly from gitleaks' MIT-licensed rule set (the reference
// OSS implementation for this exact problem), not reinvented.
var secretPrefixPattern = regexp.MustCompile(
	`\bsk-ant-[A-Za-z0-9_\-]{20,}\b` + // Anthropic
		`|\bsk-proj-[A-Za-z0-9_\-]{20,}\b` + // OpenAI (project-scoped)
		`|\bsk-[A-Za-z0-9]{20,}\b` + // OpenAI (legacy)
		`|\bgh[pousr]_[A-Za-z0-9]{36}\b` + // GitHub PAT variants
		`|\b(?:AKIA|ASIA)[A-Z0-9]{16}\b` + // AWS access key
		`|\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{16,}\b`, // Stripe
)

// genericSecretAssignmentPattern matches a generic
// "key/token/secret/password = <value>"-shaped assignment — gated by
// Shannon entropy (see shannonEntropy) rather than reported unconditionally,
// since the bare pattern alone would false-positive on any ordinary
// assignment ("password = hunter2").
var genericSecretAssignmentPattern = regexp.MustCompile(
	`(?i)(?:api[_-]?key|access[_-]?token|secret|token|password)[\s:=]+['"]?([A-Za-z0-9_\-/+=]{20,})['"]?`,
)

// genericSecretEntropyThreshold is gitleaks' own convention for
// distinguishing a real high-entropy secret from an ordinary word or
// short phrase assigned to a similarly-named variable.
const genericSecretEntropyThreshold = 3.5

// shannonEntropy computes the Shannon entropy (bits per character) of s
// — higher values indicate more randomness (a plausible generated
// secret); an ordinary word or repeated-character string scores low.
func shannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	counts := make(map[rune]int)
	for _, r := range s {
		counts[r]++
	}
	entropy := 0.0
	length := float64(len(s))
	for _, count := range counts {
		p := float64(count) / length
		entropy -= p * math.Log2(p)
	}
	return entropy
}

// SecretKeyDetector detects known API-key/secret prefixes and a generic,
// entropy-gated key/token/secret assignment pattern — CategoryCredential
// (Block tier): AWS Bedrock's own guidance names credentials as the
// canonical "masking isn't enough, block it" category.
type SecretKeyDetector struct{}

func (SecretKeyDetector) Name() string       { return "secretkey" }
func (SecretKeyDetector) Category() Category { return CategoryCredential }

// Detect matches against stripHiddenUnicode(text)'s stripped copy, not
// text directly — a round-4 backlog-audit finding: secretPrefixPattern's
// and genericSecretAssignmentPattern's own {20,} unbounded-minimum runs
// are the identical contiguous-character-class shape already fixed for
// phone.go/creditcard.go/ssn.go/iban.go against the same evasion
// (regcorpus-guardrail-23) — a single injected zero-width character every
// few characters keeps every contiguous run under 20 chars, so neither
// pattern ever matches at all, silently downgrading what should be a
// hard Block into nothing. shannonEntropy is computed over the STRIPPED
// substring (the value as actually captured, mirroring creditcard.go's
// Luhn check on its own stripped substring); every reported
// Finding.Start/End is remapped back to the ORIGINAL text via remapMatch.
func (SecretKeyDetector) Detect(_ context.Context, text string) ([]Finding, error) {
	stripped, origOffsets := stripHiddenUnicode(text)
	var findings []Finding
	for _, loc := range secretPrefixPattern.FindAllStringIndex(stripped, -1) {
		start, end := remapMatch(origOffsets, loc[0], loc[1])
		findings = append(findings, Finding{Category: CategoryCredential, Detector: "secretkey", Start: start, End: end})
	}
	for _, match := range genericSecretAssignmentPattern.FindAllStringSubmatchIndex(stripped, -1) {
		valueStart, valueEnd := match[2], match[3]
		if valueStart < 0 {
			continue
		}
		value := stripped[valueStart:valueEnd]
		if shannonEntropy(value) >= genericSecretEntropyThreshold {
			start, end := remapMatch(origOffsets, match[0], match[1])
			findings = append(findings, Finding{Category: CategoryCredential, Detector: "secretkey", Start: start, End: end})
		}
	}
	return findings, nil
}
