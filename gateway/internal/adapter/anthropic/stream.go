package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// NewStreamDecoder implements streaming.StreamingAdapter, returning a
// fresh, request-scoped decoder with its own independent state.
//
// This file is the streaming decoder for Anthropic's Messages API.
// Unlike OpenAI's homogeneous chat.completion.chunk stream, Anthropic
// sends a *typed* event sequence (message_start,
// content_block_start/delta/stop, message_delta, message_stop, plus
// keep-alive pings) where a content_block_delta event carries only an
// index and a raw fragment — it does not repeat whether that index is a
// text block or a tool_use block. This implements the stateful
// per-stream parser gateway/ARCHITECTURE.md's "Canonical Schema &
// Provider Adapters" section already documents as a hazard: the decoder
// must remember, across Decode calls, which content-block index is open
// and what kind of block it is, so an input_json_delta fragment lands in
// the right ToolCallDelta.
func (a *Adapter) NewStreamDecoder() streaming.StreamDecoder {
	return &streamDecoder{
		blockKinds: make(map[int]blockKind),
	}
}

// blockKind identifies which kind of content block a given open index
// refers to, so a later content_block_delta event for that index knows
// how to interpret its delta payload without Anthropic having to repeat
// that information on every event.
type blockKind int

const (
	blockKindText blockKind = iota
	blockKindToolUse
)

// streamDecoder implements streaming.StreamDecoder for one in-flight
// Anthropic streaming request. It is stateful by design (see the package
// doc above) and must never be shared or reused across requests.
type streamDecoder struct {
	// id and model are captured from message_start and stamped onto every
	// canonical chunk this decoder emits — Anthropic only sends them once,
	// at the very start of the stream.
	id    string
	model string
	// sentRole is true once a chunk carrying Delta.Role has already been
	// emitted. Anthropic never sends a role field on its stream events at
	// all (the canonical contract requires Role only on the first chunk of
	// the whole message), so the decoder must track this itself rather
	// than reading it off any single event.
	sentRole bool
	// blockKinds maps an open content block's index to its kind. Entries
	// are added on content_block_start and removed on content_block_stop,
	// so a block index can be legitimately reused later in the same
	// stream once closed (Anthropic does not guarantee otherwise, and this
	// decoder must not assume it).
	blockKinds map[int]blockKind
	// inputTokens is captured from message_start's usage field.
	// Anthropic splits usage across two separate events — input tokens at
	// message_start, output tokens at message_delta — so the decoder must
	// remember the first half in order to hand back one complete
	// adapter.Usage when the second half arrives.
	inputTokens int
}

// rawEnvelope is unmarshaled first, for every event, purely to read the
// "type" discriminator before parsing the event-specific shape.
type rawEnvelope struct {
	Type string `json:"type"`
}

type rawMessageStart struct {
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type rawContentBlockStart struct {
	Index        int `json:"index"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id,omitempty"`
		Name string `json:"name,omitempty"`
	} `json:"content_block"`
}

type rawContentBlockDelta struct {
	Index int `json:"index"`
	Delta struct {
		Type string `json:"type"`
		// Text carries the fragment for a text_delta.
		Text string `json:"text,omitempty"`
		// PartialJSON carries the fragment for an input_json_delta —
		// Anthropic's own field name for a raw JSON-string chunk of a
		// tool call's arguments, still assembling.
		PartialJSON string `json:"partial_json,omitempty"`
	} `json:"delta"`
}

type rawContentBlockStop struct {
	Index int `json:"index"`
}

type rawMessageDelta struct {
	Delta struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// finishReasonFromStopReason maps Anthropic's stop_reason vocabulary onto
// the canonical (OpenAI-shaped) finish_reason vocabulary, since the
// canonical schema is OpenAI Chat-Completions-shaped per
// gateway/ARCHITECTURE.md and a client reading finish_reason should never
// need to know which upstream provider actually served the request.
func finishReasonFromStopReason(stopReason string) string {
	switch stopReason {
	case "end_turn", "stop_sequence", "pause_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	case "":
		return ""
	default:
		// Forward-compatible: an Anthropic stop_reason this mapping
		// doesn't know about yet is passed through verbatim rather than
		// silently dropped — a client sees *something* rather than an
		// empty finish_reason on a completed turn.
		return stopReason
	}
}

type rawStreamError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Decode implements streaming.StreamDecoder.
func (d *streamDecoder) Decode(raw streaming.SSEEvent) ([]streaming.ChatCompletionChunk, bool, *adapter.Usage, error) {
	var env rawEnvelope
	if err := json.Unmarshal([]byte(raw.Data), &env); err != nil {
		return nil, false, nil, fmt.Errorf("anthropic: decoding SSE event %q payload: %w", raw.Event, err)
	}

	switch env.Type {
	case "message_start":
		return d.decodeMessageStart(raw.Data)
	case "ping":
		// Keep-alive only — no client-visible chunk, no state change.
		return nil, false, nil, nil
	case "content_block_start":
		return d.decodeContentBlockStart(raw.Data)
	case "content_block_delta":
		return d.decodeContentBlockDelta(raw.Data)
	case "content_block_stop":
		return d.decodeContentBlockStop(raw.Data)
	case "message_delta":
		return d.decodeMessageDelta(raw.Data)
	case "message_stop":
		return nil, true, nil, nil
	case "error":
		return d.decodeError(raw.Data)
	default:
		// Forward-compatible: an event type this decoder doesn't know
		// about yet produces no client-visible chunk rather than failing
		// the whole stream.
		return nil, false, nil, nil
	}
}

func (d *streamDecoder) decodeMessageStart(data string) ([]streaming.ChatCompletionChunk, bool, *adapter.Usage, error) {
	var m rawMessageStart
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		return nil, false, nil, fmt.Errorf("anthropic: decoding message_start: %w", err)
	}
	d.id = m.Message.ID
	d.model = m.Message.Model
	d.inputTokens = m.Message.Usage.InputTokens
	return nil, false, nil, nil
}

