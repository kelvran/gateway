package guardrail

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// capturingLogger mirrors internal/admin/admin_test.go's own helper of the
// same name -- a real *slog.Logger writing to an in-memory buffer, so a
// test can assert on exact log output rather than just Verdict fields.
func capturingLogger() (*slog.Logger, *strings.Builder) {
	var buf strings.Builder
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

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

// panickingDetector is a test-only Detector that panics unconditionally
// -- for proving detectSafely's own recover, distinct from fakeDetector's
// ordinary error-return path above.
type panickingDetector struct {
	name     string
	category Category
}

func (p panickingDetector) Name() string       { return p.name }
func (p panickingDetector) Category() Category { return p.category }
func (p panickingDetector) Detect(_ context.Context, _ string) ([]Finding, error) {
	panic("simulated detector panic")
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

// TestEngineCheckBlockTierFindingWinsOverWarnTierFinding is the
// regression proof for a real gap: no test previously exercised two
// DIFFERENT detectors firing in the SAME Check call with conflicting
// Warn/Block verdicts. Check's own loop never short-circuits on the
// first finding -- every detector always runs, and Blocked is a
// monotonic OR across all of them -- so a Block-tier finding must win
// regardless of which detector (first or second in the slice) produced
// it, and BOTH findings must still be recorded.
func TestEngineCheckBlockTierFindingWinsOverWarnTierFinding(t *testing.T) {
	e := NewEngine([]Detector{
		fakeDetector{name: "warn-detector", category: CategoryContactInfo, findings: []Finding{{Category: CategoryContactInfo, Detector: "warn-detector"}}},
		fakeDetector{name: "block-detector", category: CategoryCredential, findings: []Finding{{Category: CategoryCredential, Detector: "block-detector"}}},
	}, DefaultPolicy(), "test", nil)

	verdict := e.Check(context.Background(), "irrelevant text")
	if !verdict.Blocked {
		t.Error("Blocked = false, want true — a Block-tier finding must win even though a Warn-tier detector ran first")
	}
	if len(verdict.Findings) != 2 {
		t.Errorf("Findings len = %d, want 2 — both detectors' findings must be recorded, not just the blocking one", len(verdict.Findings))
	}
}

// TestEngineCheckBlockTierFindingWinsRegardlessOfDetectorOrder is the
// same proof with the two detectors' order reversed, so this property
// doesn't depend on which one happens to run first.
func TestEngineCheckBlockTierFindingWinsRegardlessOfDetectorOrder(t *testing.T) {
	e := NewEngine([]Detector{
		fakeDetector{name: "block-detector", category: CategoryCredential, findings: []Finding{{Category: CategoryCredential, Detector: "block-detector"}}},
		fakeDetector{name: "warn-detector", category: CategoryContactInfo, findings: []Finding{{Category: CategoryContactInfo, Detector: "warn-detector"}}},
	}, DefaultPolicy(), "test", nil)

	verdict := e.Check(context.Background(), "irrelevant text")
	if !verdict.Blocked {
		t.Error("Blocked = false, want true")
	}
	if len(verdict.Findings) != 2 {
		t.Errorf("Findings len = %d, want 2", len(verdict.Findings))
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

// TestEngineCheckPanickingDetectorDoesNotPanicAndSetsDetectorError is the
// regression proof for the real bug fixed in detectSafely's own doc
// comment: a Detector panicking previously had no recover anywhere in
// Check's call path, unwinding straight out of Check instead of
// degrading exactly like a normal Detect error already does. Mirrors
// TestEngineCheckDetectorErrorOnBlockTierCategoryBlocks's own structure
// with a panicking detector in place of an error-returning one.
func TestEngineCheckPanickingDetectorDoesNotPanicAndSetsDetectorError(t *testing.T) {
	e := NewEngine([]Detector{
		panickingDetector{name: "fake-panicking", category: CategoryCredential},
	}, DefaultPolicy(), "test", nil)

	verdict := e.Check(context.Background(), "irrelevant text")
	if !verdict.Blocked {
		t.Error("Blocked = false, want true for a panicking detector on a Block-tier category — a panic must degrade exactly like a Detect error")
	}
	if verdict.DetectorError == nil {
		t.Error("DetectorError is nil, want a wrapped error describing the panic")
	}
}

// TestEngineCheckPanickingDetectorDoesNotPreventLaterDetectorsFromRunning
// proves the panic-recovery loop actually continues to the NEXT
// detector, rather than merely not crashing — the same "fail open, keep
// going" property Check's own err != nil branch already has via
// "continue".
func TestEngineCheckPanickingDetectorDoesNotPreventLaterDetectorsFromRunning(t *testing.T) {
	e := NewEngine([]Detector{
		panickingDetector{name: "fake-panicking", category: CategoryContactInfo},
		fakeDetector{name: "fake-after-panic", category: CategoryCredential, findings: []Finding{{Category: CategoryCredential, Detector: "fake-after-panic"}}},
	}, DefaultPolicy(), "test", nil)

	verdict := e.Check(context.Background(), "irrelevant text")
	if !verdict.Blocked {
		t.Error("Blocked = false, want true — the detector AFTER the panicking one must still have run and found a Block-tier finding")
	}
	if len(verdict.Findings) != 1 {
		t.Errorf("Findings len = %d, want 1 (only the detector after the panic contributes a finding)", len(verdict.Findings))
	}
}

// TestEngineCheckWarnTierFindingIsLogged proves a Warn-tier-only finding
// (one that never sets Blocked) is not silently dropped -- before this
// test's own fix, Engine.Check only logged when Blocked was true, so a
// real prompt-injection/contact-info/network-id detection with no
// blocking effect had zero log output anywhere, contradicting
// gateway/ARCHITECTURE.md's own "fail-open-with-logging" claim for
// exactly these categories. Found via a live evaluation against the real
// pilot gateway: a hidden-Unicode-tag-character probe produced no log
// line at all, even though the detector's own doc comment claims hidden-
// Unicode detection is real.
func TestEngineCheckWarnTierFindingIsLogged(t *testing.T) {
	logger, buf := capturingLogger()
	e := NewEngine([]Detector{
		fakeDetector{name: "fake", category: CategoryContactInfo, findings: []Finding{{Category: CategoryContactInfo, Detector: "fake"}}},
	}, DefaultPolicy(), "test", logger)

	verdict := e.Check(context.Background(), "irrelevant text")
	if verdict.Blocked {
		t.Fatal("Blocked = true, want false for a Warn-tier-only finding")
	}
	if !strings.Contains(buf.String(), "guardrail_verdict_warn") {
		t.Errorf("log output = %q, want it to contain \"guardrail_verdict_warn\"", buf.String())
	}
}

// TestEngineCheckNoFindingsProducesNoLogOutput proves the fix above
// doesn't over-fire -- an entirely clean Check (no findings at all,
// the overwhelmingly common case) must still produce zero guardrail log
// lines, exactly as before this change.
func TestEngineCheckNoFindingsProducesNoLogOutput(t *testing.T) {
	logger, buf := capturingLogger()
	e := NewEngine(DefaultDetectors(), DefaultPolicy(), "test", logger)

	e.Check(context.Background(), "just an ordinary, entirely harmless message")
	if buf.String() != "" {
		t.Errorf("log output = %q, want empty for a Check with zero findings", buf.String())
	}
}

func TestEngineCheckNoFindingsNoBlock(t *testing.T) {
	e := NewEngine(DefaultDetectors(), DefaultPolicy(), "test", nil)
	verdict := e.Check(context.Background(), "just an ordinary, entirely harmless message")
	if verdict.Blocked {
		t.Errorf("Blocked = true for entirely harmless text, findings: %v", verdict.Findings)
	}
}

// TestEngineVersion proves Version() still carries the caller-provided
// version verbatim as its own prefix -- NewEngine now appends an
// automatic policy fingerprint after it (see
// TestNewEngineVersionChangesWhenPolicyChangesEvenIfVersionStringDoesNot
// for the fingerprint half's own load-bearing proof), but the explicit
// version string itself must remain fully intact and recoverable by a
// human reading a raw cache key or log line.
func TestEngineVersion(t *testing.T) {
	e := NewEngine(nil, DefaultPolicy(), "v1.2.3", nil)
	if got := e.Version(); !strings.HasPrefix(got, "v1.2.3") {
		t.Errorf("Version() = %q, want a string starting with %q (the caller-provided version, plus an automatic policy-fingerprint suffix)", got, "v1.2.3")
	}
}

// TestNewEngineVersionChangesWhenPolicyChangesEvenIfVersionStringDoesNot
// is the load-bearing regression proof for the real gap found
// 2026-09-28: two Engines built with the IDENTICAL explicit version
// string but a genuinely DIFFERENT effective Policy (the exact shape
// cmd/gateway's newGuardrailEngine produces when an operator changes
// guardrails.category_overrides without also bumping policy_version)
// must still report different Version() values -- otherwise a cache
// entry written under the old policy stays servable under the new one.
func TestNewEngineVersionChangesWhenPolicyChangesEvenIfVersionStringDoesNot(t *testing.T) {
	unchanged := NewEngine(nil, DefaultPolicy(), "v1", nil)

	changed := DefaultPolicy()
	changed.Actions[CategoryContactInfo] = ActionBlock // was ActionWarn
	changedEngine := NewEngine(nil, changed, "v1", nil)

	if unchanged.Version() == changedEngine.Version() {
		t.Fatalf("Version() = %q for both an unchanged and a changed policy sharing the same explicit version string %q -- a policy change must force a different cache-key version", unchanged.Version(), "v1")
	}
}

// TestNewEngineVersionIsDeterministic proves policyFingerprint doesn't
// depend on Go's own randomized map iteration order: two Policy values
// built by inserting the identical (category, action) pairs in a
// different order must produce byte-identical Version() output.
func TestNewEngineVersionIsDeterministic(t *testing.T) {
	a := Policy{Actions: map[Category]Action{}, ErrorActions: map[Category]Action{}}
	for _, cat := range []Category{CategoryCredential, CategoryContactInfo, CategoryNetworkID} {
		a.Actions[cat] = ActionWarn
		a.ErrorActions[cat] = ActionBlock
	}
	b := Policy{Actions: map[Category]Action{}, ErrorActions: map[Category]Action{}}
	for _, cat := range []Category{CategoryNetworkID, CategoryCredential, CategoryContactInfo} {
		b.Actions[cat] = ActionWarn
		b.ErrorActions[cat] = ActionBlock
	}

	ea := NewEngine(nil, a, "v1", nil)
	eb := NewEngine(nil, b, "v1", nil)
	if ea.Version() != eb.Version() {
		t.Errorf("Version() = %q and %q for two Policy values with identical content inserted in a different order, want identical", ea.Version(), eb.Version())
	}
}

// TestEngineDetectorsReturnsConfiguredDetectors proves the pass-through
// accessor cmd/gateway's run() uses to discover, via a type assertion,
// which detector(s) support credential hot-reload.
func TestEngineDetectorsReturnsConfiguredDetectors(t *testing.T) {
	detectors := DefaultDetectors()
	e := NewEngine(detectors, DefaultPolicy(), "test", nil)
	got := e.Detectors()
	if len(got) != len(detectors) {
		t.Fatalf("len(Detectors()) = %d, want %d", len(got), len(detectors))
	}
	for i := range detectors {
		if got[i] != detectors[i] {
			t.Errorf("Detectors()[%d] = %v, want %v", i, got[i], detectors[i])
		}
	}
}
