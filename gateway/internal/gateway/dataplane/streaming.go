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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/gateway/internal/cache"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/streaming"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// ErrStreamingNotSupported is returned when a request asks to stream but
// the resolved deployment's provider adapter does not implement
// streaming.StreamingAdapter -- every provider currently registered in
// cmd/gateway/main.go (openai, anthropic, gemini, openaicompat, and
// bedrock via its own binary-framed streamDeploymentBedrock path) does, so
// this only fires for a future provider added without a streaming
// implementation, or a misconfigured registry entry for "bedrock" whose
// value isn't *bedrock.Adapter. Never silently falls back to buffering,
// per docs/rfcs/2026-09-02-streaming-support.md's explicit scope boundary.
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
		cacheInfo             cacheProvenance
		resp                  adapter.ChatResponse
		vk                    *identity.VirtualKey
		dep                   Deployment
		rateLimitFailedOpen   bool
		fallback              fallbackInfo
		budgetSpentAtDecision decimal.Decimal
		billable              bool
		// See HandleChatCompletion's identical fields: checkRateLimit's/
		// budget.Reserve's own return values, threaded through to
		// finalize's ReconcileTPM/Reconcile calls on every return path,
		// per docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md.
		tpmReserved       bool
		tpmReservedTokens float64
		budgetReserved    bool
		budgetReservedUSD decimal.Decimal
	)
	start := time.Now()
	ctx, span := telemetry.Tracer.Start(ctx, "chat "+req.Model)
	defer func() {
		// See HandleChatCompletion's identical comment: attachRetryAfter
		// must run before finalize, per
		// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md's design
		// (a).
		err = p.attachRetryAfter(vk, err)
		p.finalize(ctx, span, vk, dep, req, resp, cacheInfo, rateLimitFailedOpen, fallback, budgetSpentAtDecision, billable, budgetReserved, budgetReservedUSD, tpmReserved, tpmReservedTokens, err, time.Since(start))
	}()

	vk, verifyErr := p.verifier.Load().Verify(authorizationHeader)
	if verifyErr != nil {
		err = fmt.Errorf("dataplane: auth: %w", verifyErr)
		return
	}
	if !isModelAllowed(vk, req.Model) {
		err = fmt.Errorf("%w: %q", ErrModelNotAllowed, req.Model)
		return
	}
	var rateLimitOK bool
	rateLimitOK, rateLimitFailedOpen, tpmReserved, tpmReservedTokens = p.checkRateLimit(ctx, vk, req.Model)
	if !rateLimitOK {
		err = ErrRateLimited
		return
	}
	// Per-identity concurrency cap — see HandleChatCompletion's identical
	// block for the full rationale. Both the buffered and streaming paths
	// need this, or the cap would be trivially bypassed by setting
	// "stream": true.
	if !p.checkConcurrency(vk) {
		err = ErrConcurrencyLimitExceeded
		return
	}
	defer p.releaseConcurrency(vk)

	budgetSpentAtDecision = p.budget.SpentUSD(vk.ID, vk.BudgetResetInterval)
	var budgetOK bool
	budgetOK, budgetReserved, budgetReservedUSD = p.budget.Reserve(vk.ID, vk.BudgetUSD, vk.BudgetResetInterval)
	if !budgetOK {
		err = ErrBudgetExceeded
		return
	}

	sw, swErr := streaming.NewWriter(w)
	if swErr != nil {
		err = fmt.Errorf("dataplane: stream: %w", swErr)
		return
	}

	l1Key := cache.Key(vk.ID, req.Model, serializeMessages(req.Messages), req.Temperature, req.MaxTokens, p.guardrails.Version())
	l2Key := cache.NormalizedKey(vk.ID, req.Model, normalizeMessages(req.Messages), req.Temperature, req.MaxTokens, p.guardrails.Version())
	l3Signature := cache.MinHashSignature(cache.Shingles(normalizeMessages(req.Messages), l3ShingleWords), l3SignatureSize)

	if cached, layer, writtenAt, ok := p.checkCache(ctx, vk.ID, l1Key, l2Key); ok {
		var cachedResp adapter.ChatResponse
		if unmarshalErr := json.Unmarshal(cached, &cachedResp); unmarshalErr == nil {
			resp = cachedResp
			cacheInfo = cacheProvenance{Layer: layer, AgeMs: float64(time.Since(writtenAt).Milliseconds())}
			err = writeFakeStream(sw, cachedResp)
			return
		}
		// A corrupt cache entry is treated as a miss, not a request
		// failure — same fallthrough behavior as the buffered path.
	}

	if cached, similarity, ageMs, ok := p.checkLexicalCache(ctx, vk, req, l1Key, l3Signature); ok {
		var cachedResp adapter.ChatResponse
		if unmarshalErr := json.Unmarshal(cached, &cachedResp); unmarshalErr == nil {
			resp = cachedResp
			cacheInfo = cacheProvenance{Layer: "L3", Similarity: similarity, AgeMs: ageMs}
			err = writeFakeStream(sw, cachedResp)
			return
		}
		// A corrupt cache entry is treated as a miss, not a request
		// failure — same fallthrough behavior as the buffered path.
	}

	// Guardrail pre-call — identical position and reasoning to the
	// buffered path (dataplane.go's HandleChatCompletion): after L1/L2/L3
	// all miss, before the router. The request text is fully known here
	// regardless of streaming/buffered, so there is no half-formed-
	// content problem on the input side.
	if verdict := p.guardrails.Check(ctx, serializeMessages(req.Messages)); verdict.Blocked {
		p.logger.Warn("guardrail_blocked_precall", "key_id", vk.ID, "finding_count", len(verdict.Findings))
		err = ErrGuardrailBlocked
		return
	}

	if p.upstreamStream == nil {
		err = ErrStreamingNotConfigured
		return
	}

	var found bool
	dep, found = p.nextDeployment(req.Model)
	if !found {
		err = fmt.Errorf("%w: %q", ErrNoDeployment, req.Model)
		return
	}

	msr := midStreamReservation{vk: vk, budgetReservedUSD: &budgetReservedUSD, tpmReservedTokens: &tpmReservedTokens}
	resp, dep, fallback, err = p.streamDeploymentWithFallback(ctx, dep, req, sw, vk.ID, msr)
	if err != nil {
		err = fmt.Errorf("dataplane: streaming upstream call failed for model %q: %w", req.Model, err)
		return
	}
	// The streaming path has no singleflight coalescing (unlike
	// runMissPath's buffered miss path) — every completed stream is its
	// own real, unshared upstream call, so it's always billable, per
	// docs/rfcs/2026-09-05-gateway-cost-double-counting.md.
	billable = true

	if encoded, marshalErr := json.Marshal(resp); marshalErr == nil {
		p.writeCache(ctx, vk.ID, l1Key, l2Key, l3Signature, Fingerprint(req.Messages), req.Model, encoded)
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
// chunk has reached the client, it walks the same error-classified,
// multi-hop fallback chain runMissPath uses (dataplane.go's fallback.go),
// or — when dep has no fallback_chains configured at all — falls back to
// the next deployment for the same model exactly like the buffered
// path's pre-existing single-fallback rule. Once a chunk has been
// written to the client, no further hop is attempted at all — checked
// before EVERY hop, not only the first — per the RFC's explicit scope
// boundary, there is no clean way to retry a partially-delivered stream
// without risking duplicated content. The returned fallbackInfo stays
// its zero value (happened: false) in that already-streamed case, even
// though err is still non-nil — a streaming response that errored after
// its first chunk did NOT fall back, and the two must never be conflated
// in GatewayDecisionEvent.
//
// keyID (vk.ID) is threaded through purely so streamDeployment/
// streamDeploymentBedrock can attribute the mid-stream runaway-completion
// guard's "streaming_runaway_guard_triggered" warning log (see
// streamrunaway.go) to the virtual key whose stream was cut off — it has
// no effect on fallback routing, error classification, or anything else
// this function already did. msr carries the SAME request's own
// outstanding budget/TPM reservations (see midStreamReservation's own doc
// comment) — threaded through unchanged across a fallback hop, since a
// reservation made against the client's originally-requested model stays
// that same reservation regardless of which deployment ultimately serves
// the response.
func (p *Pipeline) streamDeploymentWithFallback(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw *streaming.Writer, keyID string, msr midStreamReservation) (adapter.ChatResponse, Deployment, fallbackInfo, error) {
	var firstChunkSent bool
	var fallback fallbackInfo

	resp, err := p.streamDeploymentWithCapacityCheck(ctx, dep, req, sw, &firstChunkSent, keyID, msr)
	if err == nil || firstChunkSent {
		return resp, dep, fallback, err
	}

	originalDep, originalErr := dep, err
	if targets, configured := fallbackTargets(dep, err); configured {
		tried := map[string]bool{dep.Name: true}
		hopDep, hopResp, hopErr, attempted := p.attemptFallbackChain(ctx, targets, tried,
			func(d Deployment) (adapter.ChatResponse, error) {
				defer p.releaseDeploymentConcurrency(d.Name)
				return p.streamDeployment(ctx, d, req, sw, &firstChunkSent, keyID, msr)
			},
			func() bool { return firstChunkSent },
			func(model string) bool { return p.checkFallbackTargetRateLimit(ctx, keyID, model) },
			func(depName string) bool { return p.checkDeploymentCapacity(ctx, depName) },
		)
		if attempted {
			fallback = fallbackInfo{happened: true, from: originalDep.Name, reason: originalErr.Error()}
			dep, resp, err = hopDep, hopResp, hopErr
		}
	} else if fallbackDep, hasFallback := p.nextDeployment(req.Model); hasFallback && fallbackDep.Name != dep.Name {
		fallback = fallbackInfo{happened: true, from: dep.Name, reason: err.Error()}
		dep = fallbackDep
		resp, err = p.streamDeploymentWithCapacityCheck(ctx, dep, req, sw, &firstChunkSent, keyID, msr)
	}
	return resp, dep, fallback, err
}

// streamDeploymentWithCapacityCheck wraps streamDeployment with dep's
// own checkDeploymentCapacity gate and guaranteed release — the
// streaming sibling of callDeploymentWithCapacityCheck (dataplane.go),
// used for EVERY call to a deployment, hop 1 included.
func (p *Pipeline) streamDeploymentWithCapacityCheck(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw *streaming.Writer, firstChunkSent *bool, keyID string, msr midStreamReservation) (adapter.ChatResponse, error) {
	if !p.checkDeploymentRateLimit(ctx, dep.Name) {
		return adapter.ChatResponse{}, &DeploymentCapacityError{Deployment: dep.Name, Reason: "rate_limit"}
	}
	if !p.checkDeploymentConcurrency(dep.Name) {
		return adapter.ChatResponse{}, &DeploymentCapacityError{Deployment: dep.Name, Reason: "concurrency"}
	}
	defer p.releaseDeploymentConcurrency(dep.Name)
	return p.streamDeployment(ctx, dep, req, sw, firstChunkSent, keyID, msr)
}

// streamDeployment runs the streaming-specific adapter+upstream-call steps
// for one deployment: canonical -> provider-native (ToProvider, Stream
// forced true) -> upstream streaming call -> StreamDecoder.Decode per raw
// SSE event -> tee (write to client via sw AND accumulate into acc for
// cache write-back), exactly per the RFC's dataplane wiring design.
// *firstChunkSent is set to true the moment any chunk is successfully
// written to the client, so the caller can enforce the fallback rule above.
//
// keyID is used only by the mid-stream runaway-completion guard below, to
// attribute its warning log to the virtual key whose stream was cut off —
// see streamrunaway.go. msr is that same guard's sibling: this request's
// own outstanding budget/TPM reservations, topped up in place as real
// accumulated output grows past them — see checkMidStreamReservationTopup.
func (p *Pipeline) streamDeployment(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw *streaming.Writer, firstChunkSent *bool, keyID string, msr midStreamReservation) (adapter.ChatResponse, error) {
	if dep.Provider == "bedrock" {
		return p.streamDeploymentBedrock(ctx, dep, req, sw, firstChunkSent, keyID, msr)
	}

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
	upstreamReq.DisableCacheControlAutoPopulate = dep.effectiveCacheControlAutoDisabled()

	providerReq, err := streamAdapter.ToProvider(upstreamReq)
	if err != nil {
		return adapter.ChatResponse{}, fmt.Errorf("adapter %q ToProvider: %w", dep.Provider, err)
	}

	// upstreamCtx is a CHILD of ctx, scoped to exactly this one upstream
	// stream call — deliberately not ctx itself. The mid-stream runaway
	// guard below cancels upstreamCtx (never ctx) once it trips, so it
	// stops/interrupts only the upstream connection/body Read; ctx itself
	// stays live for finishStreamedResponse's guardrail post-call check
	// below and for finalize (dataplane.go, run via defer in the caller),
	// both of which must still run normally on this path. Canceled
	// unconditionally via defer on every return — a no-op by the time a
	// normal, non-runaway completion reaches here, since nothing is left
	// reading from upstreamCtx's request by then.
	upstreamCtx, cancelUpstream := context.WithCancel(ctx)
	defer cancelUpstream()

	body, err := p.upstreamStream(upstreamCtx, dep, providerReq)
	if err != nil {
		return adapter.ChatResponse{}, fmt.Errorf("upstream stream call to deployment %q: %w", dep.Name, err)
	}
	defer func() { _ = body.Close() }()

	decoder := streamAdapter.NewStreamDecoder()
	reader := streaming.NewReader(body)
	acc := newStreamAccumulator()
	var finalUsage *adapter.Usage
	runawayCeiling := streamRunawayCharsCeiling(req.MaxTokens)

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
		// Mid-stream runaway-completion guard — see streamrunaway.go's
		// package doc. Checked once per decoded batch, right after the
		// same acc.add loop that just ran above, using acc's own
		// accumulated-character-length proxy (no real token count is
		// available yet, possibly ever). Tripping this does NOT return an
		// error: the request already legitimately started and passed its
		// own budget/TPM reservation, so it finishes as a normal,
		// truncated-but-valid stream via finishStreamedResponse below —
		// exactly as if the provider itself had simply stopped sending
		// chunks, never classified as an upstream failure.
		accumulatedChars := acc.totalContentLen()
		if accumulatedChars > runawayCeiling {
			p.logger.Warn("streaming_runaway_guard_triggered",
				"key_id", keyID,
				"deployment", dep.Name,
				"provider", dep.Provider,
				"model", req.Model,
				"accumulated_chars", accumulatedChars,
				"ceiling_chars", runawayCeiling,
			)
			cancelUpstream()
			break
		}
		// Mid-stream reservation top-up guard — see
		// checkMidStreamReservationTopup's own doc comment
		// (streamrunaway.go). Distinct from the runaway-completion
		// ceiling check above: that guards against an unreasonably LONG
		// completion regardless of budget; this guards against THIS
		// stream's own growing real cost silently exceeding what it
		// reserved, which a concurrent sibling on the same key would
		// otherwise never see. Same non-error, graceful-truncation
		// treatment on trip.
		if !p.checkMidStreamReservationTopup(dep, req, accumulatedChars, msr) {
			p.logger.Warn("streaming_midstream_reservation_topup_exhausted",
				"key_id", keyID,
				"deployment", dep.Name,
				"provider", dep.Provider,
				"model", req.Model,
				"accumulated_chars", accumulatedChars,
			)
			cancelUpstream()
			break
		}
		if done {
			break
		}
	}

	return p.finishStreamedResponse(ctx, dep, req, sw, acc, finalUsage)
}

