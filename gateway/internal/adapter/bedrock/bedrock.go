// Package bedrock implements a genuine translation adapter between the
// canonical schema and Amazon Bedrock Runtime's Converse API. Real field
// shapes confirmed directly against AWS's live API reference
// (docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html)
// and real, current aws-sdk-go-v2 source — per
// docs/rfcs/2026-09-04-bedrock-adapter.md.
//
// Buffered/non-streaming only this pass — ConverseStream's real wire
// format is AWS's binary application/vnd.amazon.eventstream framing, not
// SSE, and is deliberately deferred to a follow-on RFC (see
// gateway/internal/streaming/types.go's doc comment, which already
// scopes Bedrock out of streaming.StreamingAdapter).
//
// Like anthropic/gemini, this adapter earns its keep handling real
// normalization hazards:
//
//  1. System-prompt placement: Converse's messages[].role accepts only
//     "user"/"assistant" — a role:"system" message is hoisted into a
//     top-level system[] field, the same hazard Anthropic/Gemini already
//     solve.
//  2. Tool-call argument encoding: a toolUse block's "input" is an
//     already-parsed JSON object, not a string — ToProvider parses
//     ArgumentsJSON, FromProvider re-marshals back, same as
//     Anthropic/Gemini's Input/Args pattern.
//  3. Tool-result placement: a tool result is a role:"user" message
//     carrying a toolResult content block — genuinely simpler than
//     Gemini's hazard, since toolResult correlates purely by toolUseId
//     with no required "name" field at all, so no name-lookup-from-
//     history is needed.
//  4. No native response-ID field exists on Converse's response, unlike
//     OpenAI/Anthropic/Gemini — ChatResponse.ID is left empty, an honest
//     absence, never a fabricated placeholder.
package bedrock

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// Message is Bedrock Converse's native message shape. Role is "user" or
// "assistant" only — Converse has no "system" role in messages[] (hazard
// #1) and no "tool" role (hazard #3: a tool result is role:"user").
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock is one block of Converse's content union — only one of
// Text/ToolUse/ToolResult is ever populated per block, mirroring
// Anthropic's ContentBlock / Gemini's Part union convention.
type ContentBlock struct {
	Text       string      `json:"text,omitempty"`
	ToolUse    *ToolUse    `json:"toolUse,omitempty"`
	ToolResult *ToolResult `json:"toolResult,omitempty"`
}

// ToolUse is Converse's native tool-call shape. Input is an
// already-parsed JSON object (confirmed real field shape), per hazard #2.
type ToolUse struct {
	ToolUseID string         `json:"toolUseId"`
	Name      string         `json:"name"`
	Input     map[string]any `json:"input"`
}

// ToolResult is Converse's native tool-result shape. Correlates purely by
// ToolUseID — no "name" field exists at all, per hazard #3.
type ToolResult struct {
	ToolUseID string              `json:"toolUseId"`
	Content   []ToolResultContent `json:"content"`
	Status    string              `json:"status,omitempty"`
}

// ToolResultContent is one block of a tool result's own content — text
// only, this pass (Converse also allows json/image content here, out of
// scope).
type ToolResultContent struct {
	Text string `json:"text,omitempty"`
}

// SystemContentBlock is one block of Converse's top-level system[] field.
type SystemContentBlock struct {
	Text string `json:"text,omitempty"`
}

// Tool is Converse's native tool-definition shape.
type Tool struct {
	ToolSpec ToolSpec `json:"toolSpec"`
}

// ToolSpec describes one callable tool. InputSchema.JSON is a parsed JSON
// Schema object, not a string, same as Anthropic's InputSchema.
type ToolSpec struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema InputSchema `json:"inputSchema"`
}

// InputSchema wraps a tool's parsed JSON Schema under Converse's real
// "json" key.
type InputSchema struct {
	JSON map[string]any `json:"json,omitempty"`
}

// ToolConfig is Converse's native tool-configuration container.
type ToolConfig struct {
	Tools []Tool `json:"tools"`
}

// InferenceConfig carries the subset of Converse's real inferenceConfig
// fields this adapter maps from the canonical schema.
type InferenceConfig struct {
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   *int     `json:"maxTokens,omitempty"`
}

// Request is Bedrock Converse's native request shape.
type Request struct {
	Messages        []Message            `json:"messages"`
	System          []SystemContentBlock `json:"system,omitempty"`
	InferenceConfig *InferenceConfig     `json:"inferenceConfig,omitempty"`
	ToolConfig      *ToolConfig          `json:"toolConfig,omitempty"`
}

// Usage is Converse's native token-accounting shape (confirmed real field
// names — inputTokens/outputTokens/totalTokens).
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	TotalTokens  int `json:"totalTokens"`
}

// Output wraps Converse's response message.
type Output struct {
	Message Message `json:"message"`
}

// Response is Bedrock Converse's native response shape. Confirmed: there
// is no native response-ID field at all, unlike OpenAI/Anthropic/Gemini —
// see hazard #4.
type Response struct {
	Output     Output `json:"output"`
	StopReason string `json:"stopReason"`
	Usage      Usage  `json:"usage"`
}

// Adapter implements adapter.Adapter for Bedrock.
type Adapter struct{}

// New constructs a Bedrock Adapter.
func New() *Adapter {
	return &Adapter{}
}

// Name implements adapter.Adapter.
func (a *Adapter) Name() string {
	return "bedrock"
}

