package anthropicmsgs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

const (
	relayMessageStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_up\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-upstream-id\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n"
	relayBlockStart   = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"
	relayTextDelta    = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"
	relayBlockStop    = "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n"
	relayMessageDelta = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n"
	relayMessageStop  = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	relayErrorFrame   = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
)

func relayAll(t *testing.T, e *SSEEncoder, frames ...string) {
	t.Helper()
	for _, f := range frames {
		if err := e.WriteRaw([]byte(f)); err != nil {
			t.Fatalf("WriteRaw(%q): %v", f, err)
		}
	}
}

func relayEventNames(stream string) []string {
	var names []string
	for _, line := range strings.Split(stream, "\n") {
		if strings.HasPrefix(line, "event: ") {
			names = append(names, strings.TrimPrefix(line, "event: "))
		}
	}
	return names
}

// WriteRaw writes the frame as received, counts it as keepalive activity and
// mirrors the upstream's state (message_start seen, the open block's index)
// so the encoder can still end the message itself; RelayHeaders puts the
// upstream's relayable headers on the response before the first byte.
func TestSSEEncoderWriteRawRelaysBytesAndMirrorsState(t *testing.T) {
	rec := httptest.NewRecorder()
	e := NewSSEEncoder(rec)
	var _ streaming.RawRelay = e
	e.RelayHeaders(http.Header{"X-Should-Retry": {"false"}, "Anthropic-Ratelimit-Unified-Status": {"allowed"}})
	relayAll(t, e, relayMessageStart, relayBlockStart, relayTextDelta)
	if got := rec.Body.String(); got != relayMessageStart+relayBlockStart+relayTextDelta {
		t.Fatalf("relayed bytes = %q, want the three frames unchanged", got)
	}
	if rec.Header().Get("X-Should-Retry") != "false" || rec.Header().Get("Anthropic-Ratelimit-Unified-Status") != "allowed" {
		t.Errorf("relay headers missing from the response: %v", rec.Header())
	}
	if e.writesSeen() != 3 {
		t.Errorf("writes = %d, want 3 (a relayed frame is keepalive activity)", e.writesSeen())
	}
	if !e.started || e.open == nil || e.open.index != 2 || e.finished {
		t.Errorf("mirror state started=%v open=%+v finished=%v, want started, block 2 open, not finished", e.started, e.open, e.finished)
	}
	// A frame whose data is not JSON is still relayed and leaves the mirror
	// untouched: the relay never fails on the upstream's framing.
	relayAll(t, e, "event: weird\ndata: not json\n\n")
	if !strings.HasSuffix(rec.Body.String(), "event: weird\ndata: not json\n\n") || e.open == nil || e.open.index != 2 {
		t.Errorf("unparsable frame changed the relay or the mirror: %q open=%+v", rec.Body.String(), e.open)
	}
}

// A guard cut after relayed frames ends the message the way RFC-1 §7 asks:
// content_block_stop for the UPSTREAM's open block index, message_delta with
// stop_reason max_tokens and the gateway's usage, message_stop -- and never a
// second message_start.
func TestSSEEncoderWriteFinishAfterRelayedCutClosesTheUpstreamBlock(t *testing.T) {
	var out strings.Builder
	e := NewSSEEncoder(&out)
	relayAll(t, e, relayMessageStart, relayBlockStart, relayTextDelta)
	prefix := out.String()
	if err := e.WriteFinish(streaming.StreamEnd{Usage: adapter.Usage{PromptTokens: 3, CompletionTokens: 7}, Truncated: true}); err != nil {
		t.Fatalf("WriteFinish: %v", err)
	}
	tail := strings.TrimPrefix(out.String(), prefix)
	if names := relayEventNames(tail); strings.Join(names, ",") != "content_block_stop,message_delta,message_stop" {
		t.Fatalf("events after the cut = %v, want content_block_stop,message_delta,message_stop; tail %q", names, tail)
	}
	if !strings.Contains(tail, "\"index\":2}") {
		t.Errorf("content_block_stop does not close the upstream's block 2: %q", tail)
	}
	if !strings.Contains(tail, `"stop_reason":"max_tokens"`) || !strings.Contains(tail, `"output_tokens":7`) {
		t.Errorf("message_delta lacks max_tokens or the gateway's usage: %q", tail)
	}
	if strings.Count(out.String(), "event: message_start") != 1 {
		t.Errorf("message_start written more than once: %q", out.String())
	}
}

// A relayed message_stop or error frame is terminal: WriteFinish, WriteDone
// and WriteError write nothing after it, so the gateway never appends its own
// ending or error to the upstream's.
func TestSSEEncoderRelayedTerminalFrameMakesLaterWritesNoOps(t *testing.T) {
	for name, terminal := range map[string]string{"message_stop": relayMessageStop, "error": relayErrorFrame} {
		var out strings.Builder
		e := NewSSEEncoder(&out)
		relayAll(t, e, relayMessageStart, relayBlockStart, relayTextDelta, relayBlockStop, relayMessageDelta, terminal)
		before := out.String()
		if err := e.WriteFinish(streaming.StreamEnd{Truncated: true}); err != nil {
			t.Fatalf("%s: WriteFinish: %v", name, err)
		}
		if err := e.WriteDone(); err != nil {
			t.Fatalf("%s: WriteDone: %v", name, err)
		}
		if err := e.WriteError("api_error", "gateway error"); err != nil {
			t.Fatalf("%s: WriteError: %v", name, err)
		}
		if out.String() != before {
			t.Errorf("%s: bytes written after a relayed terminal frame: %q", name, strings.TrimPrefix(out.String(), before))
		}
	}
}

// When the upstream's message_delta was relayed but the stream was cut before
// message_stop, the ending adds message_stop alone: a second message_delta
// would contradict the one the client already holds.
func TestSSEEncoderWriteFinishAfterRelayedMessageDeltaWritesOnlyMessageStop(t *testing.T) {
	var out strings.Builder
	e := NewSSEEncoder(&out)
	relayAll(t, e, relayMessageStart, relayBlockStart, relayTextDelta, relayBlockStop, relayMessageDelta)
	before := out.String()
	if err := e.WriteFinish(streaming.StreamEnd{}); err != nil {
		t.Fatalf("WriteFinish: %v", err)
	}
	if tail := strings.TrimPrefix(out.String(), before); tail != relayMessageStop {
		t.Errorf("tail = %q, want exactly message_stop", tail)
	}
}
