// Package dataplane implements the gateway's request pipeline exactly as
// gateway/ARCHITECTURE.md's Request Lifecycle describes, minus streaming
// (every response is buffered, non-streaming, this pass) and minus
// guardrails/MCP (not built yet — Phase 1+ per PRD.md):
//
//	auth -> rate-limit -> cache lookup (L1 exact) -> hit? return
//	     -> miss -> router (round-robin + single fallback) -> adapter
//	     -> upstream HTTP call -> adapter (response) -> cache write-back
//	     -> structured JSON log (incl. cost) -> response
//
// Cost/observability finalization (the log line) always runs, even on
// error, via a deferred closure over named return values — mirroring
// gateway/ARCHITECTURE.md's note that this step must always execute,
// since a partial generation still consumed billable output tokens.
package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/trace"

	"github.com/kelvran/gateway/internal/adapter"
	"github.com/kelvran/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/internal/budget"
	"github.com/kelvran/gateway/internal/cache"
	"github.com/kelvran/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/internal/identity"
	"github.com/kelvran/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/internal/telemetry"
)

// ErrRateLimited is returned by HandleChatCompletion when the caller's
// virtual key has exhausted its own rate-limit token bucket.
var ErrRateLimited = errors.New("dataplane: rate limit exceeded")

// ErrBudgetExceeded is returned when the caller's virtual key has spent at
// least its configured BudgetUSD cap. See internal/budget.
var ErrBudgetExceeded = errors.New("dataplane: budget exceeded")

// ErrModelNotAllowed is returned when the caller's virtual key is
// configured with a non-empty AllowedModels list that does not include the
// requested model.
var ErrModelNotAllowed = errors.New("dataplane: model not allowed for this virtual key")

// Deployment is a resolved upstream route: a concrete provider/endpoint a
// canonical model can be sent to, with its API key already resolved from
// the environment (never the raw config file) by the caller (cmd/gateway).
type Deployment struct {
	Name          string
	Model         string
	Provider      string
	UpstreamModel string
	BaseURL       string
	APIKey        string
}

// UpstreamCaller performs the actual upstream HTTP call for one
// deployment, given the provider-native request adapter.ToProvider
// produced. It returns the provider-native response value
// adapter.FromProvider expects. Injecting this as a dependency (rather
// than hardcoding net/http inside the pipeline) is what makes
// HandleChatCompletion testable without a real network call, per
// docs/testing/TESTING.md's "never hit a real upstream LLM provider API
// in CI" ban — production wiring uses NewHTTPUpstreamCaller; tests use a
// fake.
type UpstreamCaller func(ctx context.Context, dep Deployment, providerReq any) (providerResp any, err error)

// Config bundles every dependency the Pipeline needs. All fields are
// required except Logger and CacheTTL, which default to slog.Default()
// and 5 minutes respectively.
type Config struct {
	Verifier *identity.Verifier
	// Limiter enforces each virtual key's own burst/refill rate limit —
	// either in-memory (ratelimit.NewInMemoryKeyLimiter) or Redis-backed
	// (ratelimit.NewRedisKeyLimiter), per
	// docs/rfcs/2026-09-03-distributed-rate-limiting.md. Pre-built by the
	// caller (cmd/gateway), exactly like Budget below — NewPipeline
	// itself never needs to know which virtual keys exist to construct
	// this, only to use it.
	Limiter *ratelimit.KeyLimiter
	// Budget tracks each virtual key's cumulative spend against its
	// configured BudgetUSD cap. See internal/budget.
	Budget         *budget.Tracker
	Cache          cache.Cache
	Adapters       adapter.Registry
	Deployments    []Deployment
	CostCalculator *costaccounting.Calculator
	Upstream       UpstreamCaller
	Logger         *slog.Logger
	CacheTTL       time.Duration
	// UpstreamStream is required only for streaming requests on a
	// cache MISS — a streaming cache HIT never touches it. Left nil, a
	// Pipeline still handles every non-streaming request and every
	// streaming cache hit exactly as if it were configured; a streaming
	// cache-miss request instead fails with ErrStreamingNotConfigured.
	// This is deliberately optional (not validated in NewPipeline) so
	// existing non-streaming-only callers/tests don't need updating.
	UpstreamStream UpstreamStreamCaller
}