// ToProvider implements adapter.Adapter. It hoists role:"system" messages
// into System, and converts role:"tool" messages into a toolResult
// content block — correlated purely by ToolCallID, per hazard #3 (unlike
// Gemini, no Name-resolution-from-history is needed).
func (a *Adapter) ToProvider(req adapter.ChatRequest) (any, error) {
	var systemParts []string
	messages := make([]Message, 0, len(req.Messages))

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			systemParts = append(systemParts, m.Content)
			continue
		case "tool":
			messages = append(messages, Message{
				Role: "user",
				Content: []ContentBlock{
					{
						ToolResult: &ToolResult{
							ToolUseID: m.ToolCallID,
							Content:   []ToolResultContent{{Text: m.Content}},
							Status:    "success",
						},
					},
				},
			})
			continue
		}

		var blocks []ContentBlock
		if m.Content != "" {
			blocks = append(blocks, ContentBlock{Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			input := map[string]any{}
			if tc.ArgumentsJSON != "" {
				if err := json.Unmarshal([]byte(tc.ArgumentsJSON), &input); err != nil {
					return nil, fmt.Errorf("bedrock: tool call %q has invalid ArgumentsJSON: %w", tc.ID, err)
				}
			}
			blocks = append(blocks, ContentBlock{
				ToolUse: &ToolUse{ToolUseID: tc.ID, Name: tc.Name, Input: input},
			})
		}
		messages = append(messages, Message{Role: m.Role, Content: blocks})
	}

	var toolConfig *ToolConfig
	if len(req.Tools) > 0 {
		tools := make([]Tool, 0, len(req.Tools))
		for _, t := range req.Tools {
			var schema map[string]any
			if t.ParametersJSON != "" {
				if err := json.Unmarshal([]byte(t.ParametersJSON), &schema); err != nil {
					return nil, fmt.Errorf("bedrock: tool %q has invalid ParametersJSON: %w", t.Name, err)
				}
			}
			tools = append(tools, Tool{
				ToolSpec: ToolSpec{
					Name:        t.Name,
					Description: t.Description,
					InputSchema: InputSchema{JSON: schema},
				},
			})
		}
		toolConfig = &ToolConfig{Tools: tools}
	}

	var inferenceConfig *InferenceConfig
	if req.Temperature != nil || req.MaxTokens != nil {
		inferenceConfig = &InferenceConfig{
			Temperature: req.Temperature,
			MaxTokens:   req.MaxTokens,
		}
	}

	var system []SystemContentBlock
	if len(systemParts) > 0 {
		system = []SystemContentBlock{{Text: strings.Join(systemParts, "\n\n")}}
	}

	return &Request{
		Messages:        messages,
		System:          system,
		InferenceConfig: inferenceConfig,
		ToolConfig:      toolConfig,
	}, nil
}

// FromProvider implements adapter.Adapter, converting a Bedrock native
// Response back into the canonical ChatResponse shape. ID is left empty —
// Converse has no native response-ID field, per hazard #4.
func (a *Adapter) FromProvider(resp any) (adapter.ChatResponse, error) {
	native, ok := resp.(*Response)
	if !ok {
		return adapter.ChatResponse{}, fmt.Errorf("bedrock: FromProvider expected *Response, got %T", resp)
	}

	var textParts []string
	var toolCalls []adapter.ToolCall
	for _, block := range native.Output.Message.Content {
		switch {
		case block.ToolUse != nil:
			argsJSON, err := json.Marshal(block.ToolUse.Input)
			if err != nil {
				return adapter.ChatResponse{}, fmt.Errorf("bedrock: marshaling toolUse %q input: %w", block.ToolUse.Name, err)
			}
			toolCalls = append(toolCalls, adapter.ToolCall{
				ID:            block.ToolUse.ToolUseID,
				Name:          block.ToolUse.Name,
				ArgumentsJSON: string(argsJSON),
			})
		case block.Text != "":
			textParts = append(textParts, block.Text)
		}
	}

	finishReason, err := finishReasonFromBedrock(native.StopReason)
	if err != nil {
		return adapter.ChatResponse{}, err
	}

	message := adapter.Message{
		Role:      "assistant",
		Content:   strings.Join(textParts, ""),
		ToolCalls: toolCalls,
	}

	return adapter.ChatResponse{
		Choices: []adapter.Choice{
			{Index: 0, Message: message, FinishReason: finishReason},
		},
		Usage: adapter.Usage{
			PromptTokens:     native.Usage.InputTokens,
			CompletionTokens: native.Usage.OutputTokens,
			TotalTokens:      native.Usage.TotalTokens,
		},
	}, nil
}

// finishReasonFromBedrock maps Converse's real stopReason enum onto the
// canonical (OpenAI-shaped) finish_reason vocabulary, confirmed against
// AWS's real API reference. The 2 malformed-output values return a real
// error rather than a fake successful Choice — they mean the model's own
// tool-use machinery broke, matching gemini.go's identical convention.
func finishReasonFromBedrock(stopReason string) (string, error) {
	switch stopReason {
	case "end_turn", "stop_sequence":
		return "stop", nil
	case "tool_use":
		return "tool_calls", nil
	case "max_tokens":
		return "length", nil
	case "guardrail_intervened", "content_filtered":
		return "content_filter", nil
	case "malformed_tool_use", "malformed_model_output":
		return "", fmt.Errorf("bedrock: model's tool-use machinery failed (stopReason %q)", stopReason)
	case "model_context_window_exceeded":
		// A named, documented approximation -- the canonical schema has
		// no exact equivalent for "context window exceeded," and "length"
		// is the closest honest fit, not a silent guess.
		return "length", nil
	default:
		// Forward-compatible: any future stopReason this mapping doesn't
		// know about yet passes through as "stop" rather than erroring
		// the whole response.
		return "stop", nil
	}
}
