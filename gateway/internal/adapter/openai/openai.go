// Package openai implements the near-identity adapter for OpenAI's
// Chat Completions API. The canonical schema in gateway/internal/adapter
// is already OpenAI-shaped, so this adapter's transforms are simple field
// mappings — but they are still done explicitly, field by field, rather
// than via a raw type-cast, so the adapter seam is real even though the
// transform itself is close to a no-op.
package openai

import (
	"encoding/json"
	"fmt"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// Request is OpenAI's native Chat Completions request shape.
type Request struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	// StreamOptions is only ever sent when Stream is true. include_usage
	// is required for the dataplane to get cost-accounting data on a
	// streamed response at all — OpenAI only emits a final usage-bearing
	// chunk when this is explicitly requested, per
	// internal/adapter/openai/stream.go's documented ASSUMPTION.
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
	// PromptCacheKey is OpenAI's real top-level cache-routing hint —
	// unlike Anthropic's/Bedrock's opt-in "what to cache" markers,
	// OpenAI's own prompt caching is fully automatic; this field only
	// ever influences which warm machine a request is routed to. Set
	// from the first adapter.CacheControl.Key found anywhere in the
	// canonical request, per findCacheKey and
	// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md. Omitted
	// entirely (never a fabricated value) when no caller-supplied Key is
	// found.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	// ResponseFormat is OpenAI's real structured-output request field,
	// confirmed against OpenAI's own live shared_params/
	// response_format_json_schema.py: {"type":"json_schema",
	// "json_schema":{"name":...,"strict":...,"schema":...}} -- an exact
	// match for the canonical adapter.ResponseFormat/adapter.JSONSchema
	// shape, so this adapter's translation is a direct field mapping.
	// Nil (the default) omits the field entirely, byte-identical to
	// today's existing behavior.
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

// StreamOptions is OpenAI's native streaming-configuration object.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ResponseFormat is OpenAI's native structured-output request shape.
type ResponseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *JSONSchema `json:"json_schema,omitempty"`
}

// JSONSchema is OpenAI's native structured-output schema payload,
// confirmed real (name/strict/schema, name and schema required, strict
// optional) against OpenAI's own shared_params/
// response_format_json_schema.py.
type JSONSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict,omitempty"`
	Schema json.RawMessage `json:"schema"`
}

// Message is OpenAI's native message shape. Content is json.RawMessage,
// not string, so ToProvider can emit either a plain JSON string (the
// common, text-only case — byte-identical to this adapter's behavior
// before docs/rfcs/2026-09-06-gateway-multimodal-content.md) or a JSON
// array of multi-modal parts — the real "content is either a string or
// an array" duality every OpenAI SDK uses. FromProvider only ever
// decodes this back into a plain string; multi-modal RESPONSE content
// is out of scope this pass (see that RFC's Alternatives Considered).
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// nativeContentPart is one element of OpenAI's native multi-modal
// content array.
type nativeContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *nativeImageURL `json:"image_url,omitempty"`
}

// nativeImageURL is OpenAI's native image_url object — url is either a
// real URL or a base64 "data:" URI.
type nativeImageURL struct {
	URL string `json:"url"`
}