// Pipeline is the wired dataplane request pipeline.
type Pipeline struct {
	verifier           *identity.Verifier
	limiter            *ratelimit.KeyLimiter
	budget             *budget.Tracker
	cache              cache.Cache
	adapters           adapter.Registry
	deploymentsByModel map[string][]Deployment
	rrMu               sync.Mutex
	rrCounters         map[string]*atomic.Uint64
	costCalc           *costaccounting.Calculator
	upstream           UpstreamCaller
	upstreamStream     UpstreamStreamCaller
	logger             *slog.Logger
	cacheTTL           time.Duration
}

// NewPipeline validates cfg and constructs a Pipeline.
func NewPipeline(cfg Config) (*Pipeline, error) {
	switch {
	case cfg.Verifier == nil:
		return nil, fmt.Errorf("dataplane: Config.Verifier is required")
	case cfg.Limiter == nil:
		return nil, fmt.Errorf("dataplane: Config.Limiter is required")
	case cfg.Budget == nil:
		return nil, fmt.Errorf("dataplane: Config.Budget is required")
	case cfg.Cache == nil:
		return nil, fmt.Errorf("dataplane: Config.Cache is required")
	case cfg.Adapters == nil:
		return nil, fmt.Errorf("dataplane: Config.Adapters is required")
	case len(cfg.Deployments) == 0:
		return nil, fmt.Errorf("dataplane: Config.Deployments must be non-empty")
	case cfg.CostCalculator == nil:
		return nil, fmt.Errorf("dataplane: Config.CostCalculator is required")
	case cfg.Upstream == nil:
		return nil, fmt.Errorf("dataplane: Config.Upstream is required")
	}

	byModel := map[string][]Deployment{}
	for _, d := range cfg.Deployments {
		byModel[d.Model] = append(byModel[d.Model], d)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}

	return &Pipeline{
		verifier:           cfg.Verifier,
		limiter:            cfg.Limiter,
		budget:             cfg.Budget,
		cache:              cfg.Cache,
		adapters:           cfg.Adapters,
		deploymentsByModel: byModel,
		rrCounters:         map[string]*atomic.Uint64{},
		costCalc:           cfg.CostCalculator,
		upstream:           cfg.Upstream,
		upstreamStream:     cfg.UpstreamStream,
		logger:             logger,
		cacheTTL:           ttl,
	}, nil
}

// Close releases any resources the Pipeline owns — an optional
// restart-durable budget store (see internal/budget.Tracker.Close and
// docs/rfcs/2026-09-03-budget-persistence.md) and an optional
// Redis-backed rate limiter (see internal/ratelimit.KeyLimiter.Close and
// docs/rfcs/2026-09-03-distributed-rate-limiting.md). p.budget and
// p.limiter are never nil at this point (NewPipeline already validates
// both), so this always has real values to close, even ones with
// nothing to actually release (a no-op in the in-memory/no-store cases).
// Both Close calls always run, even if the first errors — a resource
// leak in one must never suppress cleanup of the other.
func (p *Pipeline) Close() error {
	budgetErr := p.budget.Close()
	limiterErr := p.limiter.Close()
	return errors.Join(budgetErr, limiterErr)
}

// checkRateLimit reports whether vk may proceed. A Redis backend error
// (network failure, timeout) is logged and the request is allowed
// through rather than rejected — see
// docs/rfcs/2026-09-03-distributed-rate-limiting.md's "Fail-open, not
// fail-closed" section for why that's the right default specifically for
// Kelvran: internal/budget.Tracker's per-key USD cap is a second,
// independent control that never touches Redis, so a rate-limiter
// outage alone does not remove every spending control at once. In
// in-memory mode, p.limiter.Allow never returns an error at all, so this
// fail-open path is only ever exercised when a Redis backend is
// configured.
func (p *Pipeline) checkRateLimit(ctx context.Context, vk *identity.VirtualKey) bool {
	allowed, err := p.limiter.Allow(ctx, vk.ID)
	if err != nil {
		p.logger.Warn("ratelimit_backend_unavailable", "key_id", vk.ID, "error", err.Error())
		return true
	}
	return allowed
}

// isModelAllowed reports whether vk is permitted to request model. An
// empty AllowedModels set means every configured model is allowed.
func isModelAllowed(vk *identity.VirtualKey, model string) bool {
	if len(vk.AllowedModels) == 0 {
		return true
	}
	_, ok := vk.AllowedModels[model]
	return ok
}

