package guardrail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
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

// NewEngine constructs an Engine. version is combined with an automatic
// fingerprint of policy's own actual enforcement state (see
// policyFingerprint) into the single string Version() returns, which is
// stamped into every cache write (see cache.Key/NormalizedKey's
// guardrailPolicyVersion parameter and LexicalCandidate.
// GuardrailPolicyVersion) so a policy/detector change invalidates stale
// cache entries rather than silently serving a hit that was never
// checked under the current rules.
//
// version itself still must be bumped by hand whenever DETECTOR code
// (not policy data) changes and a new binary is released — the
// automatic policy-fingerprint half cannot observe a code change, only
// a change to the resulting Policy value. It closes a real, separate
// gap found 2026-09-28: cmd/gateway's newGuardrailEngine applies
// cfg.CategoryOverrides on top of DefaultPolicy() to build policy, but
// PolicyVersion is a config value the operator sets independently — an
// operator who changes category_overrides without realizing they also
// need to bump policy_version got a silent cache-key collision. The
// fingerprint makes closing that collision automatic and unconditional
// — no operator action required — without weakening version's own,
// separate, code-change-tracking half of the contract.
//
// **Corrected 2026-09-28, same day, via live end-to-end verification**:
// this doc comment originally claimed "a Redis-backed L2/L3 cache
// survives the resulting gateway restart" as the reason this collision
// is reachable in practice today. That's wrong — no Redis-backed L1/L2/L3
// cache exists in shipped code; internal/cache/inprocess is the only
// real implementation, and docs/rfcs/2026-09-11-gateway-redis-backed-cache-design.md
// is still design-only. NewEngine is called exactly once per process
// (grep-confirmed: cmd/gateway's newGuardrailEngine is its only call
// site), so Version() never changes within one process's lifetime
// either way — and category_overrides can only change via a config
// edit plus a full restart, which wipes an in-process cache clean
// regardless of this fix. The collision this fix closes is therefore
// NOT reachable in Kelvran's current, real deployment shape. It remains
// worth keeping: it's a genuine correctness improvement (Version()
// should reflect the actual enforcement state, not just an
// operator-remembered label) that costs nothing and closes a latent
// landmine the moment the Redis-backed cache RFC above ships — at which
// point this exact scenario becomes live-reachable and this fix will
// already be in place. Framed honestly as forward-looking hardening,
// not an active-exploit fix, going forward.
//
// Scope limit, deliberate: the fingerprint covers policy (Actions/
// ErrorActions) only, never the active detector SET. Enabling/disabling
// cfg.BedrockGuardrails/cfg.EmbedSim via config also changes what a
// cached "clean" verdict actually means (it was never checked by a
// detector that didn't exist yet), but neither is reflected in policy
// at all — an operator doing that today still needs to bump version by
// hand, exactly as before this fix. Automating that too would need
// hashing the resolved detector configuration, not just Policy; left
// for its own follow-up rather than folded in here.
func NewEngine(detectors []Detector, policy Policy, version string, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{detectors: detectors, policy: policy, version: version + policyFingerprint(policy), logger: logger}
}

// Version returns the Engine's own cache-differentiating version
// string: the caller-provided version, combined with an automatic
// fingerprint of policy's actual enforcement state — see NewEngine's
// own doc comment for why both halves matter. The exact composition
// (separator, hash format/length) is an internal implementation detail
// — callers must treat the whole string as an opaque cache-key input,
// never parse or reconstruct a half of it.
func (e *Engine) Version() string { return e.version }