// streamDeploymentBedrock is streamDeployment's Bedrock-specific sibling:
// Bedrock's ConverseStream wire format is binary
// (application/vnd.amazon.eventstream), not the SSE framing every other
// streaming provider uses, so it cannot be driven by streamDeployment's
// streaming.Reader loop above -- see
// docs/rfcs/2026-09-04-bedrock-converse-stream.md's Detailed Design for why
// a shared interface across both framings isn't warranted for a single
// binary-framed implementor. Everything after decoding (accumulation,
// client tee, final-response assembly) is identical, via the shared
// finishStreamedResponse.
func (p *Pipeline) streamDeploymentBedrock(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw *streaming.Writer, firstChunkSent *bool, keyID string, msr midStreamReservation) (adapter.ChatResponse, error) {
	a, ok := p.adapters[dep.Provider]
	if !ok {
		return adapter.ChatResponse{}, fmt.Errorf("no adapter registered for provider %q", dep.Provider)
	}
	bedrockAdapter, ok := a.(*bedrock.Adapter)
	if !ok {
		return adapter.ChatResponse{}, fmt.Errorf("%w: provider %q", ErrStreamingNotSupported, dep.Provider)
	}

	upstreamReq := req
	upstreamReq.Model = dep.UpstreamModel
	upstreamReq.Stream = true
	upstreamReq.DisableCacheControlAutoPopulate = dep.effectiveCacheControlAutoDisabled()

	providerReq, err := bedrockAdapter.ToProvider(upstreamReq)
	if err != nil {
		return adapter.ChatResponse{}, fmt.Errorf("adapter %q ToProvider: %w", dep.Provider, err)
	}

	// See streamDeployment's identical upstreamCtx comment: scoped to
	// exactly this upstream call, canceled by the runaway guard below
	// without ever touching ctx itself.
	upstreamCtx, cancelUpstream := context.WithCancel(ctx)
	defer cancelUpstream()

	body, err := p.upstreamStream(upstreamCtx, dep, providerReq)
	if err != nil {
		return adapter.ChatResponse{}, fmt.Errorf("upstream stream call to deployment %q: %w", dep.Name, err)
	}
	defer func() { _ = body.Close() }()

	decoder := bedrock.NewStreamDecoder()
	eventDecoder := eventstream.NewDecoder()
	acc := newStreamAccumulator()
	var finalUsage *adapter.Usage
	runawayCeiling := streamRunawayCharsCeiling(req.MaxTokens)

	// payloadBuf is reused across Decode calls per eventstream.Decoder's own
	// doc comment -- safe because each message's Payload is fully consumed
	// (unmarshaled by decoder.Decode, below) before the next Decode call
	// overwrites the same backing array.
	var payloadBuf []byte
	for {
		msg, readErr := eventDecoder.Decode(body, payloadBuf)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return adapter.ChatResponse{}, fmt.Errorf("reading binary event stream from deployment %q: %w", dep.Name, readErr)
		}
		payloadBuf = msg.Payload

		chunks, usage, decErr := decoder.Decode(msg)
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
		// See streamDeployment's identical runaway-guard comment/rationale.
		accumulatedChars := acc.totalContentLen()
		if accumulatedChars > runawayCeiling {
			p.logger.Warn("streaming_runaway_guard_triggered",
				"key_id", keyID,
				"deployment", dep.Name,
				"provider", dep.Provider,
				"model", req.Model,
				"accumulated_chars", accumulatedChars,
				"ceiling_chars", runawayCeiling,
			)
			cancelUpstream()
			break
		}
		// See streamDeployment's identical mid-stream reservation
		// top-up guard comment/rationale.
		if !p.checkMidStreamReservationTopup(dep, req, accumulatedChars, msr) {
			p.logger.Warn("streaming_midstream_reservation_topup_exhausted",
				"key_id", keyID,
				"deployment", dep.Name,
				"provider", dep.Provider,
				"model", req.Model,
				"accumulated_chars", accumulatedChars,
			)
			cancelUpstream()
			break
		}
	}

	return p.finishStreamedResponse(ctx, dep, req, sw, acc, finalUsage)
}

