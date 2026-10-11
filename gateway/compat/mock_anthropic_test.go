package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordedRequest is one request a mock upstream received, kept so a test can
// assert what the gateway forwarded (the rewritten model, the deployment's
// credential, a replayed signature byte for byte).
type recordedRequest struct {
	Path   string
	Header http.Header
	Body   []byte
}

func (r recordedRequest) bodyHas(needle string) bool { return bytes.Contains(r.Body, []byte(needle)) }

func (r recordedRequest) jsonBody(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("forwarded body is not JSON: %v\n%s", err, r.Body)
	}
	return m
}

type recorder struct {
	mu   sync.Mutex
	reqs []recordedRequest
}

func (rec *recorder) record(r *http.Request, body []byte) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.reqs = append(rec.reqs, recordedRequest{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
}

func (rec *recorder) reset() {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.reqs = nil
}

func (rec *recorder) count() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.reqs)
}

func (rec *recorder) last(t *testing.T) recordedRequest {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.reqs) == 0 {
		t.Fatal("the mock upstream received no request")
	}
	return rec.reqs[len(rec.reqs)-1]
}

// anthropicUpstream replays recorded Anthropic Messages goldens. It picks the
// golden from the request: a streaming request gets the SSE golden, a request
// with tools whose last message is not yet a tool_result gets the
// thinking+tool_use turn, everything else gets the plain text turn, and
// count_tokens gets a fixed count. It never inspects credentials -- what the
// gateway forwarded is the tests' business, recorded for them.
type anthropicUpstream struct {
	recorder
	srv     *httptest.Server
	goldens map[string][]byte
}

const (
	goldenText         = "anthropic_response_text.json"
	goldenToolThinking = "anthropic_response_tool_use_thinking.json"
	goldenStreamText   = "anthropic_stream_text.txt"
	goldenStreamTool   = "anthropic_stream_thinking_tool.txt"
	mockTokenCount     = `{"input_tokens":2095}`
	// A request whose text carries this marker is answered with Anthropic's
	// own 400 envelope -- the shape RFC-1 §9 relays verbatim so Claude Code's
	// recovery can match Anthropic's wording.
	verbatim400Marker = "[verbatim-400]"
	verbatim400Body   = `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: 64 > 32, which is the maximum allowed number of output tokens for claude-upstream-id"},"request_id":"req_mock_verbatim"}`
)

// scriptedFailure is an upstream answer the gateway must NOT relay verbatim:
// outside §9's {anthropic, 400|422, parsed envelope} exception by status or by
// shape. Each is reached through a marker in the request text; its wording is
// what the redaction tests look for on the client side.
type scriptedFailure struct {
	status  int
	headers map[string]string
	body    string
}

var scriptedFailures = map[string]scriptedFailure{
	"[upstream-500]":       {status: http.StatusInternalServerError, body: `{"type":"error","error":{"type":"api_error","message":"mock shard-XYZZY unavailable"}}`},
	"[upstream-429]":       {status: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": "7"}, body: `{"type":"error","error":{"type":"rate_limit_error","message":"mock-org-42 would exceed its rate limit"}}`},
	"[upstream-401]":       {status: http.StatusUnauthorized, body: `{"type":"error","error":{"type":"authentication_error","message":"mock rejects the deployment credential"}}`},
	"[upstream-400-plain]": {status: http.StatusBadRequest, body: `{"message":"mock-plain-400: a 400 whose body is not an Anthropic envelope"}`},
}

// scriptedFailureFor finds the marker a request text carries, if any.
func scriptedFailureFor(body []byte) (scriptedFailure, bool) {
	for marker, failure := range scriptedFailures {
		if bytes.Contains(body, []byte(marker)) {
			return failure, true
		}
	}
	return scriptedFailure{}, false
}

func newAnthropicUpstream() (*anthropicUpstream, error) {
	u := &anthropicUpstream{goldens: map[string][]byte{}}
	for _, name := range []string{goldenText, goldenToolThinking, goldenStreamText, goldenStreamTool} {
		b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // fixed golden names under testdata/
		if err != nil {
			return nil, fmt.Errorf("loading golden: %w", err)
		}
		u.goldens[name] = b
	}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	return u, nil
}

func (u *anthropicUpstream) URL() string { return u.srv.URL }
func (u *anthropicUpstream) Close()      { u.srv.Close() }

func (u *anthropicUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "mock: reading body", http.StatusInternalServerError)
		return
	}
	u.record(r, body)
	w.Header().Set("x-should-retry", "false")
	w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
	switch r.URL.Path {
	case "/v1/messages/count_tokens":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, mockTokenCount)
	case "/v1/messages":
		u.serveMessages(w, body)
	default:
		http.NotFound(w, r)
	}
}

func (u *anthropicUpstream) serveMessages(w http.ResponseWriter, body []byte) {
	var req struct {
		Stream   bool              `json:"stream"`
		Tools    []json.RawMessage `json:"tools"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"mock: unparsable body"}}`)
		return
	}
	if bytes.Contains(body, []byte(verbatim400Marker)) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, verbatim400Body)
		return
	}
	if failure, ok := scriptedFailureFor(body); ok {
		for k, v := range failure.headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(failure.status)
		_, _ = io.WriteString(w, failure.body)
		return
	}
	toolTurn := len(req.Tools) > 0
	if n := len(req.Messages); n > 0 && bytes.Contains(req.Messages[n-1].Content, []byte(`"tool_result"`)) {
		toolTurn = false // the loop's second turn: answer with text so the loop ends
	}
	if req.Stream {
		u.stream(w, pick(toolTurn, goldenStreamTool, goldenStreamText))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(u.goldens[pick(toolTurn, goldenToolThinking, goldenText)])
}

// streamEvents is the golden split into its SSE events, each re-terminated
// with the blank line, exactly as stream writes them.
func (u *anthropicUpstream) streamEvents(golden string) []string {
	events := strings.Split(strings.TrimSpace(string(u.goldens[golden])), "\n\n")
	for i, event := range events {
		events[i] = event + "\n\n"
	}
	return events
}

// streamWire is the byte sequence a client must receive when the gateway
// relays this golden: the events as written, nothing added.
func (u *anthropicUpstream) streamWire(golden string) string {
	return strings.Join(u.streamEvents(golden), "")
}

// stream writes the golden one SSE event at a time, flushing between events,
// so the gateway's relay sees frames arrive the way a provider sends them.
func (u *anthropicUpstream) stream(w http.ResponseWriter, golden string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, event := range u.streamEvents(golden) {
		_, _ = io.WriteString(w, event)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func pick(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}
