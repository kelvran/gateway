package guardrail

// Policy maps each Category to its Action on detection AND on Detector
// error — two independent axes, per
// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md's fail-open/
// fail-closed table. Never a single global default: the rate limiter's
// blanket fail-open policy (docs/rfcs/2026-09-03-distributed-rate-limiting.md)
// is safety-netted by internal/budget.Tracker as an independent second
// control; guardrail has no equivalent second control, so it does not
// inherit that default.
type Policy struct {
	Actions      map[Category]Action
	ErrorActions map[Category]Action
	// RoleScopes narrows a Category's detectors to the message roles it
	// lists (RoleSystem/RoleUser/RoleAssistant/RoleTool). A Category with
	// no entry -- and every Category when the map is nil -- is scanned on
	// every role, which is exactly the behaviour before role scoping
	// existed; an entry that is nil or empty means the same (the loader
	// refuses one at load time, and the Go API treats it as absent, never
	// as "off" -- category_overrides deliberately has no off action). The
	// dataplane scans the post-call response as the assistant role and an
	// embeddings input as the user role. Operator shape:
	// guardrails.role_scopes.<category>, a mapping of role -> true
	// (round-5 Tier S item 25, LiteLLM v1.97.0's scan_only_tool_results
	// generalised to a role set per category).
	RoleScopes map[Category][]string
}

// DefaultPolicy returns the RFC's own v1 fail-open/fail-closed table.
// Block tier (credential/financial_id/government_id) pairs real legal/
// liability teeth with high-precision, checksum-validated detectors —
// per AWS Bedrock's own published guidance, "BLOCK for the most
// sensitive categories such as credentials... where masking isn't
// enough." Warn tier (contact_info/network_id/prompt_injection) pairs
// lower-stakes exposure with lower-precision, bare-regex/heuristic
// detectors — hard-blocking there would trade a marginal security
// benefit for a real UX cost.
func DefaultPolicy() Policy {
	return Policy{
		Actions: map[Category]Action{
			CategoryCredential:      ActionBlock,
			CategoryFinancialID:     ActionBlock,
			CategoryGovernmentID:    ActionBlock,
			CategoryContactInfo:     ActionWarn,
			CategoryNetworkID:       ActionWarn,
			CategoryPromptInjection: ActionWarn,
		},
		ErrorActions: map[Category]Action{
			CategoryCredential:      ActionBlock,
			CategoryFinancialID:     ActionBlock,
			CategoryGovernmentID:    ActionBlock,
			CategoryContactInfo:     ActionWarn,
			CategoryNetworkID:       ActionWarn,
			CategoryPromptInjection: ActionWarn,
		},
	}
}
