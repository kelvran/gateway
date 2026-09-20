package guardrail

import (
	"context"
	"regexp"
	"strings"
)

// injectionVerbs and injectionTargets are combined into a phrase pattern
// (see injectionPhrasePattern) — the same verb×target combinatoric
// approach LiteLLM's own default prompt-injection detector uses
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
//
// Each verb's own common inflections ("-ing"/"-ed"/irregular past forms)
// are listed right alongside its base form, added by a later audit
// finding that a plain "ignore all instructions" match against only the
// bare verb missed "ignoring all instructions" / "ignored all
// instructions" entirely — a one-suffix bypass an attacker would
// obviously try first. Same widening precedent as the paragraph above:
// real, common inflections of a verb already in the list, listed
// literally rather than computed by a stemming algorithm, matching this
// file's own "simple, auditable" design philosophy (see
// PromptInjectionDetector's doc comment below).
var injectionVerbs = []string{
	"ignore", "ignoring", "ignored",
	"disregard", "disregarding", "disregarded",
	"skip", "skipping", "skipped",
	"forget", "forgetting", "forgot", "forgotten",
	"override", "overriding", "overrode", "overridden",
	"bypass", "bypassing", "bypassed",
	"disobey", "disobeying", "disobeyed",
	"circumvent", "circumventing", "circumvented",
	"subvert", "subverting", "subverted",
}

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

// injectionPhrasePattern is injectionVerbs × injectionTargets compiled
// once, at package init, into a single case-insensitive regex — \W+ (one
// or more non-word characters) standing in for every literal space this
// package used before this fix. A literal-space join matched "ignore all
// instructions" but not "ignore  all instructions" (double space),
// "ignore\nall instructions" (newline), or "disobey,  your system
// prompt" (punctuation before the space) — every one a trivial
// find-and-replace or copy-paste artifact away from evading the whole
// detector. \W+ covers all three uniformly (whitespace and/or
// punctuation, one-or-more), while still requiring at least one
// separator character, so an ordinary "verb target" phrase with a single
// space still matches exactly as before — this is a strictly wider
// matcher, never a narrower one, so it cannot regress the True Negative
// case.
var injectionPhrasePattern = regexp.MustCompile(buildInjectionPhrasePattern())

func buildInjectionPhrasePattern() string {
	return "(?i)(?:" + joinAsWordSeparatedAlternation(injectionVerbs) + `)\W+(?:` + joinAsWordSeparatedAlternation(injectionTargets) + ")"
}

// joinAsWordSeparatedAlternation turns a list of one-or-more-word phrases
// (e.g. "your system prompt") into a single regex alternation: each
// phrase's own literal words are regexp.QuoteMeta-escaped and joined by
// \W+ (so a multi-word target tolerates the same whitespace/punctuation
// variation as the verb-target boundary does), and the phrases
// themselves are joined by "|".
func joinAsWordSeparatedAlternation(phrases []string) string {
	alternatives := make([]string, len(phrases))
	for i, phrase := range phrases {
		words := strings.Fields(phrase)
		quoted := make([]string, len(words))
		for j, w := range words {
			quoted[j] = regexp.QuoteMeta(w)
		}
		alternatives[i] = strings.Join(quoted, `\W+`)
	}
	return strings.Join(alternatives, "|")
}

// hiddenUnicodeRanges are the real, documented hidden-Unicode
// prompt-injection ranges Kong's AI Prompt Guard plugin publishes as
// known attack vectors: zero-width characters, bidirectional-control
// characters, and Unicode tag characters — none of which have any
// legitimate reason to appear in ordinary chat input.
//
// Widened 2026-09-20, by this repo's own end-to-end research round
// (docs/upgrade-research/ai-security-hardening-tier1-2026-09-20.md):
// the OWASP GenAI LLM Top 10 2026 edition names two further ranges as a
// live, real-world-cited smuggling technique this list was missing --
// U+2060 (WORD JOINER, adjacent to but not covered by the 200B-200D
// zero-width range above) and the full U+FE00-FE0F variation-selector
// block. Per that same research's own finding, this widening closes a
// concrete gap but is not itself a strong defense: OWASP's 2026 edition
// explicitly documents that no input-side detector, regex or ML,
// reliably stops an adaptive attacker (cites Nasr et al. 2025: static
// defenses ~0%, adaptive >90% success against 12 recent defenses) --
// this list stays a real, cheap, disclosed mitigation for the known,
// already-published vectors it covers, not a claim of completeness.
var hiddenUnicodeRanges = [][2]rune{
	{0x200B, 0x200D},   // zero-width space/non-joiner/joiner
	{0x2060, 0x2060},   // word joiner
	{0xFE00, 0xFE0F},   // variation selectors
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
// combinatoric phrase match (see injectionPhrasePattern) and a
// hidden-Unicode scan (see hiddenUnicodeRanges) — both
// CategoryPromptInjection (Warn tier), per THREAT_MODEL.md's LLM01
// mapping. Deliberately regex/substring-based rather than true
// fuzzy/edit-distance matching, keeping the heuristic simple and
// auditable for v1 — the same "simplest thing that works" discipline
// this project already applied to Cache L3-lite's MinHash choice over a
// heavier alternative.
type PromptInjectionDetector struct{}

func (PromptInjectionDetector) Name() string       { return "promptinjection" }
func (PromptInjectionDetector) Category() Category { return CategoryPromptInjection }

// Detect runs its phrase match against stripHiddenUnicode(text)'s
// stripped copy, not text directly — this used to be the one detector
// in this package that was an exception to every other regex detector's
// own convention
// (see stripHiddenUnicode's own doc comment). Without it, a zero-width
// space split mid-verb ("ign​ore all instructions") silently broke
// injectionPhrasePattern's own match, with no signal beyond the separate,
// generic hidden-unicode scan below (which reports only that *some*
// hidden character exists, not the specific injection-phrase signal).
// Every phrase match's offsets are remapped back to the ORIGINAL text via
// remapMatch, per that field's own documented offset contract.
//
// The hidden-Unicode scan itself deliberately keeps ranging over the
// ORIGINAL text, not the stripped copy — stripHiddenUnicode's whole job
// is to remove exactly the characters this scan exists to find, so
// running it post-strip would make it find nothing.
func (PromptInjectionDetector) Detect(_ context.Context, text string) ([]Finding, error) {
	var findings []Finding

	stripped, origOffsets := stripHiddenUnicode(text)
	for _, loc := range injectionPhrasePattern.FindAllStringIndex(stripped, -1) {
		start, end := remapMatch(origOffsets, loc[0], loc[1])
		findings = append(findings, Finding{
			Category: CategoryPromptInjection, Detector: "promptinjection",
			Start: start, End: end,
		})
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
