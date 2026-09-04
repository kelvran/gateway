package bedrock

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream/eventstreamapi"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// Real Bedrock ConverseStream exception types, confirmed directly against
// aws-sdk-go-v2's own deserializers.go
// (awsRestjson1_deserializeEventStreamExceptionConverseStreamOutput) --
// this is the exact 5-value set ConverseStream itself can send over
// :exception-type, deliberately narrower than the set some sibling
// streaming APIs support (InvokeModelWithBidirectionalStream and
// InvokeModelWithResponseStream also have a modelTimeoutException, which
// ConverseStream's own deserializer does not dispatch on).
var (
	// ErrBedrockThrottled means the request was rate-limited by Bedrock
	// itself -- retryable, ideally with backoff.
	ErrBedrockThrottled = errors.New("bedrock: request throttled")
	// ErrBedrockValidation means the request itself was malformed --
	// never retryable unmodified.
	ErrBedrockValidation = errors.New("bedrock: request validation failed")
	// ErrBedrockServiceUnavailable means a transient Bedrock-side outage
	// -- retryable.
	ErrBedrockServiceUnavailable = errors.New("bedrock: service unavailable")
	// ErrBedrockInternalServer means an unspecified Bedrock-side fault --
	// retryable, though the specific cause is opaque.
	ErrBedrockInternalServer = errors.New("bedrock: internal server error")
	// ErrBedrockModelStreamError means the underlying model itself faulted
	// mid-stream (distinct from a Bedrock-service-level fault) --
	// retryable, but a repeat with the same input may fault again.
	ErrBedrockModelStreamError = errors.New("bedrock: model stream error")
)

// bedrockStreamExceptionError maps a real ConverseStream :exception-type
// header value to one of the typed sentinels above, so callers can
// errors.Is() against a specific category (e.g. a future retry/circuit-
// breaker policy distinguishing throttling from a malformed request)
// rather than only ever seeing one generic, unstructured error string.
// An exception type this decoder doesn't recognize (a future AWS
// addition, or the "error" message-type's own generic RPC-level framing,
// which carries no :exception-type header at all) still produces a real
// error, just not one of the typed sentinels -- forward-compatible, never
// silently dropped.
func bedrockStreamExceptionError(exceptionType string, payload []byte) error {
	switch strings.ToLower(exceptionType) {
	case "throttlingexception":
		return fmt.Errorf("%w: %s", ErrBedrockThrottled, string(payload))
	case "validationexception":
		return fmt.Errorf("%w: %s", ErrBedrockValidation, string(payload))
	case "serviceunavailableexception":
		return fmt.Errorf("%w: %s", ErrBedrockServiceUnavailable, string(payload))
	case "internalserverexception":
		return fmt.Errorf("%w: %s", ErrBedrockInternalServer, string(payload))
	case "modelstreamerrorexception":
		return fmt.Errorf("%w: %s", ErrBedrockModelStreamError, string(payload))
	default:
		return fmt.Errorf("bedrock: upstream stream exception %q: %s", exceptionType, string(payload))
	}
}

// messageStartEvent is Converse/ConverseStream's real "messageStart" event
// payload (confirmed field name against aws-sdk-go-v2's deserializers.go).
type messageStartEvent struct {
	Role string `json:"role"`
}

// contentBlockStartEvent is the real "contentBlockStart" event payload.
// toolUse carries no "input" yet -- that arrives only via subsequent
// contentBlockDelta events.
type contentBlockStartEvent struct {
	ContentBlockIndex int `json:"contentBlockIndex"`
	Start             struct {
		ToolUse *struct {
			ToolUseID string `json:"toolUseId"`
			Name      string `json:"name"`
		} `json:"toolUse,omitempty"`
	} `json:"start"`
}

// contentBlockDeltaEvent is the real "contentBlockDelta" event payload.
// ToolUse.Input is confirmed, against the real Go SDK struct
// (types.ToolUseBlockDelta.Input *string) and the real wire deserializer's
// own string-type assertion, to be an accumulating JSON-string FRAGMENT --
// the same contract as OpenAI's/openaicompat's tool-call arguments, never
// a complete object per chunk the way Gemini's is.
type contentBlockDeltaEvent struct {
	ContentBlockIndex int `json:"contentBlockIndex"`
	Delta             struct {
		Text    string `json:"text,omitempty"`
		ToolUse *struct {
			Input string `json:"input"`
		} `json:"toolUse,omitempty"`
	} `json:"delta"`
}

// messageStopEvent is the real "messageStop" event payload. StopReason
// reuses the exact same real stopReason vocabulary the buffered Converse
// API returns -- finishReasonFromBedrock (in bedrock.go) is reused
// unchanged.
type messageStopEvent struct {
	StopReason string `json:"stopReason"`
}

// metadataEvent is the real "metadata" event payload -- confirmed to
// arrive AFTER messageStop, the last real event before the stream closes.
type metadataEvent struct {
	Usage Usage `json:"usage"`
}

// StreamDecoder decodes Bedrock ConverseStream's real binary
// application/vnd.amazon.eventstream frames into canonical streaming
// chunks. Deliberately stateless (zero fields) -- unlike Anthropic's
// decoder (must track open content-block kinds, since content_block_delta
// doesn't repeat its type) or Gemini's (a sentRole bool), every Bedrock
// event is self-describing: contentBlockDelta's own payload carries
// either "text" or "toolUse" directly, and messageStart is structurally
// guaranteed to fire exactly once, at the true start of the real event
// sequence (messageStart -> [contentBlockStart -> contentBlockDelta* ->
// contentBlockStop]* -> messageStop -> metadata).
//
// Decode never returns a "done" signal (unlike streaming.StreamDecoder) --
// metadata (carrying real usage) is confirmed to arrive AFTER messageStop,
// so signaling done there would drop the final usage event. The binary
// decode loop (dataplane/streaming.go's streamDeploymentBedrock) relies
// purely on eventstream.Decoder.Decode returning io.EOF when the
// transport closes -- the same pattern already proven correct for
// gemini's stream.go, which likewise always returns done=false.
type StreamDecoder struct{}

