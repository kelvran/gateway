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
	// ReasoningContent, when set, is Converse's real reasoningContent
	// block -- confirmed against docs.aws.amazon.com/bedrock/latest/
	// APIReference/API_runtime_ContentBlock.html: a sibling union member
	// of Text/ToolUse/ToolResult/Image/Document/CachePoint on the real
	// ContentBlock union ("This data type is a UNION, so only one of the
	// following members can be specified when used or returned"). Per
	// docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md.
	ReasoningContent *ReasoningContentBlock `json:"reasoningContent,omitempty"`
}

// ReasoningContentBlock is Converse's real reasoningContent shape,
// confirmed against API_runtime_ReasoningContentBlock.html: "This data
// type is a UNION, so only one of the following members can be
// specified when used or returned." ReasoningText carries the plaintext
// case; RedactedContent carries the provider-encrypted-ciphertext case
// (Base64-encoded binary data, per that same doc page) -- mutually
// exclusive, mirroring Anthropic's "thinking" vs. "redacted_thinking"
// block-type split but as one union struct rather than two block types.
type ReasoningContentBlock struct {
	ReasoningText   *ReasoningTextBlock `json:"reasoningText,omitempty"`
	RedactedContent string              `json:"redactedContent,omitempty"`
}

// ReasoningTextBlock is Converse's real reasoningText shape, confirmed
// against API_runtime_ReasoningTextBlock.html: Text is documented
// Required; Signature is documented optional but, per that same page,
// "If you pass a reasoning block back to the API in a multi-turn
// conversation, include the text and its signature unmodified" -- the
// same must-replay-verbatim contract as Anthropic's thinking.signature,
// per docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md.
type ReasoningTextBlock struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
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

// defaultSystemCacheControl is the marker Kelvran auto-populates on an
// otherwise-unmarked system message, per
// docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md. Bedrock's
// CachePoint has no TTL concept at all ({"type":"default"} is the only
// shape AWS documents), so the zero-value adapter.CacheControl carries
// nothing this adapter reads beyond its own non-nil-ness — the value
// only ever flows into appendSystemCachePointIfNeeded's nil check.
var defaultSystemCacheControl = &adapter.CacheControl{}

// effectiveSystemCacheControl mirrors anthropic.go's identical-named
// helper — a deliberately duplicated, package-private helper, not
// shared code (adapters don't import each other, per
// gateway/ARCHITECTURE.md's dependency rules). See that copy's doc
// comment for the full precedence rule; called from exactly the same
// single call site here — the "system" case in ToProvider's per-message
// switch, never the general user/assistant/tool path.
func effectiveSystemCacheControl(explicit *adapter.CacheControl, autoDisabled bool) *adapter.CacheControl {
	if explicit != nil {
		return explicit
	}
	if autoDisabled {
		return nil
	}
	return defaultSystemCacheControl
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

// Tool is one element of Converse's toolConfig.tools[] array. Real AWS
// union type -- "only one of the following members can be specified"
// per element -- confirmed against
// docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Tool.html
// for docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md's
// tool-definition-level addendum: cachePoint, systemTool, toolSpec.
// Kelvran emits only toolSpec and (new) cachePoint elements; systemTool
// (Bedrock's own built-in tools, e.g. code execution) has no canonical
// schema equivalent and is out of scope. ToolSpec is a pointer (with
// omitempty) so a cachePoint-only element never marshals a spurious
// empty "toolSpec":{}.
type Tool struct {
	ToolSpec   *ToolSpec   `json:"toolSpec,omitempty"`
	CachePoint *CachePoint `json:"cachePoint,omitempty"`
}

// appendToolCachePointIfNeeded appends a standalone {"cachePoint":{...}}
// element to tools, immediately after tools' own current last element,
// when cc is set -- mirroring appendCachePointIfNeeded's exact shape
// (nil cc or empty tools is a no-op; a last element that's already a
// CachePoint is left alone rather than duplicated), applied to the
// tools[] union array specifically. Confirmed against AWS's own "tools
// checkpoints" worked example
// (docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html):
// cachePoint is a standalone array element sibling to toolSpec elements,
// never a property attached to a toolSpec object, per
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md's addendum.
func appendToolCachePointIfNeeded(tools []Tool, cc *adapter.CacheControl) []Tool {
	if cc == nil || len(tools) == 0 {
		return tools
	}
	if tools[len(tools)-1].CachePoint != nil {
		return tools
	}
	return append(tools, Tool{CachePoint: &CachePoint{Type: "default"}})
}

// ToolSpec describes one callable tool. InputSchema.JSON is a parsed JSON
// Schema object, not a string, same as Anthropic's InputSchema.
//
// Deliberately has no Strict field: unlike Anthropic's direct Messages
// API (see anthropic.Tool.Strict), no per-tool "strict" wire shape for
// Converse's toolSpec is confirmed real -- this session's live
// verification covered only the request-level additionalModelRequestFields/
// output_config escape hatch above, not a per-tool grammar flag. Given
// AWS's OWN demonstrated behavior of hard-rejecting unrecognized fields
// (the very output_config.format ValidationException that motivated this
// feature), guessing at an unconfirmed field here risks a real, breaking
// AWS error rather than a silent no-op. adapter.ToolDef.Strict is
// therefore read only by the Anthropic adapter in v1; Bedrock is a named
// scope limit, not an oversight.
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
	// AdditionalModelRequestFields is Converse's real, genuine escape-hatch
	// field for model-specific request parameters the Converse API itself
	// doesn't otherwise model -- confirmed real and used here for the
	// first time, live-verified 2026-09-10 against a real Converse API
	// call: {"output_config": {"format": {"type": "json_schema",
	// "schema": {...}}}} produces schema-conforming output -- but ONLY for
	// a specific whitelist of Bedrock-hosted Claude models (see
	// adapter.SupportsStructuredOutput's own doc comment). Calling it
	// against a model NOT on that whitelist is a real, AWS-enforced
	// rejection (a clean ValidationException: "output_config.format:
	// Extra inputs are not permitted"), not a silently-ignored no-op --
	// see additionalModelRequestFieldsFor for how this adapter avoids
	// ever sending that combination.
	AdditionalModelRequestFields map[string]any `json:"additionalModelRequestFields,omitempty"`
}

