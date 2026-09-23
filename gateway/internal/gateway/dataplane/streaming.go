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
	"github.com/kelvran/gateway/gateway/internal/idempotency"
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

// ErrBedrockStreamTruncated is returned by streamDeploymentBedrock when
// its own eventstream.Decoder.Decode call reports io.EOF before a
// messageStop event was ever observed -- see that call site's own doc
// comment for why bare io.EOF is not a reliable "clean end of stream"
// signal for this specific decoder.
var ErrBedrockStreamTruncated = errors.New("dataplane: bedrock event stream ended before a messageStop event was received")

// maxBedrockStreamBytes bounds a single Bedrock ConverseStream response
// body's total bytes -- see streamDeploymentBedrock's own doc comment on
// why this exists (aws-sdk-go-v2's eventstream.Decoder places no upper
// bound on a single frame's declared length). 256MiB is far beyond any
// realistic Converse response (even an unusually large multi-turn/
// tool-heavy conversation's JSON-wrapped event-stream framing) while
// still bounding a pathological input to a known, finite worst case.
const maxBedrockStreamBytes = 256 << 20

// limitedReadCloser wraps an io.ReadCloser with a byte ceiling on Read
// (via io.LimitReader) while still forwarding Close to the original
// closer -- io.LimitReader alone drops the Close method, and body must
// stay a real io.ReadCloser for this function's own deferred Close call.
type limitedReadCloser struct {
	io.Reader
	closer io.Closer
}

