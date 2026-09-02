package streaming

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// doneSentinel is the literal SSE payload OpenAI-compatible clients expect
// to mark the end of a stream — not a JSON value, by convention.
const doneSentinel = "data: [DONE]\n\n"

// Writer writes canonical chunks to an HTTP client as Server-Sent Events,
// flushing after every write so the client receives each chunk as soon as
// it's ready rather than buffered until the response completes. Scoped to
// one in-flight request; not safe for concurrent use.
type Writer struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// NewWriter constructs a Writer over w. Returns an error if w does not
// support flushing — the stdlib *http.response returned by net/http's own
// server always does, but this is checked explicitly rather than assumed,
// since a caller could in principle wrap ResponseWriter in something that
// doesn't.
func NewWriter(w http.ResponseWriter) (*Writer, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming: http.ResponseWriter does not implement http.Flusher")
	}
	return &Writer{w: w, flusher: flusher}, nil
}

// WriteChunk writes one canonical chunk as a single SSE "data:" frame and
// flushes it to the client immediately.
func (sw *Writer) WriteChunk(chunk ChatCompletionChunk) error {
	payload, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("streaming: marshaling chunk: %w", err)
	}
	if _, err := fmt.Fprintf(sw.w, "data: %s\n\n", payload); err != nil {
		return fmt.Errorf("streaming: writing chunk: %w", err)
	}
	sw.flusher.Flush()
	return nil
}

// WriteDone writes the terminal "[DONE]" sentinel and flushes it. Callers
// must call this exactly once, after the last real chunk, and never write
// anything to the response after calling it.
func (sw *Writer) WriteDone() error {
	if _, err := fmt.Fprint(sw.w, doneSentinel); err != nil {
		return fmt.Errorf("streaming: writing done sentinel: %w", err)
	}
	sw.flusher.Flush()
	return nil
}
