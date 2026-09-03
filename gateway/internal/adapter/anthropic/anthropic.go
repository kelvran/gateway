// Package anthropic implements a genuine translation adapter between the
// canonical schema and Anthropic's Messages API. Unlike the near-identity
// openai adapter, this one earns its keep by handling two of the four
// documented normalization hazards from gateway/ARCHITECTURE.md's
// "Canonical Schema & Provider Adapters" section for real:
//
//  1. System-prompt placement: the canonical schema carries an in-array
//     role:"system" message; Anthropic requires it pulled out into a
//     top-level "system" string field.
//  2. Tool-call argument encoding: the canonical schema always carries
//     ArgumentsJSON as a JSON-encoded string; Anthropic's native shape is
//     an already-parsed object (map[string]any here), so ToProvider must
//     parse the string and FromProvider must re-marshal the object back
//     into a string to keep the canonical type consistent across
//     providers.
package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// defaultMaxTokens is used when the canonical request doesn't specify one.
// Anthropic's Messages API requires max_tokens; the canonical schema's
// MaxTokens is optional (OpenAI treats it as optional), so this adapter
// must supply a default rather than send an invalid request upstream.
const defaultMaxTokens = 4096

// Request is Anthropic's native Messages API request shape.
type Request struct {
	Model       string    `json:"model"`
	System      string    `json:"system,omitempty"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature *float64  `json:"temperature,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
}

// Message is Anthropic's native message shape: role is only "user" or
// "assistant" (never "system" — see the package doc), and content is a
// list of typed blocks rather than a single string.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock is one block of Anthropic's typed content-block union.
// Only the fields relevant to the block's Type are populated.
type ContentBlock struct {
	Type string `json:"type"` // "text", "tool_use", or "tool_result"

	// "text" block
	Text string `json:"text,omitempty"`

	// "tool_use" block — Input is an already-parsed JSON object, not a
	// string, per the tool-call-argument-encoding hazard this adapter
	// exists to handle.
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`

	// "tool_result" block
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
}

// Tool is Anthropic's native tool-definition shape. InputSchema is a
// parsed JSON Schema object, not a string.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// Response is Anthropic's native Messages API response shape.
type Response struct {
	ID         string         `json:"id"`
	Model      string         `json:"model"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      Usage          `json:"usage"`
}

// Usage is Anthropic's native token-accounting shape (note the different
// field names from OpenAI's prompt_tokens/completion_tokens).
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Adapter implements adapter.Adapter for Anthropic.
type Adapter struct{}

// New constructs an Anthropic Adapter.
func New() *Adapter {
	return &Adapter{}
}

// Name implements adapter.Adapter.
func (a *Adapter) Name() string {
	return "anthropic"
}

// ToProvider implements adapter.Adapter. It pulls any role:"system"
// messages out of the canonical Messages slice into the native System
// field, and converts every other message into Anthropic's block-based
// content shape.
func (a *Adapter) ToProvider(req adapter.ChatRequest) (any, error) {
	var systemParts []string
	messages := make([]Message, 0, len(req.Messages))

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			systemParts = append(systemParts, m.Content)
			continue
		case "tool":
			// Anthropic has no "tool" role: a tool result is sent as a
			// "user" message carrying a tool_result content block.
			messages = append(messages, Message{
				Role: "user",
				Content: []ContentBlock{
					{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content},
				},
			})
			continue
		}

		var blocks []ContentBlock
		if m.Content != "" {
			blocks = append(blocks, ContentBlock{Type: "text", Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			input := map[string]any{}
			if tc.ArgumentsJSON != "" {
				if err := json.Unmarshal([]byte(tc.ArgumentsJSON), &input); err != nil {
					return nil, fmt.Errorf("anthropic: tool call %q has invalid ArgumentsJSON: %w", tc.ID, err)
				}
			}
			blocks = append(blocks, ContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Name,
				Input: input,
			})
		}
		messages = append(messages, Message{Role: m.Role, Content: blocks})
	}

	var tools []Tool
	if len(req.Tools) > 0 {
		tools = make([]Tool, 0, len(req.Tools))
		for _, t := range req.Tools {
			var schema map[string]any
			if t.ParametersJSON != "" {
				if err := json.Unmarshal([]byte(t.ParametersJSON), &schema); err != nil {
					return nil, fmt.Errorf("anthropic: tool %q has invalid ParametersJSON: %w", t.Name, err)
				}
			}
			tools = append(tools, Tool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: schema,
			})
		}
	}

	maxTokens := defaultMaxTokens
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}

	return &Request{
		Model:       req.Model,
		System:      strings.Join(systemParts, "\n\n"),
		Messages:    messages,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
		Tools:       tools,
		Stream:      req.Stream,
	}, nil
}

// FromProvider implements adapter.Adapter, converting an Anthropic native
// Response back into the canonical ChatResponse shape. Text blocks are
// concatenated into Message.Content; tool_use blocks become canonical
// ToolCalls with Input re-marshaled back into ArgumentsJSON strings.
func (a *Adapter) FromProvider(resp any) (adapter.ChatResponse, error) {
	native, ok := resp.(*Response)
	if !ok {
		return adapter.ChatResponse{}, fmt.Errorf("anthropic: FromProvider expected *Response, got %T", resp)
	}

	var textParts []string
	var toolCalls []adapter.ToolCall
	for _, block := range native.Content {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "tool_use":
			argsJSON, err := json.Marshal(block.Input)
			if err != nil {
				return adapter.ChatResponse{}, fmt.Errorf("anthropic: marshaling tool_use %q input: %w", block.ID, err)
			}
			toolCalls = append(toolCalls, adapter.ToolCall{
				ID:            block.ID,
				Name:          block.Name,
				ArgumentsJSON: string(argsJSON),
			})
		}
	}

	message := adapter.Message{
		Role:      native.Role,
		Content:   strings.Join(textParts, ""),
		ToolCalls: toolCalls,
	}

	finishReason := native.StopReason

	return adapter.ChatResponse{
		ID:    native.ID,
		Model: native.Model,
		Choices: []adapter.Choice{
			{Index: 0, Message: message, FinishReason: finishReason},
		},
		Usage: adapter.Usage{
			PromptTokens:     native.Usage.InputTokens,
			CompletionTokens: native.Usage.OutputTokens,
			TotalTokens:      native.Usage.InputTokens + native.Usage.OutputTokens,
		},
	}, nil
}