// Usage is Converse's native token-accounting shape (confirmed real field
// names — inputTokens/outputTokens/totalTokens — plus cacheReadInputTokens/
// cacheWriteInputTokens, verified live against
// docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_TokenUsage.html.
// TotalTokens is documented there as "the total of input tokens and tokens
// generated by the model" -- i.e. InputTokens+OutputTokens ONLY, so cache
// tokens are NOT already folded into it, mirroring Anthropic's own native
// shape (see adapter.Usage's doc comment). Both cache fields are optional
// (omitted/0 when no cache activity occurred).
type Usage struct {
	InputTokens           int `json:"inputTokens"`
	OutputTokens          int `json:"outputTokens"`
	TotalTokens           int `json:"totalTokens"`
	CacheReadInputTokens  int `json:"cacheReadInputTokens,omitempty"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens,omitempty"`
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
			systemBlocks = appendSystemCachePointIfNeeded(systemBlocks, effectiveSystemCacheControl(m.CacheControl, req.DisableCacheControlAutoPopulate))
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
		if len(m.ToolCalls) == 0 {
			blocks = append(blocks, reasoningBlocksToProvider(m.ReasoningBlocks, 0, true)...)
		} else {
			blocks = append(blocks, reasoningBlocksToProvider(m.ReasoningBlocks, 0, false)...)
		}
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
		for i, tc := range m.ToolCalls {
			input := map[string]any{}
			if tc.ArgumentsJSON != "" {
				if err := json.Unmarshal([]byte(tc.ArgumentsJSON), &input); err != nil {
					return nil, fmt.Errorf("bedrock: tool call %q has invalid ArgumentsJSON: %w", tc.ID, err)
				}
			}
			blocks = append(blocks, ContentBlock{
				ToolUse: &ToolUse{ToolUseID: tc.ID, Name: tc.Name, Input: input},
			})
			if i+1 < len(m.ToolCalls) {
				blocks = append(blocks, reasoningBlocksToProvider(m.ReasoningBlocks, i+1, false)...)
			} else {
				blocks = append(blocks, reasoningBlocksToProvider(m.ReasoningBlocks, i+1, true)...)
			}
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
				ToolSpec: &ToolSpec{
					Name:        t.Name,
					Description: t.Description,
					InputSchema: InputSchema{JSON: schema},
				},
			})
			// A tool-definition-level CacheControl appends a trailing
			// cachePoint element immediately after this specific tool,
			// per docs/rfcs/2026-09-07-gateway-provider-prompt-
			// caching.md's addendum -- the same "append right after the
			// marked item" convention already used for content parts,
			// not restricted to only the request's very last tool.
			tools = appendToolCachePointIfNeeded(tools, t.CacheControl)
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

	additionalFields, err := additionalModelRequestFieldsFor(req.ResponseFormat, req.Model)
	if err != nil {
		return nil, err
	}

	return &Request{
		Messages:                     messages,
		System:                       systemBlocks,
		InferenceConfig:              inferenceConfig,
		ToolConfig:                   toolConfig,
		AdditionalModelRequestFields: additionalFields,
	}, nil
}

