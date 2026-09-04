// Package openaicompat implements the adapter for generic OpenAI-compatible
// self-hosted inference runtimes (vLLM, Ollama, TGI, llama.cpp, LocalAI, and
// similar), per docs/rfcs/2026-09-04-openaicompat-adapter.md. It is a
// near-verbatim copy of internal/adapter/openai's types and logic —
// deliberately duplicated, not shared via a common package, matching this
// codebase's existing "every adapter package is self-contained" convention
// (no adapter package imports another adapter package's types anywhere in
// this codebase) — because the wire format itself is uniformly OpenAI-
// compatible across every self-hosted runtime surveyed while grounding that
// RFC: SSE framing, the "[DONE]" sentinel, stream_options.include_usage
// mechanics, and tool-call JSON shape (an array of
// {id, type, function:{name, arguments-as-string}}) all match real OpenAI's
// own API, confirmed against each runtime's actual source code, not just
// its (often silent) documentation.
//
// Real, sourced compatibility differences exist at the response-*content*
// level, not the wire-*shape* level, and are already handled correctly by
// this near-verbatim design with zero extra code: FinishReason is a bare
// string (not a closed Go enum), so runtime-specific values (vLLM's
// "abort"/"repetition", TGI's "stop_sequence") pass through unmodified;
// Go's encoding/json already ignores unrecognized response fields by
// default (vLLM's stop_reason/token_ids/kv_transfer_params, llama.cpp's
// timings/reasoning_content). The one thing that IS a real, documented
// caveat: TGI never emits finish_reason="tool_calls", even for a genuine
// tool-call response — see Choice's own doc comment.
package openaicompat

import (
	"encoding/json"
	"fmt"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// Request is the native OpenAI-compatible Chat Completions request shape.
type Request struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	// StreamOptions is only ever sent when Stream is true. include_usage is
	// required to get cost-accounting data on a streamed response at all —
	// confirmed during this adapter's own grounding research that every
	// self-hosted runtime surveyed (vLLM, llama.cpp, Ollama, TGI, LocalAI)
	// correctly honors this flag, matching real OpenAI's own behavior.
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

// StreamOptions is the native streaming-configuration object.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Message is the native message shape.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is the native tool-call shape. Arguments is a JSON-encoded
// string natively, matching the canonical ArgumentsJSON representation
// exactly — confirmed real across every self-hosted runtime surveyed
// (vLLM, TGI, Ollama, llama.cpp, LocalAI all encode tool-call arguments as
// a JSON string, not an already-parsed object).
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

// Tool is the native tool-definition shape.
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

// Response is the native Chat Completions response shape.
type Response struct {
	ID      string   `json:"id"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice is a single native completion candidate.
//
// FinishReason is intentionally NOT a closed enum: self-hosted runtimes
// can emit values real OpenAI never does (vLLM: "abort", "repetition";
// TGI: "stop_sequence") — confirmed against each runtime's real source
// while grounding docs/rfcs/2026-09-04-openaicompat-adapter.md. More
// importantly, TGI never emits "tool_calls" as FinishReason even for a
// genuine tool-call response (it always reports "stop"/"length" instead) —
// callers must detect tool calls by checking whether Message.ToolCalls is
// non-empty, never by FinishReason's value.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage is the native token-accounting shape.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Adapter implements adapter.Adapter for generic OpenAI-compatible
// self-hosted runtimes.
type Adapter struct{}

// New constructs an openaicompat Adapter.
func New() *Adapter {
	return &Adapter{}
}

// Name implements adapter.Adapter.
func (a *Adapter) Name() string {
	return "openaicompat"
}

// ToProvider implements adapter.Adapter, converting a canonical ChatRequest
// into the native Request shape via explicit field mapping.
func (a *Adapter) ToProvider(req adapter.ChatRequest) (any, error) {
	messages := make([]Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		toolCalls, err := toolCallsToProvider(m.ToolCalls)
		if err != nil {
			return nil, fmt.Errorf("openaicompat: converting message tool calls: %w", err)
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
					return nil, fmt.Errorf("openaicompat: tool %q has invalid ParametersJSON", t.Name)
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

// FromProvider implements adapter.Adapter, converting a native Response
// back into the canonical ChatResponse shape via explicit field mapping.
func (a *Adapter) FromProvider(resp any) (adapter.ChatResponse, error) {
	native, ok := resp.(*Response)
	if !ok {
		return adapter.ChatResponse{}, fmt.Errorf("openaicompat: FromProvider expected *Response, got %T", resp)
	}

	choices := make([]adapter.Choice, 0, len(native.Choices))
	for _, c := range native.Choices {
		toolCalls, err := toolCallsFromProvider(c.Message.ToolCalls)
		if err != nil {
			return adapter.ChatResponse{}, fmt.Errorf("openaicompat: converting choice tool calls: %w", err)
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

// toolCallsToProvider converts canonical tool calls to the native shape.
// The native Arguments field is already a JSON string, matching the
// canonical ArgumentsJSON representation exactly — no re-encoding needed,
// but the mapping is still explicit per field.
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

// toolCallsFromProvider converts native tool calls back to the canonical
// shape.
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
