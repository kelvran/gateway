package dataplane

import (
	"testing"

	"github.com/kelvran/gateway/internal/adapter"
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
