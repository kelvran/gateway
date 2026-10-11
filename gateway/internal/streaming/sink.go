package streaming

import (
	"net/http"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

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

// RawRelay is the optional ChunkSink extension for a sink that can relay an
// upstream's own SSE frames (RFC-1 §9, item 11 slice S11b2). When the
// request reached an `anthropic` deployment as a passthrough and the sink
// implements it, the dataplane calls RelayHeaders once with the upstream's
// relayable response headers before the first frame, then WriteRaw with
// each event's bytes as read, before the event is decoded into the
// canonical shadow, and writes no canonical chunk beside them; the sink
// still ends the stream through WriteFinish/WriteDone, which must write
// nothing after a relayed terminal frame. *Writer deliberately does not
// implement it: the OpenAI route never carries another provider's frames.
type RawRelay interface {
	RelayHeaders(http.Header)
	WriteRaw(frame []byte) error
}

var _ ChunkSink = (*Writer)(nil)
