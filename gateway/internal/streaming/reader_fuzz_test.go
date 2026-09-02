package streaming

import (
	"bytes"
	"strings"
	"testing"
)

// FuzzReaderNeverPanics exercises Reader.Next() against arbitrary bytes,
// standing in for a malicious or simply buggy upstream provider's raw HTTP
// response body. Per THREAT_MODEL.md's provider trust boundary, Kelvran
// treats upstream bytes as untrusted input at the parsing layer, exactly
// like internal/cache's FuzzKey treats caller-supplied request text as
// untrusted — the Reader must degrade to "some sequence of events, or an
// error", never crash the gateway process handling every other tenant's
// concurrent request.
func FuzzReaderNeverPanics(f *testing.F) {
	seeds := [][]byte{
		[]byte("data: {\"hello\":\"world\"}\n\n"),
		[]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"),
		[]byte(""),
		[]byte("data: unterminated, no trailing newline"),
		[]byte(": comment only\n\n"),
		[]byte("data: line one\ndata: line two\n\n"),
		[]byte("garbage\x00\x01\x02not even sse"),
		[]byte("data: " + strings.Repeat("x", 2048)), // long line, no newline
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewReader(bytes.NewReader(data))

		// Property: Next() must never panic, and must terminate (return an
		// error — io.EOF or a wrapped scan error, both acceptable) within a
		// number of calls bounded by the input size. Each call that returns
		// a real event without error consumed at least one non-blank line
		// of input, so this bound can never be legitimately exceeded on a
		// finite input — hitting it indicates a real bug, not just an
		// unexpected-but-valid parse.
		maxCalls := len(data) + 16
		for i := 0; i < maxCalls; i++ {
			if _, err := r.Next(); err != nil {
				return
			}
		}
		t.Fatalf("Next() did not terminate within %d calls for input of length %d", maxCalls, len(data))
	})
}
