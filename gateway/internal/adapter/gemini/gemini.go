// Package gemini implements a genuine translation adapter between the
// canonical schema and Google's Gemini generateContent/streamGenerateContent
// API. Real field shapes confirmed directly against Google's live API
// discovery document (generativelanguage.googleapis.com/$discovery/rest,
// version v1beta) — not assumed from memory or secondhand docs — per
// docs/rfcs/2026-09-04-gemini-adapter.md.
//
// Like anthropic, this adapter earns its keep handling real normalization
// hazards documented in gateway/ARCHITECTURE.md's "Canonical Schema &
// Provider Adapters" section, plus two more specific to Gemini's real shape:
//
//  1. System-prompt placement: Gemini's contents[].role accepts only "user"
//     or "model" — a role:"system" message is hoisted into a top-level
//     systemInstruction field, same hazard Anthropic already solves.
//  2. Tool-call argument encoding: Gemini's native functionCall.args is an
//     already-parsed JSON object, not a string — ToProvider parses
//     ArgumentsJSON, FromProvider re-marshals back, same as Anthropic's
//     Input map[string]any pattern.
//  3. Tool-result placement: Gemini has no "tool"/"function" role in
//     contents[] — a tool result is sent as a role:"user" content carrying
//     a functionResponse part.
//  4. functionResponse.name resolution: unlike OpenAI/Anthropic, a Gemini
//     tool result has no field carrying the canonical Message.Content
//     alone — it requires the *originating* tool call's Name, which the
//     canonical role:"tool" message never carries directly (only
//     ToolCallID). ToProvider must resolve it by scanning prior messages'
//     ToolCalls for a matching ID before it can honestly build the
//     functionResponse part.
//  5. functionResponse.response must be a JSON object, never a bare
//     string — wrapped as {"result": <content>}, an explicit, named
//     convention (Gemini's own schema uses "result" as its example key).
package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// Content is Gemini's native message shape. Role is "user" or "model" only
// — Gemini has no "system" role in contents[] (see package doc hazard #1)
// and no "tool" role (hazard #3: a tool result is role:"user").
type Content struct {
	Role  string `json:"role,omitempty"`
	Parts []Part `json:"parts"`
}

// Part is one block of Gemini's part union — only one of Text/FunctionCall/
// FunctionResponse is ever populated per Part, mirroring Anthropic's
// ContentBlock union convention.
type Part struct {
	Text             string            `json:"text,omitempty"`
	FunctionCall     *FunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *FunctionResponse `json:"functionResponse,omitempty"`
}

// FunctionCall is Gemini's native function-call shape. Args is an
// already-parsed JSON object (confirmed: the real schema's "args" property
// is additionalProperties:any, type:object — not a string), per hazard #2.
// ID is optional in the real schema ("if populated, the client... return[s]
// the response with the matching id") — this adapter always sets it from
// the canonical ToolCall.ID, which is always non-empty.
type FunctionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// FunctionResponse is Gemini's native tool-result shape. Name is required
// (per hazard #4, this adapter resolves it via a ToolCall.ID->Name lookup,
// never left empty). Response must be a JSON object (hazard #5).
type FunctionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

// Tool is Gemini's native tool-definition shape — a container for one or
// more function declarations.
type Tool struct {
	FunctionDeclarations []FunctionDeclaration `json:"functionDeclarations"`
}

// FunctionDeclaration describes one callable function. Parameters is a
// parsed JSON Schema object, not a string, same as Anthropic's InputSchema.
type FunctionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// GenerationConfig carries the subset of Gemini's real generationConfig
// fields this adapter maps from the canonical schema.
type GenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
}

// Request is Gemini's native generateContent/streamGenerateContent request
// shape.
type Request struct {
	Contents          []Content         `json:"contents"`
	SystemInstruction *Content          `json:"systemInstruction,omitempty"`
	GenerationConfig  *GenerationConfig `json:"generationConfig,omitempty"`
	Tools             []Tool            `json:"tools,omitempty"`
}

