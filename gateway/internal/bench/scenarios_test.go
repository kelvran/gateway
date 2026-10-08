package bench

import "testing"

func TestScenarioPresetsCoverThePlannedSetAndAreWellFormed(t *testing.T) {
	names := map[string]bool{}
	for _, s := range Scenarios() {
		if names[s.Name] {
			t.Errorf("duplicate scenario name %q", s.Name)
		}
		names[s.Name] = true
		if s.Description == "" || s.RPS <= 0 && len(s.RPSSteps) == 0 {
			t.Errorf("scenario %q lacks a description or a rate", s.Name)
		}
		if s.Stream && s.MockChunks < 1 {
			t.Errorf("streaming scenario %q has no chunks", s.Name)
		}
	}
	for _, want := range []string{"S1a", "S1b", "S2", "S3", "S4", "S5", "S6"} {
		if !names[want] {
			t.Errorf("missing planned scenario %q", want)
		}
	}
	if _, ok := LookupScenario("S2"); !ok {
		t.Error("LookupScenario(S2) not found")
	}
	if _, ok := LookupScenario("nope"); ok {
		t.Error("LookupScenario(nope) found something")
	}
}
