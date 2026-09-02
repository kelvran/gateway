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

	"github.com/kelvran/gateway/internal/adapter"
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
}

// StreamOptions is OpenAI's native streaming-configuration object.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Message is OpenAI's native message shape.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
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
		messages = append(messages, Message{
			Role:       m.Role,
			Content:    m.Content,
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
		Model:         req.Model,
		Messages:      messages,
		Temperature:   req.Temperature,
		MaxTokens:     req.MaxTokens,
		Tools:         tools,
		Stream:        req.Stream,
		StreamOptions: streamOpts,
	}, nil
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
		choices = append(choices, adapter.Choice{
			Index: c.Index,
			Message: adapter.Message{
				Role:       c.Message.Role,
				Content:    c.Message.Content,
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
