package anthropicmsgs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// TestSSEEncoderWriteFinishTruncatedSynthesisesMaxTokens: when the dataplane
// cut the upstream off (runaway ceiling, reservation top-up), no finish
// chunk ever arrived; WriteFinish closes the open block and ends the message
// with stop_reason max_tokens and the dataplane's final usage, so the client
// never sees a clean close before message_delta (RFC-1 §7).
func TestSSEEncoderWriteFinishTruncatedSynthesisesMaxTokens(t *testing.T) {
	var buf bytes.Buffer
	enc := NewSSEEncoder(&buf)
	if err := enc.WriteChunk(streaming.ChatCompletionChunk{ID: "s6", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Role: "assistant", Content: "partial"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteFinish(streaming.StreamEnd{Usage: adapter.Usage{PromptTokens: 10, CompletionTokens: 7, TotalTokens: 17}, Truncated: true}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	stop := strings.Index(out, `{"type":"content_block_stop","index":0}`)
	delta := strings.Index(out, `"delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"input_tokens":10,"output_tokens":7,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`)
	if stop < 0 || delta < 0 || stop > delta {
		t.Errorf("want content_block_stop then message_delta{max_tokens, usage 10/7}:\n%s", out)
	}
	if !strings.HasSuffix(out, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") {
		t.Errorf("stream must end with message_stop:\n%s", out)
	}
}

// TestSSEEncoderWriteFinishUsesTheFinalUsage: the dataplane's final usage
// (real or estimated, estimateOrRealUsage) wins over whatever the chunks
// carried, and an un-truncated finish keeps the recorded stop reason.
func TestSSEEncoderWriteFinishUsesTheFinalUsage(t *testing.T) {
	var buf bytes.Buffer
	enc := NewSSEEncoder(&buf)
	stop := "stop"
	chunks := []streaming.ChatCompletionChunk{
		{ID: "s7", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Content: "hi"}}}},
		{ID: "s7", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, FinishReason: &stop, StopReason: "end_turn"}}},
	}
	for _, c := range chunks {
		if err := enc.WriteChunk(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.WriteFinish(streaming.StreamEnd{Usage: adapter.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":3,"output_tokens":1,`) {
		t.Errorf("want the recorded end_turn with the final usage 3/1:\n%s", buf.String())
	}
}

// TestSSEEncoderKeepAlivePingsOnlyWhenIdle: a tick with no write since the
// previous tick emits a ping; a tick after a write does not; nothing is
// pinged before message_start; cancelling the context ends the loop.
func TestSSEEncoderKeepAlivePingsOnlyWhenIdle(t *testing.T) {
	var buf bytes.Buffer
	enc := NewSSEEncoder(&buf)
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { enc.KeepAlive(ctx, ticks); close(done) }()
	tick := func() {
		before := enc.ticksSeen()
		ticks <- time.Time{}
		for enc.ticksSeen() == before {
			time.Sleep(time.Millisecond)
		}
	}
	tick() // before message_start: never a ping
	if err := enc.WriteChunk(streaming.ChatCompletionChunk{ID: "s8", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Content: "a"}}}}); err != nil {
		t.Fatal(err)
	}
	tick() // written since the last tick: no ping
	tick() // idle: ping
	if err := enc.WriteChunk(streaming.ChatCompletionChunk{ID: "s8", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Content: "b"}}}}); err != nil {
		t.Fatal(err)
	}
	tick() // written: no ping
	tick() // idle: ping
	cancel()
	<-done
	if err := enc.WriteDone(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if n := strings.Count(out, "event: ping\n"); n != 2 {
		t.Errorf("want exactly 2 pings, got %d:\n%s", n, out)
	}
	if strings.Index(out, "event: ping") < strings.Index(out, "event: message_start") {
		t.Errorf("a ping preceded message_start:\n%s", out)
	}
}

