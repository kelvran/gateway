package dataplane

// Streaming request handling, kept in its own file from dataplane.go's
// buffered path per docs/rfcs/2026-09-02-streaming-support.md — the two
// paths share auth/rate-limit/cache-key logic but diverge enough after that
// (a real tee to the client instead of a single return value) that forcing
// them into one function would hurt readability more than sharing helps.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/kelvran/gateway/internal/adapter"
	"github.com/kelvran/gateway/internal/cache"
	"github.com/kelvran/gateway/internal/identity"
	"github.com/kelvran/gateway/internal/streaming"
	"github.com/kelvran/gateway/internal/telemetry"
)

// ErrStreamingNotSupported is returned when a request asks to stream but
// the resolved deployment's provider adapter does not implement
// streaming.StreamingAdapter (Gemini, Bedrock, and openaicompat, in the
// current scaffolding) — never silently falls back to buffering, per
// docs/rfcs/2026-09-02-streaming-support.md's explicit scope boundary.
var ErrStreamingNotSupported = errors.New("dataplane: streaming not supported for this provider")

// ErrStreamingNotConfigured is returned when Config.UpstreamStream was left
// nil (e.g. a test Pipeline built only for the non-streaming path) and a
// cache-miss streaming request needs to actually call upstream. A
// streaming cache HIT still works without this configured, since it never
// touches UpstreamStream at all.
var ErrStreamingNotConfigured = errors.New("dataplane: streaming is not configured for this pipeline")

// UpstreamStreamCaller performs the actual upstream HTTP call for one
// deployment when streaming, returning the raw response body for the
// caller to read as SSE frames — unlike UpstreamCaller, it does not decode
// the body, since the whole point of streaming is to read it incrementally.
// The caller is responsible for closing the returned io.ReadCloser.
type UpstreamStreamCaller func(ctx context.Context, dep Deployment, providerReq any) (io.ReadCloser, error)

// HandleChatCompletionStream runs the streaming request pipeline, writing
// canonical chunks directly to w as they become available (or, on a cache
// hit, synthesized from the cached complete response) and returning once
// the stream is fully written. Cost accounting and structured logging
// still happen exactly once per request, via the same deferred logRequest
// pattern HandleChatCompletion uses, since a streamed generation is just
// as billable as a buffered one.
func (p *Pipeline) HandleChatCompletionStream(ctx context.Context, authorizationHeader string, req adapter.ChatRequest, w http.ResponseWriter) (err error) {
	var (
		cacheHit bool
		resp     adapter.ChatResponse
		vk       *identity.VirtualKey
		dep      Deployment
	)
	ctx, span := telemetry.Tracer.Start(ctx, "chat "+req.Model)
	defer func() {
		p.finalize(ctx, span, vk, dep, req, resp, cacheHit, err)
	}()

	vk, verifyErr := p.verifier.Verify(authorizationHeader)
	if verifyErr != nil {
		err = fmt.Errorf("dataplane: auth: %w", verifyErr)
		return
	}
	if !isModelAllowed(vk, req.Model) {
		err = fmt.Errorf("%w: %q", ErrModelNotAllowed, req.Model)
		return
	}
	if !p.limiterFor(vk.ID).Allow() {
		err = ErrRateLimited
		return
	}
	if !p.budget.Allow(vk.ID, vk.BudgetUSD) {
		err = ErrBudgetExceeded
		return
	}

	sw, swErr := streaming.NewWriter(w)
	if swErr != nil {
		err = fmt.Errorf("dataplane: stream: %w", swErr)
		return
	}

	key := cache.Key(vk.ID, req.Model, serializeMessages(req.Messages), req.Temperature, req.MaxTokens)

	if cached, ok, getErr := p.cache.Get(ctx, key); getErr == nil && ok {
		var cachedResp adapter.ChatResponse
		if unmarshalErr := json.Unmarshal(cached, &cachedResp); unmarshalErr == nil {
			resp = cachedResp
			cacheHit = true
			err = writeFakeStream(sw, cachedResp)
			return
		}
		// A corrupt cache entry is treated as a miss, not a request
		// failure — same fallthrough behavior as the buffered path.
	}

	if p.upstreamStream == nil {
		err = ErrStreamingNotConfigured
		return
	}

	var found bool
	dep, found = p.nextDeployment(req.Model)
	if !found {
		err = fmt.Errorf("dataplane: no deployment configured for model %q", req.Model)
		return
	}

	resp, dep, err = p.streamDeploymentWithFallback(ctx, dep, req, sw)
	if err != nil {
		err = fmt.Errorf("dataplane: streaming upstream call failed for model %q: %w", req.Model, err)
		return
	}

	if encoded, marshalErr := json.Marshal(resp); marshalErr == nil {
		_ = p.cache.Put(ctx, key, encoded, p.cacheTTL)
	}
	return
}

// writeFakeStream synthesizes a stream from an already-known, complete
// response — the cache-hit path. It is explicitly a synthesis of the
// already-known answer, not a re-play of the original token timing (see
// the RFC's Unresolved Questions: chunking the content more finely to
// mimic real streaming UX is a possible future refinement, not required
// for correctness).
func writeFakeStream(sw *streaming.Writer, resp adapter.ChatResponse) error {
	for _, c := range resp.Choices {
		finishReason := c.FinishReason
		chunk := streaming.ChatCompletionChunk{
			ID:    resp.ID,
			Model: resp.Model,
			Choices: []streaming.ChunkChoice{{
				Index: c.Index,
				Delta: streaming.MessageDelta{
					Role:      c.Message.Role,
					Content:   c.Message.Content,
					ToolCalls: toChunkToolCallDeltas(c.Message.ToolCalls),
				},
				FinishReason: &finishReason,
			}},
		}
		if err := sw.WriteChunk(chunk); err != nil {
			return fmt.Errorf("writing fake-streamed chunk: %w", err)
		}
	}

	usage := resp.Usage
	if err := sw.WriteChunk(streaming.ChatCompletionChunk{ID: resp.ID, Model: resp.Model, Usage: &usage}); err != nil {
		return fmt.Errorf("writing fake-streamed usage chunk: %w", err)
	}
	if err := sw.WriteDone(); err != nil {
		return fmt.Errorf("writing fake-streamed done sentinel: %w", err)
	}
	return nil
}

