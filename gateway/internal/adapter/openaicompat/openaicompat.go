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
// default (vLLM's stop_reason/token_ids/kv_transfer_params). The one thing
// that IS a real, documented caveat: TGI never emits
// finish_reason="tool_calls", even for a genuine tool-call response — see
// Choice's own doc comment.
//
// Reasoning-content field name is genuinely fragmented across this
// package's target runtimes, confirmed against each runtime's actual
// source (not docs) on 2026-09-13: llama.cpp emits "reasoning_content";
// vLLM renamed its own field away from "reasoning_content" to "reasoning"
// (accepting the old name only as a request-side backward-compat alias,
// never emitting it); Ollama's OpenAI-compat layer uses "reasoning"; TGI
// has no reasoning field at all. Message.Reasoning/Message.ReasoningContent
// below carry both wire names so this adapter round-trips whichever one a
// given runtime actually uses, per
// docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md's
// Phase 5.
package openaicompat

import (
	"encoding/json"
	"fmt"
	"strings"

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
	// ResponseFormat mirrors real OpenAI's own response_format wire shape
	// exactly (see internal/adapter/openai.Request.ResponseFormat's doc
	// comment), per this package's own "near-verbatim copy" convention.
	// Fidelity caveat, not silently assumed: unlike the OpenAI-hosted API, actual
	// grammar-backed ENFORCEMENT of the schema varies by self-hosted
	// backend (vLLM/TGI/Ollama/llama.cpp/LocalAI each implement their own
	// constrained-decoding support, at varying completeness) --
	// SupportsStructuredOutput("openaicompat", ...) is unconditionally
	// true purely on wire-shape-acceptance grounds, the same "wire shape
	// matches, enforcement quality is the operator's own responsibility"
	// stance this package's doc comment already takes for tool-calling.
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

// StreamOptions is the native streaming-configuration object.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ResponseFormat mirrors internal/adapter/openai.ResponseFormat exactly.
type ResponseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *JSONSchema `json:"json_schema,omitempty"`
}

// JSONSchema mirrors internal/adapter/openai.JSONSchema exactly.
type JSONSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict,omitempty"`
	Schema json.RawMessage `json:"schema"`
}

