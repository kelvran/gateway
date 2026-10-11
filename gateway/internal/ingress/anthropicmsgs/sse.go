package anthropicmsgs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

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
// reports anything (zeros); message_delta carries the totals. KeepAlive and
// StartKeepAlive emit pings during silence; WriteError emits an error event,
// the clean-end rule for a stream that must abort after a block started, and
// is terminal like message_stop. Every write holds one mutex, so KeepAlive's goroutine can emit a
// ping between the caller's writes without ever splitting an event.
//
// On RFC-1 §9's passthrough path (item 11 slice S11b2) the encoder is also
// the streaming.RawRelay: WriteRaw relays an anthropic deployment's own
// frames as read and mirrors their meaning (started, the open block index,
// a relayed message_delta, the terminal frames), so WriteFinish can still
// end a cut stream and WriteError adds nothing after the upstream's own end.
type SSEEncoder struct {
	mu       sync.Mutex
	writes   uint64 // events written, read by KeepAlive to detect silence
	ticks    uint64 // KeepAlive ticks handled (tests synchronise on it)
	w        io.Writer
	flusher  http.Flusher // w's Flush, when it has one: every event is flushed
	started  bool         // message_start reached the writer
	finished bool         // message_stop or an error event was written; nothing follows
	next     int          // the next Anthropic block index
	open     *block       // the block currently open, if any
	textOpen bool         // whether open is the text block
	tools    map[int]*block
	toolMeta map[int]toolIdentity // id and name per canonical tool index, for a continuation block
	thinking map[int]*block
	pending  strings.Builder // text held back while a tool_use block is still open
	stop     *string         // the stop_reason once a finish chunk arrived
	stopSeq  string
	usage    *adapter.Usage
	// relayedDelta is set when the upstream's own message_delta was relayed
	// (WriteRaw): a later finish then adds message_stop alone.
	relayedDelta bool
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

// SSEEncoder satisfies streaming.ChunkSink and streaming.Finisher: the
// dataplane writes chunks through WriteChunk and ends the message with
// WriteFinish (final usage, truncation) or, on a cache replay whose chunks
// already carried both, WriteDone.
var (
	_ streaming.ChunkSink = (*SSEEncoder)(nil)
	_ streaming.Finisher  = (*SSEEncoder)(nil)
	_ streaming.RawRelay  = (*SSEEncoder)(nil)
)

// NewSSEEncoder writes Anthropic events to w, flushing after every event
// when w is an http.Flusher (net/http buffers 2 KiB before the chunked
// writer, so an unflushed ping or delta sits unsent and defeats a client's
// stall detection). The route hands it the response writer directly.
func NewSSEEncoder(w io.Writer) *SSEEncoder {
	e := &SSEEncoder{w: w, tools: map[int]*block{}, toolMeta: map[int]toolIdentity{}, thinking: map[int]*block{}}
	if f, ok := w.(http.Flusher); ok {
		e.flusher = f
	}
	return e
}

// event writes one frame under the mutex.
func (e *SSEEncoder) event(name string, payload any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.eventLocked(name, payload)
}

// eventLocked writes one frame and flushes it; the caller holds e.mu.
func (e *SSEEncoder) eventLocked(name string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("anthropicmsgs: encoding %s: %w", name, err)
	}
	if _, err := fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return err
	}
	e.writes++ // only a frame that reached the writer counts as activity for KeepAlive
	if e.flusher != nil {
		e.flusher.Flush()
	}
	return nil
}

// start writes message_start once. The check and the write share one
// critical section, and started flips only after the write succeeded, so a
// keepalive tick can never ping ahead of message_start and a failed first
// write is retried by the next chunk (a fallback retry streams through the
// same encoder).
func (e *SSEEncoder) start(c streaming.ChatCompletionChunk) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return nil
	}
	if err := e.eventLocked("message_start", messageStartEvent{Type: "message_start", Message: messageStartWire{
		ID: c.ID, Type: "message", Role: "assistant", Model: c.Model, Content: []blockWire{},
	}}); err != nil {
		return err
	}
	e.started = true
	return nil
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

// errStreamFinished is returned for a chunk written after the message ended.
var errStreamFinished = errors.New("anthropicmsgs: write after the message ended")

