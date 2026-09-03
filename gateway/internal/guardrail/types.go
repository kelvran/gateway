// Package guardrail implements Kelvran's pre-call/post-call content
// middleware, per docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md.
// Pure-Go, stdlib-only in this pass — regex/checksum PII+secrets
// detection plus a prompt-injection heuristic, never free-text NER and
// never an ML/third-party moderation model (see that RFC's "why not real
// embeddings yet"-style narrowing). guardrail never imports adapter,
// cache, or any provider-specific package — text in, Verdict out; the
// distinction between pre-call/post-call and streaming/buffered is a
// dataplane concern, never this package's.
package guardrail

import "context"

// Category is one of the RFC's fixed content categories — each maps to
// exactly one Action in a Policy's Actions/ErrorActions maps.
type Category string

const (
	CategoryCredential      Category = "credential"
	CategoryFinancialID     Category = "financial_id"
	CategoryGovernmentID    Category = "government_id"
	CategoryContactInfo     Category = "contact_info"
	CategoryNetworkID       Category = "network_id"
	CategoryPromptInjection Category = "prompt_injection"
)

// Action is what a Policy does with a Category, on detection or on
// detector error (see Policy).
type Action int

const (
	ActionWarn Action = iota
	ActionBlock
)

// Finding is one detector hit within a checked text — Start/End are byte
// offsets into that text, for future redaction/audit-record use; this
// pass logs Findings but does not redact.
type Finding struct {
	Category Category
	Detector string
	Start    int
	End      int
}

// Detector is intentionally I/O-agnostic: v1 implementations are pure
// regexp/stdlib and cannot practically error, but the interface must not
// assume Detect can't fail — ARCHITECTURE.md's Guardrails Subsystem
// section already commits to a future third-party-moderation Detector
// that genuinely can error over the network, and this contract exists so
// that detector's error-handling doesn't need retrofitting later.
type Detector interface {
	Name() string
	Category() Category
	Detect(ctx context.Context, text string) ([]Finding, error)
}

// Verdict is one Engine.Check call's outcome. DetectorError is non-nil
// only when a Detector itself failed (as opposed to running cleanly and
// simply finding nothing) — see Policy.ErrorActions for how that
// distinction affects Blocked.
type Verdict struct {
	Blocked       bool
	Findings      []Finding
	DetectorError error
}