// HandleChatCompletion runs the full request pipeline for one canonical
// ChatRequest, given the raw Authorization header value.
func (p *Pipeline) HandleChatCompletion(ctx context.Context, authorizationHeader string, req adapter.ChatRequest) (resp adapter.ChatResponse, err error) {
	var (
		cacheHit bool
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

	if !p.checkRateLimit(ctx, vk) {
		err = ErrRateLimited
		return
	}

	if !p.budget.Allow(vk.ID, vk.BudgetUSD) {
		err = ErrBudgetExceeded
		return
	}

	key := cache.Key(vk.ID, req.Model, serializeMessages(req.Messages), req.Temperature, req.MaxTokens)

	if cached, ok, getErr := p.cache.Get(ctx, key); getErr == nil && ok {
		var cachedResp adapter.ChatResponse
		if unmarshalErr := json.Unmarshal(cached, &cachedResp); unmarshalErr == nil {
			resp = cachedResp
			cacheHit = true
			return
		}
		// A corrupt cache entry is treated as a miss, not a request
		// failure — fall through to the upstream path below.
	}

	var found bool
	dep, found = p.nextDeployment(req.Model)
	if !found {
		err = fmt.Errorf("dataplane: no deployment configured for model %q", req.Model)
		return
	}

	resp, err = p.callDeployment(ctx, dep, req)
	if err != nil {
		// Single fallback to the next deployment for the same model, per
		// gateway/ARCHITECTURE.md's router step.
		if fallbackDep, hasFallback := p.nextDeployment(req.Model); hasFallback && fallbackDep.Name != dep.Name {
			dep = fallbackDep
			resp, err = p.callDeployment(ctx, dep, req)
		}
	}
	if err != nil {
		err = fmt.Errorf("dataplane: upstream call failed for model %q: %w", req.Model, err)
		return
	}

	if encoded, marshalErr := json.Marshal(resp); marshalErr == nil {
		_ = p.cache.Put(ctx, key, encoded, p.cacheTTL)
	}

	return
}

// callDeployment runs the adapter+upstream-call steps for one deployment:
// canonical -> provider-native (ToProvider) -> upstream call -> canonical
// (FromProvider).
func (p *Pipeline) callDeployment(ctx context.Context, dep Deployment, req adapter.ChatRequest) (adapter.ChatResponse, error) {
	a, ok := p.adapters[dep.Provider]
	if !ok {
		return adapter.ChatResponse{}, fmt.Errorf("no adapter registered for provider %q", dep.Provider)
	}

	// Send the deployment's upstream-side model identifier, not the
	// client-facing canonical model name — they may differ (e.g. a
	// versioned Anthropic model ID).
	upstreamReq := req
	upstreamReq.Model = dep.UpstreamModel

	providerReq, err := a.ToProvider(upstreamReq)
	if err != nil {
		return adapter.ChatResponse{}, fmt.Errorf("adapter %q ToProvider: %w", dep.Provider, err)
	}

	providerResp, err := p.upstream(ctx, dep, providerReq)
	if err != nil {
		return adapter.ChatResponse{}, fmt.Errorf("upstream call to deployment %q: %w", dep.Name, err)
	}

	resp, err := a.FromProvider(providerResp)
	if err != nil {
		return adapter.ChatResponse{}, fmt.Errorf("adapter %q FromProvider: %w", dep.Provider, err)
	}

	// Echo back the client-facing canonical model name, matching the
	// convention of OpenAI-shaped APIs (the response's "model" field
	// reflects what the caller asked for).
	resp.Model = req.Model
	return resp, nil
}

// nextDeployment round-robins across the deployments configured for
// model. The second return value is false if no deployment is configured
// for model at all.
func (p *Pipeline) nextDeployment(model string) (Deployment, bool) {
	deps := p.deploymentsByModel[model]
	if len(deps) == 0 {
		return Deployment{}, false
	}

	p.rrMu.Lock()
	counter, ok := p.rrCounters[model]
	if !ok {
		counter = &atomic.Uint64{}
		p.rrCounters[model] = counter
	}
	p.rrMu.Unlock()

	idx := counter.Add(1) - 1
	return deps[idx%uint64(len(deps))], true
}

// finalize is the single "a request just finished (or failed)" step,
// shared by HandleChatCompletion and HandleChatCompletionStream: compute
// cost once, record it against the caller's budget, record the OTel span
// (per docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md), end the span,
// and emit the structured JSON log line — in that order, so the span is
// still open while telemetry.RecordChatCompletionResult sets its final
// attributes. Always called via defer, so it runs even when err != nil —
// a partial/failed generation still gets logged and spanned, per
// gateway/ARCHITECTURE.md's "ALWAYS runs, even on error/cancel" note. vk
// is nil when auth itself failed (there is no resolved identity yet in
// that case); dep is the zero value whenever no deployment was ever
// resolved or called (auth/model/rate-limit/budget rejections, and cache
// hits, which never touch a deployment at all).
func (p *Pipeline) finalize(ctx context.Context, span trace.Span, vk *identity.VirtualKey, dep Deployment, req adapter.ChatRequest, resp adapter.ChatResponse, cacheHit bool, err error) {
	// Zero value (decimal.Decimal{}) is a valid, correct "no cost yet"
	// default on the err != nil path — verified explicitly in
	// internal/budget's own tests, not assumed here too.
	var cost decimal.Decimal
	if err == nil {
		cost = p.costCalc.Calculate(req.Model, costaccounting.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		})
		if vk != nil {
			p.budget.Record(vk.ID, cost)
		}
	}

	var virtualKeyID string
	if vk != nil {
		virtualKeyID = vk.ID
	}
	telemetry.RecordChatCompletionResult(span, telemetry.ChatCompletionResult{
		VirtualKeyID:   virtualKeyID,
		Provider:       dep.Provider,
		DeploymentName: dep.Name,
		ResponseModel:  resp.Model,
		ResponseID:     resp.ID,
		FinishReasons:  finishReasons(resp),
		InputTokens:    resp.Usage.PromptTokens,
		OutputTokens:   resp.Usage.CompletionTokens,
		CacheHit:       cacheHit,
		// telemetry stays a dependency-free leaf (no decimal.Decimal
		// import) per docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md —
		// the exact decimal string is formatted here, at the boundary,
		// per docs/rfcs/2026-09-02-decimal-cost-accounting.md.
		CostUSD:    cost.String(),
		AgentRunID: telemetry.AgentRunIDFromContext(ctx),
		Err:        err,
	})
	span.End()

	p.logRequest(vk, req, resp, cacheHit, cost, err)
}