// WriteChunk renders one canonical chunk's deltas. A chunk after message_stop
// or an error event is a caller bug and fails loudly (a repeated finish, by
// contrast, is a harmless no-op).
func (e *SSEEncoder) WriteChunk(c streaming.ChatCompletionChunk) error {
	if e.isFinished() {
		return errStreamFinished
	}
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
// the stop reason (end_turn when no finish chunk arrived) and the usage the
// chunks carried, then message_stop. The dataplane's live paths call
// WriteFinish instead, which also knows the final usage and whether the
// upstream was cut off; WriteDone is the streaming.ChunkSink form, right for
// a cache replay whose chunks carry both.
func (e *SSEEncoder) WriteDone() error {
	stop := "end_turn"
	if e.stop != nil {
		stop = *e.stop
	}
	return e.finish(stop)
}

// WriteFinish ends the message with what the dataplane knows at the end of
// a live stream (streaming.Finisher): end.Usage replaces whatever the chunks
// carried (it is the provider's final figure, or the estimate the gateway
// bills by), and end.Truncated -- the runaway ceiling or a reservation
// top-up cut the upstream off, so no finish chunk arrived -- ends the
// message with stop_reason max_tokens, so a guard trip never ends the stream
// with a clean close before message_delta (RFC-1 §7).
func (e *SSEEncoder) WriteFinish(end streaming.StreamEnd) error {
	u := end.Usage
	e.usage = &u
	stop := "end_turn"
	switch {
	case end.Truncated:
		stop = "max_tokens"
	case e.stop != nil:
		stop = *e.stop
	}
	return e.finish(stop)
}

// finish closes the open block, emits any deferred text, then message_delta
// and message_stop. It is terminal: once the message ended (or an error
// event aborted it) a later finish is a no-op, so a second WriteDone or a
// WriteFinish after WriteError never writes a second ending; a later
// WriteChunk fails with errStreamFinished.
func (e *SSEEncoder) finish(stop string) error {
	if e.isFinished() {
		return nil
	}
	if err := e.start(streaming.ChatCompletionChunk{}); err != nil {
		return err
	}
	if err := e.closeOpen(); err != nil {
		return err
	}
	if err := e.flushPendingText(); err != nil {
		return err
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
	e.mu.Lock()
	relayedDelta := e.relayedDelta
	e.mu.Unlock()
	// A relayed message_delta is the client's already; a second one would
	// contradict it, so the ending adds message_stop alone (slice S11b2).
	if !relayedDelta {
		if err := e.event("message_delta", messageDeltaEvent{
			Type:  "message_delta",
			Delta: messageDeltaWire{StopReason: stop, StopSequence: stopSeq},
			Usage: usage,
		}); err != nil {
			return err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.eventLocked("message_stop", typeOnlyEvent{Type: "message_stop"}); err != nil {
		return err
	}
	e.finished = true
	return nil
}

func (e *SSEEncoder) isFinished() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.finished
}

// Ping emits Anthropic's keepalive event, unless the message already ended.
// KeepAlive decides when; a caller may also ping directly.
func (e *SSEEncoder) Ping() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finished {
		return nil
	}
	return e.eventLocked("ping", typeOnlyEvent{Type: "ping"})
}

// KeepAlive emits a ping on every tick that follows a tick with no event
// written in between -- so with a 15 s ticker the client hears from the
// gateway after at most 30 s and at least 15 s of silence (RFC-1 §7: Bedrock
// streams carry no pings of their own, and Claude Code's stall detection
// needs them). Nothing is pinged before message_start. The ticker is the
// caller's (time.NewTicker(15 * time.Second).C in the handler, a manual
// channel in tests), so the idle threshold is the tick period and no clock
// is needed. Returns when ctx is done or ticks is closed; run it in its own
// goroutine beside the writes -- the check and the ping share one critical
// section with every other event write. StartKeepAlive is the packaged
// form with a ticker and a stop that waits for the goroutine.
func (e *SSEEncoder) KeepAlive(ctx context.Context, ticks <-chan time.Time) {
	var seen uint64
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			e.mu.Lock()
			if e.started && !e.finished && e.writes == seen {
				_ = e.eventLocked("ping", typeOnlyEvent{Type: "ping"}) // a write error surfaces on the caller's next write
			}
			seen = e.writes
			e.ticks++
			e.mu.Unlock()
		}
	}
}