// TestSSEEncoderKeepAliveIsRaceFreeWithWrites: pings and chunk writes from
// two goroutines never interleave inside an event; every frame on the wire
// is a complete "event:" + "data:" pair with valid JSON (run under -race).
func TestSSEEncoderKeepAliveIsRaceFreeWithWrites(t *testing.T) {
	var buf bytes.Buffer
	enc := NewSSEEncoder(&buf)
	ticker := time.NewTicker(50 * time.Microsecond)
	defer ticker.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); enc.KeepAlive(ctx, ticker.C) }()
	for i := 0; i < 2000; i++ {
		if err := enc.WriteChunk(streaming.ChatCompletionChunk{ID: "s9", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Content: "x"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	wg.Wait()
	if err := enc.WriteDone(); err != nil {
		t.Fatal(err)
	}
	frames := strings.Split(strings.TrimSuffix(buf.String(), "\n\n"), "\n\n")
	if !strings.HasPrefix(frames[0], "event: message_start\n") {
		t.Errorf("the first frame must be message_start, never a ping: %q", frames[0])
	}
	pings := 0
	for _, f := range frames {
		lines := strings.Split(f, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") || !json.Valid([]byte(strings.TrimPrefix(lines[1], "data: "))) {
			t.Fatalf("malformed frame %q", f)
		}
		if lines[0] == "event: ping" {
			pings++
		}
	}
	if pings == 0 {
		t.Log("no ping landed during the writes (timing); frames were still well-formed")
	}
}

// flakyWriter fails its first n writes, then writes through.
type flakyWriter struct {
	buf   bytes.Buffer
	fails int
}

func (f *flakyWriter) Write(p []byte) (int, error) {
	if f.fails > 0 {
		f.fails--
		return 0, errors.New("write failed")
	}
	return f.buf.Write(p)
}

// TestSSEEncoderMessageStartFailureLeavesEncoderUnstarted: started flips only
// after message_start reached the writer, so a failed first write is retried
// by the next chunk and no content block is ever emitted without its
// message_start (a fallback retry streams through the same encoder).
func TestSSEEncoderMessageStartFailureLeavesEncoderUnstarted(t *testing.T) {
	w := &flakyWriter{fails: 1}
	enc := NewSSEEncoder(w)
	chunk := streaming.ChatCompletionChunk{ID: "s10", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Content: "x"}}}}
	if err := enc.WriteChunk(chunk); err == nil {
		t.Fatal("want the first write's error")
	}
	if err := enc.WriteChunk(chunk); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(w.buf.String(), "event: message_start\n") {
		t.Errorf("after a failed message_start the retry must begin with message_start:\n%s", w.buf.String())
	}
}

