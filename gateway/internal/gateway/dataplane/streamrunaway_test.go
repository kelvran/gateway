package dataplane

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// TestStreamRunawayCharsCeiling pins streamRunawayCharsCeiling's two
// branches against its own documented constants, so a future accidental
// change to any of the three (streamRunawayCharsPerToken,
// streamRunawayMaxTokensMultiplier, streamRunawayAbsoluteCharsCeiling) is
// caught here first, before it's ever observed indirectly through a live
// streaming test.
func TestStreamRunawayCharsCeiling(t *testing.T) {
	tests := []struct {
		name      string
		maxTokens *int
		want      int
	}{
		{"nil MaxTokens falls back to the absolute ceiling", nil, streamRunawayAbsoluteCharsCeiling},
		{"zero MaxTokens falls back to the absolute ceiling", intPtr(0), streamRunawayAbsoluteCharsCeiling},
		{"negative MaxTokens falls back to the absolute ceiling", intPtr(-1), streamRunawayAbsoluteCharsCeiling},
		{"positive MaxTokens scales by chars-per-token * multiplier", intPtr(5), 5 * streamRunawayCharsPerToken * streamRunawayMaxTokensMultiplier},
		{"a larger positive MaxTokens scales the same way", intPtr(1000), 1000 * streamRunawayCharsPerToken * streamRunawayMaxTokensMultiplier},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := streamRunawayCharsCeiling(tt.maxTokens); got != tt.want {
				t.Errorf("streamRunawayCharsCeiling(%v) = %d, want %d", tt.maxTokens, got, tt.want)
			}
		})
	}
}

func intPtr(n int) *int { return &n }

// runawaySSEReader generates an effectively unbounded, OpenAI-shaped SSE
// stream, one fixed-size content frame at a time, governed ENTIRELY by
// ctx -- exactly like a real upstream HTTP response body, whose Read is
// interrupted the moment its own request context is canceled (see
// NewHTTPUpstreamStreamCaller's streamCtx/idleTimeoutReader). It has no
// content-based termination condition of its own, only maxFrames -- a
// safety net set far above anything a working runaway guard should ever
// let through, so a genuinely broken guard makes this reader run until the
// TEST's own bounded outer context (not this reader) times out, rather
// than hanging forever.
type runawaySSEReader struct {
	ctx        context.Context
	chunkChars int
	maxFrames  int

	mu           sync.Mutex
	framesServed int
	pending      []byte
}

func newRunawaySSEReader(ctx context.Context, chunkChars, maxFrames int) *runawaySSEReader {
	return &runawaySSEReader{ctx: ctx, chunkChars: chunkChars, maxFrames: maxFrames}
}

func (r *runawaySSEReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(r.pending) == 0 {
		r.mu.Lock()
		if r.framesServed >= r.maxFrames {
			r.mu.Unlock()
			return 0, io.EOF
		}
		r.framesServed++
		r.mu.Unlock()
		content := strings.Repeat("x", r.chunkChars)
		r.pending = []byte(`data: {"id":"chatcmpl-runaway","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"` + content + `"},"finish_reason":null}]}` + "\n\n")
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *runawaySSEReader) Close() error { return nil }

func (r *runawaySSEReader) FramesServed() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.framesServed
}