// StartKeepAlive runs KeepAlive on its own goroutine with a ticker of the
// given period and returns the function that ends it: cancel, stop the
// ticker, and wait for the goroutine to exit. The route must call stop
// before its handler returns -- net/http forbids touching the ResponseWriter
// after ServeHTTP returns, and a keepalive mid-write would do exactly that.
func (e *SSEEncoder) StartKeepAlive(ctx context.Context, period time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	ticker := time.NewTicker(period)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.KeepAlive(ctx, ticker.C)
	}()
	return func() {
		cancel()
		ticker.Stop()
		<-done
	}
}

// ticksSeen reports how many ticks KeepAlive has handled (tests synchronise
// on it).
func (e *SSEEncoder) ticksSeen() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ticks
}

// writesSeen reports how many events have been written (tests).
func (e *SSEEncoder) writesSeen() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.writes
}

// WriteError emits an Anthropic error event so a stream that must abort
// after a block started never ends cleanly (the protocol reads a clean end
// before message_delta as a dropped connection). It is terminal: no
// message_delta, message_stop or ping follows it -- and after the message
// already ended (message_stop, or a relayed error frame) it writes nothing,
// so the gateway never appends its own error to the upstream's.
func (e *SSEEncoder) WriteError(errType, message string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finished {
		return nil
	}
	e.finished = true
	return e.eventLocked("error", errorWire{Type: "error", Error: errorBodyWire{Type: errType, Message: message}})
}

// RelayHeaders puts the upstream's relayable response headers (the
// dataplane's relayResponseHeaders: x-should-retry and
// anthropic-ratelimit-unified-*) on the response when the writer is the
// response itself, replacing any value already held under the same name. It
// must run before the first frame; a plain io.Writer gets nothing.
func (e *SSEEncoder) RelayHeaders(h http.Header) {
	e.mu.Lock()
	defer e.mu.Unlock()
	hw, ok := e.w.(interface{ Header() http.Header })
	if !ok {
		return
	}
	for name, values := range h {
		hw.Header().Del(name)
		for _, v := range values {
			hw.Header().Add(name, v)
		}
	}
}

// WriteRaw relays one upstream frame exactly as read (streaming.RawRelay,
// RFC-1 §9, item 11 slice S11b2) and mirrors what it means for the message
// so the encoder can still end it itself after a guard cut: message_start
// marks the stream started (the keepalive may ping during upstream silence
// from here on), content_block_start/stop track the upstream's open block
// index for closeOpen, message_delta is remembered so finish adds no second
// one, and message_stop or an error frame ends the message -- nothing is
// written after either, and a frame whose data is not JSON is relayed
// unchanged and leaves the mirror as it was. A relayed frame counts as
// keepalive activity.
func (e *SSEEncoder) WriteRaw(frame []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finished {
		return nil
	}
	if _, err := e.w.Write(frame); err != nil {
		return err
	}
	e.writes++
	if e.flusher != nil {
		e.flusher.Flush()
	}
	kind, index, hasIndex := relayedFrameShape(frame)
	switch kind {
	case "message_start":
		e.started = true
	case "content_block_start":
		if hasIndex {
			e.open = &block{index: index, kind: "relayed"}
			e.textOpen = false
			if index >= e.next {
				e.next = index + 1
			}
		}
	case "content_block_stop":
		// The spec always carries index; closing the open block when it is
		// missing is defensive, so a malformed stop never leaves a block open.
		if e.open != nil && (!hasIndex || e.open.index == index) {
			e.open.closed = true
			e.open = nil
			e.textOpen = false
		}
	case "message_delta":
		e.relayedDelta = true
	case "message_stop", "error":
		e.finished = true
	}
	return nil
}

// relayedFrameShape reads an SSE frame's data payload for Anthropic's type
// and index members, falling back to the event: field name when the data
// carries no type. Anything unparsable yields "" and the mirror stays put.
func relayedFrameShape(frame []byte) (kind string, index int, hasIndex bool) {
	var event string
	var data []string
	for _, line := range strings.Split(string(frame), "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	var shape struct {
		Type  string `json:"type"`
		Index *int   `json:"index"`
	}
	if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &shape); err != nil {
		return "", 0, false
	}
	kind = shape.Type
	if kind == "" {
		kind = event
	}
	if shape.Index != nil {
		return kind, *shape.Index, true
	}
	return kind, 0, false
}
