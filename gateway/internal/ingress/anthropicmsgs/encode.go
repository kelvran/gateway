package anthropicmsgs

import (
	"encoding/json"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// messageWire is Anthropic's Message response shape.
type messageWire struct {
	ID                   string                        `json:"id"`
	Type                 string                        `json:"type"`
	Role                 string                        `json:"role"`
	Model                string                        `json:"model"`
	Content              []blockWire                   `json:"content"`
	StopReason           string                        `json:"stop_reason"`
	StopSequence         *string                       `json:"stop_sequence"`
	Usage                usageWire                     `json:"usage"`
	InputTransformations []adapter.InputTransformation `json:"input_transformations,omitempty"`
}

// blockWire is one response content block: text, tool_use, thinking or
// redacted_thinking.
type blockWire struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
}

// usageWire is Anthropic's usage object. input_tokens is the inverse of the
// adapter's inclusive PromptTokens (which adds the cache tokens in), so the
// three add back up to it; output_tokens_details carries thinking_tokens
// only when the provider reported reasoning tokens.
type usageWire struct {
	InputTokens              int               `json:"input_tokens"`
	OutputTokens             int               `json:"output_tokens"`
	CacheReadInputTokens     int               `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int               `json:"cache_creation_input_tokens"`
	OutputTokensDetails      *outputTokensWire `json:"output_tokens_details,omitempty"`
}

type outputTokensWire struct {
	ThinkingTokens int `json:"thinking_tokens"`
}

// stopReasonFor is the forward table for a canonical finish_reason when the
// response carries no native stop_reason (openai, openaicompat, gemini, a
// cache entry written before S3): stop→end_turn, length→max_tokens,
// tool_calls→tool_use, content_filter→refusal, anything else verbatim --
// the mirror of the anthropic decoder's own forward-compatible default.
func stopReasonFor(resp adapter.ChatResponse) string {
	if resp.StopReason != "" {
		return resp.StopReason
	}
	finish := ""
	if len(resp.Choices) > 0 {
		finish = resp.Choices[0].FinishReason
	}
	switch finish {
	case "stop", "":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return finish
	}
}

// usageFor converts the canonical usage.
func usageFor(u adapter.Usage) usageWire {
	input := u.PromptTokens - u.CacheReadTokens - u.CacheCreationTokens
	if input < 0 {
		input = 0
	}
	w := usageWire{InputTokens: input, OutputTokens: u.CompletionTokens, CacheReadInputTokens: u.CacheReadTokens, CacheCreationInputTokens: u.CacheCreationTokens}
	if u.ReasoningTokens > 0 {
		w.OutputTokensDetails = &outputTokensWire{ThinkingTokens: u.ReasoningTokens}
	}
	return w
}

// contentBlocksFor lays Choices[0].Message out in Anthropic's order using
// the adapter's replay convention: every ReasoningBlock whose Sequence is k
// precedes tool_use k, the text block sits after the Sequence-0 reasoning,
// and reasoning sequenced past the last tool call trails. Signatures and
// redacted data are copied byte-for-byte; a tool_use input that is not valid
// JSON becomes {} rather than a malformed document.
func contentBlocksFor(msg adapter.Message) []blockWire {
	var out []blockWire
	emitReasoning := func(seq int, trailing bool) {
		for _, rb := range msg.ReasoningBlocks {
			if (trailing && rb.Sequence >= seq) || (!trailing && rb.Sequence == seq) {
				if rb.Redacted {
					out = append(out, blockWire{Type: "redacted_thinking", Data: rb.Data})
				} else {
					out = append(out, blockWire{Type: "thinking", Thinking: rb.Text, Signature: rb.Signature})
				}
			}
		}
	}
	emitText := func() {
		if msg.Content != "" {
			out = append(out, blockWire{Type: "text", Text: msg.Content})
		}
	}
	if len(msg.ToolCalls) == 0 {
		emitReasoning(0, true)
		emitText()
		return out
	}
	for k, tc := range msg.ToolCalls {
		emitReasoning(k, false)
		if k == 0 {
			emitText()
		}
		input := json.RawMessage(tc.ArgumentsJSON)
		if !json.Valid(input) || len(input) == 0 {
			input = json.RawMessage("{}")
		}
		out = append(out, blockWire{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
	}
	emitReasoning(len(msg.ToolCalls), true)
	return out
}

// EncodeResponse renders a canonical response as an Anthropic Message
// (RFC-1 §1): id, type "message", role, model, content[] in replay order,
// stop_reason from the native S3 field or the forward table, stop_sequence
// (null when none), usage, and input_transformations when the provider
// reported any. Used for translate hops and cache replays; the passthrough
// path relays the provider's own body.
func EncodeResponse(resp adapter.ChatResponse) []byte {
	var msg adapter.Message
	if len(resp.Choices) > 0 {
		msg = resp.Choices[0].Message
	}
	wire := messageWire{
		ID:                   resp.ID,
		Type:                 "message",
		Role:                 "assistant",
		Model:                resp.Model,
		Content:              contentBlocksFor(msg),
		StopReason:           stopReasonFor(resp),
		Usage:                usageFor(resp.Usage),
		InputTransformations: resp.InputTransformations,
	}
	if wire.Content == nil {
		wire.Content = []blockWire{}
	}
	if resp.StopSequence != "" {
		seq := resp.StopSequence
		wire.StopSequence = &seq
	}
	out, err := json.Marshal(wire)
	if err != nil {
		// Only a non-finite float or an unmarshalable map could fail here, and
		// the wire holds neither; a static envelope is still better than a
		// panic on a response path.
		return []byte(`{"type":"error","error":{"type":"api_error","message":"anthropicmsgs: encoding the response failed"}}`)
	}
	return out
}

// errorWire is Anthropic's error envelope plus Kelvran's code and param.
type errorWire struct {
	Type  string        `json:"type"`
	Error errorBodyWire `json:"error"`
}

type errorBodyWire struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

// EncodeError renders {"type":"error","error":{type,message,code,param}}.
// Anthropic's own envelope has no code or param; Kelvran keeps both (omitted
// when empty) because kelvran connect's model_not_found probe reads
// error.code (internal/cli/connect.go).
func EncodeError(errType, message, code, param string) []byte {
	out, err := json.Marshal(errorWire{Type: "error", Error: errorBodyWire{Type: errType, Message: message, Code: code, Param: param}})
	if err != nil {
		return []byte(`{"type":"error","error":{"type":"api_error","message":"anthropicmsgs: encoding the error failed"}}`)
	}
	return out
}