// additionalModelRequestFieldsFor builds Converse's real
// additionalModelRequestFields escape-hatch value from rf, but ONLY when
// model is on adapter.SupportsStructuredOutput's Bedrock whitelist --
// calling output_config.format against an unsupported model is a real,
// AWS-enforced rejection (see Request.AdditionalModelRequestFields' own
// doc comment), never something this adapter sends and hopes for the
// best on. model is expected to already be the deployment's real
// upstream model ID (dataplane.callDeployment/streamDeployment already
// set req.Model to dep.UpstreamModel before calling ToProvider), not the
// client-facing canonical model name -- the same value
// adapter.SupportsStructuredOutput's Bedrock branch is designed to match
// against.
//
// A named, accepted scope limit for the non-whitelisted case: this
// helper simply omits the field rather than erroring, so a FIRST-ATTEMPT
// (non-fallback) call against an unsupported model with ResponseFormat
// set silently proceeds WITHOUT schema enforcement. Closing that gap is
// deliberately scoped to fallback hops only -- dataplane's
// attemptFallbackChain capabilityOK gate skips an incapable fallback
// TARGET before ever calling it -- not the first-attempt router pick,
// per this feature's own v1 design.
func additionalModelRequestFieldsFor(rf *adapter.ResponseFormat, model string) (map[string]any, error) {
	if rf == nil {
		return nil, nil
	}
	if !adapter.SupportsStructuredOutput("bedrock", model) {
		return nil, nil
	}
	format := map[string]any{"type": rf.Type}
	if rf.JSONSchema != nil && len(rf.JSONSchema.Schema) > 0 {
		var schema map[string]any
		if err := json.Unmarshal(rf.JSONSchema.Schema, &schema); err != nil {
			return nil, fmt.Errorf("bedrock: response_format has invalid JSONSchema.Schema: %w", err)
		}
		format["schema"] = schema
	}
	return map[string]any{"output_config": map[string]any{"format": format}}, nil
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

// reasoningBlocksToProvider mirrors anthropic.go's identical-purpose
// helper (reasoningBlocksToProvider) -- returns the ContentBlocks for
// every canonical ReasoningBlock in rbs whose Sequence matches seq
// exactly (or, when trailing is true, every remaining block with
// Sequence >= seq), reconstructing exact original block order relative
// to ToolCalls, per docs/rfcs/2026-09-12-gateway-reasoning-content-
// canonical-schema.md.
func reasoningBlocksToProvider(rbs []adapter.ReasoningBlock, seq int, trailing bool) []ContentBlock {
	var out []ContentBlock
	for _, rb := range rbs {
		if (trailing && rb.Sequence >= seq) || (!trailing && rb.Sequence == seq) {
			out = append(out, reasoningBlockToProvider(rb))
		}
	}
	return out
}

// reasoningBlockToProvider converts one canonical adapter.ReasoningBlock
// into Converse's native reasoningContent block shape.
func reasoningBlockToProvider(rb adapter.ReasoningBlock) ContentBlock {
	if rb.Redacted {
		return ContentBlock{ReasoningContent: &ReasoningContentBlock{RedactedContent: rb.Data}}
	}
	return ContentBlock{ReasoningContent: &ReasoningContentBlock{
		ReasoningText: &ReasoningTextBlock{Text: rb.Text, Signature: rb.Signature},
	}}
}

// reasoningBlockFromProvider converts one Converse-native
// ReasoningContentBlock into a canonical adapter.ReasoningBlock.
// Sequence is passed in by the caller as len(toolCalls) captured so far
// -- exactly mirroring reasoningBlocksToProvider's replay logic above,
// and anthropic.go's identical-purpose FromProvider capture -- per
// docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md.
func reasoningBlockFromProvider(rc *ReasoningContentBlock, sequence int) adapter.ReasoningBlock {
	if rc.RedactedContent != "" {
		return adapter.ReasoningBlock{Sequence: sequence, Redacted: true, Data: rc.RedactedContent}
	}
	var text, signature string
	if rc.ReasoningText != nil {
		text = rc.ReasoningText.Text
		signature = rc.ReasoningText.Signature
	}
	return adapter.ReasoningBlock{Sequence: sequence, Text: text, Signature: signature}
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
	var reasoningBlocks []adapter.ReasoningBlock
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
		case block.ReasoningContent != nil:
			// Sequence == len(toolCalls) so far records "immediately
			// before the next toolUse block" -- see
			// reasoningBlockFromProvider's own doc comment.
			reasoningBlocks = append(reasoningBlocks, reasoningBlockFromProvider(block.ReasoningContent, len(toolCalls)))
		case block.Text != "":
			textParts = append(textParts, block.Text)
		}
	}

	finishReason, err := finishReasonFromBedrock(native.StopReason)
	if err != nil {
		return adapter.ChatResponse{}, err
	}

	message := adapter.Message{
		Role:            "assistant",
		Content:         strings.Join(textParts, ""),
		ToolCalls:       toolCalls,
		ReasoningBlocks: reasoningBlocks,
	}

	return adapter.ChatResponse{
		Choices: []adapter.Choice{
			{Index: 0, Message: message, FinishReason: finishReason},
		},
		Usage: adapter.Usage{
			PromptTokens:        native.Usage.InputTokens + native.Usage.CacheReadInputTokens + native.Usage.CacheWriteInputTokens,
			CompletionTokens:    native.Usage.OutputTokens,
			TotalTokens:         native.Usage.TotalTokens + native.Usage.CacheReadInputTokens + native.Usage.CacheWriteInputTokens,
			CacheReadTokens:     native.Usage.CacheReadInputTokens,
			CacheCreationTokens: native.Usage.CacheWriteInputTokens,
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
