package dataplane

import (
	"context"

	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// noteGuardrailFailOpen records a guardrail fail-open: a detector errored
// (or panicked — guardrail.Engine converts both into Verdict.DetectorError)
// AND the request was still allowed to proceed. It is a no-op when no
// detector errored, and deliberately also a no-op when the verdict
// blocked: a blocked request is fail-closed (the category's ErrorAction
// was Block) or a genuine finding, and neither is "allowed through with
// coverage missing", which is the only thing this signal means.
//
// Lives in the dataplane rather than inside guardrail.Engine.Check for two
// reasons. guardrail is a pure leaf (gateway/.go-arch-lint.yml denies it
// every project-internal import, telemetry included), and Engine.Check
// knows neither the virtual-key id nor WHICH check it is — the two
// attributes the sibling rate-limit/budget fail-open counters already
// carry and an operator needs to tell "an unscreened prompt reached a
// provider" (precall) from "an unscreened response reached a client"
// (postcall). The engine's own guardrail_detector_error line (detector
// name + category, no trace/key correlation) stays as-is; this adds the
// correlated log line and the aggregate, alertable counter next to it,
// per RecordGuardrailFailOpen's own convention.
//
// stage must be one of telemetry.GuardrailStagePrecall/Postcall/
// Embeddings. Callers invoke this once per request per stage — the
// embeddings call site folds its per-input loop into one call.
func (p *Pipeline) noteGuardrailFailOpen(ctx context.Context, keyID, stage string, v guardrail.Verdict) {
	if v.DetectorError == nil || v.Blocked {
		return
	}
	p.logger.Warn("guardrail_fail_open", append(traceLogFields(ctx),
		"key_id", keyID,
		"stage", stage,
		"error", v.DetectorError.Error())...)
	telemetry.RecordGuardrailFailOpen(ctx, keyID, stage)
}
