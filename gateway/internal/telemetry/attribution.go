package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Attribution is the bounded, gateway-derived record of WHICH client sent
// a request, per docs/rfcs/2026-10-09-gateway-attribution-and-spend-ledger.md
// (plan item 13a). cmd/gateway's captureAttribution middleware fills it
// from the User-Agent and x-claude-code-* request headers once per
// request, caps and validates every value, and attaches it to the
// context with WithAttribution; dataplane's finalize (and HandleEmbeddings)
// read it with AttributionFromContext. It is never an input to the cache
// key, guardrails, routing, fallback eligibility, rate limits, budgets or
// idempotency — attribution only.
//
// Two classes of field, with different sinks:
//
//   - Bounded values (ClientTool, RequestClass, Compaction,
//     ContextCompacted, the PrevTool* numbers) may be metric dimensions and
//     top-level log fields. ClientTool and RequestClass are closed
//     vocabularies (see the ClientTool*/RequestClass* constants).
//   - Identifiers (SessionID, AgentID, ParentAgentID, ClaudeCodePromptID,
//     and AgentType, whose vocabulary the protocol page leaves open) are
//     span attributes only — never a metric label, never a log field —
//     and are blanked by WithoutIdentifiers when identifier capture is
//     switched off globally or for the virtual key.
//
// Every field is a plain string or int so this package stays a
// dependency-free leaf.
type Attribution struct {
	// ClientTool is the normalised User-Agent product: one of the
	// ClientTool* constants, ClientToolOther when unknown or absent.
	ClientTool string

	// SessionID is x-claude-code-session-id: Claude Code's identifier for
	// the session, sent on every POST /v1/messages request to any base URL
	// (not on its GET /v1/models or HEAD /api/hello calls — captured live
	// 2026-10-09 against 2.1.295).
	SessionID string
	// AgentID is x-claude-code-agent-id, present only on requests issued
	// by a subagent Claude Code spawned inside the session.
	AgentID string
	// ParentAgentID is x-claude-code-parent-agent-id, present only for
	// nested agents.
	ParentAgentID string
	// ClaudeCodePromptID is x-claude-code-prompt-id, a random UUID shared
	// by every request serving one user prompt (a hint header). Distinct
	// from ChatCompletionResult.PromptID, which is Kelvran's own
	// prompt-management id.
	ClaudeCodePromptID string

	// RequestClass is x-claude-code-request-class: one of the five page
	// values (main, subagent, workflow, compaction, auxiliary),
	// RequestClassOther for an unknown value, RequestClassNone when the
	// header was absent (the default behind a custom base URL without
	// CLAUDE_CODE_GATEWAY_HINT_HEADERS=1).
	RequestClass string
	// AgentType is x-claude-code-agent-type (built-in agent names,
	// "custom", "teammate", "fork" — an open set, so span-only).
	AgentType string
	// Compaction and ContextCompacted take auto / manual / reactive, or
	// "other" for an unknown value; empty when absent.
	Compaction       string
	ContextCompacted string

	// PrevToolDurations is the wire form of the well-formed
	// x-claude-code-prev-tool-durations entries — printable ASCII, a
	// percent-decodable name, a duration of at most one day; <= 32 entries,
	// <= 4 KB — kept as a span attribute; PrevToolCount and PrevToolTotalMS
	// are their parsed summary. All zero when the header was absent or had
	// no well-formed entry — which the protocol page says must not be read
	// as "no tools ran".
	PrevToolDurations string
	PrevToolCount     int
	PrevToolTotalMS   int
}

// ClientTool* are the closed kelvran.client.tool vocabulary.
const (
	ClientToolClaudeCode      = "claude_code"
	ClientToolOpenAIPython    = "openai_python"
	ClientToolOpenAINode      = "openai_node"
	ClientToolOpenAIGo        = "openai_go"
	ClientToolAnthropicPython = "anthropic_python"
	ClientToolAnthropicNode   = "anthropic_node"
	ClientToolAnthropicGo     = "anthropic_go"
	ClientToolCodex           = "codex"
	ClientToolAider           = "aider"
	ClientToolContinue        = "continue"
	ClientToolLiteLLM         = "litellm"
	ClientToolCurl            = "curl"
	ClientToolOther           = "other"
)

// RequestClass* are the two Kelvran-added values of the
// kelvran.claude_code.request_class vocabulary; the five page values
// (main, subagent, workflow, compaction, auxiliary) pass through verbatim.
const (
	RequestClassNone  = "none"
	RequestClassOther = "other"
)

// WithoutIdentifiers returns a copy with the identifier fields blanked
// (SessionID, AgentID, ParentAgentID, ClaudeCodePromptID, AgentType) and
// every bounded field kept — what the per-key and global identifier
// switches produce.
func (a Attribution) WithoutIdentifiers() Attribution {
	a.SessionID = ""
	a.AgentID = ""
	a.ParentAgentID = ""
	a.ClaudeCodePromptID = ""
	a.AgentType = ""
	return a
}

// HasIdentifiers reports whether any identifier field is set.
func (a Attribution) HasIdentifiers() bool {
	return a.SessionID != "" || a.AgentID != "" || a.ParentAgentID != "" || a.ClaudeCodePromptID != "" || a.AgentType != ""
}

// NormalizedClientTool maps an empty ClientTool to ClientToolOther so every
// metric data point carries the dimension.
func NormalizedClientTool(tool string) string {
	if tool == "" {
		return ClientToolOther
	}
	return tool
}

// NormalizedRequestClass maps an empty RequestClass to RequestClassNone so
// every metric data point carries the dimension.
func NormalizedRequestClass(class string) string {
	if class == "" {
		return RequestClassNone
	}
	return class
}

// attributionContextKey is the private context key for WithAttribution —
// the same unexported-struct-key convention dataplane/overhead.go uses,
// so no other package can read or forge the value by name.
type attributionContextKey struct{}

// WithAttribution returns a copy of ctx carrying a. It is a plain context
// value, deliberately NOT a W3C Baggage member: Baggage is the client's
// propagation channel and would travel upstream with the first outbound
// transport that injects it, while attribution must never leave the
// gateway.
func WithAttribution(ctx context.Context, a Attribution) context.Context {
	return context.WithValue(ctx, attributionContextKey{}, a)
}

// AttributionFromContext returns the Attribution WithAttribution stored,
// or the zero value when none was stored (requests that never passed the
// middleware, such as tests driving the pipeline directly).
func AttributionFromContext(ctx context.Context) Attribution {
	a, _ := ctx.Value(attributionContextKey{}).(Attribution)
	return a
}

// attributionDroppedCounter counts request headers the attribution
// middleware refused to carry — an identifier longer than the cap or
// outside its alphabet, an agent type over its cap or non-printable, a
// prev-tool-durations value with no well-formed entry — so silent loss is
// visible in aggregate. The one attribute is the header name (the closed
// set documented on kelvran.attribution.header in
// docs/reference/metrics-and-logs.md), never the value.
var attributionDroppedCounter = mustInt64Counter(
	meter,
	"kelvran.attribution.dropped",
	metric.WithDescription("Attribution headers dropped by the gateway's caps and vocabularies (the header name is the only attribute, never the value)."),
	metric.WithUnit("{header}"),
)

// RecordAttributionDropped increments attributionDroppedCounter for one
// dropped header. header is the lower-case request header name; the only
// caller is cmd/gateway's captureAttribution middleware.
func RecordAttributionDropped(ctx context.Context, header string) {
	attributionDroppedCounter.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrKelvranAttributionHeader, header)))
}
