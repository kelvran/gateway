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

	"github.com/kelvran/gateway/internal/adapter"
	"github.com/kelvran/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/internal/cache"
	"github.com/kelvran/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/internal/identity"
	"github.com/kelvran/gateway/internal/ratelimit"
)

// ErrRateLimited is returned by HandleChatCompletion when the caller has
// exhausted the configured rate-limit token bucket.
var ErrRateLimited = errors.New("dataplane: rate limit exceeded")

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
	Verifier       *identity.Verifier
	Limiter        *ratelimit.TokenBucket
	Cache          cache.Cache
	Adapters       adapter.Registry
	Deployments    []Deployment
	CostCalculator *costaccounting.Calculator
	Upstream       UpstreamCaller
	Logger         *slog.Logger
	CacheTTL       time.Duration
}

// Pipeline is the wired dataplane request pipeline.
type Pipeline struct {
	verifier           *identity.Verifier
	limiter            *ratelimit.TokenBucket
	cache              cache.Cache
	adapters           adapter.Registry
	deploymentsByModel map[string][]Deployment
	rrMu               sync.Mutex
	rrCounters         map[string]*atomic.Uint64
	costCalc           *costaccounting.Calculator
	upstream           UpstreamCaller
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
		cache:              cfg.Cache,
		adapters:           cfg.Adapters,
		deploymentsByModel: byModel,
		rrCounters:         map[string]*atomic.Uint64{},
		costCalc:           cfg.CostCalculator,
		upstream:           cfg.Upstream,
		logger:             logger,
		cacheTTL:           ttl,
	}, nil
}

// HandleChatCompletion runs the full request pipeline for one canonical
// ChatRequest, given the raw Authorization header value.
func (p *Pipeline) HandleChatCompletion(ctx context.Context, authorizationHeader string, req adapter.ChatRequest) (resp adapter.ChatResponse, err error) {
	var cacheHit bool

	defer func() {
		p.logRequest(req, resp, cacheHit, err)
	}()

	if verifyErr := p.verifier.Verify(authorizationHeader); verifyErr != nil {
		err = fmt.Errorf("dataplane: auth: %w", verifyErr)
		return
	}

	if !p.limiter.Allow() {
		err = ErrRateLimited
		return
	}

	key := cache.Key(req.Model, serializeMessages(req.Messages), req.Temperature, req.MaxTokens)

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

	dep, found := p.nextDeployment(req.Model)
	if !found {
		err = fmt.Errorf("dataplane: no deployment configured for model %q", req.Model)
		return
	}

	resp, err = p.callDeployment(ctx, dep, req)
	if err != nil {
		// Single fallback to the next deployment for the same model, per
		// gateway/ARCHITECTURE.md's router step.
		if fallbackDep, hasFallback := p.nextDeployment(req.Model); hasFallback && fallbackDep.Name != dep.Name {
			resp, err = p.callDeployment(ctx, fallbackDep, req)
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

// logRequest emits the structured JSON log line for one request,
// including the cost calculation on success. It is always called via
// defer in HandleChatCompletion, so it runs even when err != nil — a
// partial/failed generation still gets logged, per
// gateway/ARCHITECTURE.md's "ALWAYS runs, even on error/cancel" note.
func (p *Pipeline) logRequest(req adapter.ChatRequest, resp adapter.ChatResponse, cacheHit bool, err error) {
	if err != nil {
		p.logger.Error("chat_completion",
			"model", req.Model,
			"cache_hit", cacheHit,
			"error", err.Error(),
		)
		return
	}

	cost := p.costCalc.Calculate(req.Model, costaccounting.Usage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
	})

	p.logger.Info("chat_completion",
		"model", req.Model,
		"cache_hit", cacheHit,
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
		"total_tokens", resp.Usage.TotalTokens,
		"cost_usd", cost,
	)
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
		defer httpResp.Body.Close()

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