// finishStreamedResponse builds the final ChatResponse from an
// accumulator and finalUsage, runs the audit-only post-call guardrail
// check, and writes the client-facing done sentinel — the provider-
// agnostic tail shared by streamDeployment (SSE-framed providers) and
// streamDeploymentBedrock (binary-framed) per
// docs/rfcs/2026-09-04-bedrock-converse-stream.md: none of this logic
// depends on how chunks actually arrived.
func (p *Pipeline) finishStreamedResponse(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw *streaming.Writer, acc *streamAccumulator, finalUsage *adapter.Usage) (adapter.ChatResponse, error) {
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

	// Guardrail post-call, streaming path — audit-only, per
	// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md's named,
	// accepted residual risk: every chunk has already been flushed to the
	// client (sw.WriteChunk, above) strictly before resp is known here —
	// there is no point in this function where a post-call check can run
	// before content reaches the client without a buffering layer this
	// RFC deliberately does not add. This check can only log, at
	// elevated severity, never withhold what's already been delivered.
	if postVerdict := p.guardrails.Check(ctx, serializeResponse(resp)); postVerdict.Blocked {
		p.logger.Warn("guardrail_blocked_postcall_streaming_audit_only",
			"deployment", dep.Name, "finding_count", len(postVerdict.Findings))
	}

	if err := sw.WriteDone(); err != nil {
		return adapter.ChatResponse{}, fmt.Errorf("writing done sentinel for deployment %q: %w", dep.Name, err)
	}

	return resp, nil
}
