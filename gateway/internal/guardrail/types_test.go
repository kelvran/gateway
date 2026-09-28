package guardrail

import "testing"

// TestVerdictDetectorNamesEmptyForZeroFindings proves the documented
// "always non-nil, even when empty" contract -- a caller logging this
// value directly must never see a nil slice.
func TestVerdictDetectorNamesEmptyForZeroFindings(t *testing.T) {
	got := Verdict{}.DetectorNames()
	if got == nil {
		t.Fatal("DetectorNames() = nil, want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("DetectorNames() = %v, want empty", got)
	}
}

// TestVerdictDetectorNamesDeduplicatesAndPreservesFirstSeenOrder proves
// the result is deduplicated and NOT alphabetically sorted -- "b" appears
// before "a" in the fixture specifically to catch an implementation that
// sorts instead of preserving first-seen order.
func TestVerdictDetectorNamesDeduplicatesAndPreservesFirstSeenOrder(t *testing.T) {
	v := Verdict{Findings: []Finding{
		{Detector: "b"},
		{Detector: "a"},
		{Detector: "b"},
		{Detector: "b"},
	}}
	got := v.DetectorNames()
	want := []string{"b", "a"}
	if len(got) != len(want) {
		t.Fatalf("DetectorNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DetectorNames() = %v, want %v (first-seen order, deduplicated)", got, want)
		}
	}
}

// TestVerdictDetectorNamesDistinguishesBedrockPromptAttackFromRegexPromptInjection
// is the direct proof this feature exists for: two findings sharing the
// SAME Category (CategoryPromptInjection) but different Detector must
// both appear, distinctly -- proving attribution is by Detector, not by
// Category, which is the exact gap a live end-to-end verification pass
// found (the Bedrock ML PROMPT_ATTACK detector's own contribution wasn't
// attributable separately from the regex/heuristic prompt-injection
// detector).
func TestVerdictDetectorNamesDistinguishesBedrockPromptAttackFromRegexPromptInjection(t *testing.T) {
	v := Verdict{Findings: []Finding{
		{Category: CategoryPromptInjection, Detector: "bedrock_guardrails_prompt_attack"},
		{Category: CategoryPromptInjection, Detector: "promptinjection"},
	}}
	got := v.DetectorNames()
	want := []string{"bedrock_guardrails_prompt_attack", "promptinjection"}
	if len(got) != len(want) {
		t.Fatalf("DetectorNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DetectorNames() = %v, want %v -- the ML Bedrock detector and the regex detector must both be attributed distinctly, not collapsed into their shared Category", got, want)
		}
	}
}