// TestSSEEncoderFinishIsTerminal: after message_stop nothing else is written
// -- a second WriteDone/WriteFinish is a no-op and Ping is suppressed -- and
// after WriteError no clean end follows (the stream must read as aborted).
func TestSSEEncoderFinishIsTerminal(t *testing.T) {
	var buf bytes.Buffer
	enc := NewSSEEncoder(&buf)
	if err := enc.WriteChunk(streaming.ChatCompletionChunk{ID: "s11", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Content: "x"}}}}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{enc.WriteDone, enc.WriteDone, enc.Ping, func() error { return enc.WriteFinish(streaming.StreamEnd{Truncated: true}) }} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.WriteChunk(streaming.ChatCompletionChunk{ID: "s11", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Content: "late"}}}}); err == nil {
		t.Error("WriteChunk after the message ended must fail loudly: data after message_stop is a caller bug")
	}
	out := buf.String()
	if strings.Contains(out, "late") {
		t.Errorf("a chunk written after message_stop reached the wire:\n%s", out)
	}
	if n := strings.Count(out, "event: message_stop\n"); n != 1 {
		t.Errorf("message_stop count = %d, want 1:\n%s", n, out)
	}
	if strings.Contains(out, "event: ping") || strings.Count(out, "event: message_delta\n") != 1 {
		t.Errorf("nothing may follow message_stop:\n%s", out)
	}
	var buf2 bytes.Buffer
	enc2 := NewSSEEncoder(&buf2)
	if err := enc2.WriteChunk(streaming.ChatCompletionChunk{ID: "s12", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Content: "x"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := enc2.WriteError("overloaded_error", "upstream overloaded"); err != nil {
		t.Fatal(err)
	}
	if err := enc2.WriteDone(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf2.String(), "message_delta") || strings.Contains(buf2.String(), "message_stop") {
		t.Errorf("a clean end followed an error event:\n%s", buf2.String())
	}
}

// TestSSEEncoderStartKeepAliveStopJoins: the stop function cancels the
// keepalive, stops its ticker and waits for the goroutine, so a handler can
// call it before returning and know no write is in flight.
func TestSSEEncoderStartKeepAliveStopJoins(t *testing.T) {
	var buf bytes.Buffer
	enc := NewSSEEncoder(&buf)
	stop := enc.StartKeepAlive(context.Background(), 200*time.Microsecond)
	if err := enc.WriteChunk(streaming.ChatCompletionChunk{ID: "s13", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Content: "x"}}}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	stop()
	before := enc.writesSeen()
	time.Sleep(5 * time.Millisecond)
	if after := enc.writesSeen(); after != before {
		t.Errorf("writes after stop: %d -> %d; the keepalive must have exited", before, after)
	}
	if !strings.Contains(buf.String(), "event: ping") {
		t.Errorf("want at least one ping during the idle 5 ms:\n%s", buf.String())
	}
}

// flushCounter is an io.Writer that also flushes, counting the flushes.
type flushCounter struct {
	buf     bytes.Buffer
	flushes int
}

func (f *flushCounter) Write(p []byte) (int, error) { return f.buf.Write(p) }
func (f *flushCounter) Flush()                      { f.flushes++ }

// TestSSEEncoderFlushesEveryEventWhenTheWriterCanFlush: net/http buffers
// before the chunked writer, so a ping or a delta that is not flushed sits
// unsent and defeats stall detection; every event is flushed when the
// writer can.
func TestSSEEncoderFlushesEveryEventWhenTheWriterCanFlush(t *testing.T) {
	w := &flushCounter{}
	enc := NewSSEEncoder(w)
	for _, text := range []string{"a", "b"} {
		if err := enc.WriteChunk(streaming.ChatCompletionChunk{ID: "s14", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Content: text}}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteDone(); err != nil {
		t.Fatal(err)
	}
	if events := strings.Count(w.buf.String(), "event: "); w.flushes != events {
		t.Errorf("flushes = %d, events = %d; every event must be flushed", w.flushes, events)
	}
}

// TestSSEEncoderKeepAliveStopsPingingAfterFinish: once the message ended,
// idle ticks must not ping -- a ping after message_stop would be a frame the
// protocol has no place for -- until the route stops the keepalive.
func TestSSEEncoderKeepAliveStopsPingingAfterFinish(t *testing.T) {
	var buf bytes.Buffer
	enc := NewSSEEncoder(&buf)
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { enc.KeepAlive(ctx, ticks); close(done) }()
	tick := func() {
		before := enc.ticksSeen()
		ticks <- time.Time{}
		for enc.ticksSeen() == before {
			time.Sleep(time.Millisecond)
		}
	}
	if err := enc.WriteChunk(streaming.ChatCompletionChunk{ID: "s15", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Content: "x"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteDone(); err != nil {
		t.Fatal(err)
	}
	tick() // first idle tick after the finish: writes moved since the last tick
	tick() // second idle tick: would ping if the finish did not stop it
	tick()
	cancel()
	<-done
	if strings.Contains(buf.String(), "event: ping") {
		t.Errorf("a ping was written after message_stop:\n%s", buf.String())
	}
	if !strings.HasSuffix(buf.String(), "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") {
		t.Errorf("message_stop must be the last frame:\n%s", buf.String())
	}
}
