package cache

import (
	"testing"
)

func trueJaccard(a, b []string) float64 {
	setA := map[string]struct{}{}
	for _, s := range a {
		setA[s] = struct{}{}
	}
	setB := map[string]struct{}{}
	for _, s := range b {
		setB[s] = struct{}{}
	}
	intersection := 0
	for s := range setA {
		if _, ok := setB[s]; ok {
			intersection++
		}
	}
	union := len(setA)
	for s := range setB {
		if _, ok := setA[s]; !ok {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func TestShinglesOverlappingWindows(t *testing.T) {
	got := Shingles("the quick brown fox jumps", 3)
	want := []string{"the quick brown", "quick brown fox", "brown fox jumps"}
	if len(got) != len(want) {
		t.Fatalf("Shingles() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Shingles()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestShinglesShortTextYieldsOneShingle(t *testing.T) {
	got := Shingles("hi there", 3)
	if len(got) != 1 || got[0] != "hi there" {
		t.Errorf("Shingles() on text shorter than k = %v, want a single shingle covering the whole text", got)
	}
}

func TestShinglesEmptyTextYieldsNil(t *testing.T) {
	if got := Shingles("", 3); got != nil {
		t.Errorf("Shingles(\"\", 3) = %v, want nil", got)
	}
}

func TestMinHashSignatureIsDeterministic(t *testing.T) {
	shingles := Shingles("the quick brown fox jumps over the lazy dog", 3)
	sig1 := MinHashSignature(shingles, 64)
	sig2 := MinHashSignature(shingles, 64)
	for i := range sig1 {
		if sig1[i] != sig2[i] {
			t.Fatalf("MinHashSignature is not deterministic: position %d differs (%d != %d)", i, sig1[i], sig2[i])
		}
	}
}

func TestJaccardEstimateApproximatesTrueJaccard(t *testing.T) {
	textA := Shingles("the quick brown fox jumps over the lazy dog", 3)
	textB := Shingles("a quick brown fox leaps over a lazy dog", 3)

	sigA := MinHashSignature(textA, 128)
	sigB := MinHashSignature(textB, 128)

	estimate, ok := JaccardEstimate(sigA, sigB)
	if !ok {
		t.Fatal("JaccardEstimate returned ok=false for two valid equal-length signatures")
	}

	trueJ := trueJaccard(textA, textB)
	if diff := estimate - trueJ; diff > 0.25 || diff < -0.25 {
		t.Errorf("JaccardEstimate = %.3f, true Jaccard = %.3f — estimate too far off (128-value MinHash should approximate within a reasonable margin)", estimate, trueJ)
	}
}

func TestJaccardEstimateIdenticalInputIsOne(t *testing.T) {
	shingles := Shingles("the quick brown fox jumps", 3)
	sig := MinHashSignature(shingles, 64)
	estimate, ok := JaccardEstimate(sig, sig)
	if !ok || estimate != 1.0 {
		t.Errorf("JaccardEstimate(sig, sig) = %.3f, ok=%v, want 1.0, true", estimate, ok)
	}
}

func TestJaccardEstimateDisjointInputIsNearZero(t *testing.T) {
	sigA := MinHashSignature(Shingles("apples oranges bananas grapes melons", 3), 128)
	sigB := MinHashSignature(Shingles("rockets planets asteroids comets nebulae", 3), 128)
	estimate, ok := JaccardEstimate(sigA, sigB)
	if !ok {
		t.Fatal("JaccardEstimate returned ok=false")
	}
	if estimate > 0.1 {
		t.Errorf("JaccardEstimate on two disjoint shingle sets = %.3f, want close to 0", estimate)
	}
}

func TestJaccardEstimateMismatchedLengthReturnsNotOK(t *testing.T) {
	if _, ok := JaccardEstimate(make([]uint64, 4), make([]uint64, 8)); ok {
		t.Error("JaccardEstimate on mismatched-length signatures returned ok=true, want false")
	}
}

func TestMinHashSignatureEmptyShinglesIsAllZeroAndNeverPanics(t *testing.T) {
	sig := MinHashSignature(nil, 64)
	if len(sig) != 64 {
		t.Fatalf("MinHashSignature(nil, 64) length = %d, want 64", len(sig))
	}
	for i, v := range sig {
		if v != 0 {
			t.Errorf("MinHashSignature(nil, 64)[%d] = %d, want 0", i, v)
		}
	}
}