func toChunkToolCallDeltas(toolCalls []adapter.ToolCall) []streaming.ToolCallDelta {
	if len(toolCalls) == 0 {
		return nil
	}
	deltas := make([]streaming.ToolCallDelta, 0, len(toolCalls))
	for i, tc := range toolCalls {
		deltas = append(deltas, streaming.ToolCallDelta{
			Index:         i,
			ID:            tc.ID,
			Name:          tc.Name,
			ArgumentsJSON: tc.ArgumentsJSON,
		})
	}
	return deltas
}

// streamDeploymentWithFallback attempts dep first; if it fails before any
// chunk has reached the client, it falls back to the next deployment for
// the same model exactly like the buffered path's single-fallback rule.
// Once a chunk has been written to the client, no fallback is attempted —
// per the RFC's explicit scope boundary, there is no clean way to retry a
// partially-delivered stream without risking duplicated content.
func (p *Pipeline) streamDeploymentWithFallback(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw *streaming.Writer) (adapter.ChatResponse, Deployment, error) {
	var firstChunkSent bool

	resp, err := p.streamDeployment(ctx, dep, req, sw, &firstChunkSent)
	if err != nil && !firstChunkSent {
		if fallbackDep, hasFallback := p.nextDeployment(req.Model); hasFallback && fallbackDep.Name != dep.Name {
			dep = fallbackDep
			resp, err = p.streamDeployment(ctx, dep, req, sw, &firstChunkSent)
		}
	}
	return resp, dep, err
}

// streamDeployment runs the streaming-specific adapter+upstream-call steps
// for one deployment: canonical -> provider-native (ToProvider, Stream
// forced true) -> upstream streaming call -> StreamDecoder.Decode per raw
// SSE event -> tee (write to client via sw AND accumulate into acc for
// cache write-back), exactly per the RFC's dataplane wiring design.
// *firstChunkSent is set to true the moment any chunk is successfully
// written to the client, so the caller can enforce the fallback rule above.
func (p *Pipeline) streamDeployment(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw *streaming.Writer, firstChunkSent *bool) (adapter.ChatResponse, error) {
	a, ok := p.adapters[dep.Provider]
	if !ok {
		return adapter.ChatResponse{}, fmt.Errorf("no adapter registered for provider %q", dep.Provider)
	}
	streamAdapter, ok := a.(streaming.StreamingAdapter)
	if !ok {
		return adapter.ChatResponse{}, fmt.Errorf("%w: provider %q", ErrStreamingNotSupported, dep.Provider)
	}

	upstreamReq := req
	upstreamReq.Model = dep.UpstreamModel
	upstreamReq.Stream = true

	providerReq, err := streamAdapter.ToProvider(upstreamReq)
	if err != nil {
		return adapter.ChatResponse{}, fmt.Errorf("adapter %q ToProvider: %w", dep.Provider, err)
	}

	body, err := p.upstreamStream(ctx, dep, providerReq)
	if err != nil {
		return adapter.ChatResponse{}, fmt.Errorf("upstream stream call to deployment %q: %w", dep.Name, err)
	}
	defer func() { _ = body.Close() }()

	decoder := streamAdapter.NewStreamDecoder()
	reader := streaming.NewReader(body)
	acc := newStreamAccumulator()
	var finalUsage *adapter.Usage

	for {
		ev, readErr := reader.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return adapter.ChatResponse{}, fmt.Errorf("reading stream from deployment %q: %w", dep.Name, readErr)
		}

		chunks, done, usage, decErr := decoder.Decode(ev)
		if decErr != nil {
			return adapter.ChatResponse{}, fmt.Errorf("decoding stream from deployment %q: %w", dep.Name, decErr)
		}
		if usage != nil {
			finalUsage = usage
		}
		for _, c := range chunks {
			acc.add(c)
			if writeErr := sw.WriteChunk(c); writeErr != nil {
				return adapter.ChatResponse{}, fmt.Errorf("writing streamed chunk to client: %w", writeErr)
			}
			*firstChunkSent = true
		}
		if done {
			break
		}
	}

	var usage adapter.Usage
	if finalUsage != nil {
		usage = *finalUsage
	} else {
		// Per the RFC's Cost Accounting section: a provider stream that
		// never sends usage does not fail the request, but must not be
		// silently unmetered either — a zero-usage entry is recorded and
		// flagged loudly here so it's visible in logs, not just absent.
		p.logger.Warn("stream_missing_usage",
			"deployment", dep.Name,
			"provider", dep.Provider,
			"model", req.Model,
		)
	}

	resp := acc.build(usage)
	// Echo back the client-facing canonical model name, matching
	// callDeployment's convention for the buffered path.
	resp.Model = req.Model

	if err := sw.WriteDone(); err != nil {
		return adapter.ChatResponse{}, fmt.Errorf("writing done sentinel for deployment %q: %w", dep.Name, err)
	}

	return resp, nil
}
