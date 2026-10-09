package dataplane

import (
	"context"

	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// effectiveAttribution returns the request's telemetry.Attribution after the
// identifier switches, per docs/rfcs/2026-10-09-gateway-attribution-and-
// spend-ledger.md (13a): the record cmd/gateway's middleware placed on ctx,
// with the identifier fields blanked when the gateway
// (attribution.capture_ids: false) or the virtual key
// (attribution_capture_ids: false) has identifier capture off — and always
// when vk is nil. finalize records the chat span even when Verify failed,
// and an unauthenticated request has no attribution owner: the per-key
// switch of the key the client MEANT cannot be consulted, so a client of an
// opted-out key presenting a revoked or mistyped token must not have its
// session, agent and prompt ids recorded on the 401 span. The span
// attributes and the spend counter read this filtered copy (finalize and
// HandleEmbeddings call it once each, so a key-level opt-out needs no
// second parse); the log line reads only the two bounded fields from the
// raw carrier (attributionLogFields), which no switch touches. Attribution
// never feeds the cache key, guardrails, routing, fallback eligibility,
// rate limits, budgets or idempotency.
func (p *Pipeline) effectiveAttribution(ctx context.Context, vk *identity.VirtualKey) telemetry.Attribution {
	a := telemetry.AttributionFromContext(ctx)
	if vk == nil || p.attributionIDsDisabled || vk.AttributionIDsDisabled {
		a = a.WithoutIdentifiers()
	}
	return a
}

// attributionLogFields returns the two BOUNDED attribution fields for a
// request log line (client_tool, request_class), normalised so they are
// always present. Identifiers are never log fields — the span is their only
// sink — so this reads the raw carrier without consulting the switches.
func attributionLogFields(ctx context.Context) []any {
	a := telemetry.AttributionFromContext(ctx)
	return []any{
		"client_tool", telemetry.NormalizedClientTool(a.ClientTool),
		"request_class", telemetry.NormalizedRequestClass(a.RequestClass),
	}
}
