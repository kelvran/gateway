package telemetry

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// GenAI semantic-convention attribute keys, per
// docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md's Detailed Design.
// Hardcoded as string constants rather than depending on
// go.opentelemetry.io/otel/semconv's incubating GenAI module — that
// module's Go API surface changes between SDK versions as the still-
// "development"-stability upstream spec itself changes; these constants
// are pinned to the semantic-conventions repo's gen-ai model as of this
// implementation and will need a follow-up pass if/when that spec
// stabilizes and renames anything.
const (
	AttrGenAIOperationName         = "gen_ai.operation.name"
	AttrGenAIProviderName          = "gen_ai.provider.name"
	AttrGenAIRequestModel          = "gen_ai.request.model"
	AttrGenAIRequestStream         = "gen_ai.request.stream"
	AttrGenAIResponseModel         = "gen_ai.response.model"
	AttrGenAIResponseID            = "gen_ai.response.id"
	AttrGenAIResponseFinishReasons = "gen_ai.response.finish_reasons"
	AttrGenAIUsageInputTokens      = "gen_ai.usage.input_tokens"
	AttrGenAIUsageOutputTokens     = "gen_ai.usage.output_tokens"

	// Kelvran-custom attributes, under a kelvran.* namespace per
	// docs/operations/TELEMETRY.md's existing framing.
	AttrKelvranVirtualKeyID   = "kelvran.virtual_key.id"
	AttrKelvranAgentRunID     = "kelvran.agent_run_id"
	AttrKelvranCacheHit       = "kelvran.cache.hit"
	AttrKelvranCostUSD        = "kelvran.cost.usd"
	AttrKelvranDeploymentName = "kelvran.deployment.name"
	// AttrKelvranCacheLayer/CacheSimilarity/CacheAgeMs are per
	// docs/rfcs/2026-09-05-gateway-cache-hit-provenance.md and
	// docs/upgrade-research/cache-2026-09-06.md Finding 6: which cache
	// layer (if any) served this request, its age (every hit layer), and
	// — for Cache L3-lite only, where a similarity concept applies at
	// all — the estimated similarity of the served entry. See
	// ChatCompletionResult.CacheSimilarity's own doc comment for why
	// similarity specifically stays L3-only.
	AttrKelvranCacheLayer      = "kelvran.cache.layer"
	AttrKelvranCacheSimilarity = "kelvran.cache.similarity"
	AttrKelvranCacheAgeMs      = "kelvran.cache.age_ms"
	// AttrKelvranCacheL3Gate/CacheL3Outcome are per
	// docs/upgrade-research/cache-2026-09-06.md Finding 1's per-gate
	// ablation instrumentation — see telemetry.RecordCacheL3GateOutcome.
	AttrKelvranCacheL3Gate    = "kelvran.cache.l3.gate"
	AttrKelvranCacheL3Outcome = "kelvran.cache.l3.outcome"
	// AttrKelvranInstanceID is per
	// docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md — see
	// InstanceID's own doc comment (telemetry.go).
	AttrKelvranInstanceID = "kelvran.instance.id"
)

// genAIProviderNameOverrides maps Kelvran's own internal provider
// identifier (Deployment.Provider / adapter.Registry's key) to
// gen_ai.provider.name's real well-known enum value, per
// docs/rfcs/2026-09-05-gateway-gen-ai-provider-name-validation.md —
// resolving the exact gap docs/rfcs/2026-09-02-otel-tracing-agent-run-
// id.md's own Unresolved Questions first named ("a future non-standard
// provider name wouldn't be caught"). Confirmed directly against the
// real registry (open-telemetry/semantic-conventions's
// model/gen-ai/deprecated/registry-deprecated.yaml), not assumed: only
// "bedrock"/"gemini" need remapping — "openai"/"anthropic" already
// match the spec's well-known values verbatim, exactly as that original
// RFC's own reasoning said. "openaicompat" has no well-known value in
// the registry at all (a self-hosted, wire-protocol-compatible runtime
// isn't a distinct GenAI provider in OTel's vocabulary) — passed
// through verbatim by genAIProviderName below rather than forced into a
// wrong mapping.
var genAIProviderNameOverrides = map[string]string{
	"bedrock": "aws.bedrock",
	"gemini":  "gcp.gemini",
}

// genAIProviderName returns provider's gen_ai.provider.name value,
// remapped via genAIProviderNameOverrides where Kelvran's own internal
// identifier differs from the spec's well-known value, or returned
// verbatim otherwise.
func genAIProviderName(provider string) string {
	if mapped, ok := genAIProviderNameOverrides[provider]; ok {
		return mapped
	}
	return provider
}

