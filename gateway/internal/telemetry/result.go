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
	// docs/rfcs/2026-09-05-gateway-cache-hit-provenance.md: which cache
	// layer (if any) served this request, and — for Cache L3-lite only,
	// where the data is already captured at write time — the estimated
	// similarity and age of the served entry. See
	// ChatCompletionResult.CacheLayer's own doc comment for why
	// similarity/age are L3-only.
	AttrKelvranCacheLayer      = "kelvran.cache.layer"
	AttrKelvranCacheSimilarity = "kelvran.cache.similarity"
	AttrKelvranCacheAgeMs      = "kelvran.cache.age_ms"
)

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
	// CacheSimilarity/CacheAgeMs are only ever meaningful when
	// CacheLayer == "L3" — L1 is an exact byte match (no similarity
	// concept applies) and L2's normalized-match layer doesn't currently
	// capture a write-time age at all. Left at their zero value (0.0) for
	// any other CacheLayer, and RecordChatCompletionResult only emits
	// their attributes when CacheLayer == "L3", never a fabricated 0.0
	// for L1/L2.
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
		attrs = append(attrs, attribute.String(AttrGenAIProviderName, r.Provider))
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
	}
	// Similarity/age are only ever real, write-time-captured data for an
	// L3 hit — see CacheSimilarity/CacheAgeMs's own doc comment. Never
	// emitted for L1/L2/no-hit, which would otherwise report a
	// fabricated 0.0 rather than a genuinely absent value.
	if r.CacheLayer == "L3" {
		attrs = append(attrs,
			attribute.Float64(AttrKelvranCacheSimilarity, r.CacheSimilarity),
			attribute.Float64(AttrKelvranCacheAgeMs, r.CacheAgeMs),
		)
	}

	span.SetAttributes(attrs...)

	if r.Err != nil {
		span.RecordError(r.Err)
		span.SetStatus(codes.Error, r.Err.Error())
	}
}
