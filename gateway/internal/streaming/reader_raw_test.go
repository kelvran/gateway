package streaming

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

// Raw holds each event's bytes exactly as read -- comment lines, id: fields,
// CRLF terminators, multi-line data and the blank lines between events
// included -- so a relay that writes Raw reproduces the upstream stream
// byte-for-byte, while Event and Data keep their parsed values (RFC-1 §9,
// item 11 slice S11b2).
func TestReaderRawHoldsEachEventsBytesExactly(t *testing.T) {
	frames := []string{
		": keep-alive\r\nevent: message_start\r\ndata: {\"type\":\"message_start\"}\r\n\r\n",
		"\nid: 7\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\ndata:  \"index\":0}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}",
	}
	r := NewReader(strings.NewReader(strings.Join(frames, "")))
	var got []SSEEvent
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, ev)
	}
	if len(got) != len(frames) {
		t.Fatalf("events = %d, want %d", len(got), len(frames))
	}
	for i, ev := range got {
		if string(ev.Raw) != frames[i] {
			t.Errorf("event %d Raw = %q, want %q", i, ev.Raw, frames[i])
		}
	}
	if got[0].Event != "message_start" || got[0].Data != `{"type":"message_start"}` {
		t.Errorf("event 0 parsed = %+v, want message_start with its data (CR stripped)", got[0])
	}
	if got[1].Event != "content_block_delta" || got[1].Data != "{\"type\":\"content_block_delta\",\n \"index\":0}" {
		t.Errorf("event 1 parsed Data = %q, want the two data lines joined with one space of the second kept", got[1].Data)
	}
	if got[2].Event != "message_stop" || got[2].Data != `{"type":"message_stop"}` {
		t.Errorf("event 2 parsed = %+v, want message_stop without a trailing blank line", got[2])
	}
}

// Raw is the caller's to keep: reading the next event must not overwrite an
// earlier event's bytes.
func TestReaderRawIsNotAliasedAcrossEvents(t *testing.T) {
	r := NewReader(strings.NewReader("data: first\n\ndata: second\n\n"))
	first, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if _, err := r.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if string(first.Raw) != "data: first\n\n" {
		t.Errorf("first.Raw after reading the second event = %q", first.Raw)
	}
}

// One event is bounded as a whole, not only per line: an upstream that keeps
// an event open with comment lines (or any other lines) is refused once the
// event's raw bytes pass maxSSEEventBytes.
func TestReaderRejectsEventExceedingMaxSSEEventBytes(t *testing.T) {
	endless := strings.Repeat(": keep-alive\n", (4<<20)/13+2) + "data: real\n\n"
	r := NewReader(strings.NewReader(endless))
	_, err := r.Next()
	if err == nil || !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("Next() error = %v, want errors.Is(err, bufio.ErrTooLong) for an event exceeding maxSSEEventBytes", err)
	}
}
