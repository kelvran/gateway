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

// Request is Anthropic's native Messages API request shape. System is an
// array of blocks (restructured from a plain string, per
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md) so each
// canonical role:"system" message can carry its own independent
// cache_control breakpoint rather than being flattened into one joined
// string.
type Request struct {
	Model       string        `json:"model"`
	System      []SystemBlock `json:"system,omitempty"`
	Messages    []Message     `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature *float64      `json:"temperature,omitempty"`
	Tools       []Tool        `json:"tools,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
}

// SystemBlock is one block of Anthropic's real system-array shape, per
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md. Type is
// always "text" — Anthropic's system array supports no other block
// type today.
type SystemBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text"`
	CacheControl *CacheControlWire `json:"cache_control,omitempty"`
}

// CacheControlWire is Anthropic's real cache_control block shape —
// {"type":"ephemeral","ttl":"5m"|"1h"} — confirmed against
// docs/upgrade-research/gateway-provider-prompt-caching-2026-09-07.md's
// fetched vendor documentation, per
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md. "ephemeral"
// is the only Type value Anthropic documents; an empty TTL means
// Anthropic's own 5-minute default.
type CacheControlWire struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// cacheControlWire converts a canonical adapter.CacheControl marker into
// Anthropic's native cache_control block shape. nil in, nil out — an
// unset marker is a silent no-op, never an error, per
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md.
func cacheControlWire(cc *adapter.CacheControl) *CacheControlWire {
	if cc == nil {
		return nil
	}
	return &CacheControlWire{Type: "ephemeral", TTL: cc.TTL}
}

// defaultSystemCacheControl is the marker Kelvran auto-populates on an
// otherwise-unmarked system message, per
// docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md. The zero
// value — empty TTL — requests Anthropic's own default (5-minute) cache
// lifetime, the cheapest tier: auto-populate is applied without the
// caller ever having asked, so it must never silently commit them to the
// more expensive 1-hour tier.
var defaultSystemCacheControl = &adapter.CacheControl{}

// effectiveSystemCacheControl resolves the CacheControl marker to apply
// to a system message, per docs/rfcs/2026-09-07-gateway-cache-control-
// auto-populate.md's precedence rule: an explicit, caller-supplied
// marker (non-nil, even if every field on it is itself zero-valued)
// always wins outright; only when the caller left it completely unset
// does auto-populate ever apply, and only when the deployment hasn't
// opted out (autoDisabled == false) — otherwise nil, the pre-existing,
// caller-explicit-only behavior. Called from exactly one place in
// ToProvider (the "system" case) — never from the general
// user/assistant/tool path, matching that RFC's heuristic (a): auto-
// populate never applies to arbitrary user/assistant content, content
// parts, or tool definitions.
func effectiveSystemCacheControl(explicit *adapter.CacheControl, autoDisabled bool) *adapter.CacheControl {
	if explicit != nil {
		return explicit
	}
	if autoDisabled {
		return nil
	}
	return defaultSystemCacheControl
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

	// "image"/"document" block, per
	// docs/rfcs/2026-09-06-gateway-multimodal-content.md.
	Source *ContentSource `json:"source,omitempty"`

	// CacheControl, when set, marks this specific block as a caching
	// breakpoint, per docs/rfcs/2026-09-07-gateway-provider-prompt-
	// caching.md. Valid on any block type, mirroring Anthropic's own
	// real per-content-block cache_control placement.
	CacheControl *CacheControlWire `json:"cache_control,omitempty"`
}

