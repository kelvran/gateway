package guardrail

import (
	"context"
	"errors"
	"testing"
)

// fakeDetector is a test-only Detector returning a fixed set of findings
// and/or a fixed error, for controlling Engine.Check's inputs precisely.
type fakeDetector struct {
	name     string
	category Category
	findings []Finding
	err      error
}

func (f fakeDetector) Name() string       { return f.name }
func (f fakeDetector) Category() Category { return f.category }
func (f fakeDetector) Detect(_ context.Context, _ string) ([]Finding, error) {
	return f.findings, f.err
}

func TestEngineCheckBlockTierFindingBlocks(t *testing.T) {
	e := NewEngine([]Detector{
		fakeDetector{name: "fake", category: CategoryCredential, findings: []Finding{{Category: CategoryCredential, Detector: "fake"}}},
	}, DefaultPolicy(), "test", nil)

	verdict := e.Check(context.Background(), "irrelevant text")
	if !verdict.Blocked {
		t.Error("Blocked = false, want true for a Block-tier finding")
	}
}

func TestEngineCheckWarnTierFindingDoesNotBlock(t *testing.T) {
	e := NewEngine([]Detector{
		fakeDetector{name: "fake", category: CategoryContactInfo, findings: []Finding{{Category: CategoryContactInfo, Detector: "fake"}}},
	}, DefaultPolicy(), "test", nil)

	verdict := e.Check(context.Background(), "irrelevant text")
	if verdict.Blocked {
		t.Error("Blocked = true, want false for a Warn-tier-only finding")
	}
	if len(verdict.Findings) != 1 {
		t.Errorf("Findings = %v, want exactly 1 (still recorded, even though not blocking)", verdict.Findings)
	}
}

func TestEngineCheckDetectorErrorOnBlockTierCategoryBlocks(t *testing.T) {
	e := NewEngine([]Detector{
		fakeDetector{name: "fake", category: CategoryCredential, err: errors.New("simulated detector failure")},
	}, DefaultPolicy(), "test", nil)

	verdict := e.Check(context.Background(), "irrelevant text")
	if !verdict.Blocked {
		t.Error("Blocked = false, want true for a detector error on a Block-tier category")
	}
	if verdict.DetectorError == nil {
		t.Error("DetectorError is nil, want the simulated error")
	}
}

func TestEngineCheckDetectorErrorOnWarnTierCategoryDoesNotBlock(t *testing.T) {
	e := NewEngine([]Detector{
		fakeDetector{name: "fake", category: CategoryContactInfo, err: errors.New("simulated detector failure")},
	}, DefaultPolicy(), "test", nil)

	verdict := e.Check(context.Background(), "irrelevant text")
	if verdict.Blocked {
		t.Error("Blocked = true, want false for a detector error on a Warn-tier category")
	}
}

func TestEngineCheckNoFindingsNoBlock(t *testing.T) {
	e := NewEngine(DefaultDetectors(), DefaultPolicy(), "test", nil)
	verdict := e.Check(context.Background(), "just an ordinary, entirely harmless message")
	if verdict.Blocked {
		t.Errorf("Blocked = true for entirely harmless text, findings: %v", verdict.Findings)
	}
}

func TestEngineVersion(t *testing.T) {
	e := NewEngine(nil, DefaultPolicy(), "v1.2.3", nil)
	if got := e.Version(); got != "v1.2.3" {
		t.Errorf("Version() = %q, want %q", got, "v1.2.3")
	}
}
