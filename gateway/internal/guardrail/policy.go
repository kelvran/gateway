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
