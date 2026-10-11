package streaming

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

// maxSSELineBytes bounds a single SSE line's length so a malicious or
// misbehaving upstream can't exhaust memory by never sending a newline —
// generous enough for any real provider payload (a single tool-call
// argument fragment is never anywhere close to this size).
const maxSSELineBytes = 1 << 20 // 1 MiB

// readerBufferBytes is the bufio.Reader size: lines longer than it are read
// in slices and re-joined, up to maxSSELineBytes.
const readerBufferBytes = 64 * 1024

// maxSSEEventBytes bounds one event's raw bytes (every line of it, comment
// and id: lines included), so an upstream cannot grow a single event -- and
// the Raw slice a relay would write -- without end; a few data lines of the
// maximal line length still fit.
const maxSSEEventBytes = 4 << 20 // 4 MiB

// Reader reads framed Server-Sent Events from a raw io.Reader (an upstream
// provider's streaming HTTP response body). It is not safe for concurrent
// use, matching its single-request-scoped lifetime.
//
// Beside the parsed Event and Data, every SSEEvent carries Raw: the event's
// bytes exactly as read, from the first byte after the previous event's
// terminating blank line to the end of its own (comment lines, id: and
// retry: fields, CR/LF terminators and multi-line data included), so a relay
// that writes Raw frame by frame reproduces the upstream stream byte for
// byte (RFC-1 §9, item 11 slice S11b2). Raw is freshly allocated per event
// and never aliased by a later Next.
type Reader struct {
	br   *bufio.Reader
	raw  []byte // the bytes of the event being assembled
	done bool   // the final unterminated line has been returned
}

// NewReader constructs a Reader over r.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, readerBufferBytes)}
}

// Next reads and returns the next SSE event. It returns io.EOF (wrapped, so
// callers should compare with errors.Is) once the underlying stream ends
// with no further events pending.
//
// Per the SSE spec: events are separated by a blank line; a "data:" field
// may repeat within one event, in which case its values are joined with
// "\n"; lines beginning with ":" are comments and are ignored; other
// unrecognized fields (id:, retry:) are ignored, since no provider Kelvran
// talks to requires Kelvran to act on them. Every line read still lands in
// the event's Raw.
func (r *Reader) Next() (SSEEvent, error) {
	if r.done {
		return SSEEvent{}, io.EOF
	}
	var ev SSEEvent
	var dataLines []string
	sawField := false
	r.raw = nil // a fresh slice per event (also discards a partial event a failed call left behind)
	finish := func() SSEEvent {
		ev.Data = strings.Join(dataLines, "\n")
		ev.Raw = r.raw
		return ev
	}

	for {
		line, atEOF, err := r.readLine()
		if err != nil {
			return SSEEvent{}, fmt.Errorf("streaming: reading SSE stream: %w", err)
		}
		if atEOF && line == nil {
			// The stream ended. An event whose fields were read without a
			// trailing blank line is still a complete event, per how real
			// upstreams sometimes close the connection right after the last
			// frame.
			r.done = true
			if sawField {
				return finish(), nil
			}
			return SSEEvent{}, io.EOF
		}

		switch {
		case len(line) == 0:
			if sawField {
				return finish(), nil
			}
			// Blank line before any field seen yet — just extra whitespace
			// between events; keep reading (its bytes stay in Raw).
		case line[0] == ':':
			// comment line
		default:
			sawField = true
			text := string(line)
			switch {
			case strings.HasPrefix(text, "event:"):
				ev.Event = strings.TrimSpace(strings.TrimPrefix(text, "event:"))
			case strings.HasPrefix(text, "data:"):
				dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(text, "data:"), " "))
			default:
				// id:, retry:, or any other field — intentionally ignored.
			}
		}
		if atEOF {
			r.done = true
			if sawField {
				return finish(), nil
			}
			return SSEEvent{}, io.EOF
		}
	}
}

// readLine returns the next line without its terminator (one optional CR
// before the LF, as bufio.ScanLines strips it) and appends the line with its
// terminator to r.raw. atEOF reports that the stream ended: with a nil line
// nothing more was read; with a non-nil line, that line was unterminated. A
// line longer than maxSSELineBytes fails with bufio.ErrTooLong.
func (r *Reader) readLine() (line []byte, atEOF bool, err error) {
	var acc []byte
	for {
		chunk, readErr := r.br.ReadSlice('\n')
		r.raw = append(r.raw, chunk...)
		acc = append(acc, chunk...)
		if len(r.raw) > maxSSEEventBytes {
			return nil, false, fmt.Errorf("SSE event exceeds %d bytes: %w", maxSSEEventBytes, bufio.ErrTooLong)
		}
		// Checked on every slice, so an oversized line that ends the stream
		// (no newline, io.EOF) is refused like one that keeps going. The two
		// extra bytes are the line's own CR LF.
		if len(acc) > maxSSELineBytes+2 {
			return nil, false, fmt.Errorf("SSE line exceeds %d bytes: %w", maxSSELineBytes, bufio.ErrTooLong)
		}
		switch {
		case readErr == nil:
			return trimEOL(acc), false, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
		case errors.Is(readErr, io.EOF):
			if len(acc) == 0 {
				return nil, true, nil
			}
			return trimEOL(acc), true, nil
		default:
			return nil, false, readErr
		}
	}
}

// trimEOL drops one trailing LF and one optional CR before it.
func trimEOL(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	if line == nil {
		return []byte{}
	}
	return line
}
