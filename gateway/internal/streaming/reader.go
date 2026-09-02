package streaming

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// maxSSELineBytes bounds a single SSE line's length so a malicious or
// misbehaving upstream can't exhaust memory by never sending a newline —
// generous enough for any real provider payload (a single tool-call
// argument fragment is never anywhere close to this size).
const maxSSELineBytes = 1 << 20 // 1 MiB

// Reader reads framed Server-Sent Events from a raw io.Reader (an upstream
// provider's streaming HTTP response body). It is not safe for concurrent
// use, matching its single-request-scoped lifetime.
type Reader struct {
	scanner *bufio.Scanner
}

// NewReader constructs a Reader over r.
func NewReader(r io.Reader) *Reader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineBytes)
	return &Reader{scanner: scanner}
}

// Next reads and returns the next SSE event. It returns io.EOF (wrapped, so
// callers should compare with errors.Is) once the underlying stream ends
// with no further events pending.
//
// Per the SSE spec: events are separated by a blank line; a "data:" field
// may repeat within one event, in which case its values are joined with
// "\n"; lines beginning with ":" are comments and are ignored; other
// unrecognized fields (id:, retry:) are ignored, since no provider Kelvran
// talks to requires Kelvran to act on them.
func (r *Reader) Next() (SSEEvent, error) {
	var ev SSEEvent
	var dataLines []string
	sawField := false

	for r.scanner.Scan() {
		line := r.scanner.Text()

		if line == "" {
			if sawField {
				ev.Data = strings.Join(dataLines, "\n")
				return ev, nil
			}
			// Blank line before any field seen yet — just extra
			// whitespace between events; keep reading.
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment line
		}

		sawField = true
		switch {
		case strings.HasPrefix(line, "event:"):
			ev.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// id:, retry:, or any other field — intentionally ignored.
		}
	}

	if err := r.scanner.Err(); err != nil {
		return SSEEvent{}, fmt.Errorf("streaming: reading SSE stream: %w", err)
	}
	if sawField {
		// The stream ended without a trailing blank line after the final
		// event's fields — still a complete event, per how real upstreams
		// sometimes close the connection immediately after the last frame.
		ev.Data = strings.Join(dataLines, "\n")
		return ev, nil
	}
	return SSEEvent{}, io.EOF
}
