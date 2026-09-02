package dataplane

import (
	"sort"
	"strings"

	"github.com/kelvran/gateway/internal/adapter"
	"github.com/kelvran/gateway/internal/streaming"
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
}

type accumulatingChoice struct {
	role          string
	content       strings.Builder
	toolCalls     map[int]*accumulatingToolCall
	toolCallOrder []int // tool-call indices in first-seen order, within this choice
	finishReason  string
}

type accumulatingToolCall struct {
	id   string
	name string
	args strings.Builder
}

func newStreamAccumulator() *streamAccumulator {
	return &streamAccumulator{choices: map[int]*accumulatingChoice{}}
}

// add folds one chunk's content into the accumulator. Chunks may arrive
// with zero choices (a usage-only chunk, e.g. OpenAI's final chunk or
// Anthropic's message_delta translation) — add handles that by simply
// updating id/model and returning, matching every StreamDecoder's
// documented contract that a chunk can legitimately carry no choices.
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
			c = &accumulatingChoice{toolCalls: map[int]*accumulatingToolCall{}}
			acc.choices[cc.Index] = c
			acc.order = append(acc.order, cc.Index)
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
	}
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

		role := c.role
		if role == "" {
			role = "assistant"
		}

		choices = append(choices, adapter.Choice{
			Index: idx,
			Message: adapter.Message{
				Role:      role,
				Content:   c.content.String(),
				ToolCalls: toolCalls,
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
