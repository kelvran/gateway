// Package adapter defines the canonical request/response schema shared by
// every provider adapter (OpenAI, Anthropic, Gemini, Bedrock, openaicompat)
// plus the Adapter interface each of them implements.
//
// The canonical schema is OpenAI Chat-Completions-shaped, per
// gateway/ARCHITECTURE.md's "Canonical Schema & Provider Adapters" section.
// Every adapter's ToProvider/FromProvider pair is responsible for the four
// documented normalization hazards described there: tool-call argument
// encoding, system-prompt placement, streaming event shape, and
// unknown-field preservation.
package adapter

import "encoding/json"

// Message is one turn in a canonical chat conversation. Its JSON shape is
// Kelvran's own client-facing wire format — OpenAI Chat-Completions-shaped,
// per gateway/ARCHITECTURE.md, since the canonical schema doubles as the
// gateway's own inbound/outbound API surface, not just an internal type.
type Message struct {
	// Role is one of "system", "user", "assistant", or "tool".
	Role string `json:"role"`
	// Content is the message's text content. For assistant messages that
	// only carry tool calls, Content may be empty. Unchanged by Parts
	// below — a multi-modal message may still carry lead-in text here.
	Content string `json:"content,omitempty"`
	// Parts, when non-empty, is this message's multi-modal content —
	// image/document parts alongside (or instead of) Content — per
	// docs/rfcs/2026-09-06-gateway-multimodal-content.md. Deliberately
	// additive: Content's own type is unchanged, so every existing
	// call site across every adapter/dataplane/test keeps working
	// exactly as before for the common, Parts-empty, text-only case.
	Parts []ContentPart `json:"parts,omitempty"`
	// ToolCalls holds any tool/function calls the assistant requested in
	// this message. Empty for non-assistant messages.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID identifies which prior ToolCall this message is a result
	// for. Only set on role:"tool" messages.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// CacheControl, when set, opts this whole message into provider-side
	// prompt caching, per
	// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md. Nil (the
	// default) is a silent no-op for every adapter -- it never changes
	// what's forwarded, only whether a caching marker rides alongside it.
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// ContentPart is one piece of a multi-modal message's content, per
// docs/rfcs/2026-09-06-gateway-multimodal-content.md.
type ContentPart struct {
	// Type is "text", "image", or "document".
	Type string `json:"type"`
	// Text is set when Type == "text".
	Text string `json:"text,omitempty"`
	// MediaType is the MIME type (e.g. "image/png", "application/pdf"),
	// set when Type != "text".
	MediaType string `json:"media_type,omitempty"`
	// Data is base64-encoded inline content. Mutually exclusive with
	// URL — exactly one of the two is set when Type != "text".
	Data string `json:"data,omitempty"`
	// URL is a remote reference passed through to the provider
	// verbatim — Kelvran itself never fetches it.
	URL string `json:"url,omitempty"`
	// CacheControl, when set, opts this specific part into provider-side
	// prompt caching independently of its parent Message's own
	// CacheControl, per
	// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md. Nil (the
	// default) is a silent no-op for every adapter.
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// CacheControl is an opt-in marker requesting provider-side prompt
// caching for the message or content-part it's attached to, per
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md. A nil
// CacheControl (the default on every Message/ContentPart) is a silent
// no-op for every adapter — it changes nothing about how the request is
// forwarded, matching this schema's existing optional-field convention
// (ContentPart/Parts's own additive, no-op-when-empty precedent from
// docs/rfcs/2026-09-06-gateway-multimodal-content.md).
type CacheControl struct {
	// TTL requests a non-default cache lifetime. Only Anthropic (direct,
	// and Bedrock's Anthropic-family cachePoint) reads this — "" means
	// that provider's own default (5 minutes); "1h" opts into the
	// longer, more expensive lifetime both document. Every other
	// provider/model family ignores TTL as a no-op, never an error,
	// since neither Bedrock's non-Anthropic families nor OpenAI expose
	// an equivalent per-breakpoint TTL knob.
	TTL string `json:"ttl,omitempty"`
	// Key is a stable, caller-supplied identity (e.g. a session or
	// conversation ID) that only OpenAI's adapter reads, to set its
	// top-level prompt_cache_key routing hint — see
	// internal/adapter/openai's ToProvider doc comment for why Kelvran
	// never invents this value itself. Ignored (a no-op) by every other
	// adapter.
	Key string `json:"key,omitempty"`
}

// ToolCall is a single tool/function invocation requested by the model.
//
// ArgumentsJSON is always a JSON-encoded string in the canonical schema,
// even though some providers (Anthropic, Gemini, Bedrock) hand back an
// already-parsed object natively — adapters are responsible for
// marshaling/unmarshaling at the boundary so the canonical type stays
// consistent across every provider. ToolCall implements a custom
// MarshalJSON/UnmarshalJSON pair so its *wire* shape still matches
// OpenAI's real tool_calls[].function.{name,arguments} nesting, even
// though the Go struct itself stays flat for adapters' convenience.
type ToolCall struct {
	ID            string
	Name          string
	ArgumentsJSON string
}

type toolCallWire struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// MarshalJSON implements json.Marshaler, producing OpenAI's native
// tool_calls[] element shape.
func (t ToolCall) MarshalJSON() ([]byte, error) {
	var w toolCallWire
	w.ID = t.ID
	w.Type = "function"
	w.Function.Name = t.Name
	w.Function.Arguments = t.ArgumentsJSON
	return json.Marshal(w)
}

// UnmarshalJSON implements json.Unmarshaler, parsing OpenAI's native
// tool_calls[] element shape back into the flat canonical fields.
func (t *ToolCall) UnmarshalJSON(data []byte) error {
	var w toolCallWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	t.ID = w.ID
	t.Name = w.Function.Name
	t.ArgumentsJSON = w.Function.Arguments
	return nil
}

// ToolDef describes a tool/function the model is allowed to call. Like
// ToolCall, it carries a custom JSON (de)serialization pair so the wire
// shape matches OpenAI's native tools[].function.{name,description,
// parameters} nesting.
type ToolDef struct {
	Name        string
	Description string
	// ParametersJSON is the tool's JSON Schema parameter definition,
	// encoded as a string for the same reason ToolCall.ArgumentsJSON is.
	ParametersJSON string
}

type toolDefWire struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// MarshalJSON implements json.Marshaler.
func (t ToolDef) MarshalJSON() ([]byte, error) {
	var w toolDefWire
	w.Type = "function"
	w.Function.Name = t.Name
	w.Function.Description = t.Description
	if t.ParametersJSON != "" {
		w.Function.Parameters = json.RawMessage(t.ParametersJSON)
	}
	return json.Marshal(w)
}

// UnmarshalJSON implements json.Unmarshaler.
func (t *ToolDef) UnmarshalJSON(data []byte) error {
	var w toolDefWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	t.Name = w.Function.Name
	t.Description = w.Function.Description
	if len(w.Function.Parameters) > 0 {
		t.ParametersJSON = string(w.Function.Parameters)
	}
	return nil
}

// ChatRequest is the canonical, provider-agnostic chat completion request.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	Tools       []ToolDef `json:"tools,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
}

// Usage is token accounting for a single completion.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Choice is a single generated completion candidate.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// ChatResponse is the canonical, provider-agnostic chat completion response.
type ChatResponse struct {
	ID      string   `json:"id"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Adapter translates between the canonical schema above and one upstream
// provider's native wire format. Implementations must be pure functions:
// no network calls, no hidden state beyond what's passed in.
type Adapter interface {
	// ToProvider converts a canonical request into the provider's native
	// request shape. The returned value's concrete type is
	// provider-specific (e.g. an OpenAI-native or Anthropic-native
	// request struct) and is opaque to callers outside the adapter.
	ToProvider(ChatRequest) (any, error)
	// FromProvider converts a provider-native response (the same concrete
	// type ToProvider's caller would send upstream and get back) into the
	// canonical response shape.
	FromProvider(any) (ChatResponse, error)
	// Name identifies the adapter (e.g. "openai", "anthropic").
	Name() string
}