// Candidate is one native response candidate. FinishReason's real enum has
// 22 values (confirmed against the live schema) — far more than OpenAI's or
// Anthropic's — see finishReasonFromGemini.
type Candidate struct {
	Content      Content `json:"content"`
	FinishReason string  `json:"finishReason"`
}

// UsageMetadata is Gemini's native token-accounting shape (confirmed real
// field names — not completion_tokens-shaped like OpenAI's).
type UsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// Response is Gemini's native generateContent response shape. Confirmed
// against the live discovery schema that streamGenerateContent's SSE
// frames reuse this exact same type — there is no separate stream-chunk
// shape, unlike OpenAI's chat.completion.chunk.
type Response struct {
	ResponseID    string        `json:"responseId,omitempty"`
	ModelVersion  string        `json:"modelVersion,omitempty"`
	Candidates    []Candidate   `json:"candidates"`
	UsageMetadata UsageMetadata `json:"usageMetadata"`
}

// Adapter implements adapter.Adapter for Gemini.
type Adapter struct{}

// New constructs a Gemini Adapter.
func New() *Adapter {
	return &Adapter{}
}

// Name implements adapter.Adapter.
func (a *Adapter) Name() string {
	return "gemini"
}

// ToProvider implements adapter.Adapter. It hoists role:"system" messages
// into SystemInstruction, maps role:"assistant" to "model", and converts
// role:"tool" messages into a functionResponse part — resolving the
// required Name field via a lookup built from every prior message's
// ToolCalls, per the package doc's hazard #4.
func (a *Adapter) ToProvider(req adapter.ChatRequest) (any, error) {
	toolNameByID := make(map[string]string)
	for _, m := range req.Messages {
		for _, tc := range m.ToolCalls {
			toolNameByID[tc.ID] = tc.Name
		}
	}

	var systemParts []string
	contents := make([]Content, 0, len(req.Messages))

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			systemParts = append(systemParts, m.Content)
			continue
		case "tool":
			name, ok := toolNameByID[m.ToolCallID]
			if !ok {
				return nil, fmt.Errorf("gemini: tool message references unknown tool_call_id %q", m.ToolCallID)
			}
			contents = append(contents, Content{
				Role: "user",
				Parts: []Part{
					{
						FunctionResponse: &FunctionResponse{
							ID:       m.ToolCallID,
							Name:     name,
							Response: map[string]any{"result": m.Content},
						},
					},
				},
			})
			continue
		}

		role := m.Role
		if role == "assistant" {
			role = "model"
		}

		var parts []Part
		if m.Content != "" {
			parts = append(parts, Part{Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			args := map[string]any{}
			if tc.ArgumentsJSON != "" {
				if err := json.Unmarshal([]byte(tc.ArgumentsJSON), &args); err != nil {
					return nil, fmt.Errorf("gemini: tool call %q has invalid ArgumentsJSON: %w", tc.ID, err)
				}
			}
			parts = append(parts, Part{
				FunctionCall: &FunctionCall{ID: tc.ID, Name: tc.Name, Args: args},
			})
		}
		contents = append(contents, Content{Role: role, Parts: parts})
	}

	var tools []Tool
	if len(req.Tools) > 0 {
		decls := make([]FunctionDeclaration, 0, len(req.Tools))
		for _, t := range req.Tools {
			var params map[string]any
			if t.ParametersJSON != "" {
				if err := json.Unmarshal([]byte(t.ParametersJSON), &params); err != nil {
					return nil, fmt.Errorf("gemini: tool %q has invalid ParametersJSON: %w", t.Name, err)
				}
			}
			decls = append(decls, FunctionDeclaration{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			})
		}
		tools = []Tool{{FunctionDeclarations: decls}}
	}

	var genConfig *GenerationConfig
	if req.Temperature != nil || req.MaxTokens != nil {
		genConfig = &GenerationConfig{
			Temperature:     req.Temperature,
			MaxOutputTokens: req.MaxTokens,
		}
	}

	var systemInstruction *Content
	if len(systemParts) > 0 {
		systemInstruction = &Content{Parts: []Part{{Text: strings.Join(systemParts, "\n\n")}}}
	}

	return &Request{
		Contents:          contents,
		SystemInstruction: systemInstruction,
		GenerationConfig:  genConfig,
		Tools:             tools,
	}, nil
}

