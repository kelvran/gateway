package telemetry

import (
	"time"

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
	// AttrGenAIUsageCacheReadInputTokens/CacheCreationInputTokens are
	// real GenAI semantic-convention attributes, added in the spec's
	// v1.40.0 (Feb 2026) — confirmed directly against
	// open-telemetry/semantic-conventions-genai, not assumed. Round-4
	// research (docs/upgrade-research/gateway-observability-sre-round4-
	// 2026-09-11.md, Finding 1) initially dismissed these as inapplicable
	// to Kelvran ("no provider-side prompt-caching integration yet") —
	// found FALSE by a later backlog audit: Kelvran's own cache-token
	// cost-accounting work (adapter.Usage.CacheReadTokens/
	// CacheCreationTokens, populated for all 5 adapters) already computes
	// exactly this data on every request; it simply never reached these
	// two span attributes. Deliberately span-attributes only in this
	// pass — NOT also added to the gen_ai.client.token.usage histogram's
	// token-type breakdown (telemetry.go's GenAITokenTypeInput/Output):
	// this codebase could not independently verify whether the spec's
	// gen_ai.token.type enum has real "cache_read"/"cache_creation"
	// values (as opposed to only "input"/"output"), and fabricating an
	// enum value neither this file nor its research grounding actually
	// confirmed is worse than a narrower, honestly-scoped fix — named
	// here as a real, disclosed gap rather than guessed at.
	AttrGenAIUsageCacheReadInputTokens     = "gen_ai.usage.cache_read.input_tokens"
	AttrGenAIUsageCacheCreationInputTokens = "gen_ai.usage.cache_creation.input_tokens"
	// AttrGenAIRequestModel is defined above but was previously never set
	// on the span — RecordChatCompletionMetrics now sets it on the two
	// new GenAI Metrics histograms (see telemetry.go), per
	// docs/upgrade-research/gateway-2026-09-06.md Finding 3.
	AttrGenAITokenType = "gen_ai.token.type"

	// AttrErrorType is OTel's general (non-GenAI-specific) semantic-
	// convention attribute for a low-cardinality description of what
	// went wrong — conditionally required on
	// gen_ai.client.operation.duration whenever the operation itself
	// failed. Not namespaced under gen_ai.* or kelvran.*: it's a
	// cross-signal generic attribute defined by OTel's own error
	// attribute group, reused verbatim rather than given a Kelvran-local
	// name.
	AttrErrorType = "error.type"

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
	// AttrKelvranPromptID/PromptVersion are per
	// docs/rfcs/2026-09-13-gateway-prompt-management.md — prompts are a
	// full, admin-API-exposed, versioned resource, but resolving one had
	// zero corresponding observability signal until now (found by a
	// post-round-4 backlog audit): an operator debugging a bad response
	// from a resolved prompt template had no way to see which prompt_id/
	// version produced it from any span. Only set when req.PromptID !=
	// "" — every request that doesn't use server-side prompt management
	// (the common case) emits neither attribute, never a fabricated "".
	AttrKelvranPromptID      = "kelvran.prompt.id"
	AttrKelvranPromptVersion = "kelvran.prompt.version"
	// AttrKelvranResponseFormatRequestedNotEnforced is per
	// docs/rfcs/2026-09-12-gateway-structured-output-normalization.md's
	// own disclosed Drawback: a first-attempt (non-fallback) call to a
	// Bedrock model outside the structured-output whitelist silently
	// omits schema enforcement, with zero prior error/log/span signal —
	// found by a post-round-4 backlog audit to have no way for an
	// operator to detect this silent degradation happened. Set only
	// when a request actually asked for adapter.ChatRequest.ResponseFormat
	// AND the serving deployment could not honor it — never emitted
	// (not even `false`) for a request that never asked for structured
	// output at all, or that got it correctly enforced.
	AttrKelvranResponseFormatRequestedNotEnforced = "kelvran.response_format.requested_not_enforced"
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
	// CacheReadTokens/CacheCreationTokens mirror
	// adapter.Usage.CacheReadTokens/CacheCreationTokens (already computed
	// by cost accounting at dataplane.finalize's call site, for all 5
	// adapters) — see AttrGenAIUsageCacheReadInputTokens's own doc
	// comment for why this data previously never reached a span
	// attribute despite already existing on every request. Left at 0
	// (the honest "no cache tokens" default, real for the large majority
	// of requests) when not applicable; RecordChatCompletionResult only
	// emits the corresponding attribute when > 0, matching
	// InputTokens/OutputTokens's own identical convention.
	CacheReadTokens     int
	CacheCreationTokens int
	// PromptID/PromptVersion are req.PromptID/req.PromptVersion at
	// dataplane.finalize's call site — "" / 0 (PromptID's own zero
	// value) whenever server-side prompt management wasn't used for
	// this request. See AttrKelvranPromptID's own doc comment.
	PromptID      string
	PromptVersion int
	// ResponseFormatRequestedNotEnforced is true only when this request
	// asked for structured output (adapter.ChatRequest.ResponseFormat !=
	// nil) AND the deployment that actually served it (dep, at
	// dataplane.finalize's call site) could not honor that requirement —
	// re-derived via the same capabilityOKForRequest pure function
	// attemptFallbackChain already uses to skip an incapable FALLBACK
	// target, called again here purely for observability on whichever
	// deployment ultimately served the request (including a first-
	// attempt deployment attemptFallbackChain's own capability check
	// never runs against at all, per that RFC's own disclosed v1 scope
	// limit). See AttrKelvranResponseFormatRequestedNotEnforced's own
	// doc comment.
	ResponseFormatRequestedNotEnforced bool
	// CostUSD is a pre-formatted decimal string (e.g. "0.0000575"), not a
	// float64 — per docs/rfcs/2026-09-02-decimal-cost-accounting.md, OTel's
	// attribute value model has no decimal type, and converting back to
	// float64 here would reintroduce the exact precision loss that RFC
	// removes, one hop before the data leaves the process. This also keeps
	// this package from taking on a dependency on the money-type choice.
	CostUSD    string
	AgentRunID string
	Err        error

	// RequestModel is req.Model at dataplane.finalize's call site — the
	// client-requested canonical model name, distinct from ResponseModel
	// (which is "" whenever no response was ever produced, e.g. an auth
	// failure). Used only by RecordChatCompletionMetrics as the GenAI
	// gen_ai.request.model attribute; RecordChatCompletionResult does not
	// set it on the span, since the span name ("chat "+req.Model) already
	// carries this information for tracing.
	RequestModel string
	// Duration is the elapsed time from HandleChatCompletion/
	// HandleChatCompletionStream's own entry to finalize actually
	// running — the gateway's full request boundary, not just the
	// upstream call — per
	// docs/rfcs/2026-09-07-gateway-genai-metrics.md. Always meaningful
	// (there is no "no duration" case), recorded on every call to
	// RecordChatCompletionMetrics regardless of Err.
	Duration time.Duration
	// Billable mirrors dataplane.finalize's own billable parameter
	// (docs/rfcs/2026-09-05-gateway-cost-double-counting.md): true only
	// for a genuine, unshared upstream call this specific request itself
	// paid for. RecordChatCompletionMetrics gates
	// gen_ai.client.token.usage on this — never CacheHit — so a cache hit
	// or coalesced singleflight follower never replays another call's
	// already-recorded token count into the histogram a second time. Has
	// no effect on RecordChatCompletionResult's span attributes, which
	// report InputTokens/OutputTokens regardless (informational, per
	// CostUSD's own doc comment above).
	Billable bool
	// ErrorType is "" whenever Err == nil, and a low-cardinality
	// description of what went wrong otherwise — the GenAI semantic-
	// conventions spec's error.type attribute, conditionally required on
	// gen_ai.client.operation.duration on failure. Computed by the
	// caller (dataplane.finalize, via errorTypeFor(outcomeFor(err))), not
	// here — this package stays a dependency-free leaf with no knowledge
	// of dataplane's sentinel errors or GatewayDecisionEvent_Outcome enum.
	ErrorType string
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
	if r.CacheReadTokens > 0 {
		attrs = append(attrs, attribute.Int(AttrGenAIUsageCacheReadInputTokens, r.CacheReadTokens))
	}
	if r.CacheCreationTokens > 0 {
		attrs = append(attrs, attribute.Int(AttrGenAIUsageCacheCreationInputTokens, r.CacheCreationTokens))
	}
	if r.AgentRunID != "" {
		attrs = append(attrs, attribute.String(AttrKelvranAgentRunID, r.AgentRunID))
	}
	if r.PromptID != "" {
		attrs = append(attrs,
			attribute.String(AttrKelvranPromptID, r.PromptID),
			attribute.Int(AttrKelvranPromptVersion, r.PromptVersion),
		)
	}
	if r.ResponseFormatRequestedNotEnforced {
		attrs = append(attrs, attribute.Bool(AttrKelvranResponseFormatRequestedNotEnforced, true))
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
