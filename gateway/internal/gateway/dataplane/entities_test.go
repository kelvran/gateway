package dataplane

import (
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

func fingerprintOf(content string) map[string]struct{} {
	return Fingerprint([]adapter.Message{{Role: "user", Content: content}})
}

// TestFingerprintDiffersOnDifferentDollarAmounts is the first of this
// feature's load-bearing safety tests — proving the hard gate's whole
// reason for existing actually holds. A query about $92 must never be
// mistaken for a query about $250.
func TestFingerprintDiffersOnDifferentDollarAmounts(t *testing.T) {
	a := fingerprintOf("What is 15% of $92")
	b := fingerprintOf("What is 15% of $250")
	if fingerprintsEqual(a, b) {
		t.Fatalf("fingerprints for $92 and $250 are equal (%v) — the hard gate would incorrectly allow a mismatched-amount cache hit", a)
	}
}

func TestFingerprintDiffersOnDifferentCities(t *testing.T) {
	a := fingerprintOf("What is the weather in Paris")
	b := fingerprintOf("What is the weather in London")
	if fingerprintsEqual(a, b) {
		t.Fatalf("fingerprints for Paris and London are equal (%v) — the hard gate would incorrectly allow a mismatched-city cache hit", a)
	}
}

func TestFingerprintDiffersOnDifferentDates(t *testing.T) {
	a := fingerprintOf("What happened on 2024-01-15")
	b := fingerprintOf("What happened on 2024-01-16")
	if fingerprintsEqual(a, b) {
		t.Fatalf("fingerprints for 2024-01-15 and 2024-01-16 are equal (%v) — the hard gate would incorrectly allow a mismatched-date cache hit", a)
	}
}

// TestFingerprintEmptyOnEntitylessParaphrase is the load-bearing
// "doesn't over-block" proof: two genuinely safe paraphrases that carry
// no numbers/dates/entities at all must produce EMPTY, therefore EQUAL,
// fingerprints — proving the gate doesn't reject legitimate lexical
// near-duplicates just because they start with different question words.
func TestFingerprintEmptyOnEntitylessParaphrase(t *testing.T) {
	a := fingerprintOf("How do I reverse a list")
	b := fingerprintOf("What's the way to reverse a list")
	if len(a) != 0 {
		t.Errorf("fingerprint for %q = %v, want empty (sentence-initial word must be excluded)", "How do I reverse a list", a)
	}
	if len(b) != 0 {
		t.Errorf("fingerprint for %q = %v, want empty (sentence-initial word must be excluded)", "What's the way to reverse a list", b)
	}
	if !fingerprintsEqual(a, b) {
		t.Errorf("fingerprints for two entity-less paraphrases are not equal: %v != %v", a, b)
	}
}

func TestFingerprintCapturesMultiWordEntity(t *testing.T) {
	fp := fingerprintOf("Tell me about the Golden Gate Bridge")
	if _, ok := fp["Golden Gate Bridge"]; !ok {
		t.Errorf("fingerprint = %v, want it to contain the multi-word entity %q", fp, "Golden Gate Bridge")
	}
}

func TestFingerprintCapturesPercentage(t *testing.T) {
	fp := fingerprintOf("what is 15% of 200")
	if _, ok := fp["15%"]; !ok {
		t.Errorf("fingerprint = %v, want it to contain %q", fp, "15%")
	}
	if _, ok := fp["200"]; !ok {
		t.Errorf("fingerprint = %v, want it to contain %q", fp, "200")
	}
}

func negationFingerprintOf(content string) map[string]struct{} {
	return NegationFingerprint([]adapter.Message{{Role: "user", Content: content}})
}

// TestNegationFingerprintDiffersOnInsertedNegationParticle is this new
// gate's own load-bearing safety test — proving it catches the failure
// mode it exists for: a negation particle inserted into an otherwise
// identical query must produce a different fingerprint.
func TestNegationFingerprintDiffersOnInsertedNegationParticle(t *testing.T) {
	a := negationFingerprintOf("Should I take ibuprofen for this?")
	b := negationFingerprintOf("Should I not take ibuprofen for this?")
	if fingerprintsEqual(a, b) {
		t.Fatalf("negation fingerprints for %q and %q are equal (%v) — the gate would incorrectly allow a negation-reversed cache hit", "Should I take ibuprofen for this?", "Should I not take ibuprofen for this?", a)
	}
}

func TestNegationFingerprintCatchesContractedForm(t *testing.T) {
	a := negationFingerprintOf("Can I take ibuprofen for this?")
	b := negationFingerprintOf("Can't I take ibuprofen for this?")
	if fingerprintsEqual(a, b) {
		t.Fatalf("negation fingerprints for the contracted form are equal (%v) — %q must be detected", a, "can't")
	}
}

// TestNegationFingerprintCatchesContractedFormWithTypographicApostrophe
// is the same proof as TestNegationFingerprintCatchesContractedForm
// above, but using the typographic ("smart quote") apostrophe U+2019
// real user- or LLM-generated text very commonly uses instead of ASCII
// U+0027. Before apostropheVariantsReplacer existed, this produced an
// EMPTY negation fingerprint — byte-identical to the genuinely
// negation-free query — silently defeating the hard gate for this
// entire, common input class.
func TestNegationFingerprintCatchesContractedFormWithTypographicApostrophe(t *testing.T) {
	a := negationFingerprintOf("Can I take ibuprofen for this?")
	b := negationFingerprintOf("Can’t I take ibuprofen for this?")
	if fingerprintsEqual(a, b) {
		t.Fatalf("negation fingerprints using a typographic apostrophe are equal (%v) — %q must still be detected", a, "can’t")
	}
	if len(b) == 0 {
		t.Fatalf("negation fingerprint for the typographic-apostrophe contraction is empty, want it to contain a normalized negation particle")
	}
}

// TestNegationFingerprintEmptyOnEntitylessParaphrase mirrors
// TestFingerprintEmptyOnEntitylessParaphrase's own "doesn't over-block"
// proof: two genuinely safe paraphrases carrying no negation particles
// at all must produce EMPTY, therefore EQUAL, negation fingerprints.
func TestNegationFingerprintEmptyOnEntitylessParaphrase(t *testing.T) {
	a := negationFingerprintOf("How do I reverse a list")
	b := negationFingerprintOf("What's the way to reverse a list")
	if len(a) != 0 || len(b) != 0 {
		t.Errorf("negation fingerprints = %v / %v, want both empty", a, b)
	}
	if !fingerprintsEqual(a, b) {
		t.Errorf("negation fingerprints for two negation-less paraphrases are not equal: %v != %v", a, b)
	}
}

// TestNegationFingerprintDoesNotCatchAntonymVerbFlip is the explicit,
// documented non-goal proof: this gate deliberately does NOT close the
// antonym-verb-flip case DECISIONS.md's [2026-09-08] entry already
// investigated and rejected fixing here — "withhold" and "administer"
// contain zero negation particles, so this fingerprint alone is
// identical (empty) for both. See NegationFingerprint's own doc comment.
func TestNegationFingerprintDoesNotCatchAntonymVerbFlip(t *testing.T) {
	withhold := negationFingerprintOf("Should the nurse withhold the study drug from the patient?")
	administer := negationFingerprintOf("Should the nurse administer the study drug to the patient?")
	if !fingerprintsEqual(withhold, administer) {
		t.Fatalf("negation fingerprints for the antonym-flip case differ (%v vs %v) — this test exists to prove the documented boundary stays honest; if this now fails, NegationFingerprint's scope has silently grown beyond what DECISIONS.md's [2026-09-12] entry describes", withhold, administer)
	}
}