// FromProvider implements adapter.Adapter, converting a Gemini native
// Response back into the canonical ChatResponse shape. Only the first
// candidate is translated — candidateCount > 1 is out of scope this pass
// (see the RFC's Alternatives Considered).
func (a *Adapter) FromProvider(resp any) (adapter.ChatResponse, error) {
	native, ok := resp.(*Response)
	if !ok {
		return adapter.ChatResponse{}, fmt.Errorf("gemini: FromProvider expected *Response, got %T", resp)
	}
	if len(native.Candidates) == 0 {
		return adapter.ChatResponse{}, fmt.Errorf("gemini: response contains no candidates (possibly blocked by safety filtering; promptFeedback is not surfaced this pass)")
	}

	candidate := native.Candidates[0]

	var textParts []string
	var toolCalls []adapter.ToolCall
	for _, part := range candidate.Content.Parts {
		switch {
		case part.FunctionCall != nil:
			argsJSON, err := json.Marshal(part.FunctionCall.Args)
			if err != nil {
				return adapter.ChatResponse{}, fmt.Errorf("gemini: marshaling functionCall %q args: %w", part.FunctionCall.Name, err)
			}
			toolCalls = append(toolCalls, adapter.ToolCall{
				ID:            part.FunctionCall.ID,
				Name:          part.FunctionCall.Name,
				ArgumentsJSON: string(argsJSON),
			})
		case part.Text != "":
			textParts = append(textParts, part.Text)
		}
	}

	finishReason, err := finishReasonFromGemini(candidate.FinishReason, len(toolCalls) > 0)
	if err != nil {
		return adapter.ChatResponse{}, err
	}

	message := adapter.Message{
		Role:      "assistant",
		Content:   strings.Join(textParts, ""),
		ToolCalls: toolCalls,
	}

	return adapter.ChatResponse{
		ID:    native.ResponseID,
		Model: native.ModelVersion,
		Choices: []adapter.Choice{
			{Index: 0, Message: message, FinishReason: finishReason},
		},
		Usage: adapter.Usage{
			PromptTokens:     native.UsageMetadata.PromptTokenCount,
			CompletionTokens: native.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      native.UsageMetadata.TotalTokenCount,
		},
	}, nil
}

// finishReasonFromGemini maps Gemini's real 22-value finishReason enum
// (confirmed against the live discovery schema) onto the canonical
// (OpenAI-shaped) finish_reason vocabulary. hasFunctionCall is required
// because, unlike OpenAI, Gemini has no distinct "I stopped to call a
// tool" reason — a STOP with a functionCall part present must be detected
// by scanning parts, not by the finishReason string's value alone.
//
// The 5 "malformed tool call" values return a real error rather than a
// fake successful Choice — they mean the model's own tool-call machinery
// broke, not that it produced a usable answer, matching this codebase's
// "never fabricate success" convention.
func finishReasonFromGemini(finishReason string, hasFunctionCall bool) (string, error) {
	switch finishReason {
	case "STOP", "":
		if hasFunctionCall {
			return "tool_calls", nil
		}
		return "stop", nil
	case "MAX_TOKENS":
		return "length", nil
	case "SAFETY", "PROHIBITED_CONTENT", "SPII", "BLOCKLIST":
		return "content_filter", nil
	case "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL", "TOO_MANY_TOOL_CALLS",
		"MISSING_THOUGHT_SIGNATURE", "MALFORMED_RESPONSE":
		return "", fmt.Errorf("gemini: model's tool-call machinery failed (finishReason %q)", finishReason)
	default:
		// Forward-compatible: RECITATION, LANGUAGE, OTHER, ESCALATION,
		// PUP_LIMITED_DISABLED, FINISH_REASON_UNSPECIFIED, the image-
		// generation-only reasons (never reachable via this text/tool-use
		// adapter), and any future value this mapping doesn't know about
		// yet all pass through as "stop" rather than erroring the whole
		// response.
		return "stop", nil
	}
}