// finishReasons collects every non-empty FinishReason across resp's
// choices, in order — most responses have exactly one choice, but the
// canonical schema allows more, and gen_ai.response.finish_reasons is
// documented as an array for exactly that reason.
func finishReasons(resp adapter.ChatResponse) []string {
	var reasons []string
	for _, c := range resp.Choices {
		if c.FinishReason != "" {
			reasons = append(reasons, c.FinishReason)
		}
	}
	return reasons
}

// logRequest emits the structured JSON log line for one request. cost is
// precomputed by finalize (decimal.Zero when err != nil) so it's never
// calculated twice.
func (p *Pipeline) logRequest(vk *identity.VirtualKey, req adapter.ChatRequest, resp adapter.ChatResponse, cacheHit bool, cost decimal.Decimal, err error) {
	fields := []any{"model", req.Model, "cache_hit", cacheHit}
	if vk != nil {
		fields = append(fields, "virtual_key_id", vk.ID)
	}

	if err != nil {
		p.logger.Error("chat_completion", append(fields, "error", err.Error())...)
		return
	}

	fields = append(fields,
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
		"total_tokens", resp.Usage.TotalTokens,
		// A JSON string, not a bare number — decimal.Decimal.String() is
		// exact; a deliberate, documented format change from the old
		// float64 field per docs/rfcs/2026-09-02-decimal-cost-accounting.md.
		"cost_usd", cost.String(),
	)
	p.logger.Info("chat_completion", fields...)
}

// serializeMessages deterministically encodes a request's messages for
// use in the L1 cache key fabricator (internal/cache.Key). encoding/json
// cannot fail on this struct shape (no channels/funcs/unsupported map key
// types), so a failure here indicates a bug in the canonical types, not a
// runtime condition callers should have to handle.
func serializeMessages(messages []adapter.Message) string {
	b, err := json.Marshal(messages)
	if err != nil {
		panic(fmt.Sprintf("dataplane: marshaling messages for cache key: %v", err))
	}
	return string(b)
}