// ToolCall is OpenAI's native tool-call shape. Arguments is a JSON-encoded
// string natively — this is the OpenAI side of the "tool-call argument
// encoding" normalization hazard documented in gateway/ARCHITECTURE.md
// (OpenAI/DeepSeek/Qwen return a JSON string; Anthropic/Gemini/Bedrock
// return an already-parsed object).
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the function-call payload nested inside a ToolCall.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is OpenAI's native tool-definition shape.
type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef describes a callable function's name/description/schema.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Response is OpenAI's native Chat Completions response shape.
type Response struct {
	ID      string   `json:"id"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice is a single native completion candidate.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage is OpenAI's native token-accounting shape.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Adapter implements adapter.Adapter for OpenAI.
type Adapter struct{}

// New constructs an OpenAI Adapter.
func New() *Adapter {
	return &Adapter{}
}

// Name implements adapter.Adapter.
func (a *Adapter) Name() string {
	return "openai"
}

// ToProvider implements adapter.Adapter, converting a canonical ChatRequest
// into OpenAI's native Request shape via explicit field mapping.
func (a *Adapter) ToProvider(req adapter.ChatRequest) (any, error) {
	messages := make([]Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		toolCalls, err := toolCallsToProvider(m.ToolCalls)
		if err != nil {
			return nil, fmt.Errorf("openai: converting message tool calls: %w", err)
		}
		content, err := contentToNative(m.Content, m.Parts)
		if err != nil {
			return nil, fmt.Errorf("openai: converting message content: %w", err)
		}
		messages = append(messages, Message{
			Role:       m.Role,
			Content:    content,
			ToolCalls:  toolCalls,
			ToolCallID: m.ToolCallID,
		})
	}

	var tools []Tool
	if len(req.Tools) > 0 {
		tools = make([]Tool, 0, len(req.Tools))
		for _, t := range req.Tools {
			var params json.RawMessage
			if t.ParametersJSON != "" {
				if !json.Valid([]byte(t.ParametersJSON)) {
					return nil, fmt.Errorf("openai: tool %q has invalid ParametersJSON", t.Name)
				}
				params = json.RawMessage(t.ParametersJSON)
			}
			tools = append(tools, Tool{
				Type: "function",
				Function: FunctionDef{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  params,
				},
			})
		}
	}

	var streamOpts *StreamOptions
	if req.Stream {
		streamOpts = &StreamOptions{IncludeUsage: true}
	}

	return &Request{
		Model:          req.Model,
		Messages:       messages,
		Temperature:    req.Temperature,
		MaxTokens:      req.MaxTokens,
		Tools:          tools,
		Stream:         req.Stream,
		StreamOptions:  streamOpts,
		PromptCacheKey: findCacheKey(req.Messages),
		ResponseFormat: responseFormatToProvider(req.ResponseFormat),
	}, nil
}

// responseFormatToProvider converts a canonical adapter.ResponseFormat
// into OpenAI's native ResponseFormat -- a direct field mapping, since
// OpenAI's own real shape matches the canonical schema exactly (see
// Request.ResponseFormat's own doc comment). Nil in, nil out.
func responseFormatToProvider(rf *adapter.ResponseFormat) *ResponseFormat {
	if rf == nil {
		return nil
	}
	native := &ResponseFormat{Type: rf.Type}
	if rf.JSONSchema != nil {
		native.JSONSchema = &JSONSchema{
			Name:   rf.JSONSchema.Name,
			Strict: rf.JSONSchema.Strict,
			Schema: rf.JSONSchema.Schema,
		}
	}
	return native
}

// findCacheKey scans every message (and, for multi-modal messages, every
// content part) in order for the first non-empty
// adapter.CacheControl.Key, and returns it as OpenAI's real top-level
// prompt_cache_key routing hint, per
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md. OpenAI's own
// caching is fully automatic -- this field only ever influences which
// warm machine a request is routed to, never whether caching happens at
// all -- so, unlike Anthropic's/Bedrock's opt-in markers, an empty
// return here (no CacheControl set anywhere, or one set with an empty
// Key) is not a missed opt-in; it is Kelvran deliberately declining to
// invent a routing-affinity value the caller never supplied one for,
// per this codebase's "never fabricate a value" convention (see e.g.
// bedrock.go's own honest-absence ChatResponse.ID doc comment).
func findCacheKey(messages []adapter.Message) string {
	for _, m := range messages {
		if m.CacheControl != nil && m.CacheControl.Key != "" {
			return m.CacheControl.Key
		}
		for _, part := range m.Parts {
			if part.CacheControl != nil && part.CacheControl.Key != "" {
				return part.CacheControl.Key
			}
		}
	}
	return ""
}

// FromProvider implements adapter.Adapter, converting an OpenAI native
// Response back into the canonical ChatResponse shape via explicit field
// mapping.
func (a *Adapter) FromProvider(resp any) (adapter.ChatResponse, error) {
	native, ok := resp.(*Response)
	if !ok {
		return adapter.ChatResponse{}, fmt.Errorf("openai: FromProvider expected *Response, got %T", resp)
	}

	choices := make([]adapter.Choice, 0, len(native.Choices))
	for _, c := range native.Choices {
		toolCalls, err := toolCallsFromProvider(c.Message.ToolCalls)
		if err != nil {
			return adapter.ChatResponse{}, fmt.Errorf("openai: converting choice tool calls: %w", err)
		}
		content, err := contentFromNative(c.Message.Content)
		if err != nil {
			return adapter.ChatResponse{}, fmt.Errorf("openai: converting choice content: %w", err)
		}
		choices = append(choices, adapter.Choice{
			Index: c.Index,
			Message: adapter.Message{
				Role:       c.Message.Role,
				Content:    content,
				ToolCalls:  toolCalls,
				ToolCallID: c.Message.ToolCallID,
			},
			FinishReason: c.FinishReason,
		})
	}

	return adapter.ChatResponse{
		ID:      native.ID,
		Model:   native.Model,
		Choices: choices,
		Usage: adapter.Usage{
			PromptTokens:     native.Usage.PromptTokens,
			CompletionTokens: native.Usage.CompletionTokens,
			TotalTokens:      native.Usage.TotalTokens,
		},
	}, nil
}

// contentToNative builds Message.Content's wire value from the
// canonical Content/Parts pair, per
// docs/rfcs/2026-09-06-gateway-multimodal-content.md. Returns nil (both
// content and parts empty — omitted from the marshaled JSON, exactly
// like the plain-string field's own prior omitempty behavior) or a
// plain JSON string (parts empty — byte-identical to this adapter's
// behavior before that RFC) or a JSON array of parts (parts non-empty).
// The Chat Completions API has no native document content-part type —
// a "document" part returns a real, typed error rather than a silently
// wrong mapping.
func contentToNative(content string, parts []adapter.ContentPart) (json.RawMessage, error) {
	if len(parts) == 0 {
		if content == "" {
			return nil, nil
		}
		return json.Marshal(content)
	}

	native := make([]nativeContentPart, 0, len(parts)+1)
	if content != "" {
		native = append(native, nativeContentPart{Type: "text", Text: content})
	}
	for _, p := range parts {
		switch p.Type {
		case "text":
			native = append(native, nativeContentPart{Type: "text", Text: p.Text})
		case "image":
			url := p.URL
			if p.Data != "" {
				url = fmt.Sprintf("data:%s;base64,%s", p.MediaType, p.Data)
			}
			if url == "" {
				return nil, fmt.Errorf("openai: image part has neither Data nor URL set")
			}
			native = append(native, nativeContentPart{Type: "image_url", ImageURL: &nativeImageURL{URL: url}})
		case "document":
			return nil, fmt.Errorf("openai: document content parts are not supported by the Chat Completions API")
		default:
			return nil, fmt.Errorf("openai: unsupported content part type %q", p.Type)
		}
	}
	return json.Marshal(native)
}

// contentFromNative decodes Message.Content's wire value back into a
// plain string. Multi-modal RESPONSE content (a JSON array) is out of
// scope this pass — see docs/rfcs/2026-09-06-gateway-multimodal-
// content.md's Alternatives Considered — returns a real, typed error
// rather than silently discarding non-text parts.
func contentFromNative(native json.RawMessage) (string, error) {
	if len(native) == 0 {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(native, &text); err != nil {
		return "", fmt.Errorf("openai: response content is not a plain string (multi-modal response content is not supported): %w", err)
	}
	return text, nil
}

// toolCallsToProvider converts canonical tool calls to OpenAI's native
// shape. OpenAI's native Arguments field is already a JSON string, matching
// the canonical ArgumentsJSON representation exactly — no re-encoding
// needed, but the mapping is still explicit per field.
func toolCallsToProvider(calls []adapter.ToolCall) ([]ToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	out := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		if c.ArgumentsJSON != "" && !json.Valid([]byte(c.ArgumentsJSON)) {
			return nil, fmt.Errorf("tool call %q has invalid ArgumentsJSON", c.ID)
		}
		out = append(out, ToolCall{
			ID:   c.ID,
			Type: "function",
			Function: FunctionCall{
				Name:      c.Name,
				Arguments: c.ArgumentsJSON,
			},
		})
	}
	return out, nil
}

// toolCallsFromProvider converts OpenAI native tool calls back to the
// canonical shape.
func toolCallsFromProvider(calls []ToolCall) ([]adapter.ToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	out := make([]adapter.ToolCall, 0, len(calls))
	for _, c := range calls {
		if c.Function.Arguments != "" && !json.Valid([]byte(c.Function.Arguments)) {
			return nil, fmt.Errorf("tool call %q has invalid Arguments", c.ID)
		}
		out = append(out, adapter.ToolCall{
			ID:            c.ID,
			Name:          c.Function.Name,
			ArgumentsJSON: c.Function.Arguments,
		})
	}
	return out, nil
}
