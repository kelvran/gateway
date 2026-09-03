package guardrail

import (
	"context"
	"log/slog"
)

// Engine is Kelvran's guardrail check — one instance shared across every
// pre-call/post-call check, buffered and streaming alike.
// dataplane.Pipeline calls Check identically for both; the distinction
// between pre/post-call and streaming/buffered enforcement lives there,
// never here.
type Engine struct {
	detectors []Detector
	policy    Policy
	version   string
	logger    *slog.Logger
}

// NewEngine constructs an Engine. version is stamped into every cache
// write (see cache.Key/NormalizedKey's guardrailPolicyVersion parameter
// and LexicalCandidate.GuardrailPolicyVersion) so a policy/detector
// change invalidates stale cache entries rather than silently serving a
// hit that was never checked under the current rules — bump it whenever
// detectors or policy change and a new binary is released.
func NewEngine(detectors []Detector, policy Policy, version string, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{detectors: detectors, policy: policy, version: version, logger: logger}
}

// Version returns the Engine's own policy-version string.
func (e *Engine) Version() string { return e.version }

// Check runs every configured Detector against text and returns the
// combined Verdict. A Block-tier finding, or a Detector error on a
// Block-tier category (per Policy.ErrorActions), sets Blocked — an
// error on a Warn-tier category is logged but does not set Blocked, per
// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md's category-tiered
// fail-open/fail-closed design. Every Block-tier finding and every
// detector error is logged at Warn level — never silent, matching
// LiteLLM's own "critical"-level fail-open logging convention for
// exactly this kind of decision.
func (e *Engine) Check(ctx context.Context, text string) Verdict {
	var verdict Verdict
	for _, d := range e.detectors {
		findings, err := d.Detect(ctx, text)
		if err != nil {
			e.logger.Warn("guardrail_detector_error", "detector", d.Name(), "category", string(d.Category()), "error", err.Error())
			verdict.DetectorError = err
			if e.policy.ErrorActions[d.Category()] == ActionBlock {
				verdict.Blocked = true
			}
			continue
		}
		for _, f := range findings {
			verdict.Findings = append(verdict.Findings, f)
			if e.policy.Actions[f.Category] == ActionBlock {
				verdict.Blocked = true
			}
		}
	}
	if verdict.Blocked {
		e.logger.Warn("guardrail_verdict_blocked", "finding_count", len(verdict.Findings))
	}
	return verdict
}

// DefaultDetectors returns the RFC's own v1 detector set — every
// pure-Go, stdlib-only detector this pass ships, in no particular order
// (Engine.Check runs all of them regardless of order).
func DefaultDetectors() []Detector {
	return []Detector{
		EmailDetector{},
		PhoneDetector{},
		SSNDetector{},
		IBANDetector{},
		CreditCardDetector{},
		IPAddressDetector{},
		SecretKeyDetector{},
		PromptInjectionDetector{},
	}
}
