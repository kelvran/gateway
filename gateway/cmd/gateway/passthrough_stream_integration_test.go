package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const passthroughStreamBody = `{"model":"claude-sys","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`

var passthroughStreamHeaders = map[string]string{"Authorization": "Bearer all-secret", "anthropic-version": "2024-06-01"}

// newScriptedAnthropicStreamUpstream answers every request with the given SSE
// frames, flushed one by one under the given headers, and counts the calls.
func newScriptedAnthropicStreamUpstream(t *testing.T, frames []string, headers map[string]string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			_, _ = fmt.Fprint(w, f)
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

var relayedStreamFrames = []string{
	": upstream comment\r\nevent: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stream\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-upstream-id\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\r\n\r\n",
	"id: 7\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n",
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n",
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
}

// A streamed turn on an anthropic deployment reaches the client as the
// upstream's own frames byte-for-byte -- comment lines, id: fields and CRLF
// terminators included -- with its relayable headers; an event type the
// gateway does not know travels too, which makes the turn unrepresentable, so
// the next identical request streams from upstream again.
func TestIntegrationMessagesPassthroughStreamRelaysTheRawFrames(t *testing.T) {
	frames := append(append([]string{}, relayedStreamFrames[:3]...), append([]string{"event: new_thing\ndata: {\"type\":\"new_thing\"}\n\n"}, relayedStreamFrames[3:]...)...)
	upstream, calls := newScriptedAnthropicStreamUpstream(t, frames, upstreamRelayHeaders)
	gw := newPassthroughIntegrationServer(t, upstream.URL, nil)
	for i := 1; i <= 2; i++ {
		status, header, raw := postMessages(t, gw.URL, passthroughStreamHeaders, passthroughStreamBody)
		if status != http.StatusOK {
			t.Fatalf("call %d: status = %d; body %s", i, status, raw)
		}
		if string(raw) != strings.Join(frames, "") {
			t.Fatalf("call %d: body is not the upstream's frames:\n%q\nwant\n%q", i, raw, strings.Join(frames, ""))
		}
		if header.Get("X-Should-Retry") != "false" || header.Get("Anthropic-Ratelimit-Unified-Status") != "allowed" {
			t.Errorf("call %d: relay headers missing: %v", i, header)
		}
		if header.Get("Request-Id") != "" || header.Get("Anthropic-Organization-Id") != "" {
			t.Errorf("call %d: non-relayable upstream header reached the client: %v", i, header)
		}
		if header.Get("Content-Type") != "text/event-stream" {
			t.Errorf("call %d: Content-Type = %q", i, header.Get("Content-Type"))
		}
	}
	if calls.Load() != 2 {
		t.Errorf("upstream calls = %d, want 2: an unknown event type is never cached", calls.Load())
	}
}

// A representable streamed turn is relayed once and re-encoded on the cache
// hit: the second identical request makes no upstream call and streams the
// canonical message (the gateway's own frames, canonical model name), never
// the relayed bytes.
func TestIntegrationMessagesPassthroughStreamCacheHitIsReEncoded(t *testing.T) {
	upstream, calls := newScriptedAnthropicStreamUpstream(t, relayedStreamFrames, nil)
	gw := newPassthroughIntegrationServer(t, upstream.URL, nil)
	status, _, first := postMessages(t, gw.URL, passthroughStreamHeaders, passthroughStreamBody)
	if status != http.StatusOK || string(first) != strings.Join(relayedStreamFrames, "") {
		t.Fatalf("first call: status %d body %q, want the raw frames", status, first)
	}
	status, _, second := postMessages(t, gw.URL, passthroughStreamHeaders, passthroughStreamBody)
	if status != http.StatusOK {
		t.Fatalf("second call: status = %d; body %s", status, second)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1: the second turn is a cache hit", calls.Load())
	}
	s := string(second)
	if s == string(first) || strings.Contains(s, ": upstream comment") || !strings.Contains(s, `"model":"claude-sys"`) || !strings.Contains(s, "event: message_stop") {
		t.Errorf("cache hit = %q, want the re-encoded stream (canonical model, no upstream comment, ends with message_stop)", s)
	}
}

// An upstream error frame is relayed as Anthropic sent it and ends the
// stream: exactly one error event, no gateway error appended, no message_stop.
func TestIntegrationMessagesPassthroughStreamRelaysTheUpstreamErrorFrameOnce(t *testing.T) {
	frames := []string{
		relayedStreamFrames[0],
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n",
	}
	upstream, _ := newScriptedAnthropicStreamUpstream(t, frames, nil)
	gw := newPassthroughIntegrationServer(t, upstream.URL, nil)
	status, _, raw := postMessages(t, gw.URL, passthroughStreamHeaders, passthroughStreamBody)
	if status != http.StatusOK {
		t.Fatalf("status = %d; body %s", status, raw)
	}
	if string(raw) != strings.Join(frames, "") {
		t.Errorf("body = %q, want the upstream's two frames and nothing after the error", raw)
	}
	if strings.Count(string(raw), "event: error") != 1 || strings.Contains(string(raw), "message_stop") {
		t.Errorf("error frames or a message_stop are wrong: %q", raw)
	}
}

// A runaway upstream cut by the gateway's ceiling still ends the relayed
// stream the way the protocol requires: content_block_stop for the upstream's
// open block (index 3 here, taken from the relayed frame), message_delta with
// stop_reason max_tokens, message_stop -- and only one message_start.
func TestIntegrationMessagesPassthroughStreamGuardCutEndsWithAValidSequence(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		_, _ = fmt.Fprint(w, relayedStreamFrames[0])
		_, _ = fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":3,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		f.Flush()
		delta := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":3,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + strings.Repeat("x", 200) + "\"}}\n\n"
		for r.Context().Err() == nil {
			if _, err := fmt.Fprint(w, delta); err != nil {
				return
			}
			f.Flush()
		}
	}))
	t.Cleanup(upstream.Close)
	gw := newPassthroughIntegrationServer(t, upstream.URL, nil)
	body := `{"model":"claude-sys","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	status, _, raw := postMessages(t, gw.URL, passthroughStreamHeaders, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d; body %s", status, raw)
	}
	names := eventNamesOf(string(raw))
	if len(names) < 4 || strings.Join(names[len(names)-3:], ",") != "content_block_stop,message_delta,message_stop" {
		t.Fatalf("events = %v, want the stream to end with content_block_stop,message_delta,message_stop", names)
	}
	if strings.Count(string(raw), "event: message_start") != 1 {
		t.Errorf("message_start count != 1: %q", raw)
	}
	tail := string(raw)[strings.LastIndex(string(raw), "event: content_block_stop"):]
	if !strings.Contains(tail, "\"index\":3}") || !strings.Contains(tail, `"stop_reason":"max_tokens"`) {
		t.Errorf("ending does not close the upstream's block 3 with max_tokens: %q", tail)
	}
}

func eventNamesOf(stream string) []string {
	var names []string
	for _, line := range strings.Split(stream, "\n") {
		if strings.HasPrefix(line, "event: ") {
			names = append(names, strings.TrimSuffix(strings.TrimPrefix(line, "event: "), "\r"))
		}
	}
	return names
}

// An error frame as the very first upstream event is handled like a pre-stream
// failure: with a fallback deployment configured, the client receives the
// fallback's re-encoded stream and none of Anthropic's error bytes.
func TestIntegrationMessagesPassthroughStreamErrorFirstFrameFallsBack(t *testing.T) {
	anthropicUp, anthropicCalls := newScriptedAnthropicStreamUpstream(t, []string{
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n",
	}, upstreamRelayHeaders)
	openaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range []string{
			"data: {\"id\":\"chatcmpl-fb\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"from the fallback\"},\"finish_reason\":null}]}\n\n",
			"data: {\"id\":\"chatcmpl-fb\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = fmt.Fprint(w, f)
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(openaiUp.Close)
	gw := newPassthroughFallbackIntegrationServer(t, anthropicUp.URL, openaiUp.URL)
	status, header, raw := postMessages(t, gw.URL, passthroughStreamHeaders, passthroughStreamBody)
	if status != http.StatusOK {
		t.Fatalf("status = %d; body %s", status, raw)
	}
	s := string(raw)
	if strings.Contains(s, "Overloaded") || strings.Contains(s, "event: error") {
		t.Fatalf("the error-first frame reached the client: %q", s)
	}
	if !strings.Contains(s, "from the fallback") || !strings.Contains(s, "event: message_stop") || !strings.Contains(s, "event: message_start") {
		t.Errorf("body is not the fallback's re-encoded stream: %q", s)
	}
	if header.Get("X-Should-Retry") != "" || header.Get("Anthropic-Ratelimit-Unified-Status") != "" {
		t.Errorf("the failed hop's headers reached the client: %v", header)
	}
	if anthropicCalls.Load() != 1 {
		t.Errorf("anthropic upstream calls = %d, want 1", anthropicCalls.Load())
	}
}