// responseUnmarshalers decodes raw upstream JSON response bytes into the
// concrete provider-native type each adapter's FromProvider expects.
// Keyed by provider name (adapter.Adapter.Name()).
var responseUnmarshalers = map[string]func([]byte) (any, error){
	"openai": func(b []byte) (any, error) {
		var r openai.Response
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("unmarshaling openai response: %w", err)
		}
		return &r, nil
	},
	"anthropic": func(b []byte) (any, error) {
		var r anthropic.Response
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("unmarshaling anthropic response: %w", err)
		}
		return &r, nil
	},
}

// NewHTTPUpstreamCaller returns a real, working UpstreamCaller that POSTs
// the marshaled provider-native request to dep.BaseURL and decodes the
// response via responseUnmarshalers. This is what cmd/gateway wires up in
// production; tests inject a fake UpstreamCaller instead so the pipeline
// is fully testable without a real network call.
func NewHTTPUpstreamCaller(client *http.Client) UpstreamCaller {
	return func(ctx context.Context, dep Deployment, providerReq any) (any, error) {
		body, err := json.Marshal(providerReq)
		if err != nil {
			return nil, fmt.Errorf("marshaling provider request: %w", err)
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, dep.BaseURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("building upstream request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		setUpstreamAuthHeaders(httpReq, dep)

		httpResp, err := client.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("calling upstream %q: %w", dep.BaseURL, err)
		}
		// The response body is about to be fully drained by io.ReadAll
		// below; any error Close returns after that point is not
		// actionable (there's no reader left to retry or recover), so it
		// is discarded explicitly rather than left for errcheck to keep
		// flagging.
		defer func() { _ = httpResp.Body.Close() }()

		respBody, err := io.ReadAll(httpResp.Body)
		if err != nil {
			return nil, fmt.Errorf("reading upstream response: %w", err)
		}
		if httpResp.StatusCode >= 300 {
			return nil, fmt.Errorf("upstream %q returned status %d: %s", dep.BaseURL, httpResp.StatusCode, string(respBody))
		}

		unmarshal, ok := responseUnmarshalers[dep.Provider]
		if !ok {
			return nil, fmt.Errorf("no response unmarshaler registered for provider %q", dep.Provider)
		}
		return unmarshal(respBody)
	}
}

// NewHTTPUpstreamStreamCaller returns a real, working UpstreamStreamCaller
// that POSTs the marshaled provider-native (streaming) request to
// dep.BaseURL and, on a successful (< 300) status, returns the raw response
// body for the caller to read incrementally as SSE frames — unlike
// NewHTTPUpstreamCaller, it does not drain or unmarshal the body itself,
// since that would defeat streaming's entire purpose.
func NewHTTPUpstreamStreamCaller(client *http.Client) UpstreamStreamCaller {
	return func(ctx context.Context, dep Deployment, providerReq any) (io.ReadCloser, error) {
		body, err := json.Marshal(providerReq)
		if err != nil {
			return nil, fmt.Errorf("marshaling provider stream request: %w", err)
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, dep.BaseURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("building upstream stream request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		setUpstreamAuthHeaders(httpReq, dep)

		httpResp, err := client.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("calling upstream %q: %w", dep.BaseURL, err)
		}

		if httpResp.StatusCode >= 300 {
			// An error response is not itself a stream — safe (and
			// necessary, to avoid leaking the connection) to drain and
			// close it here rather than handing an error body to a caller
			// expecting SSE frames.
			defer func() { _ = httpResp.Body.Close() }()
			errBody, _ := io.ReadAll(httpResp.Body)
			return nil, fmt.Errorf("upstream %q returned status %d: %s", dep.BaseURL, httpResp.StatusCode, string(errBody))
		}

		return httpResp.Body, nil
	}
}

// setUpstreamAuthHeaders sets the provider-specific auth header(s) for an
// outgoing upstream request. Anthropic's Messages API uses an "x-api-key"
// header plus a required "anthropic-version" header rather than a Bearer
// token — this is exactly the kind of per-provider quirk
// gateway/ARCHITECTURE.md's adapter escape hatch exists for, applied here
// at the transport layer since it's about auth, not request-body shape.
func setUpstreamAuthHeaders(httpReq *http.Request, dep Deployment) {
	switch dep.Provider {
	case "anthropic":
		httpReq.Header.Set("x-api-key", dep.APIKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	default:
		httpReq.Header.Set("Authorization", "Bearer "+dep.APIKey)
	}
}