func (l *limitedReadCloser) Close() error { return l.closer.Close() }

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
func (p *Pipeline) HandleChatCompletionStream(ctx context.Context, authorizationHeader string, remoteAddr string, req adapter.ChatRequest, w http.ResponseWriter, idempotencyKey string) (err error) {
	var (
		cacheInfo             cacheProvenance
		resp                  adapter.ChatResponse
		vk                    *identity.VirtualKey
		dep                   Deployment
		rateLimitFailedOpen   bool
		fallback              fallbackInfo
		budgetSpentAtDecision decimal.Decimal
		billable              bool
		// idempotencyOwned/idempotencyToken mirror HandleChatCompletion's
		// identical fields — see claimIdempotency's own doc comment.
		idempotencyOwned bool
		idempotencyToken idempotency.Token
		// See HandleChatCompletion's identical fields: checkRateLimit's/
		// budget.Reserve's own return values, threaded through to
		// finalize's ReconcileTPM/Reconcile calls on every return path,
		// per docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md.
		tpmReserved            bool
		tpmReservedTokens      float64
		tpmReservationEpoch    int64
		budgetReserved         bool
		budgetReservedUSD      decimal.Decimal
		budgetReservationEpoch int64
		// cacheAttempted mirrors HandleChatCompletion's identical field —
		// see finalize's own doc comment.
		cacheAttempted bool
		// costEstimated mirrors estimateOrRealUsage's own return value —
		// see finalize's own doc comment for costEstimated. Always false
		// on the buffered path (HandleChatCompletion never estimates).
		costEstimated bool
	)
	start := time.Now()
	ctx, span := telemetry.Tracer.Start(ctx, "chat "+boundedModelForTelemetry(req.Model))
	defer func() {
		// See HandleChatCompletion's identical comment: attachRetryAfter
		// must run before finalize, per
		// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md's design
		// (a).
		err = p.attachRetryAfter(vk, err)
		if idempotencyOwned {
			p.completeIdempotency(ctx, vk.ID, idempotencyKey, idempotencyToken, resp, err)
		}
		p.finalize(ctx, span, vk, dep, req, resp, cacheInfo, rateLimitFailedOpen, fallback, budgetSpentAtDecision, billable, budgetReserved, budgetReservedUSD, budgetReservationEpoch, tpmReserved, tpmReservedTokens, tpmReservationEpoch, cacheAttempted, costEstimated, true, err, time.Since(start))
	}()

	vk, verifyErr := p.verifier.Load().Verify(authorizationHeader)
	if verifyErr != nil {
		err = fmt.Errorf("dataplane: auth: %w", verifyErr)
		return
	}
	if !isSourceIPAllowed(vk, resolveClientIP(remoteAddr)) {
		err = fmt.Errorf("%w: %q", ErrSourceIPNotAllowed, remoteAddr)
		return
	}
	if !isModelAllowed(vk, req.Model) {
		err = fmt.Errorf("%w: %q", ErrModelNotAllowed, req.Model)
		return
	}

	// Claimed here, before resolvePromptIfSet/rate-limit/budget — see
	// HandleChatCompletion's identical comment for the full ordering
	// rationale. streaming.NewWriter has no side effects of its own (only
	// WriteChunk ever writes to w), so constructing sw this early to
	// support a replay's own writeFakeStream call below is safe: no
	// bytes reach the client, and no status code is committed, until
	// something actually calls WriteChunk.
	sw, swErr := streaming.NewWriter(w)
	if swErr != nil {
		err = fmt.Errorf("dataplane: stream: %w", swErr)
		return
	}
	var idempotencyReplay bool
	var idempotencyCachedResp adapter.ChatResponse
	idempotencyOwned, idempotencyReplay, idempotencyCachedResp, idempotencyToken, err = p.claimIdempotency(ctx, vk.ID, idempotencyKey, req)
	if err != nil {
		return
	}
	if idempotencyReplay {
		resp = idempotencyCachedResp
		err = writeFakeStream(sw, idempotencyCachedResp)
		return
	}

	// See HandleChatCompletion's identical block/comment — same
	// placement (after the model-allowlist check, before rate-limiting),
	// same resolvePromptIfSet helper.
	var promptFP string
	req, promptFP, err = p.resolvePromptIfSet(req)
	if err != nil {
		return
	}
	var rateLimitOK bool
	rateLimitOK, rateLimitFailedOpen, tpmReserved, tpmReservedTokens, tpmReservationEpoch = p.checkRateLimit(ctx, vk, req.Model)
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

	budgetSpentAtDecision = p.budget.SpentUSD(ctx, vk.ID, vk.BudgetResetInterval)
	var budgetOK bool
	var budgetErr error
	budgetOK, budgetReserved, budgetReservedUSD, budgetReservationEpoch, budgetErr = p.budget.Reserve(ctx, vk.ID, vk.BudgetUSD, vk.BudgetResetInterval)
	if budgetErr != nil {
		p.logger.Warn("budget_backend_unavailable", append(traceLogFields(ctx), "key_id", vk.ID, "error", budgetErr.Error())...)
		telemetry.RecordBudgetFailOpen(ctx, vk.ID)
		budgetOK, budgetReserved = true, false
	}
	if !budgetOK {
		err = ErrBudgetExceeded
		return
	}

	l1Key := cache.Key(vk.ID, req.Model, serializeMessages(req.Messages), req.Temperature, req.MaxTokens, p.guardrails.Version(), responseFormatFingerprint(req.ResponseFormat), promptFP)
	l2Key := cache.NormalizedKey(vk.ID, req.Model, normalizeMessages(req.Messages), req.Temperature, req.MaxTokens, p.guardrails.Version(), responseFormatFingerprint(req.ResponseFormat), promptFP)
	l3Signature := cache.MinHashSignature(cache.Shingles(normalizeMessages(req.Messages), l3ShingleWords), l3SignatureSize)

	cacheAttempted = true
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

	if cached, similarity, ageMs, ok := p.checkLexicalCache(ctx, vk, req, l1Key, l3Signature, promptFP); ok {
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
	if verdict := p.guardrails.Check(ctx, guardrailScanMessages(req.Messages)); verdict.Blocked {
		p.logger.Warn("guardrail_blocked_precall", append(traceLogFields(ctx), "key_id", vk.ID, "finding_count", len(verdict.Findings))...)
		err = ErrGuardrailBlocked
		return
	}

	if p.upstreamStream == nil {
		err = ErrStreamingNotConfigured
		return
	}

	var found bool
	// Sticky routing on this first pick too, mirroring runMissPath's own
	// identical reasoning (dataplane.go) -- a streaming request from the
	// same tenant must prefer the same side of a configured canary/stable
	// pair exactly like a buffered one does; leaving this path out would
	// be a silent, surprising gap for anyone actually using the feature
	// against a model group that serves both.
	dep, found = p.nextDeploymentSticky(req.Model, nil, vk.ID)
	if !found {
		err = fmt.Errorf("%w: %q", ErrNoDeployment, req.Model)
		return
	}
	dep = p.rerouteToCapableDeploymentIfNeeded(dep, req, vk)
	if err = checkResponseFormatEnforceable(dep, req); err != nil {
		return
	}

	msr := midStreamReservation{vk: vk, budgetReservedUSD: &budgetReservedUSD, budgetReservationEpoch: &budgetReservationEpoch, tpmReservedTokens: &tpmReservedTokens, tpmReservationEpoch: &tpmReservationEpoch}
	var blocked bool
	var firstChunkSent bool
	resp, dep, fallback, blocked, costEstimated, firstChunkSent, err = p.streamDeploymentWithFallback(ctx, dep, req, sw, vk.ID, msr)
	if err != nil {
		// firstChunkSent means real content already reached the client
		// before this error (a client disconnect is the common real
		// case) — resp still carries that real, already-delivered
		// content with an estimated usage (streamDeploymentWithFallback/
		// streamDeployment's own read-error handling), so it must still
		// be billed, not silently discarded, per
		// docs/upgrade-research/request-lifecycle-reliability-2026-09-15.md.
		// billable's own meaning ("resp came from this specific call's
		// own real, unshared upstream call") extends naturally to
		// "delivered real content" here — never true on any OTHER
		// streaming error path (auth, rate limit, no deployment, etc.),
		// since none of those ever set firstChunkSent.
		billable = firstChunkSent
		err = fmt.Errorf("dataplane: streaming upstream call failed for model %q: %w", req.Model, err)
		return
	}
	// The streaming path has no singleflight coalescing (unlike
	// runMissPath's buffered miss path) — every completed stream is its
	// own real, unshared upstream call, so it's always billable, per
	// docs/rfcs/2026-09-05-gateway-cost-double-counting.md.
	billable = true

	// A Block-tier post-call verdict is audit-only for CLIENT delivery
	// (already flushed, per finishStreamedResponse's own doc comment) but
	// must NEVER durably populate the cache — mirroring runMissPath's
	// identical rule on the buffered path
	// (TestHandleChatCompletionPostCallBlockedResponseNeverCached).
	// Without this guard, a Block-tier response (credit card, SSN,
	// secret) could be replayed to this same tenant on any future
	// identical-or-near-duplicate request without the guardrail engine
	// ever running again, for the life of the cache TTL.
	if !blocked && !responseWasTruncated(resp) {
		if encoded, marshalErr := json.Marshal(resp); marshalErr == nil {
			p.writeCache(ctx, vk.ID, l1Key, l2Key, l3Signature, Fingerprint(req.Messages), req.Model, responseFormatFingerprint(req.ResponseFormat), promptFP, NegationFingerprint(req.Messages), reasoningBlocksFingerprint(req.Messages), encoded)
		}
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
					Role:            c.Message.Role,
					Content:         c.Message.Content,
					ToolCalls:       toChunkToolCallDeltas(c.Message.ToolCalls),
					ReasoningBlocks: toChunkReasoningDeltas(c.Message.ReasoningBlocks),
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

// toChunkReasoningDeltas converts a cached/idempotency-replayed response's
// already-complete ReasoningBlocks into the single-fragment-per-block
// ReasoningDeltas writeFakeStream forwards to the client. Without this, a
// cache-hit (or idempotency-replayed) streamed response would silently
// degrade further than the original miss that populated the cache: the
// accumulated response written by streamAccumulator.build already carries
// ReasoningBlocks correctly (see that type's own doc comments), but
// replaying it as a synthetic stream previously dropped them again right
// here, on every subsequent hit. Each block is forwarded whole in a single
// delta — matching this function's own synthesis-not-replay contract
// (writeFakeStream's doc comment): there is no original per-chunk timing
// to reproduce, so there is nothing to fragment Text across.
func toChunkReasoningDeltas(reasoningBlocks []adapter.ReasoningBlock) []streaming.ReasoningDelta {
	if len(reasoningBlocks) == 0 {
		return nil
	}
	deltas := make([]streaming.ReasoningDelta, 0, len(reasoningBlocks))
	for i, rb := range reasoningBlocks {
		deltas = append(deltas, streaming.ReasoningDelta{
			Index:     i,
			Text:      rb.Text,
			Signature: rb.Signature,
			Redacted:  rb.Redacted,
			Data:      rb.Data,
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
func (p *Pipeline) streamDeploymentWithFallback(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw *streaming.Writer, keyID string, msr midStreamReservation) (adapter.ChatResponse, Deployment, fallbackInfo, bool, bool, bool, error) {
	// firstChunkSent, in addition to gating fallback attempts below, is
	// also returned to the caller — HandleChatCompletionStream uses it to
	// decide whether an err != nil return is still billable (real content
	// already reached the client before the connection broke), per
	// docs/upgrade-research/request-lifecycle-reliability-2026-09-15.md.
	var firstChunkSent bool
	var fallback fallbackInfo
	// blocked is set by finishStreamedResponse (via streamDeployment/
	// streamDeploymentBedrock) when the post-call guardrail check fires —
	// see that function's own doc comment. Reused across every hop below
	// exactly like firstChunkSent already is: finishStreamedResponse is
	// only ever reached once, at the tail of whichever single hop
	// actually completes a stream, so there is no risk of one hop's
	// blocked=true bleeding into a later, unrelated hop's result.
	var blocked bool

	// estimated mirrors blocked's own "set once, at whichever single hop
	// actually completes a stream" reasoning — see
	// estimateOrRealUsage's doc comment for what it means. Reassigned by
	// the attemptFallbackChain closure below via normal closure capture
	// (attemptFallbackChain's own call signature is fixed/shared with the
	// buffered path's identical fallback machinery, so it cannot itself
	// return a third value) — never stale when attempted is false, since
	// that only happens after an ALREADY-failed attempt that itself set
	// estimated to false on its own error return.
	resp, estimated, err := p.streamDeploymentWithCapacityCheck(ctx, dep, req, sw, &firstChunkSent, keyID, msr, &blocked)
	if err == nil || firstChunkSent {
		return resp, dep, fallback, blocked, estimated, firstChunkSent, err
	}

	originalDep, originalErr := dep, err
	if targets, configured := fallbackTargets(dep, err); configured {
		tried := map[string]bool{dep.Name: true}
		hopDep, hopResp, hopErr, attempted := p.attemptFallbackChain(ctx, targets, tried,
			func(d Deployment) (adapter.ChatResponse, error) {
				defer p.releaseDeploymentConcurrency(d.Name)
				var hopResp adapter.ChatResponse
				var hopErr error
				hopResp, estimated, hopErr = p.streamDeployment(ctx, d, req, sw, &firstChunkSent, keyID, msr, &blocked)
				return hopResp, hopErr
			},
			func() bool { return firstChunkSent },
			func(model string) bool { return p.checkFallbackTargetRateLimit(ctx, keyID, model) },
			func(depName string) bool { return p.checkDeploymentCapacity(ctx, depName) },
			p.releaseDeploymentConcurrency,
			func(d Deployment) bool { return capabilityOKForRequest(d, req) },
			func(d Deployment) bool { return isRegionAllowed(msr.vk, d.Region) },
		)
		if attempted {
			fallback = fallbackInfo{happened: true, from: originalDep.Name, reason: originalErr.Error()}
			dep, resp, err = hopDep, hopResp, hopErr
		}
	} else if fallbackDep, hasFallback := p.nextDeployment(req.Model, map[string]bool{dep.Name: true}); hasFallback {
		fallback = fallbackInfo{happened: true, from: dep.Name, reason: err.Error()}
		dep = fallbackDep
		resp, estimated, err = p.streamDeploymentWithCapacityCheck(ctx, dep, req, sw, &firstChunkSent, keyID, msr, &blocked)
	}
	return resp, dep, fallback, blocked, estimated, firstChunkSent, err
}

// streamDeploymentWithCapacityCheck wraps streamDeployment with dep's
// own checkDeploymentCapacity gate and guaranteed release — the
// streaming sibling of callDeploymentWithCapacityCheck (dataplane.go),
// used for EVERY call to a deployment, hop 1 included.
func (p *Pipeline) streamDeploymentWithCapacityCheck(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw *streaming.Writer, firstChunkSent *bool, keyID string, msr midStreamReservation, blocked *bool) (adapter.ChatResponse, bool, error) {
	if !p.checkDeploymentRateLimit(ctx, dep.Name) {
		return adapter.ChatResponse{}, false, &DeploymentCapacityError{Deployment: dep.Name, Reason: "rate_limit"}
	}
	if !p.checkDeploymentConcurrency(dep.Name) {
		return adapter.ChatResponse{}, false, &DeploymentCapacityError{Deployment: dep.Name, Reason: "concurrency"}
	}
	defer p.releaseDeploymentConcurrency(dep.Name)
	return p.streamDeployment(ctx, dep, req, sw, firstChunkSent, keyID, msr, blocked)
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
func (p *Pipeline) streamDeployment(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw *streaming.Writer, firstChunkSent *bool, keyID string, msr midStreamReservation, blocked *bool) (adapter.ChatResponse, bool, error) {
	if dep.Provider == "bedrock" {
		return p.streamDeploymentBedrock(ctx, dep, req, sw, firstChunkSent, keyID, msr, blocked)
	}

	a, ok := p.adapters[dep.Provider]
	if !ok {
		return adapter.ChatResponse{}, false, fmt.Errorf("no adapter registered for provider %q", dep.Provider)
	}
	streamAdapter, ok := a.(streaming.StreamingAdapter)
	if !ok {
		return adapter.ChatResponse{}, false, fmt.Errorf("%w: provider %q", ErrStreamingNotSupported, dep.Provider)
	}

	upstreamReq := req
	upstreamReq.Model = dep.UpstreamModel
	upstreamReq.Stream = true
	upstreamReq.DisableCacheControlAutoPopulate = dep.effectiveCacheControlAutoDisabled()

	providerReq, err := streamAdapter.ToProvider(upstreamReq)
	if err != nil {
		return adapter.ChatResponse{}, false, fmt.Errorf("adapter %q ToProvider: %w", dep.Provider, err)
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
		return adapter.ChatResponse{}, false, fmt.Errorf("upstream stream call to deployment %q: %w", dep.Name, err)
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
			// A non-EOF read error here includes a client-disconnect-
			// propagated context cancellation (upstreamCtx is a child of
			// ctx, itself derived from the client's own r.Context(),
			// canceled by net/http on disconnect) — acc may already hold
			// real, already-client-delivered content at this point.
			// Estimating usage from it (rather than discarding acc
			// entirely, the pre-fix behavior) lets the caller bill for
			// what was genuinely generated and provider-billed before the
			// connection broke, per
			// docs/upgrade-research/request-lifecycle-reliability-2026-09-15.md
			// — the caller (streamDeploymentWithFallback) decides whether
			// this is actually billable, gated on *firstChunkSent, not
			// this function.
			usage, estimated := estimateOrRealUsage(req, acc, finalUsage)
			return acc.build(usage), estimated, fmt.Errorf("reading stream from deployment %q: %w", dep.Name, readErr)
		}

		chunks, done, usage, decErr := decoder.Decode(ev)
		if decErr != nil {
			return adapter.ChatResponse{}, false, fmt.Errorf("decoding stream from deployment %q: %w", dep.Name, decErr)
		}
		if usage != nil {
			finalUsage = usage
		}
		for _, c := range chunks {
			acc.add(c)
			if writeErr := sw.WriteChunk(c); writeErr != nil {
				return adapter.ChatResponse{}, false, fmt.Errorf("writing streamed chunk to client: %w", writeErr)
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
			p.logger.Warn("streaming_runaway_guard_triggered", append(traceLogFields(ctx),
				"key_id", keyID,
				"deployment", dep.Name,
				"provider", dep.Provider,
				"model", req.Model,
				"accumulated_chars", accumulatedChars,
				"ceiling_chars", runawayCeiling,
			)...)
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
		if !p.checkMidStreamReservationTopup(ctx, dep, req, accumulatedChars, msr) {
			p.logger.Warn("streaming_midstream_reservation_topup_exhausted", append(traceLogFields(ctx),
				"key_id", keyID,
				"deployment", dep.Name,
				"provider", dep.Provider,
				"model", req.Model,
				"accumulated_chars", accumulatedChars,
			)...)
			cancelUpstream()
			break
		}
		if done {
			break
		}
	}

	return p.finishStreamedResponse(ctx, dep, req, sw, acc, finalUsage, blocked)
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
func (p *Pipeline) streamDeploymentBedrock(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw *streaming.Writer, firstChunkSent *bool, keyID string, msr midStreamReservation, blocked *bool) (adapter.ChatResponse, bool, error) {
	a, ok := p.adapters[dep.Provider]
	if !ok {
		return adapter.ChatResponse{}, false, fmt.Errorf("no adapter registered for provider %q", dep.Provider)
	}
	bedrockAdapter, ok := a.(*bedrock.Adapter)
	if !ok {
		return adapter.ChatResponse{}, false, fmt.Errorf("%w: provider %q", ErrStreamingNotSupported, dep.Provider)
	}

	upstreamReq := req
	upstreamReq.Model = dep.UpstreamModel
	upstreamReq.Stream = true
	upstreamReq.DisableCacheControlAutoPopulate = dep.effectiveCacheControlAutoDisabled()

	providerReq, err := bedrockAdapter.ToProvider(upstreamReq)
	if err != nil {
		return adapter.ChatResponse{}, false, fmt.Errorf("adapter %q ToProvider: %w", dep.Provider, err)
	}

	// See streamDeployment's identical upstreamCtx comment: scoped to
	// exactly this upstream call, canceled by the runaway guard below
	// without ever touching ctx itself.
	upstreamCtx, cancelUpstream := context.WithCancel(ctx)
	defer cancelUpstream()

	body, err := p.upstreamStream(upstreamCtx, dep, providerReq)
	if err != nil {
		return adapter.ChatResponse{}, false, fmt.Errorf("upstream stream call to deployment %q: %w", dep.Name, err)
	}
	// **Fixed 2026-09-17, real bug**: aws-sdk-go-v2's own
	// eventstream.Decoder places no upper bound on a single frame's
	// declared length -- its messagePrelude.ValidateLens only rejects
	// Length == 0, confirmed by reading message.go directly. A single
	// pathological frame could otherwise make decodePayload's io.Copy
	// buffer an arbitrarily large payload (up to ~4GiB, Length's own
	// uint32 range) fully into memory before this loop ever gets a
	// chance to react to it. maxBedrockStreamBytes bounds the STREAM's
	// total bytes rather than any single frame specifically (the SDK's
	// public Decoder API gives no hook to intercept a frame's declared
	// length before it starts reading the payload) -- coarser than a
	// true per-frame cap, but it closes the unbounded-memory worst case:
	// hitting this cap surfaces as a plain io.EOF with no messageStop
	// ever seen, which the truncation check just above already turns
	// into a real, typed ErrBedrockStreamTruncated error rather than a
	// silent success.
	body = &limitedReadCloser{Reader: io.LimitReader(body, maxBedrockStreamBytes), closer: body}
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
			// **Fixed 2026-09-17, real bug**: aws-sdk-go-v2's own
			// eventstream.Decoder.Decode does not reliably distinguish a
			// clean end-of-stream from a connection truncated mid-frame.
			// Its decodePayload uses io.Copy, which (per io.Copy's own
			// documented contract) treats an EOF from the underlying
			// reader as successful completion, not an error -- a payload
			// truncated mid-read is silently accepted as "fully read,"
			// short. The VERY NEXT read (the frame's trailing CRC, via
			// io.ReadFull) then hits the now-closed connection with zero
			// bytes available for THAT read specifically, which
			// io.ReadFull reports as bare io.EOF (io.ReadFull only
			// upgrades to io.ErrUnexpectedEOF when 0 < n < requested for
			// the SAME read) -- indistinguishable, at this loop's level,
			// from a legitimate "no more frames" EOF at a real frame
			// boundary. Confirmed by reading aws-sdk-go-v2's own
			// decode.go directly, not assumed. acc.hasFinishReason()
			// closes the ambiguity: per stream.go's own documented event
			// sequence, messageStop always arrives before the stream
			// closes on a genuinely complete response -- its absence at
			// EOF means this was a truncation, not a clean end.
			if !acc.hasFinishReason() {
				usage, estimated := estimateOrRealUsage(req, acc, finalUsage)
				return acc.build(usage), estimated, fmt.Errorf("reading binary event stream from deployment %q: %w", dep.Name, ErrBedrockStreamTruncated)
			}
			break
		}
		if readErr != nil {
			// See streamDeployment's identical read-error comment: acc may
			// already hold real, already-client-delivered content, so
			// estimate usage from it rather than discarding it — the
			// caller decides billability, gated on *firstChunkSent.
			usage, estimated := estimateOrRealUsage(req, acc, finalUsage)
			return acc.build(usage), estimated, fmt.Errorf("reading binary event stream from deployment %q: %w", dep.Name, readErr)
		}
		payloadBuf = msg.Payload

		chunks, usage, decErr := decoder.Decode(msg)
		if decErr != nil {
			return adapter.ChatResponse{}, false, fmt.Errorf("decoding stream from deployment %q: %w", dep.Name, decErr)
		}
		if usage != nil {
			finalUsage = usage
		}
		for _, c := range chunks {
			acc.add(c)
			if writeErr := sw.WriteChunk(c); writeErr != nil {
				return adapter.ChatResponse{}, false, fmt.Errorf("writing streamed chunk to client: %w", writeErr)
			}
			*firstChunkSent = true
		}
		// See streamDeployment's identical runaway-guard comment/rationale.
		accumulatedChars := acc.totalContentLen()
		if accumulatedChars > runawayCeiling {
			p.logger.Warn("streaming_runaway_guard_triggered", append(traceLogFields(ctx),
				"key_id", keyID,
				"deployment", dep.Name,
				"provider", dep.Provider,
				"model", req.Model,
				"accumulated_chars", accumulatedChars,
				"ceiling_chars", runawayCeiling,
			)...)
			cancelUpstream()
			break
		}
		// See streamDeployment's identical mid-stream reservation
		// top-up guard comment/rationale.
		if !p.checkMidStreamReservationTopup(ctx, dep, req, accumulatedChars, msr) {
			p.logger.Warn("streaming_midstream_reservation_topup_exhausted", append(traceLogFields(ctx),
				"key_id", keyID,
				"deployment", dep.Name,
				"provider", dep.Provider,
				"model", req.Model,
				"accumulated_chars", accumulatedChars,
			)...)
			cancelUpstream()
			break
		}
	}

	return p.finishStreamedResponse(ctx, dep, req, sw, acc, finalUsage, blocked)
}

// estimateOrRealUsage returns finalUsage verbatim (estimated=false) when
// the provider actually sent one, or — when it never arrived, e.g. a
// mid-stream guard cut the connection before the provider's terminal
// usage frame, or a client disconnect discarded the stream before EOF —
// a conservative estimate derived from acc's own already-accumulated
// content, using the exact same provider-agnostic chars-per-token proxy
// (streamRunawayCharsPerToken) the mid-stream reservation-topup guard
// already trusts for the identical purpose (streamrunaway.go). Prompt
// tokens are estimated from req.Messages via serializeMessages — the
// same deterministic encoding already used for the L1 cache key —
// rather than left at zero, since a real, non-trivial prompt was
// genuinely sent to the provider and billed by it regardless of how the
// completion side of the exchange ended.
//
// Billing a real, non-zero estimate instead of leaving usage at its
// zero value is the actual fix for two real correctness bugs: real,
// already-provider-billed output tokens were previously either recorded
// as an explicit $0.00 charge (when err == nil, e.g. a mid-stream guard
// trip) or silently unbilled entirely (when err != nil, e.g. a client
// disconnect) — see docs/upgrade-research/request-lifecycle-reliability-2026-09-15.md.
func estimateOrRealUsage(req adapter.ChatRequest, acc *streamAccumulator, finalUsage *adapter.Usage) (usage adapter.Usage, estimated bool) {
	if finalUsage != nil {
		return *finalUsage, false
	}
	promptTokens := len(serializeMessages(req.Messages)) / streamRunawayCharsPerToken
	completionTokens := acc.totalContentLen() / streamRunawayCharsPerToken
	return adapter.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}, true
}

// finishStreamedResponse builds the final ChatResponse from an
// accumulator and finalUsage (estimating usage via estimateOrRealUsage
// when the provider never sent its own terminal usage frame — see that
// function's own doc comment), runs the audit-only post-call guardrail
// check, and writes the client-facing done sentinel — the provider-
// agnostic tail shared by streamDeployment (SSE-framed providers) and
// streamDeploymentBedrock (binary-framed) per
// docs/rfcs/2026-09-04-bedrock-converse-stream.md: none of this logic
// depends on how chunks actually arrived. The returned bool reports
// whether usage was estimated (true) rather than provider-reported
// (false) — callers thread this through to finalize purely for
// telemetry/audit disclosure, per estimateOrRealUsage's own doc comment;
// it does not change the billing decision itself.
func (p *Pipeline) finishStreamedResponse(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw *streaming.Writer, acc *streamAccumulator, finalUsage *adapter.Usage, blocked *bool) (adapter.ChatResponse, bool, error) {
	usage, estimated := estimateOrRealUsage(req, acc, finalUsage)
	if estimated {
		// Per the RFC's Cost Accounting section: a provider stream that
		// never sends usage does not fail the request, but must not be
		// silently unmetered either — an estimated, non-zero usage is
		// billed instead (see estimateOrRealUsage), flagged loudly here
		// so the estimate itself is visible in logs, not just silently
		// substituted.
		p.logger.Warn("stream_missing_usage", append(traceLogFields(ctx),
			"deployment", dep.Name,
			"provider", dep.Provider,
			"model", req.Model,
			"estimated_prompt_tokens", usage.PromptTokens,
			"estimated_completion_tokens", usage.CompletionTokens,
		)...)
	}

	if len(acc.duplicateAfterFinishIndices) > 0 {
		// Logged, never a failure -- see streamAccumulator.add's own doc
		// comment for why this anomaly is detected-and-recorded, not
		// hard-failed: the content is already safely (if unusually)
		// concatenated, and this is a visibility fix, not a correctness
		// fix for the choice/content shape itself.
		p.logger.Warn("stream_duplicate_index_after_finish", append(traceLogFields(ctx),
			"deployment", dep.Name,
			"indices", acc.duplicateAfterFinishIndices,
		)...)
	}

	resp := acc.build(usage)
	// Echo back the client-facing canonical model name, matching
	// callDeployment's convention for the buffered path.
	resp.Model = req.Model

	// Guardrail post-call, streaming path — audit-only for CLIENT DELIVERY
	// ONLY, per docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md's
	// named, accepted residual risk: every chunk has already been flushed
	// to the client (sw.WriteChunk, above) strictly before resp is known
	// here — there is no point in this function where a post-call check
	// can run before content reaches the client without a buffering layer
	// this RFC deliberately does not add. This check can only log, at
	// elevated severity, never withhold what's already been delivered.
	//
	// *blocked is a SEPARATE question that RFC never discussed: whether a
	// Block-tier response should be durably WRITTEN TO CACHE for replay to
	// this (or, via L3, a near-duplicate) future request without the
	// guardrail engine ever running again. It must not — the buffered
	// path's runMissPath already gets this right and
	// TestHandleChatCompletionPostCallBlockedResponseNeverCached proves it
	// — so *blocked signals the caller (HandleChatCompletionStream) to
	// skip writeCache, even though the client-delivery decision above
	// stays audit-only.
	if postVerdict := p.guardrails.Check(ctx, serializeResponse(resp)); postVerdict.Blocked {
		p.logger.Warn("guardrail_blocked_postcall_streaming_audit_only",
			append(traceLogFields(ctx), "deployment", dep.Name, "finding_count", len(postVerdict.Findings))...)
		*blocked = true
	}

	if err := sw.WriteDone(); err != nil {
		return adapter.ChatResponse{}, estimated, fmt.Errorf("writing done sentinel for deployment %q: %w", dep.Name, err)
	}

	return resp, estimated, nil
}