// policyFingerprint returns a short, deterministic string derived from
// p's own actual enforcement state (Actions + ErrorActions), sorted by
// Category for a result stable regardless of map iteration order.
//
// A plain colon/semicolon-joined encoding is safe here, unlike
// cache.writeField's own necessarily length-prefixed, collision-safe
// encoding (see that function's own doc comment on the KeyPooling
// class of attack it exists to close): every Category value that can
// ever appear in p.Actions/p.ErrorActions is one of this package's own
// small, fixed set of hardcoded string constants (types.go) —
// cmd/gateway's newGuardrailEngine rejects and skips any
// config-supplied category string that doesn't already match one of
// them, so this input space is never attacker- or even
// operator-string-controlled the way cache.Key's own client-supplied
// fields are; a colon or semicolon can never actually appear inside a
// Category value in practice.
func policyFingerprint(p Policy) string {
	seen := make(map[Category]struct{}, len(p.Actions)+len(p.ErrorActions))
	for cat := range p.Actions {
		seen[cat] = struct{}{}
	}
	for cat := range p.ErrorActions {
		seen[cat] = struct{}{}
	}
	categories := make([]string, 0, len(seen))
	for cat := range seen {
		categories = append(categories, string(cat))
	}
	sort.Strings(categories)

	h := sha256.New()
	for _, catStr := range categories {
		cat := Category(catStr)
		// hash.Hash's Write can never return an error (io.Writer's own
		// contract for this type), matching cache/key.go's own writeField
		// convention of discarding it explicitly rather than checking.
		_, _ = fmt.Fprintf(h, "%s:%d:%d;", catStr, p.Actions[cat], p.ErrorActions[cat])
	}
	return "#policy-" + hex.EncodeToString(h.Sum(nil))[:12]
}

// Detectors returns e's configured detector list — read-only access for
// a caller that needs to discover per-detector capabilities beyond
// Check's own aggregate result, e.g. cmd/gateway's run() locating
// whichever detector(s) support credential hot-reload via a type
// assertion (see that function's own credentialReloader interface).
func (e *Engine) Detectors() []Detector { return e.detectors }

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
		// **Fixed 2026-09-17, real bug**: a Detector panicking (e.g. a
		// nil-pointer deref, or bedrockguard hitting an unexpected
		// response shape) previously had no recover anywhere in this
		// call path -- it would unwind straight out of Check, aborting
		// this request ungracefully (net/http's own per-request recovery
		// catches it eventually, but as a bare 500, skipping every
		// REMAINING detector and defeating this subsystem's own
		// documented "fail-open-with-logging" design for every other
		// failure mode). detectSafely converts a panic into the exact
		// same (nil, error) shape a normal Detect error already
		// produces, so the existing err-handling branch below (log +
		// ErrorActions-gated block + continue to the next detector)
		// applies uniformly to both.
		findings, err := detectSafely(ctx, d, text)
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
	// A Warn-tier category's own findings (prompt_injection, contact_info,
	// network_id) never set Blocked, but they still deserve a log line --
	// "fail-open-with-logging" (gateway/ARCHITECTURE.md's Guardrails
	// Subsystem section) is only true if the "with-logging" half is real.
	// Before this, a non-blocking finding was recorded on Verdict.Findings
	// but every one of this Engine's 4 call sites (dataplane.go/
	// streaming.go, pre-call and post-call) only ever inspects
	// verdict.Blocked -- so a Warn-tier detection had zero log output,
	// zero metric, zero audit trail anywhere, indistinguishable from the
	// detector never firing at all.
	if verdict.Blocked {
		e.logger.Warn("guardrail_verdict_blocked", "finding_count", len(verdict.Findings), "finding_detectors", verdict.DetectorNames())
	} else if len(verdict.Findings) > 0 {
		e.logger.Warn("guardrail_verdict_warn", "finding_count", len(verdict.Findings), "finding_detectors", verdict.DetectorNames())
	}
	return verdict
}

// detectSafely calls d.Detect, recovering a panic into a plain error so
// Check's own err-handling branch (log + ErrorActions-gated block +
// continue) applies uniformly whether a detector returns an error or
// panics -- see Check's own doc comment on this call site for the real
// bug this closes.
func detectSafely(ctx context.Context, d Detector, text string) (findings []Finding, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("guardrail: detector %q panicked: %v", d.Name(), r)
		}
	}()
	return d.Detect(ctx, text)
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
