// Package benchupstream is the OpenAI-shaped mock provider the benchmark
// harness (cmd/kelvran-bench) points the gateway at. It answers
// POST …/chat/completions, buffered or streamed, with a configurable
// response latency, streamed-chunk count and cadence, and token counts, so a
// benchmark measures the gateway's own overhead against a provider whose
// behaviour is fixed and known rather than a live model's variance.
//
// It is deliberately stdlib-only and knows nothing about the gateway: the
// wire shapes are the OpenAI chat-completions JSON the gateway's openai
// adapter already speaks, written out by hand here so the package stays a
// leaf (see .go-arch-lint.yml).
package benchupstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Config shapes every response the server gives.
type Config struct {
	// Latency is added before a buffered response is written, or before the
	// first streamed chunk (so it is the time to first token a client sees
	// from the provider itself).
	Latency time.Duration
	// StreamChunks is the number of content chunks a streaming response
	// carries; values below 1 mean 1.
	StreamChunks int
	// ChunkInterval is the pause between consecutive streamed content
	// chunks (zero = as fast as the connection allows).
	ChunkInterval time.Duration
	// PromptTokens and CompletionTokens are reported in usage; the gateway
	// prices and rate-limits from them.
	PromptTokens     int
	CompletionTokens int
	// ContentChunk is the text of every streamed content chunk and, repeated
	// StreamChunks times, the buffered response's content. Empty means
	// "token ".
	ContentChunk string
}

// Server is an http.Handler serving one Config. It counts the requests it
// has answered so a harness can prove every request really reached it.
type Server struct {
	cfg     Config
	calls   atomic.Int64
	streams atomic.Int64
}

// New returns a Server for cfg.
func New(cfg Config) *Server {
	if cfg.StreamChunks < 1 {
		cfg.StreamChunks = 1
	}
	if cfg.ContentChunk == "" {
		cfg.ContentChunk = "token "
	}
	return &Server{cfg: cfg}
}

// Calls is the number of chat-completion requests answered (buffered and
// streamed); Streams is the streamed subset.
func (s *Server) Calls() int64   { return s.calls.Load() }
func (s *Server) Streams() int64 { return s.streams.Load() }

type request struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// ServeHTTP answers POST requests whose path ends in /chat/completions.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	n := s.calls.Add(1)
	id := fmt.Sprintf("chatcmpl-bench-%d", n)
	if !sleep(r.Context(), s.cfg.Latency) {
		return // client gone
	}
	if req.Stream {
		s.streams.Add(1)
		s.serveStream(r.Context(), w, id, req.Model)
		return
	}
	s.serveBuffered(w, id, req.Model)
}

func (s *Server) serveBuffered(w http.ResponseWriter, id, model string) {
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": strings.Repeat(s.cfg.ContentChunk, s.cfg.StreamChunks)},
			"finish_reason": "stop",
		}},
		"usage": s.usage(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) serveStream(ctx context.Context, w http.ResponseWriter, id, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	created := time.Now().Unix()
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}
	write := func(v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !write(chunk(map[string]any{"role": "assistant"}, nil)) {
		return
	}
	for i := 0; i < s.cfg.StreamChunks; i++ {
		if i > 0 && !sleep(ctx, s.cfg.ChunkInterval) {
			return
		}
		if !write(chunk(map[string]any{"content": s.cfg.ContentChunk}, nil)) {
			return
		}
	}
	if !write(chunk(map[string]any{}, "stop")) {
		return
	}
	if !write(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{}, "usage": s.usage()}) {
		return
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) usage() map[string]int {
	return map[string]int{
		"prompt_tokens":     s.cfg.PromptTokens,
		"completion_tokens": s.cfg.CompletionTokens,
		"total_tokens":      s.cfg.PromptTokens + s.cfg.CompletionTokens,
	}
}

// sleep waits d or until ctx is done; it reports whether the wait completed.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
