package streaming

// toolCallIndexer renumbers one stream's tool-call deltas from the provider's
// position space -- ToolCallDelta.Index inside the gateway: Anthropic's and
// Bedrock's content-block index, Gemini's part position, OpenAI's own ordinal
// -- to OpenAI's meaning on the wire: the call's position in the final
// tool_calls, 0 first, assigned per choice in order of first appearance. The
// official SDKs' stream accumulators trust that meaning -- openai-python's
// stream helper raises IndexError on a first index of 1 and openai-go pads a
// phantom empty call (both probed with synthetic chunks, 2026-10-12) -- and a
// thinking or text block first makes exactly that happen on an anthropic or
// bedrock deployment. For openai/openaicompat upstreams and cache replays,
// already positions, the mapping is the identity. Scoped to one Writer; the
// zero value is ready to use.
type toolCallIndexer struct {
	// positions maps a choice index to that choice's table of provider
	// index -> wire position.
	positions map[int]map[int]int
}

// renumber returns chunk with every tool-call delta's Index replaced by its
// wire position. The caller's slices are never mutated: the dataplane's
// accumulator and the post-call audit hold the same chunk and rely on the
// provider's index. A chunk without tool-call deltas is returned as is.
func (x *toolCallIndexer) renumber(chunk ChatCompletionChunk) ChatCompletionChunk {
	hasToolCalls := false
	for _, ch := range chunk.Choices {
		if len(ch.Delta.ToolCalls) > 0 {
			hasToolCalls = true
			break
		}
	}
	if !hasToolCalls {
		return chunk
	}
	choices := make([]ChunkChoice, len(chunk.Choices))
	for i, ch := range chunk.Choices {
		if len(ch.Delta.ToolCalls) > 0 {
			deltas := make([]ToolCallDelta, len(ch.Delta.ToolCalls))
			for j, d := range ch.Delta.ToolCalls {
				d.Index = x.position(ch.Index, d.Index)
				deltas[j] = d
			}
			ch.Delta.ToolCalls = deltas
		}
		choices[i] = ch
	}
	chunk.Choices = choices
	return chunk
}

// position returns the wire position of providerIndex within choice,
// assigning the next free one on first sight.
func (x *toolCallIndexer) position(choice, providerIndex int) int {
	if x.positions == nil {
		x.positions = map[int]map[int]int{}
	}
	table := x.positions[choice]
	if table == nil {
		table = map[int]int{}
		x.positions[choice] = table
	}
	pos, ok := table[providerIndex]
	if !ok {
		pos = len(table)
		table[providerIndex] = pos
	}
	return pos
}