// ChatCompletionResult carries only primitive values — never
// identity.VirtualKey or adapter.ChatResponse directly — so this package
// stays a dependency-free leaf (see the package doc). Every field is
// "best effort": a caller that doesn't have a value yet (e.g. VirtualKeyID
// on an auth failure) leaves it at its zero value, and
// RecordChatCompletionResult skips setting the corresponding attribute
// rather than writing an empty/zero placeholder.
type ChatCompletionResult struct {
	VirtualKeyID   string
	Provider       string
	DeploymentName string
	ResponseModel  string
	ResponseID     string
	FinishReasons  []string
	InputTokens    int
	OutputTokens   int
	CacheHit       bool
	// CacheLayer is "L1"/"L2"/"L3", or "" when CacheHit is false. Set
	// unconditionally by the caller (never inferred here) so this package
	// stays a dependency-free leaf with zero cache-layer knowledge of its
	// own, per this file's own existing "primitive values only" rule.
	CacheLayer string
	// CacheSimilarity is only ever meaningful when CacheLayer == "L3" —
	// L1/L2 are exact/normalized byte matches, no similarity concept
	// applies. Left at its zero value (0.0) for any other CacheLayer, and
	// RecordChatCompletionResult only emits this attribute when
	// CacheLayer == "L3", never a fabricated 0.0 for L1/L2.
	//
	// CacheAgeMs, per docs/upgrade-research/cache-2026-09-06.md
	// Finding 6, is now meaningful for every hit layer (L1/L2 via
	// cache.Cache.Get's writtenAt, L3 via LexicalCandidate.WrittenAt) —
	// RecordChatCompletionResult emits it whenever CacheLayer != "".
	CacheSimilarity float64
	CacheAgeMs      float64
	// CostUSD is a pre-formatted decimal string (e.g. "0.0000575"), not a
	// float64 — per docs/rfcs/2026-09-02-decimal-cost-accounting.md, OTel's
	// attribute value model has no decimal type, and converting back to
	// float64 here would reintroduce the exact precision loss that RFC
	// removes, one hop before the data leaves the process. This also keeps
	// this package from taking on a dependency on the money-type choice.
	CostUSD    string
	AgentRunID string
	Err        error
}

// RecordChatCompletionResult sets every attribute only knowable once a
// chat completion has finished (or failed) and records r.Err on span, if
// non-nil. Shared by dataplane's HandleChatCompletion and
// HandleChatCompletionStream so there is exactly one implementation of
// "what a finished span looks like."
func RecordChatCompletionResult(span trace.Span, r ChatCompletionResult) {
	var attrs []attribute.KeyValue

	if r.VirtualKeyID != "" {
		attrs = append(attrs, attribute.String(AttrKelvranVirtualKeyID, r.VirtualKeyID))
	}
	if r.Provider != "" {
		attrs = append(attrs, attribute.String(AttrGenAIProviderName, genAIProviderName(r.Provider)))
	}
	if r.DeploymentName != "" {
		attrs = append(attrs, attribute.String(AttrKelvranDeploymentName, r.DeploymentName))
	}
	if r.ResponseModel != "" {
		attrs = append(attrs, attribute.String(AttrGenAIResponseModel, r.ResponseModel))
	}
	if r.ResponseID != "" {
		attrs = append(attrs, attribute.String(AttrGenAIResponseID, r.ResponseID))
	}
	if len(r.FinishReasons) > 0 {
		attrs = append(attrs, attribute.StringSlice(AttrGenAIResponseFinishReasons, r.FinishReasons))
	}
	if r.InputTokens > 0 {
		attrs = append(attrs, attribute.Int(AttrGenAIUsageInputTokens, r.InputTokens))
	}
	if r.OutputTokens > 0 {
		attrs = append(attrs, attribute.Int(AttrGenAIUsageOutputTokens, r.OutputTokens))
	}
	if r.AgentRunID != "" {
		attrs = append(attrs, attribute.String(AttrKelvranAgentRunID, r.AgentRunID))
	}
	// kelvran.cache.hit and kelvran.cost.usd are always meaningful (false/
	// "0" are real values, not "unknown"), so these are always set.
	// AttrKelvranCostUSD is a string attribute (see CostUSD's doc comment
	// above) — never attribute.Float64, which would reintroduce the exact
	// precision loss docs/rfcs/2026-09-02-decimal-cost-accounting.md exists
	// to remove.
	attrs = append(attrs,
		attribute.Bool(AttrKelvranCacheHit, r.CacheHit),
		attribute.String(AttrKelvranCostUSD, r.CostUSD),
	)
	if r.CacheLayer != "" {
		attrs = append(attrs, attribute.String(AttrKelvranCacheLayer, r.CacheLayer))
		// Age is real, write-time-captured data for every hit layer as of
		// docs/upgrade-research/cache-2026-09-06.md Finding 6 — never
		// emitted for a no-hit, which would otherwise report a fabricated
		// 0.0 rather than a genuinely absent value.
		attrs = append(attrs, attribute.Float64(AttrKelvranCacheAgeMs, r.CacheAgeMs))
	}
	// Similarity is only ever real, write-time-captured data for an L3
	// hit — see CacheSimilarity's own doc comment. Never emitted for
	// L1/L2/no-hit.
	if r.CacheLayer == "L3" {
		attrs = append(attrs, attribute.Float64(AttrKelvranCacheSimilarity, r.CacheSimilarity))
	}

	span.SetAttributes(attrs...)

	if r.Err != nil {
		span.RecordError(r.Err)
		span.SetStatus(codes.Error, r.Err.Error())
	}
}