func (d *streamDecoder) decodeContentBlockStart(data string) ([]streaming.ChatCompletionChunk, bool, *adapter.Usage, error) {
	var b rawContentBlockStart
	if err := json.Unmarshal([]byte(data), &b); err != nil {
		return nil, false, nil, fmt.Errorf("anthropic: decoding content_block_start: %w", err)
	}

	var delta streaming.MessageDelta
	if !d.sentRole {
		delta.Role = "assistant"
		d.sentRole = true
	}

	switch b.ContentBlock.Type {
	case "text":
		d.blockKinds[b.Index] = blockKindText
		// Content stays empty: content_block_start for a text block
		// carries no text of its own — the fragments arrive on the
		// following content_block_delta events.
	case "tool_use":
		d.blockKinds[b.Index] = blockKindToolUse
		delta.ToolCalls = []streaming.ToolCallDelta{
			{Index: b.Index, ID: b.ContentBlock.ID, Name: b.ContentBlock.Name},
		}
	default:
		// An unrecognized content-block type: remember it as a plain text
		// block (the safest default for later deltas) but otherwise stay
		// forward-compatible rather than erroring the whole stream.
		d.blockKinds[b.Index] = blockKindText
	}

	return []streaming.ChatCompletionChunk{d.chunk(delta, nil)}, false, nil, nil
}

func (d *streamDecoder) decodeContentBlockDelta(data string) ([]streaming.ChatCompletionChunk, bool, *adapter.Usage, error) {
	var b rawContentBlockDelta
	if err := json.Unmarshal([]byte(data), &b); err != nil {
		return nil, false, nil, fmt.Errorf("anthropic: decoding content_block_delta: %w", err)
	}

	var delta streaming.MessageDelta
	switch d.blockKinds[b.Index] {
	case blockKindToolUse:
		// The fragment accumulates against this specific block's index —
		// this is the crux of the statefulness this decoder exists for:
		// with two tool_use blocks open at different indices, each
		// input_json_delta must land in the ToolCallDelta whose Index
		// matches the block it actually belongs to, never mixed together.
		delta.ToolCalls = []streaming.ToolCallDelta{
			{Index: b.Index, ArgumentsJSON: b.Delta.PartialJSON},
		}
	default: // blockKindText, or an unknown index defaulting to it
		delta.Content = b.Delta.Text
	}

	return []streaming.ChatCompletionChunk{d.chunk(delta, nil)}, false, nil, nil
}

func (d *streamDecoder) decodeContentBlockStop(data string) ([]streaming.ChatCompletionChunk, bool, *adapter.Usage, error) {
	var b rawContentBlockStop
	if err := json.Unmarshal([]byte(data), &b); err != nil {
		return nil, false, nil, fmt.Errorf("anthropic: decoding content_block_stop: %w", err)
	}
	// The block is no longer open — free its slot so the index can be
	// correctly reinterpreted if Anthropic ever reuses it later in the
	// same stream.
	delete(d.blockKinds, b.Index)
	// No client-visible delta: content_block_stop carries no content of
	// its own, only bookkeeping this decoder needs internally.
	return []streaming.ChatCompletionChunk{}, false, nil, nil
}

func (d *streamDecoder) decodeMessageDelta(data string) ([]streaming.ChatCompletionChunk, bool, *adapter.Usage, error) {
	var m rawMessageDelta
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		return nil, false, nil, fmt.Errorf("anthropic: decoding message_delta: %w", err)
	}
	usage := &adapter.Usage{
		PromptTokens:     d.inputTokens,
		CompletionTokens: m.Usage.OutputTokens,
		TotalTokens:      d.inputTokens + m.Usage.OutputTokens,
	}
	// message_delta is the ONLY event that ever carries stop_reason — a
	// chunk reconstructing the full canonical response (for cache
	// write-back, per gateway/ARCHITECTURE.md's dataplane) needs
	// FinishReason set somewhere, or it silently comes back empty on
	// every Anthropic streamed response, which this chunk exists
	// specifically to prevent.
	finishReason := finishReasonFromStopReason(m.Delta.StopReason)
	chunk := d.chunk(streaming.MessageDelta{}, &finishReason)
	return []streaming.ChatCompletionChunk{chunk}, false, usage, nil
}

func (d *streamDecoder) decodeError(data string) ([]streaming.ChatCompletionChunk, bool, *adapter.Usage, error) {
	var e rawStreamError
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		return nil, false, nil, fmt.Errorf("anthropic: decoding error event: %w", err)
	}
	return nil, false, nil, fmt.Errorf("anthropic: upstream stream error (%s): %s", e.Error.Type, e.Error.Message)
}

// chunk builds a canonical chunk carrying delta, stamped with this
// decoder's captured id/model (see message_start).
func (d *streamDecoder) chunk(delta streaming.MessageDelta, finishReason *string) streaming.ChatCompletionChunk {
	return streaming.ChatCompletionChunk{
		ID:    d.id,
		Model: d.model,
		Choices: []streaming.ChunkChoice{
			{Index: 0, Delta: delta, FinishReason: finishReason},
		},
	}
}
