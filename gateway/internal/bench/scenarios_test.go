package bench

import (
	"reflect"
	"strings"
	"testing"
)

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
	for _, want := range []string{"S1a", "S1r", "S1b", "S2", "S3", "S4", "S5", "S6"} {
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

func TestS1rIsS1asTwinInRedisMode(t *testing.T) {
	a, ok := LookupScenario("S1a")
	if !ok {
		t.Fatal("S1a missing")
	}
	r, ok := LookupScenario("S1r")
	if !ok {
		t.Fatal("S1r missing")
	}
	twin := a
	twin.Name, twin.Description = r.Name, r.Description
	if !reflect.DeepEqual(r, twin) {
		t.Errorf("S1r must differ from S1a only by name and description, so the delta is the Redis cost:\n S1a %+v\n S1r %+v", a, r)
	}
	if !strings.Contains(r.Description, "Redis") || !strings.Contains(r.Description, "BENCH_REDIS_ADDR") {
		t.Errorf("S1r's description must say it runs the gateway in Redis mode and needs BENCH_REDIS_ADDR: %q", r.Description)
	}
}
