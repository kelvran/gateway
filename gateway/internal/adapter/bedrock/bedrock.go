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
	// Image/Document, per docs/rfcs/2026-09-06-gateway-multimodal-
	// content.md.
	Image    *ImageBlock    `json:"image,omitempty"`
	Document *DocumentBlock `json:"document,omitempty"`
	// CachePoint, when set, is a standalone checkpoint block marking
	// "cache everything up to here" — per
	// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md. Unlike
	// Anthropic's cache_control, this is never combined with another
	// field on the same block; it always appears as its own block,
	// immediately after the content it caches.
	CachePoint *CachePoint `json:"cachePoint,omitempty"`
}

// CachePoint is Converse's real cache-checkpoint marker shape —
// {"cachePoint":{"type":"default"}} — a standalone block placed after
// the content to be cached (a checkpoint boundary), not a property of
// that content's own block, unlike Anthropic's cache_control. Per
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md and
// docs/upgrade-research/gateway-provider-prompt-caching-2026-09-07.md's
// fetched vendor documentation. "default" is the only Type value AWS
// documents.
type CachePoint struct {
	Type string `json:"type"`
}

// appendCachePointIfNeeded appends a CachePoint checkpoint block after
// blocks' own current last block, when cc is set — unless the last
// block is already a CachePoint (a more specific, already-placed
// marker), which would otherwise produce a meaningless, empty-content
// back-to-back checkpoint pair. nil cc or an empty blocks slice is a
// no-op, per docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md.
func appendCachePointIfNeeded(blocks []ContentBlock, cc *adapter.CacheControl) []ContentBlock {
	if cc == nil || len(blocks) == 0 {
		return blocks
	}
	if blocks[len(blocks)-1].CachePoint != nil {
		return blocks
	}
	return append(blocks, ContentBlock{CachePoint: &CachePoint{Type: "default"}})
}

// appendSystemCachePointIfNeeded is appendCachePointIfNeeded's
// SystemContentBlock counterpart, for Converse's top-level system[]
// array.
func appendSystemCachePointIfNeeded(blocks []SystemContentBlock, cc *adapter.CacheControl) []SystemContentBlock {
	if cc == nil || len(blocks) == 0 {
		return blocks
	}
	if blocks[len(blocks)-1].CachePoint != nil {
		return blocks
	}
	return append(blocks, SystemContentBlock{CachePoint: &CachePoint{Type: "default"}})
}

// ByteSource is Converse's native inline-bytes source shape. Converse
// also accepts an s3Location source (a specific s3:// URI plus an
// optional bucketOwner) — not supported by this adapter, since the
// canonical schema's generic URL field has no equivalent bucketOwner
// concept; see ImageBlock/DocumentBlock's own doc comments.
type ByteSource struct {
	Bytes string `json:"bytes"`
}

// ImageBlock is Converse's native image content shape. Format is the
// image's file format ("png", "jpeg", etc.) — Converse requires this
// explicitly, derived here from the canonical MediaType by
// mediaTypeToFormat. Only inline Data (Source.Bytes) is supported —
// see mediaTypeToFormat/contentPartToBlock for why a URL-based part
// returns a real error instead.
type ImageBlock struct {
	Format string     `json:"format"`
	Source ByteSource `json:"source"`
}

