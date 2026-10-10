package streaming

import "github.com/kelvran/gateway/gateway/internal/adapter"

// ChunkSink is where the dataplane writes a stream: the OpenAI-format
// *Writer today, the Anthropic Messages encoder (internal/ingress/
// anthropicmsgs.SSEEncoder) for RFC-1's /v1/messages route. Both upstream
// decode loops, the cache replay and finishStreamedResponse write only
// through it, so the wire format is the sink's concern alone.
type ChunkSink interface {
	WriteChunk(ChatCompletionChunk) error
	WriteDone() error
}

// StreamEnd is what the dataplane knows when a stream ends that the chunks
// themselves did not carry: the final usage (the provider's, or
// estimateOrRealUsage's estimate when the provider sent none) and whether
// the dataplane cut the upstream off (the runaway ceiling or a mid-stream
// reservation top-up), in which case no finish chunk was ever written.
type StreamEnd struct {
	Usage     adapter.Usage
	Truncated bool
}

// Finisher is the optional ChunkSink extension for a sink that renders the
// end of the message itself. The dataplane calls WriteFinish instead of
// WriteDone when the sink implements it, so an Anthropic stream can end with
// message_delta{stop_reason, usage} and message_stop -- stop_reason
// max_tokens when Truncated -- so a guard trip never ends it with a clean
// close before message_delta (RFC-1 §7). *Writer deliberately does not
// implement it:
// the OpenAI route's bytes end with the [DONE] sentinel alone, on every path.
type Finisher interface {
	WriteFinish(StreamEnd) error
}

var _ ChunkSink = (*Writer)(nil)
