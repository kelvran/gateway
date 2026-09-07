package guardrail

import (
	"context"
	"strings"
)

// injectionVerbs and injectionTargets are combined into a phrase list
// (see injectionPhrases) — the same verb×target combinatoric approach
// LiteLLM's own default prompt-injection detector uses
// (litellm/proxy/hooks/prompt_injection_detection.py), a real,
// zero-dependency, code-level heuristic used in a real production
// gateway, not invented for this project.
//
// "disobey", "circumvent", and "subvert" were added by the same-day
// vocabulary-widening fix for evals/tests/fixtures/regression_corpus_
// guardrail.json's regcorpus-guardrail-05/06 cases, which documented
// these as real, common override-instruction synonyms missing from the
// original 6-verb list — the exact same "tell the model to disregard an
// instruction" meaning the original verbs already cover, so this widens
// the same list with the same class of real, well-known synonym an
// attacker would plausibly substitute, not an arbitrary or speculative
// addition. See DECISIONS.md for the corresponding entry.
var injectionVerbs = []string{"ignore", "disregard", "skip", "forget", "override", "bypass", "disobey", "circumvent", "subvert"}

// "your system prompt" was added alongside the verb widening above: the
// bare "system prompt" target already existed, but "system prompt" is
// the one noun in this list that — unlike "instructions"/"rules"/
// "guidelines", each of which already has (or, for "instructions", is
// one of several variants that includes) a "your <noun>" form — had no
// "your"-prefixed counterpart, so a real "disobey/circumvent your
// system prompt" phrasing (regcorpus-guardrail-05's exact input)
// couldn't match even with "disobey" in injectionVerbs. Same
// combinatoric list, same reasoning, not a new mechanism.
var injectionTargets = []string{
	"prior instructions", "previous instructions", "preceding instructions",
	"earlier instructions", "all instructions", "the instructions",
	"your instructions", "system prompt", "your system prompt",
	"your rules", "your guidelines",
}

// injectionPhrases is the full combinatoric phrase list, built once at
// package init — every "<verb> <target>" pair, lowercase.
var injectionPhrases = buildInjectionPhrases()

func buildInjectionPhrases() []string {
	phrases := make([]string, 0, len(injectionVerbs)*len(injectionTargets))
	for _, v := range injectionVerbs {
		for _, t := range injectionTargets {
			phrases = append(phrases, v+" "+t)
		}
	}
	return phrases
}

// hiddenUnicodeRanges are the real, documented hidden-Unicode
// prompt-injection ranges Kong's AI Prompt Guard plugin publishes as
// known attack vectors: zero-width characters, bidirectional-control
// characters, and Unicode tag characters — none of which have any
// legitimate reason to appear in ordinary chat input.
var hiddenUnicodeRanges = [][2]rune{
	{0x200B, 0x200D},   // zero-width space/non-joiner/joiner
	{0xFEFF, 0xFEFF},   // zero-width no-break space (BOM)
	{0x202A, 0x202E},   // bidirectional control
	{0xE0020, 0xE007F}, // Unicode tag characters
}

func isHiddenUnicode(r rune) bool {
	for _, rng := range hiddenUnicodeRanges {
		if r >= rng[0] && r <= rng[1] {
			return true
		}
	}
	return false
}

// PromptInjectionDetector runs two independent checks — a case-insensitive
// combinatoric phrase match (see injectionPhrases) and a hidden-Unicode
// scan (see hiddenUnicodeRanges) — both CategoryPromptInjection (Warn
// tier), per THREAT_MODEL.md's LLM01 mapping. Deliberately substring-based
// rather than true fuzzy/edit-distance matching, keeping the heuristic
// simple and auditable for v1 — the same "simplest thing that works"
// discipline this project already applied to Cache L3-lite's MinHash
// choice over a heavier alternative.
type PromptInjectionDetector struct{}

func (PromptInjectionDetector) Name() string       { return "promptinjection" }
func (PromptInjectionDetector) Category() Category { return CategoryPromptInjection }
func (PromptInjectionDetector) Detect(_ context.Context, text string) ([]Finding, error) {
	var findings []Finding

	lower := strings.ToLower(text)
	for _, phrase := range injectionPhrases {
		if idx := strings.Index(lower, phrase); idx >= 0 {
			findings = append(findings, Finding{
				Category: CategoryPromptInjection, Detector: "promptinjection",
				Start: idx, End: idx + len(phrase),
			})
		}
	}

	for i, r := range text {
		if isHiddenUnicode(r) {
			findings = append(findings, Finding{
				Category: CategoryPromptInjection, Detector: "promptinjection_hidden_unicode",
				Start: i, End: i + len(string(r)),
			})
		}
	}

	return findings, nil
}
