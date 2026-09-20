package dataplane

import (
	"sort"
	"strings"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// streamAccumulator folds a sequence of streaming.ChatCompletionChunks back
// into a single canonical adapter.ChatResponse, so a streamed request can
// still be written to cache and cost-accounted exactly like a buffered one
// — streaming to the client and accumulating for cache write-back happen
// from the SAME chunk sequence (a tee), never a choice between the two,
// per docs/rfcs/2026-09-02-streaming-support.md's dataplane wiring design.
type streamAccumulator struct {
	id      string
	model   string
	choices map[int]*accumulatingChoice
	order   []int // choice indices in first-seen order
	// duplicateAfterFinishIndices records every choice Index that
	// received a real delta (content/role/tool-call) AFTER that same
	// index's own finishReason had already been set — see add's own doc
	// comment for why this is detected and recorded, not hard-failed.
	duplicateAfterFinishIndices []int
}

type accumulatingChoice struct {
	role            string
	content         strings.Builder
	toolCalls       map[int]*accumulatingToolCall
	toolCallOrder   []int // tool-call indices in first-seen order, within this choice
	reasoningBlocks map[int]*accumulatingReasoningBlock
	reasoningOrder  []int // reasoning-block indices in first-seen order, within this choice
	finishReason    string
}

type accumulatingToolCall struct {
	id   string
	name string
	args strings.Builder
}

// accumulatingReasoningBlock folds a sequence of streaming.ReasoningDelta
// fragments (all sharing one Index) back into one reconstructed
// adapter.ReasoningBlock, per streaming.ReasoningDelta's own doc comment:
// Text fragments concatenate in arrival order (Anthropic's thinking_delta,
// sent repeatedly), while Redacted/Signature/Data each arrive whole on a
// single delta (or content-block-start event, for a redacted block) —
// never fragmented — so those three are plain last-write-wins fields, not
// builders.
type accumulatingReasoningBlock struct {
	redacted  bool
	text      strings.Builder
	signature string
	data      string
}

func newStreamAccumulator() *streamAccumulator {
	return &streamAccumulator{choices: map[int]*accumulatingChoice{}}
}

// add folds one chunk's content into the accumulator. Chunks may arrive
// with zero choices (a usage-only chunk, e.g. OpenAI's final chunk or
// Anthropic's message_delta translation) — add handles that by simply
// updating id/model and returning, matching every StreamDecoder's
// documented contract that a chunk can legitimately carry no choices.
//
// A chunk whose Index matches an already-seen choice that ALSO already
// has a non-empty finishReason is a real anomaly: a provider bug or a
// corrupted/replayed stream reusing an index for what should be a
// logically distinct block. Detected and RECORDED (see
// duplicateAfterFinishIndices), never hard-failed — the existing
// concatenation behavior below is left unchanged as the safest default
// for this display-only corruption class (matching this codebase's own
// forward-compatible convention for stream anomalies elsewhere); the
// caller (streaming.go's finishStreamedResponse) logs a warning so the
// anomaly is at least visible, closing the SILENT half of the gap.
func (acc *streamAccumulator) add(chunk streaming.ChatCompletionChunk) {
	if chunk.ID != "" {
		acc.id = chunk.ID
	}
	if chunk.Model != "" {
		acc.model = chunk.Model
	}

	for _, cc := range chunk.Choices {
		c, ok := acc.choices[cc.Index]
		if !ok {
			c = &accumulatingChoice{toolCalls: map[int]*accumulatingToolCall{}, reasoningBlocks: map[int]*accumulatingReasoningBlock{}}
			acc.choices[cc.Index] = c
			acc.order = append(acc.order, cc.Index)
		}

		if c.finishReason != "" && (cc.Delta.Content != "" || cc.Delta.Role != "" || len(cc.Delta.ToolCalls) > 0 || len(cc.Delta.ReasoningBlocks) > 0) {
			acc.duplicateAfterFinishIndices = append(acc.duplicateAfterFinishIndices, cc.Index)
		}

		if cc.Delta.Role != "" {
			c.role = cc.Delta.Role
		}
		if cc.Delta.Content != "" {
			c.content.WriteString(cc.Delta.Content)
		}
		if cc.FinishReason != nil && *cc.FinishReason != "" {
			c.finishReason = *cc.FinishReason
		}

		for _, tcd := range cc.Delta.ToolCalls {
			tc, ok := c.toolCalls[tcd.Index]
			if !ok {
				tc = &accumulatingToolCall{}
				c.toolCalls[tcd.Index] = tc
				c.toolCallOrder = append(c.toolCallOrder, tcd.Index)
			}
			if tcd.ID != "" {
				tc.id = tcd.ID
			}
			if tcd.Name != "" {
				tc.name = tcd.Name
			}
			if tcd.ArgumentsJSON != "" {
				tc.args.WriteString(tcd.ArgumentsJSON)
			}
		}

		// See accumulatingReasoningBlock's own doc comment for why Text is
		// concatenated but Redacted/Signature/Data are last-write-wins —
		// each individual field is only written when the delta actually
		// carries it, exactly mirroring the ToolCalls loop above, so a
		// fragment that legitimately carries only one of these fields
		// (e.g. a bare signature_delta) never clobbers what an earlier
		// fragment at the same Index already set.
		for _, rbd := range cc.Delta.ReasoningBlocks {
			rb, ok := c.reasoningBlocks[rbd.Index]
			if !ok {
				rb = &accumulatingReasoningBlock{}
				c.reasoningBlocks[rbd.Index] = rb
				c.reasoningOrder = append(c.reasoningOrder, rbd.Index)
			}
			if rbd.Redacted {
				rb.redacted = true
			}
			if rbd.Text != "" {
				rb.text.WriteString(rbd.Text)
			}
			if rbd.Signature != "" {
				rb.signature = rbd.Signature
			}
			if rbd.Data != "" {
				rb.data = rbd.Data
			}
		}
	}
}

// totalContentLen returns the sum, across every choice folded in so far,
// of that choice's own accumulated content length — the provider-agnostic
// PROXY the mid-stream runaway-completion guard (streaming.go's
// streamDeployment/streamDeploymentBedrock, via streamrunaway.go's
// streamRunawayCharsCeiling) checks against a character-length ceiling,
// since no real token count is available until (if ever) the provider's
// own final usage frame arrives — see streamrunaway.go's package doc for
// why this proxy is used instead of a real token count.
// strings.Builder.Len() is O(1) (length is tracked incrementally as
// content is written, never re-scanned), so calling this once per decoded
// chunk batch in the hot read loop is cheap.
func (acc *streamAccumulator) totalContentLen() int {
	total := 0
	for _, c := range acc.choices {
		total += c.content.Len()
	}
	return total
}

// hasFinishReason reports whether any choice has ever recorded a
// non-empty finish reason -- i.e. a genuine terminal event (OpenAI/
// Anthropic/Gemini's own finish_reason chunk, or Bedrock's messageStop,
// per stream.go's own documented event sequence: "messageStop, the last
// real event before the stream closes") has actually been observed.
// Used by streamDeploymentBedrock to distinguish a clean end-of-stream
// from a mid-frame truncation that happens to also surface as io.EOF --
// see that call site's own doc comment for the real bug this closes.
func (acc *streamAccumulator) hasFinishReason() bool {
	for _, c := range acc.choices {
		if c.finishReason != "" {
			return true
		}
	}
	return false
}

// build reconstructs the canonical ChatResponse from every chunk folded in
// so far via add. Safe to call at most once per accumulator's logical use
// (it does not reset internal state), matching this type's one-request
// lifetime.
func (acc *streamAccumulator) build(usage adapter.Usage) adapter.ChatResponse {
	// Choice order matters for a deterministic cache entry — two identical
	// streamed responses must produce byte-identical cached JSON, or the
	// L1 exact-match cache's own correctness guarantee (deterministic key
	// -> deterministic value) would be undermined by non-deterministic
	// choice ordering.
	order := append([]int(nil), acc.order...)
	sort.Ints(order)

	choices := make([]adapter.Choice, 0, len(order))
	for _, idx := range order {
		c := acc.choices[idx]

		toolCallOrder := append([]int(nil), c.toolCallOrder...)
		sort.Ints(toolCallOrder)
		toolCalls := make([]adapter.ToolCall, 0, len(toolCallOrder))
		for _, tcIdx := range toolCallOrder {
			tc := c.toolCalls[tcIdx]
			toolCalls = append(toolCalls, adapter.ToolCall{
				ID:            tc.id,
				Name:          tc.name,
				ArgumentsJSON: tc.args.String(),
			})
		}

		// reasoningOrder is sorted the same way toolCallOrder is above --
		// every decoder that emits ReasoningDelta.Index (Anthropic/Bedrock's
		// contentBlockIndex, Gemini's part position) uses the exact same
		// per-message position index space for ToolCallDelta.Index, per
		// each of those stream.go's own doc comments read before writing
		// this. That lets Sequence be reconstructed here exactly like every
		// provider's own buffered FromProvider already computes it --
		// "immediately before the next tool_use/functionCall block" --
		// without this provider-agnostic accumulator needing to know which
		// provider actually produced the chunks: count how many tool calls
		// (in final build order) had a lower position index than this
		// reasoning block. openaicompat's decoder hardcodes ReasoningDelta.
		// Index to 0 (it has no per-block index concept at all, per
		// reasoningBlocksFromNative's own doc comment: "the only ordering
		// this model supports" is Sequence 0) -- since no tool-call index
		// can be negative, the count below is always 0 for it too, so the
		// single generic formula also reproduces that provider's own
		// hardcoded convention correctly.
		reasoningOrder := append([]int(nil), c.reasoningOrder...)
		sort.Ints(reasoningOrder)
		var reasoningBlocks []adapter.ReasoningBlock
		if len(reasoningOrder) > 0 {
			toolsBefore := 0
			ti := 0
			for _, rIdx := range reasoningOrder {
				for ti < len(toolCallOrder) && toolCallOrder[ti] < rIdx {
					toolsBefore++
					ti++
				}
				rb := c.reasoningBlocks[rIdx]
				reasoningBlocks = append(reasoningBlocks, adapter.ReasoningBlock{
					Sequence:  toolsBefore,
					Redacted:  rb.redacted,
					Text:      rb.text.String(),
					Signature: rb.signature,
					Data:      rb.data,
				})
			}
		}

		role := c.role
		if role == "" {
			role = "assistant"
		}

		choices = append(choices, adapter.Choice{
			Index: idx,
			Message: adapter.Message{
				Role:            role,
				Content:         c.content.String(),
				ToolCalls:       toolCalls,
				ReasoningBlocks: reasoningBlocks,
			},
			FinishReason: c.finishReason,
		})
	}

	return adapter.ChatResponse{
		ID:      acc.id,
		Model:   acc.model,
		Choices: choices,
		Usage:   usage,
	}
}
