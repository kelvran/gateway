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
	// Role is the message role of the Segment this finding starts in when
	// the check came through CheckSegments -- RoleSystem/RoleUser/
	// RoleAssistant/RoleTool, RoleOther for a message whose role was none
	// of those (the dataplane bounds the client's label before it gets
	// here), or whatever string a direct caller put in Segment.Role -- and
	// empty for a Check(ctx, text) call, which carries no role. Start/End
	// are offsets into the text the detector saw: for a role-scoped
	// detector that is the newline-join of the admitted segments only (see
	// CheckSegments), not the whole input; a finding that starts on the
	// newline between two admitted segments belongs to the following one.
	Role string
}

// The four message roles a Segment can carry and a Policy.RoleScopes
// entry can name -- adapter.Message.Role's own vocabulary. "tool" is a
// tool-call RESULT the caller fed back; a model's tool-call arguments
// travel with the assistant message that emitted them, so they are
// assistant content here. Plain strings, not a new type: this package
// never imports adapter, and the dataplane copies Message.Role straight
// into Segment.Role.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
	// RoleOther is the label the dataplane gives a message whose role is
	// none of the four before it becomes a Segment.Role and, through
	// Finding.Role, a log field: a client-chosen string must never reach
	// a log field unbounded (the rule the dataplane's boundedForTelemetry
	// enforces for the model name; here a closed vocabulary is the
	// stronger fit, since finding_roles is an enum with no cardinality to
	// bound), and "other" is not a KnownRole, so the engine admits it to
	// every detector exactly as it would the raw unknown role.
	RoleOther = "other"
)

// KnownRole reports whether r is one of the four message roles above --
// the only values a role scope may name (controlplane rejects any other
// role at load time; cmd/gateway only warns-and-skips an unknown
// category) and the only roles a scope can exclude: a Segment whose Role
// is empty, RoleOther, or any other string is admitted by every
// detector, so scoping can never hide text whose role the caller could
// not attribute.
func KnownRole(r string) bool {
	switch r {
	case RoleSystem, RoleUser, RoleAssistant, RoleTool:
		return true
	}
	return false
}

// Segment is one role-tagged piece of a checked input -- in the
// dataplane, one message's scannable text with that message's role.
// CheckSegments joins the segments a detector may see with "\n", so a
// slice whose roles are all admitted is scanned as exactly the string
// Check would have received.
type Segment struct {
	Role string
	Text string
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

// DetectorNames returns the distinct Detector strings that fired across
// v.Findings, in first-seen order, deduplicated -- e.g.
// ["bedrock_guardrails_prompt_attack", "promptinjection"] when both an
// AWS Bedrock Guardrails PROMPT_ATTACK hit and this package's own
// regex/heuristic prompt-injection detector fire on the same check.
// Every detector (bedrockguard.go, promptinjection.go, every regex
// detector in this package) already sets Finding.Detector to its own
// distinct name -- this method only ever summarizes that existing,
// already-correct data, never derives or infers a name itself. Always
// non-nil, even when v.Findings is empty -- callers (Check below, and
// dataplane/streaming.go's own log call sites) pass this directly into
// a structured log field.
//
// Deliberately NOT sorted: first-seen order preserves the
// detector-execution-order signal (Check iterates e.detectors in a
// fixed, caller-supplied order), which is itself informative for
// triage, and is already deterministic per-run without needing to sort.
func (v Verdict) DetectorNames() []string {
	names := make([]string, 0, len(v.Findings))
	seen := make(map[string]struct{}, len(v.Findings))
	for _, f := range v.Findings {
		if _, ok := seen[f.Detector]; ok {
			continue
		}
		seen[f.Detector] = struct{}{}
		names = append(names, f.Detector)
	}
	return names
}

// FindingRoles returns the distinct non-empty Finding.Role values across
// v.Findings in first-seen order -- DetectorNames' role-scoping companion,
// with the same always-non-nil, straight-into-a-log-field contract. An
// unscoped Check call's findings carry no role and contribute nothing.
func (v Verdict) FindingRoles() []string {
	roles := make([]string, 0, len(v.Findings))
	seen := make(map[string]struct{}, len(v.Findings))
	for _, f := range v.Findings {
		if f.Role == "" {
			continue
		}
		if _, ok := seen[f.Role]; ok {
			continue
		}
		seen[f.Role] = struct{}{}
		roles = append(roles, f.Role)
	}
	return roles
}
