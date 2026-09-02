package streaming

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReaderParsesSingleEvent(t *testing.T) {
	r := NewReader(strings.NewReader("data: {\"hello\":\"world\"}\n\n"))
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next() error = %v, want nil", err)
	}
	if ev.Data != `{"hello":"world"}` {
		t.Errorf("Data = %q, want %q", ev.Data, `{"hello":"world"}`)
	}
	if ev.Event != "" {
		t.Errorf("Event = %q, want empty (no event: line)", ev.Event)
	}

	_, err = r.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("second Next() error = %v, want io.EOF", err)
	}
}

func TestReaderParsesTypedEvent(t *testing.T) {
	r := NewReader(strings.NewReader("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next() error = %v, want nil", err)
	}
	if ev.Event != "message_start" {
		t.Errorf("Event = %q, want %q", ev.Event, "message_start")
	}
	if ev.Data != `{"type":"message_start"}` {
		t.Errorf("Data = %q, want %q", ev.Data, `{"type":"message_start"}`)
	}
}

func TestReaderJoinsMultiLineData(t *testing.T) {
	// Per the SSE spec, multiple "data:" lines within one event are joined
	// with "\n" — no real provider here relies on this, but a compliant
	// reader must still handle it correctly rather than silently dropping
	// all but the last line.
	r := NewReader(strings.NewReader("data: line one\ndata: line two\n\n"))
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next() error = %v, want nil", err)
	}
	if ev.Data != "line one\nline two" {
		t.Errorf("Data = %q, want %q", ev.Data, "line one\nline two")
	}
}

func TestReaderSkipsCommentLines(t *testing.T) {
	r := NewReader(strings.NewReader(": keep-alive\n\ndata: real\n\n"))
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next() error = %v, want nil", err)
	}
	if ev.Data != "real" {
		t.Errorf("Data = %q, want %q", ev.Data, "real")
	}
}

func TestReaderMultipleEventsInSequence(t *testing.T) {
	input := "event: a\ndata: 1\n\nevent: b\ndata: 2\n\n"
	r := NewReader(strings.NewReader(input))

	ev1, err := r.Next()
	if err != nil || ev1.Event != "a" || ev1.Data != "1" {
		t.Fatalf("first event = %+v, err = %v", ev1, err)
	}
	ev2, err := r.Next()
	if err != nil || ev2.Event != "b" || ev2.Data != "2" {
		t.Fatalf("second event = %+v, err = %v", ev2, err)
	}
	_, err = r.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("third Next() error = %v, want io.EOF", err)
	}
}

func TestReaderHandlesMissingTrailingBlankLine(t *testing.T) {
	// A real upstream sometimes closes the connection immediately after
	// the final event's fields, with no trailing blank line — must still
	// be treated as one complete event, not silently dropped.
	r := NewReader(strings.NewReader("data: last\n"))
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next() error = %v, want nil", err)
	}
	if ev.Data != "last" {
		t.Errorf("Data = %q, want %q", ev.Data, "last")
	}
	_, err = r.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("second Next() error = %v, want io.EOF", err)
	}
}

func TestReaderEmptyInputReturnsEOFImmediately(t *testing.T) {
	r := NewReader(strings.NewReader(""))
	_, err := r.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("Next() error = %v, want io.EOF", err)
	}
}
