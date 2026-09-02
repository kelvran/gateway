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

	span.SetAttributes(attrs...)

	if r.Err != nil {
		span.RecordError(r.Err)
		span.SetStatus(codes.Error, r.Err.Error())
	}
}