// ContentSource is Anthropic's native image/document source shape —
// either inline base64 data or a remote URL, per
// docs/rfcs/2026-09-06-gateway-multimodal-content.md.
type ContentSource struct {
	Type      string `json:"type"` // "base64" or "url"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// Tool is Anthropic's native tool-definition shape. InputSchema is a
// parsed JSON Schema object, not a string. CacheControl, when set, is a
// sibling key directly on this object -- confirmed against Anthropic's
// own live documentation (platform.claude.com/docs/en/build-with-claude/
// prompt-caching) for
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md's
// tool-definition-level addendum -- mechanically identical to
// ContentBlock's own inline cache_control placement, not a separate
// wrapper.
type Tool struct {
	Name         string            `json:"name"`
	Description  string            `json:"description,omitempty"`
	InputSchema  map[string]any    `json:"input_schema,omitempty"`
	CacheControl *CacheControlWire `json:"cache_control,omitempty"`
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
// CacheCreationInputTokens/CacheReadInputTokens are real response fields
// (verified live against platform.claude.com/docs/en/api/messages) --
// previously silently dropped by json.Unmarshal since this struct had no
// field for them.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
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
// field (one SystemBlock per canonical system message, per
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md), and converts
// every other message into Anthropic's block-based content shape.
func (a *Adapter) ToProvider(req adapter.ChatRequest) (any, error) {
	var systemBlocks []SystemBlock
	messages := make([]Message, 0, len(req.Messages))

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			systemBlocks = append(systemBlocks, SystemBlock{
				Type:         "text",
				Text:         m.Content,
				CacheControl: cacheControlWire(effectiveSystemCacheControl(m.CacheControl, req.DisableCacheControlAutoPopulate)),
			})
			continue
		case "tool":
			// Anthropic has no "tool" role: a tool result is sent as a
			// "user" message carrying a tool_result content block.
			block := ContentBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content}
			if m.CacheControl != nil {
				block.CacheControl = cacheControlWire(m.CacheControl)
			}
			messages = append(messages, Message{
				Role:    "user",
				Content: []ContentBlock{block},
			})
			continue
		}

		var blocks []ContentBlock
		if m.Content != "" {
			blocks = append(blocks, ContentBlock{Type: "text", Text: m.Content})
		}
		for _, part := range m.Parts {
			block, err := contentPartToBlock(part)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
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
		// A message-level CacheControl marks "cache everything through
		// this message" by attaching to its own last block -- Anthropic's
		// own idiomatic pattern -- unless that exact block already
		// carries a more specific part-level marker, to avoid overwriting
		// it, per docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md.
		if m.CacheControl != nil && len(blocks) > 0 && blocks[len(blocks)-1].CacheControl == nil {
			blocks[len(blocks)-1].CacheControl = cacheControlWire(m.CacheControl)
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
				Name:         t.Name,
				Description:  t.Description,
				InputSchema:  schema,
				CacheControl: cacheControlWire(t.CacheControl),
			})
		}
	}

	maxTokens := defaultMaxTokens
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}

	return &Request{
		Model:       req.Model,
		System:      systemBlocks,
		Messages:    messages,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
		Tools:       tools,
		Stream:      req.Stream,
	}, nil
}

// contentPartToBlock converts one canonical adapter.ContentPart into
// Anthropic's native ContentBlock shape, per
// docs/rfcs/2026-09-06-gateway-multimodal-content.md. Exactly one of
// p.Data/p.URL is expected for an "image"/"document" part — real, typed
// errors otherwise, never a silently-dropped field. p.CacheControl, when
// set, attaches directly to the returned block, per
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md's part-level
// granularity.
func contentPartToBlock(p adapter.ContentPart) (ContentBlock, error) {
	switch p.Type {
	case "text":
		return ContentBlock{Type: "text", Text: p.Text, CacheControl: cacheControlWire(p.CacheControl)}, nil
	case "image", "document":
		source := &ContentSource{MediaType: p.MediaType}
		switch {
		case p.Data != "":
			source.Type = "base64"
			source.Data = p.Data
		case p.URL != "":
			source.Type = "url"
			source.URL = p.URL
		default:
			return ContentBlock{}, fmt.Errorf("anthropic: %s part has neither Data nor URL set", p.Type)
		}
		return ContentBlock{Type: p.Type, Source: source, CacheControl: cacheControlWire(p.CacheControl)}, nil
	default:
		return ContentBlock{}, fmt.Errorf("anthropic: unsupported content part type %q", p.Type)
	}
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
			PromptTokens:        native.Usage.InputTokens + native.Usage.CacheReadInputTokens + native.Usage.CacheCreationInputTokens,
			CompletionTokens:    native.Usage.OutputTokens,
			TotalTokens:         native.Usage.InputTokens + native.Usage.CacheReadInputTokens + native.Usage.CacheCreationInputTokens + native.Usage.OutputTokens,
			CacheReadTokens:     native.Usage.CacheReadInputTokens,
			CacheCreationTokens: native.Usage.CacheCreationInputTokens,
		},
	}, nil
}