// TestHandleChatCompletionStreamRunawayGuardCutsOffExcessiveCompletion is
// the load-bearing proof for the mid-stream runaway-completion guard
// (streamrunaway.go / streaming.go's streamDeployment): a mock upstream
// willing to stream far beyond any legitimate completion length must be
// cut off well short of that, with a client-facing stream that ends
// cleanly (no error, no hang, a real [DONE]-equivalent close via
// finishStreamedResponse), and with the upstream connection's own context
// genuinely canceled -- not merely "the call returned."
func TestHandleChatCompletionStreamRunawayGuardCutsOffExcessiveCompletion(t *testing.T) {
	const chunkChars = 50
	const maxFrames = 1_000_000 // far more than any working guard should ever let through

	var logBuf bytes.Buffer
	var capturedReader *runawaySSEReader
	var upstreamCtxAtCallTime context.Context

	keys := []identity.VirtualKey{
		{ID: "runaway-key", KeyHash: testHashOf("runaway-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream should never be called by a streaming test")
			return nil, nil
		},
		UpstreamStream: func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
			upstreamCtxAtCallTime = ctx
			capturedReader = newRunawaySSEReader(ctx, chunkChars, maxFrames)
			return capturedReader, nil
		},
		Logger: slog.New(slog.NewJSONHandler(&logBuf, nil)),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	// Bounded outer timeout, per this test's own sanity-check discipline:
	// if the runaway guard were broken/disabled, runawaySSEReader would
	// keep producing frames indefinitely (up to maxFrames), and this is
	// what keeps that failure mode from hanging the whole suite. A
	// working guard trips within a handful of 50-byte frames, so 5s is
	// generous headroom, not a tight bound this test depends on for its
	// own success.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// ceiling = 5 * streamRunawayCharsPerToken(4) * streamRunawayMaxTokensMultiplier(10) = 200 chars.
	maxTokens := 5
	rec := httptest.NewRecorder()
	err = p.HandleChatCompletionStream(ctx, "Bearer runaway-secret", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, MaxTokens: &maxTokens,
		Messages: []adapter.Message{{Role: "user", Content: "generate as much as you possibly can"}},
	}, rec)
	if err != nil {
		t.Fatalf("HandleChatCompletionStream: %v, want nil -- a runaway cutoff must finish as a normal, truncated-but-valid stream, not an error", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("outer test context itself expired (%v) -- the guard did not trip early enough to prove real, fast cancellation independent of this test's own bounded timeout", ctx.Err())
	}

	if capturedReader == nil {
		t.Fatal("UpstreamStream was never called")
	}
	framesServed := capturedReader.FramesServed()
	if framesServed >= maxFrames {
		t.Fatalf("framesServed = %d, want far fewer than maxFrames(%d) -- the runaway guard did not cut the upstream reader off at all", framesServed, maxFrames)
	}
	if framesServed > 20 {
		t.Fatalf("framesServed = %d, want a small number close to the ceiling-crossing point (5 frames of %d chars each crosses the 200-char ceiling) -- guard tripped far later than expected", framesServed, chunkChars)
	}

	body := rec.Body.String()
	// Every content char this mock ever emits is literally 'x' (see
	// runawaySSEReader), and 'x' never appears anywhere else in this
	// fixture's JSON structure (ids/models/field names) -- a safe,
	// direct proxy for "how many content characters actually reached the
	// client."
	gotContentChars := strings.Count(body, "x")
	wantUpstreamWillingToSendChars := maxFrames * chunkChars
	if gotContentChars >= wantUpstreamWillingToSendChars {
		t.Fatalf("client received %d content chars; upstream was willing to send up to %d -- guard did not reduce what reached the client", gotContentChars, wantUpstreamWillingToSendChars)
	}
	if gotContentChars > 200+2*chunkChars {
		t.Fatalf("client received %d content chars, want at most a couple of chunks past the 200-char ceiling", gotContentChars)
	}
	if gotContentChars == 0 {
		t.Fatal("client received zero content chars -- the guard must still deliver everything accumulated before the cutoff, not withhold it")
	}

	// The connection to upstream was genuinely canceled -- checked via the
	// mock's OWN observed request context (upstreamCtxAtCallTime), which
	// is a context this pipeline created SPECIFICALLY for this one
	// upstream call (see streaming.go's streamDeployment upstreamCtx
	// comment) and is NOT the outer ctx this test passed in (which is
	// still live, per the ctx.Err() == nil check above) -- proving a
	// real, separate cancellation happened, not just "the call returned"
	// or "the whole request context was torn down."
	if upstreamCtxAtCallTime == nil {
		t.Fatal("UpstreamStreamCaller's own ctx argument was never captured")
	}
	if upstreamCtxAtCallTime.Err() == nil {
		t.Fatal("the upstream call's own context was never canceled -- the runaway guard did not genuinely cancel the upstream connection")
	}
	// Directly proves the SAME reader object under test is the one whose
	// context got canceled: reading from it again, after the whole call
	// already returned, must observe that cancellation, not silently
	// hand back yet another fresh frame.
	if _, readErr := capturedReader.Read(make([]byte, 64)); readErr == nil {
		t.Fatal("reading from the captured upstream reader again after the call returned still succeeded -- its context was not really canceled")
	}

	if !strings.Contains(logBuf.String(), "streaming_runaway_guard_triggered") {
		t.Fatalf("expected a streaming_runaway_guard_triggered warning log line, got: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"key_id":"runaway-key"`) {
		t.Fatalf("runaway guard log line missing the virtual key ID: %s", logBuf.String())
	}
}

// TestHandleChatCompletionStreamRunawayGuardUnaffectedForOrdinaryStream
// proves the new per-chunk ceiling check the runaway guard adds has zero
// effect on an ordinary, well-within-ceiling stream: every chunk still
// reaches the client, in order, and the stream still ends via the normal
// [DONE] sentinel, byte-for-byte as if the guard didn't exist.
func TestHandleChatCompletionStreamRunawayGuardUnaffectedForOrdinaryStream(t *testing.T) {
	// 20 content chunks of 20 chars each = 400 chars total, comfortably
	// under the 2000-char ceiling a MaxTokens of 50 computes to
	// (50 * streamRunawayCharsPerToken(4) * streamRunawayMaxTokensMultiplier(10)).
	const numChunks = 20
	const chunkChars = 20
	contentPiece := strings.Repeat("y", chunkChars)

	var sb strings.Builder
	sb.WriteString(`data: {"id":"chatcmpl-ordinary","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n")
	for i := 0; i < numChunks; i++ {
		sb.WriteString(`data: {"id":"chatcmpl-ordinary","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"` + contentPiece + `"},"finish_reason":null}]}` + "\n\n")
	}
	sb.WriteString(`data: {"id":"chatcmpl-ordinary","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")
	sb.WriteString(`data: {"id":"chatcmpl-ordinary","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":20,"total_tokens":25}}` + "\n\n")
	sb.WriteString("data: [DONE]\n\n")
	ordinaryStream := sb.String()

	var upstreamCalls int
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		upstreamCalls++
		return nopCloserReader{strings.NewReader(ordinaryStream)}, nil
	}, nil, adapter.Registry{"openai": openai.New()})

	maxTokens := 50
	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true, MaxTokens: &maxTokens,
		Messages: []adapter.Message{{Role: "user", Content: "write something of moderate length"}},
	}, rec)
	if err != nil {
		t.Fatalf("HandleChatCompletionStream: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1", upstreamCalls)
	}

	body := rec.Body.String()
	gotContentOccurrences := strings.Count(body, `"content":"`+contentPiece+`"`)
	if gotContentOccurrences != numChunks {
		t.Fatalf("client received %d of %d content chunks -- the runaway guard incorrectly cut off an ordinary, well-within-ceiling stream", gotContentOccurrences, numChunks)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("body missing finish_reason -- stream ended abnormally: %s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("body does not end with the [DONE] sentinel -- the runaway guard incorrectly truncated an ordinary stream: %s", body)
	}
}

// runawayBedrockReader is runawaySSEReader's binary-framed sibling for
// streamDeploymentBedrock's separate ConverseStream decode path (per
// docs/rfcs/2026-09-04-bedrock-converse-stream.md, that path is genuinely
// different code from streamDeployment's SSE loop, not a shared
// implementation -- so the runaway guard's wiring into it needs its own,
// separately-verified proof, not just symmetry-by-code-reading). Each
// frame is a real, wire-accurate contentBlockDelta event, encoded via the
// same eventstream.Encoder bedrock_stream_test.go's own fixtures use --
// bedrock.StreamDecoder is stateless (per its own doc comment), so a
// standalone repeating contentBlockDelta sequence with no messageStart/
// messageStop framing decodes exactly as a real one would.
type runawayBedrockReader struct {
	ctx        context.Context
	chunkChars int
	maxFrames  int

	mu           sync.Mutex
	framesServed int
	pending      []byte
}

func newRunawayBedrockReader(ctx context.Context, chunkChars, maxFrames int) *runawayBedrockReader {
	return &runawayBedrockReader{ctx: ctx, chunkChars: chunkChars, maxFrames: maxFrames}
}

func (r *runawayBedrockReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(r.pending) == 0 {
		r.mu.Lock()
		if r.framesServed >= r.maxFrames {
			r.mu.Unlock()
			return 0, io.EOF
		}
		r.framesServed++
		r.mu.Unlock()

		msg := bedrockWireEvent("contentBlockDelta", fmt.Sprintf(`{"contentBlockIndex":0,"delta":{"text":"%s"}}`, strings.Repeat("x", r.chunkChars)))
		var buf bytes.Buffer
		if err := eventstream.NewEncoder().Encode(&buf, msg); err != nil {
			return 0, fmt.Errorf("encoding runaway bedrock fixture frame: %w", err)
		}
		r.pending = buf.Bytes()
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *runawayBedrockReader) Close() error { return nil }

func (r *runawayBedrockReader) FramesServed() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.framesServed
}

// TestHandleChatCompletionStreamRunawayGuardCutsOffExcessiveCompletionBedrock
// is TestHandleChatCompletionStreamRunawayGuardCutsOffExcessiveCompletion's
// proof for streamDeploymentBedrock's separate binary-framed decode loop --
// per the task's own instruction not to assume the SSE path's fix covers
// Bedrock's genuinely different code path without checking.
func TestHandleChatCompletionStreamRunawayGuardCutsOffExcessiveCompletionBedrock(t *testing.T) {
	const chunkChars = 50
	const maxFrames = 1_000_000

	var logBuf bytes.Buffer
	var capturedReader *runawayBedrockReader
	var upstreamCtxAtCallTime context.Context

	authHeader := "Bearer " + "runaway-bedrock-cred"
	keys := []identity.VirtualKey{
		{ID: "runaway-bedrock-key", KeyHash: testHashOf("runaway-bedrock-cred"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "claude-bedrock", Provider: "bedrock", UpstreamModel: "anthropic.claude-3-5-sonnet-20241022-v2:0", BaseURL: "http://unused"}}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"bedrock": bedrock.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream should never be called by a streaming test")
			return nil, nil
		},
		UpstreamStream: func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
			upstreamCtxAtCallTime = ctx
			capturedReader = newRunawayBedrockReader(ctx, chunkChars, maxFrames)
			return capturedReader, nil
		},
		Logger: slog.New(slog.NewJSONHandler(&logBuf, nil)),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// ceiling = 5 * streamRunawayCharsPerToken(4) * streamRunawayMaxTokensMultiplier(10) = 200 chars.
	maxTokens := 5
	rec := httptest.NewRecorder()
	err = p.HandleChatCompletionStream(ctx, authHeader, adapter.ChatRequest{
		Model: "claude-bedrock", Stream: true, MaxTokens: &maxTokens,
		Messages: []adapter.Message{{Role: "user", Content: "generate as much as you possibly can"}},
	}, rec)
	if err != nil {
		t.Fatalf("HandleChatCompletionStream: %v, want nil -- a runaway cutoff must finish as a normal, truncated-but-valid stream, not an error", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("outer test context itself expired (%v) -- the guard did not trip early enough to prove real, fast cancellation independent of this test's own bounded timeout", ctx.Err())
	}

	if capturedReader == nil {
		t.Fatal("UpstreamStream was never called")
	}
	framesServed := capturedReader.FramesServed()
	if framesServed >= maxFrames {
		t.Fatalf("framesServed = %d, want far fewer than maxFrames(%d) -- the runaway guard did not cut the bedrock upstream reader off at all", framesServed, maxFrames)
	}
	if framesServed > 20 {
		t.Fatalf("framesServed = %d, want a small number close to the ceiling-crossing point (5 frames of %d chars each crosses the 200-char ceiling) -- guard tripped far later than expected", framesServed, chunkChars)
	}

	body := rec.Body.String()
	gotContentChars := strings.Count(body, "x")
	if gotContentChars >= maxFrames*chunkChars {
		t.Fatalf("client received %d content chars; upstream was willing to send up to %d -- guard did not reduce what reached the client", gotContentChars, maxFrames*chunkChars)
	}
	if gotContentChars > 200+2*chunkChars {
		t.Fatalf("client received %d content chars, want at most a couple of chunks past the 200-char ceiling", gotContentChars)
	}
	if gotContentChars == 0 {
		t.Fatal("client received zero content chars -- the guard must still deliver everything accumulated before the cutoff, not withhold it")
	}

	if upstreamCtxAtCallTime == nil {
		t.Fatal("UpstreamStreamCaller's own ctx argument was never captured")
	}
	if upstreamCtxAtCallTime.Err() == nil {
		t.Fatal("the upstream call's own context was never canceled -- the runaway guard did not genuinely cancel the bedrock upstream connection")
	}
	if _, readErr := capturedReader.Read(make([]byte, 64)); readErr == nil {
		t.Fatal("reading from the captured bedrock upstream reader again after the call returned still succeeded -- its context was not really canceled")
	}

	if !strings.Contains(logBuf.String(), "streaming_runaway_guard_triggered") {
		t.Fatalf("expected a streaming_runaway_guard_triggered warning log line, got: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"key_id":"runaway-bedrock-key"`) {
		t.Fatalf("runaway guard log line missing the virtual key ID: %s", logBuf.String())
	}
}