// DocumentBlock is Converse's native document content shape. Name is
// required by Converse (a display name) but has no canonical-schema
// equivalent — set to a fixed placeholder, named explicitly rather than
// silently guessed from something unreliable.
type DocumentBlock struct {
	Format string     `json:"format"`
	Name   string     `json:"name"`
	Source ByteSource `json:"source"`
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

// SystemContentBlock is one block of Converse's top-level system[]
// field. CachePoint, when set, is a standalone checkpoint block (see
// ContentBlock.CachePoint's own doc comment) rather than a property of
// the Text block itself, per
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md.
type SystemContentBlock struct {
	Text       string      `json:"text,omitempty"`
	CachePoint *CachePoint `json:"cachePoint,omitempty"`
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
// into System (one SystemContentBlock per canonical system message, per
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md), and converts
// role:"tool" messages into a toolResult content block — correlated
// purely by ToolCallID, per hazard #3 (unlike Gemini, no
// Name-resolution-from-history is needed).
func (a *Adapter) ToProvider(req adapter.ChatRequest) (any, error) {
	var systemBlocks []SystemContentBlock
	messages := make([]Message, 0, len(req.Messages))

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			systemBlocks = append(systemBlocks, SystemContentBlock{Text: m.Content})
			systemBlocks = appendSystemCachePointIfNeeded(systemBlocks, m.CacheControl)
			continue
		case "tool":
			blocks := []ContentBlock{
				{
					ToolResult: &ToolResult{
						ToolUseID: m.ToolCallID,
						Content:   []ToolResultContent{{Text: m.Content}},
						Status:    "success",
					},
				},
			}
			blocks = appendCachePointIfNeeded(blocks, m.CacheControl)
			messages = append(messages, Message{Role: "user", Content: blocks})
			continue
		}

		var blocks []ContentBlock
		if m.Content != "" {
			blocks = append(blocks, ContentBlock{Text: m.Content})
		}
		for _, part := range m.Parts {
			block, err := contentPartToBlock(part)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
			blocks = appendCachePointIfNeeded(blocks, part.CacheControl)
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
		// A message-level CacheControl marks "cache everything through
		// this message" via a trailing checkpoint, per
		// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md —
		// appendCachePointIfNeeded's own no-duplicate-checkpoint guard
		// keeps this a no-op if a part-level marker already placed one
		// on this exact last block.
		blocks = appendCachePointIfNeeded(blocks, m.CacheControl)
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

	return &Request{
		Messages:        messages,
		System:          systemBlocks,
		InferenceConfig: inferenceConfig,
		ToolConfig:      toolConfig,
	}, nil
}

// contentPartToBlock converts one canonical adapter.ContentPart into
// Converse's native ContentBlock shape, per
// docs/rfcs/2026-09-06-gateway-multimodal-content.md. Converse's real
// image/document source only accepts inline "bytes" or an s3Location (a
// specific s3:// URI plus an optional bucketOwner the canonical schema
// has no equivalent field for) — never a generic URL like every other
// provider this codebase supports. A URL-based part therefore returns a
// real, typed error rather than a silently wrong mapping; only inline
// Data is supported in this pass, a named scope limit.
func contentPartToBlock(p adapter.ContentPart) (ContentBlock, error) {
	switch p.Type {
	case "text":
		return ContentBlock{Text: p.Text}, nil
	case "image":
		if p.Data == "" {
			return ContentBlock{}, fmt.Errorf("bedrock: image part has no Data set (URL-based image parts are not supported — Converse has no generic-URL source, only inline bytes or an s3Location)")
		}
		return ContentBlock{Image: &ImageBlock{
			Format: mediaTypeToFormat(p.MediaType),
			Source: ByteSource{Bytes: p.Data},
		}}, nil
	case "document":
		if p.Data == "" {
			return ContentBlock{}, fmt.Errorf("bedrock: document part has no Data set (URL-based document parts are not supported — Converse has no generic-URL source, only inline bytes or an s3Location)")
		}
		return ContentBlock{Document: &DocumentBlock{
			Format: mediaTypeToFormat(p.MediaType),
			Name:   "document",
			Source: ByteSource{Bytes: p.Data},
		}}, nil
	default:
		return ContentBlock{}, fmt.Errorf("bedrock: unsupported content part type %q", p.Type)
	}
}

// mediaTypeToFormat derives Converse's required file-format string
// (e.g. "png", "pdf") from a canonical MIME type — the substring after
// the last "/", which matches every real format Converse documents
// (image/png, image/jpeg, application/pdf, text/csv) except a small
// number of atypical MIME types (e.g. "text/plain" would derive "plain"
// rather than Converse's own "txt") — a named limitation, not silently
// wrong for the common cases.
func mediaTypeToFormat(mediaType string) string {
	if i := strings.LastIndex(mediaType, "/"); i >= 0 {
		return mediaType[i+1:]
	}
	return mediaType
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