// Message is the native message shape. Content is json.RawMessage, not
// string — see internal/adapter/openai.Message's own doc comment for
// why, per docs/rfcs/2026-09-06-gateway-multimodal-content.md; this
// package mirrors that exact design, matching its own stated "near-
// verbatim copy" convention.
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	// Refusal mirrors internal/adapter/openai.Message.Refusal exactly --
	// see that copy's doc comment for the real, confirmed OpenAI field
	// shape this near-verbatim copy targets. Whether a given self-hosted
	// runtime's own structured-outputs implementation actually populates
	// this field is runtime-dependent and not independently verified
	// against a live runtime by this codebase (the same disclosed caveat
	// Usage.PromptTokensDetails' own doc comment already carries) -- it
	// stays empty, harmlessly, for any runtime that never sends it.
	Refusal string `json:"refusal,omitempty"`
	// Reasoning and ReasoningContent carry the same canonical reasoning text
	// under the two real, currently-live wire field names this package's
	// target runtimes use -- see the package doc comment. Both are written
	// on ToProvider (harmless for a runtime that only recognizes one name,
	// mirroring vLLM's own precedent of populating both keys from one value
	// when replaying reasoning history back into its chat template) and
	// read on FromProvider (Reasoning preferred, since every runtime except
	// llama.cpp has migrated to that name).
	Reasoning        string `json:"reasoning,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// nativeContentPart is one element of the native multi-modal content
// array.
type nativeContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *nativeImageURL `json:"image_url,omitempty"`
}

// nativeImageURL is the native image_url object — url is either a real
// URL or a base64 "data:" URI.
type nativeImageURL struct {
	URL string `json:"url"`
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

// Usage is the native token-accounting shape. PromptTokensDetails mirrors
// real OpenAI's own prompt_tokens_details.cached_tokens field (see
// internal/adapter/openai's identical struct) — whether a given
// self-hosted runtime actually populates it is runtime-dependent, and
// unlike OpenAI's own spec, this is NOT independently verified against a
// live runtime by this codebase; it stays 0 (unchanged from before this
// field existed) for any runtime that never sends it, harmlessly.
//
// A source-level survey (docs/upgrade-research/cache-provider-native-caching-audit-round4-2026-09-11.md,
// Finding 2) found the real support split is uneven across the runtimes this
// adapter targets: vLLM, Ollama, and llama.cpp genuinely wire a real
// prefix/KV-cache-hit count into this exact field — vLLM only behind a
// default-off server flag (`enable_prompt_tokens_details`); Ollama only via
// its legacy llama.cpp-backed and experimental MLX runners, not its newer
// pure-Go engine path; llama.cpp natively, from `n_prompt_tokens_cache`. TGI
// has zero cache-token support anywhere in its own response types — this
// field will simply never appear for a TGI-backed deployment. This is a
// real, source-grounded claim about upstream behavior, not a fabricated one
// — but it is still an assertion about code this codebase doesn't run
// itself, not a live-verified guarantee the way OpenAI's spec is.
type Usage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

// PromptTokensDetails is the breakdown of Usage.PromptTokens.
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// cacheReadTokensFromUsage returns native's cached-token count, or 0 when
// PromptTokensDetails is absent (the overwhelming majority of self-hosted
// runtimes today) — never a nil-pointer panic on the common case.
func cacheReadTokensFromUsage(native Usage) int {
	if native.PromptTokensDetails == nil {
		return 0
	}
	return native.PromptTokensDetails.CachedTokens
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
		content, err := contentToNative(m.Content, m.Parts)
		if err != nil {
			return nil, fmt.Errorf("openaicompat: converting message content: %w", err)
		}
		reasoning, reasoningContent := reasoningWireFromCanonical(m.ReasoningBlocks)
		messages = append(messages, Message{
			Role:             m.Role,
			Content:          content,
			ToolCalls:        toolCalls,
			ToolCallID:       m.ToolCallID,
			Reasoning:        reasoning,
			ReasoningContent: reasoningContent,
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
		Model:          req.Model,
		Messages:       messages,
		Temperature:    req.Temperature,
		MaxTokens:      req.MaxTokens,
		Tools:          tools,
		Stream:         req.Stream,
		StreamOptions:  streamOpts,
		ResponseFormat: responseFormatToProvider(req.ResponseFormat),
	}, nil
}

// responseFormatToProvider mirrors internal/adapter/openai's identically
// named helper exactly -- see that copy's doc comment.
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
		content, err := contentFromNative(c.Message.Content)
		if err != nil {
			return adapter.ChatResponse{}, fmt.Errorf("openaicompat: converting choice content: %w", err)
		}
		reasoningBlocks := reasoningBlocksFromNative(c.Message)
		choices = append(choices, adapter.Choice{
			Index: c.Index,
			Message: adapter.Message{
				Role:            c.Message.Role,
				Content:         content,
				ToolCalls:       toolCalls,
				ToolCallID:      c.Message.ToolCallID,
				Refusal:         c.Message.Refusal,
				ReasoningBlocks: reasoningBlocks,
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
			CacheReadTokens:  cacheReadTokensFromUsage(native.Usage),
		},
	}, nil
}

// contentToNative builds Message.Content's wire value from the
// canonical Content/Parts pair — see
// internal/adapter/openai.contentToNative's own doc comment for the
// full rationale; this package mirrors it exactly. Self-hosted
// OpenAI-compatible runtimes are assumed to follow the same "content is
// either a string or an array" convention as real OpenAI, matching
// this package's own stated near-verbatim-compatibility design; no
// document content-part type is assumed to exist.
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
				return nil, fmt.Errorf("openaicompat: image part has neither Data nor URL set")
			}
			native = append(native, nativeContentPart{Type: "image_url", ImageURL: &nativeImageURL{URL: url}})
		case "document":
			return nil, fmt.Errorf("openaicompat: document content parts are not supported")
		default:
			return nil, fmt.Errorf("openaicompat: unsupported content part type %q", p.Type)
		}
	}
	return json.Marshal(native)
}

// contentFromNative decodes Message.Content's wire value back into a
// plain string — see internal/adapter/openai.contentFromNative's own
// doc comment; this package mirrors it exactly.
func contentFromNative(native json.RawMessage) (string, error) {
	if len(native) == 0 {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(native, &text); err != nil {
		return "", fmt.Errorf("openaicompat: response content is not a plain string (multi-modal response content is not supported): %w", err)
	}
	return text, nil
}

// reasoningWireFromCanonical concatenates every plaintext (non-Redacted)
// ReasoningBlock's Text and returns it under BOTH real, currently-live
// wire field names this package's target runtimes use: "reasoning"
// (vLLM, Ollama) and "reasoning_content" (llama.cpp) -- see Message's
// doc comment. Mirrors vLLM's own precedent for the reverse direction
// (chat_utils.py populates both keys from one value); harmless for any
// runtime that only recognizes one name. Redacted blocks are skipped --
// no self-hosted runtime here produces or accepts ciphertext reasoning.
func reasoningWireFromCanonical(rbs []adapter.ReasoningBlock) (reasoning, reasoningContent string) {
	var sb strings.Builder
	for _, rb := range rbs {
		if rb.Redacted {
			continue
		}
		sb.WriteString(rb.Text)
	}
	text := sb.String()
	return text, text
}

// reasoningBlocksFromNative reads whichever of Reasoning/ReasoningContent
// the runtime populated (Reasoning preferred, since every runtime except
// llama.cpp has migrated to that name) and, when non-empty, returns a
// single canonical ReasoningBlock at Sequence 0 -- the only ordering this
// flat wire shape can represent (no interleaving signal exists, unlike
// Anthropic/Bedrock/Gemini's typed block arrays). Neither field set
// returns nil, byte-identical to today's behavior.
func reasoningBlocksFromNative(m Message) []adapter.ReasoningBlock {
	text := m.Reasoning
	if text == "" {
		text = m.ReasoningContent
	}
	if text == "" {
		return nil
	}
	return []adapter.ReasoningBlock{{Sequence: 0, Text: text}}
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