// NewStreamDecoder returns a fresh StreamDecoder. Since it carries no
// state, every call is equivalent, but a constructor is still provided to
// match every other adapter's own convention.
func NewStreamDecoder() *StreamDecoder {
	return &StreamDecoder{}
}

// Decode implements the binary decode contract streamDeploymentBedrock
// drives directly (not streaming.StreamDecoder's SSEEvent-shaped
// interface -- see docs/rfcs/2026-09-04-bedrock-converse-stream.md's
// Detailed Design for why a shared interface isn't warranted for a
// single binary-framed implementor).
func (d *StreamDecoder) Decode(msg eventstream.Message) ([]streaming.ChatCompletionChunk, *adapter.Usage, error) {
	if v := msg.Headers.Get(eventstreamapi.MessageTypeHeader); v != nil && v.String() != eventstreamapi.EventMessageType {
		if v.String() == eventstreamapi.ExceptionMessageType {
			var exceptionType string
			if et := msg.Headers.Get(eventstreamapi.ExceptionTypeHeader); et != nil {
				exceptionType = et.String()
			}
			return nil, nil, bedrockStreamExceptionError(exceptionType, msg.Payload)
		}
		// The generic "error" message-type -- AWS's RPC-level error
		// framing, which carries no :exception-type header at all, so no
		// per-type sentinel is possible here. Still a real, typed-enough
		// error, never silently dropped.
		return nil, nil, fmt.Errorf("bedrock: upstream stream %s: %s", v.String(), string(msg.Payload))
	}

	var eventType string
	if v := msg.Headers.Get(eventstreamapi.EventTypeHeader); v != nil {
		eventType = v.String()
	}

	switch eventType {
	case "messageStart":
		var ev messageStartEvent
		if err := json.Unmarshal(msg.Payload, &ev); err != nil {
			return nil, nil, fmt.Errorf("bedrock: decoding messageStart: %w", err)
		}
		chunk := streaming.ChatCompletionChunk{
			Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Role: "assistant"}}},
		}
		return []streaming.ChatCompletionChunk{chunk}, nil, nil

	case "contentBlockStart":
		var ev contentBlockStartEvent
		if err := json.Unmarshal(msg.Payload, &ev); err != nil {
			return nil, nil, fmt.Errorf("bedrock: decoding contentBlockStart: %w", err)
		}
		if ev.Start.ToolUse == nil {
			// A text block's start carries no client-visible delta of its
			// own -- the text itself arrives via contentBlockDelta.
			return nil, nil, nil
		}
		chunk := streaming.ChatCompletionChunk{
			Choices: []streaming.ChunkChoice{{
				Index: 0,
				Delta: streaming.MessageDelta{
					ToolCalls: []streaming.ToolCallDelta{
						{Index: ev.ContentBlockIndex, ID: ev.Start.ToolUse.ToolUseID, Name: ev.Start.ToolUse.Name},
					},
				},
			}},
		}
		return []streaming.ChatCompletionChunk{chunk}, nil, nil

	case "contentBlockDelta":
		var ev contentBlockDeltaEvent
		if err := json.Unmarshal(msg.Payload, &ev); err != nil {
			return nil, nil, fmt.Errorf("bedrock: decoding contentBlockDelta: %w", err)
		}
		var delta streaming.MessageDelta
		if ev.Delta.ToolUse != nil {
			delta.ToolCalls = []streaming.ToolCallDelta{
				{Index: ev.ContentBlockIndex, ArgumentsJSON: ev.Delta.ToolUse.Input},
			}
		} else {
			delta.Content = ev.Delta.Text
		}
		chunk := streaming.ChatCompletionChunk{
			Choices: []streaming.ChunkChoice{{Index: 0, Delta: delta}},
		}
		return []streaming.ChatCompletionChunk{chunk}, nil, nil

	case "contentBlockStop":
		// No client-visible content of its own -- purely a boundary marker.
		return nil, nil, nil

	case "messageStop":
		var ev messageStopEvent
		if err := json.Unmarshal(msg.Payload, &ev); err != nil {
			return nil, nil, fmt.Errorf("bedrock: decoding messageStop: %w", err)
		}
		finishReason, err := finishReasonFromBedrock(ev.StopReason)
		if err != nil {
			return nil, nil, err
		}
		chunk := streaming.ChatCompletionChunk{
			Choices: []streaming.ChunkChoice{{Index: 0, FinishReason: &finishReason}},
		}
		return []streaming.ChatCompletionChunk{chunk}, nil, nil

	case "metadata":
		var ev metadataEvent
		if err := json.Unmarshal(msg.Payload, &ev); err != nil {
			return nil, nil, fmt.Errorf("bedrock: decoding metadata: %w", err)
		}
		usage := &adapter.Usage{
			PromptTokens:     ev.Usage.InputTokens,
			CompletionTokens: ev.Usage.OutputTokens,
			TotalTokens:      ev.Usage.TotalTokens,
		}
		return nil, usage, nil

	default:
		// Forward-compatible: an event type this decoder doesn't know
		// about yet produces no client-visible chunk rather than failing
		// the whole stream.
		return nil, nil, nil
	}
}
