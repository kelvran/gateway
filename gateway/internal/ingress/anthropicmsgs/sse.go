package anthropicmsgs

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// SSEEncoder renders canonical chunks as Anthropic's event stream (RFC-1
// §7): message_start once, then content_block_start/delta/stop per block --
// a text block with text_delta, a tool_use block with input_json_delta, a
// thinking block with thinking_delta then signature_delta, a
// redacted_thinking block carrying its data on start -- then, on WriteDone,
// message_delta with the stop reason and the usage, and message_stop. The
// current block is closed when a different one starts, as Anthropic's own
// streams do. Text that arrives while a tool_use block is still receiving
// fragments is held (up to maxPendingTextBytes) and emitted as its own
// block once that block closes;
// a tool fragment for a block that already closed opens a continuation
// block carrying the original id and name (no provider interleaves blocks
// today, so both are defensive paths).
// message_start's usage is the only shape it can be before the provider
// reports anything (zeros); message_delta carries the totals. Ping emits a
// ping event (the caller owns the timer: S8 wires it); WriteError emits an
// error event, the clean-end rule for a stream that must abort after a block
// started. Not safe for concurrent use.
type SSEEncoder struct {
	w        io.Writer
	started  bool
	next     int    // the next Anthropic block index
	open     *block // the block currently open, if any
	textOpen bool   // whether open is the text block
	tools    map[int]*block
	toolMeta map[int]toolIdentity // id and name per canonical tool index, for a continuation block
	thinking map[int]*block
	pending  strings.Builder // text held back while a tool_use block is still open
	stop     *string         // the stop_reason once a finish chunk arrived
	stopSeq  string
	usage    *adapter.Usage
}

type block struct {
	index  int
	kind   string // "text", "tool_use", "thinking", "redacted_thinking"
	closed bool
}

type toolIdentity struct{ id, name string }

// Event payloads. Structs, not maps, so the member order is Anthropic's
// (type first) and byte-for-byte stable.
type messageStartEvent struct {
	Type    string           `json:"type"`
	Message messageStartWire `json:"message"`
}

type messageStartWire struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []blockWire    `json:"content"`
	StopReason   *string        `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        startUsageWire `json:"usage"`
}

type startUsageWire struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type blockStartEvent struct {
	Type         string         `json:"type"`
	Index        int            `json:"index"`
	ContentBlock startBlockWire `json:"content_block"`
}

// startBlockWire is the empty block a content_block_start announces; the
// pointer members are emitted as "" when set, the way Anthropic does.
type startBlockWire struct {
	Type      string          `json:"type"`
	Text      *string         `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Thinking  *string         `json:"thinking,omitempty"`
	Signature *string         `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
}

type blockDeltaEvent struct {
	Type  string    `json:"type"`
	Index int       `json:"index"`
	Delta deltaWire `json:"delta"`
}

// deltaWire is one of text_delta, input_json_delta, thinking_delta or
// signature_delta; only non-empty deltas are ever emitted, so omitempty
// cannot drop a meaningful member.
type deltaWire struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
}

type blockStopEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type messageDeltaEvent struct {
	Type  string           `json:"type"`
	Delta messageDeltaWire `json:"delta"`
	Usage usageWire        `json:"usage"`
}

type messageDeltaWire struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type typeOnlyEvent struct {
	Type string `json:"type"`
}

// maxPendingTextBytes bounds the text held behind an open tool_use block.
// The dataplane's runaway ceiling already bounds all streamed text, so this
// is defense in depth: past it the tool block is closed early and the text
// emitted, and a later fragment for that tool opens a continuation block
// (identity kept) -- a shape only a misbehaving upstream can produce.
const maxPendingTextBytes = 1 << 20

// NewSSEEncoder writes Anthropic events to w.
func NewSSEEncoder(w io.Writer) *SSEEncoder {
	return &SSEEncoder{w: w, tools: map[int]*block{}, toolMeta: map[int]toolIdentity{}, thinking: map[int]*block{}}
}

func (e *SSEEncoder) event(name string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("anthropicmsgs: encoding %s: %w", name, err)
	}
	_, err = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", name, data)
	return err
}

func (e *SSEEncoder) start(c streaming.ChatCompletionChunk) error {
	if e.started {
		return nil
	}
	e.started = true
	return e.event("message_start", messageStartEvent{Type: "message_start", Message: messageStartWire{
		ID: c.ID, Type: "message", Role: "assistant", Model: c.Model, Content: []blockWire{},
	}})
}

func (e *SSEEncoder) closeOpen() error {
	if e.open == nil || e.open.closed {
		return nil
	}
	e.open.closed = true
	err := e.event("content_block_stop", blockStopEvent{Type: "content_block_stop", Index: e.open.index})
	e.open = nil
	e.textOpen = false
	return err
}

// openBlock closes the current block, emits any text deferred behind it,
// and announces a new one.
func (e *SSEEncoder) openBlock(start startBlockWire) (*block, error) {
	if err := e.closeOpen(); err != nil {
		return nil, err
	}
	if start.Type != "text" {
		if err := e.flushPendingText(); err != nil {
			return nil, err
		}
	}
	return e.openBlockNow(start)
}

// openBlockNow announces a block with nothing in between.
func (e *SSEEncoder) openBlockNow(start startBlockWire) (*block, error) {
	b := &block{index: e.next, kind: start.Type}
	e.next++
	e.open = b
	e.textOpen = start.Type == "text"
	return b, e.event("content_block_start", blockStartEvent{Type: "content_block_start", Index: b.index, ContentBlock: start})
}

// flushPendingText emits text that arrived while a tool_use block was open
// as its own block, now that the tool block has closed.
func (e *SSEEncoder) flushPendingText() error {
	if e.pending.Len() == 0 {
		return nil
	}
	text := e.pending.String()
	e.pending.Reset()
	b, err := e.openBlockNow(startBlockWire{Type: "text", Text: emptyString()})
	if err != nil {
		return err
	}
	if err := e.delta(b, deltaWire{Type: "text_delta", Text: text}); err != nil {
		return err
	}
	return e.closeOpen()
}

func (e *SSEEncoder) delta(b *block, d deltaWire) error {
	return e.event("content_block_delta", blockDeltaEvent{Type: "content_block_delta", Index: b.index, Delta: d})
}

func emptyString() *string {
	s := ""
	return &s
}

// WriteChunk renders one canonical chunk's deltas.
func (e *SSEEncoder) WriteChunk(c streaming.ChatCompletionChunk) error {
	if err := e.start(c); err != nil {
		return err
	}
	if c.Usage != nil {
		u := *c.Usage
		e.usage = &u
	}
	for _, choice := range c.Choices {
		if choice.Index != 0 {
			continue
		}
		if err := e.writeReasoning(choice.Delta.ReasoningBlocks); err != nil {
			return err
		}
		if choice.Delta.Content != "" {
			if err := e.writeText(choice.Delta.Content); err != nil {
				return err
			}
		}
		if err := e.writeToolCalls(choice.Delta.ToolCalls); err != nil {
			return err
		}
		if choice.FinishReason != nil {
			stop := choice.StopReason
			if stop == "" {
				stop = stopReasonFor(adapter.ChatResponse{Choices: []adapter.Choice{{FinishReason: *choice.FinishReason}}})
			}
			e.stop = &stop
			e.stopSeq = choice.StopSequence
		}
	}
	return nil
}

// writeText appends to the open text block, opens one, or -- while a
// tool_use block is still receiving input_json_delta fragments -- holds the
// text until that block closes, since Anthropic blocks are sequential and
// closing the tool block early would strand its remaining fragments.
func (e *SSEEncoder) writeText(text string) error {
	if e.open != nil && !e.open.closed && e.open.kind == "tool_use" {
		if e.pending.Len()+len(text) <= maxPendingTextBytes {
			e.pending.WriteString(text)
			return nil
		}
		// Past the cap: close the tool block, emit what was held, and let
		// the text through as a normal block from here on.
		if err := e.closeOpen(); err != nil {
			return err
		}
		if err := e.flushPendingText(); err != nil {
			return err
		}
	}
	if !e.textOpen {
		if _, err := e.openBlock(startBlockWire{Type: "text", Text: emptyString()}); err != nil {
			return err
		}
	}
	return e.delta(e.open, deltaWire{Type: "text_delta", Text: text})
}

func (e *SSEEncoder) writeReasoning(deltas []streaming.ReasoningDelta) error {
	for _, rd := range deltas {
		b := e.thinking[rd.Index]
		if b == nil || b.closed || e.open != b {
			start := startBlockWire{Type: "thinking", Thinking: emptyString(), Signature: emptyString()}
			if rd.Redacted {
				start = startBlockWire{Type: "redacted_thinking", Data: rd.Data}
			}
			nb, err := e.openBlock(start)
			if err != nil {
				return err
			}
			e.thinking[rd.Index] = nb
			b = nb
		}
		if rd.Redacted {
			continue // the data travelled on the start; redacted blocks have no deltas
		}
		if rd.Text != "" {
			if err := e.delta(b, deltaWire{Type: "thinking_delta", Thinking: rd.Text}); err != nil {
				return err
			}
		}
		if rd.Signature != "" {
			if err := e.delta(b, deltaWire{Type: "signature_delta", Signature: rd.Signature}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *SSEEncoder) writeToolCalls(deltas []streaming.ToolCallDelta) error {
	for _, td := range deltas {
		meta := e.toolMeta[td.Index]
		if td.ID != "" {
			meta.id = td.ID
		}
		if td.Name != "" {
			meta.name = td.Name
		}
		e.toolMeta[td.Index] = meta
		b := e.tools[td.Index]
		if b == nil || b.closed || e.open != b {
			// A fragment for a block that already closed (no provider
			// interleaves tool calls today) opens a continuation block that
			// keeps the original id and name rather than an anonymous one.
			nb, err := e.openBlock(startBlockWire{Type: "tool_use", ID: meta.id, Name: meta.name, Input: json.RawMessage("{}")})
			if err != nil {
				return err
			}
			e.tools[td.Index] = nb
			b = nb
		}
		if td.ArgumentsJSON != "" {
			if err := e.delta(b, deltaWire{Type: "input_json_delta", PartialJSON: td.ArgumentsJSON}); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteDone closes the open block and ends the message: message_delta with
// the stop reason (end_turn when no finish chunk arrived) and the usage
// totals, then message_stop.
func (e *SSEEncoder) WriteDone() error {
	if err := e.start(streaming.ChatCompletionChunk{}); err != nil {
		return err
	}
	if err := e.closeOpen(); err != nil {
		return err
	}
	if err := e.flushPendingText(); err != nil {
		return err
	}
	stop := "end_turn"
	if e.stop != nil {
		stop = *e.stop
	}
	var stopSeq *string
	if e.stopSeq != "" {
		s := e.stopSeq
		stopSeq = &s
	}
	usage := usageWire{}
	if e.usage != nil {
		usage = usageFor(*e.usage)
	}
	if err := e.event("message_delta", messageDeltaEvent{
		Type:  "message_delta",
		Delta: messageDeltaWire{StopReason: stop, StopSequence: stopSeq},
		Usage: usage,
	}); err != nil {
		return err
	}
	return e.event("message_stop", typeOnlyEvent{Type: "message_stop"})
}

// Ping emits Anthropic's keepalive event; the caller decides when (RFC-1 §7:
// 15 s of silence, a timer slice S8 wires).
func (e *SSEEncoder) Ping() error {
	return e.event("ping", typeOnlyEvent{Type: "ping"})
}

// WriteError emits an Anthropic error event so a stream that must abort
// after a block started never ends cleanly (the protocol reads a clean end
// before message_delta as a dropped connection).
func (e *SSEEncoder) WriteError(errType, message string) error {
	return e.event("error", errorWire{Type: "error", Error: errorBodyWire{Type: errType, Message: message}})
}
