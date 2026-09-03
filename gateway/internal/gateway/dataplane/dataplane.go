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
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/text/unicode/norm"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	gatewayeventsv1 "github.com/kelvran/gateway/gateway/api/gatewayevents/v1"
	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
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

// ErrNoDeployment is returned when no configured Deployment routes the
// requested model. Wrapped (not returned bare) so callers — including
// outcomeFor's errors.Is classification for
// docs/rfcs/2026-09-03-api-gatewayevents-contract.md's GatewayDecisionEvent —
// can distinguish this from an upstream error without string-matching.
var ErrNoDeployment = errors.New("dataplane: no deployment configured for requested model")

// ErrGuardrailBlocked is returned when a pre-call or post-call guardrail
// check's Block-tier verdict rejects a request, per
// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md. Never returned
// for a Warn-tier finding, which never blocks.
var ErrGuardrailBlocked = errors.New("dataplane: request blocked by guardrail policy")

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
	Budget *budget.Tracker
	Cache  cache.Cache
	// CacheL2 is the normalized-match layer checked on an L1 (Cache) miss,
	// per docs/rfcs/2026-09-03-cache-l2-normalized-match.md. Required,
	// like every other dependency here — cmd/gateway always constructs
	// one (a second inprocess.New(...) call), there is no "L2 disabled"
	// mode.
	CacheL2 cache.Cache
	// CacheL3 is the lexical-near-duplicate layer checked on an L1/L2
	// miss, per docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md.
	// Required, like every other dependency here.
	CacheL3 cache.LexicalCache
	// Guardrails runs the pre-call/post-call content checks, per
	// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md. Required,
	// like every other dependency here, never optional the way
	// UpstreamStream is — THREAT_MODEL.md/SECURITY.md already classify
	// guardrail bypass as a named severity item, so this is a hard
	// dependency, not a rollout-optional one. A config-level "disable"
	// is expressed as an Engine with zero detectors registered, never
	// Guardrails == nil.
	Guardrails     *guardrail.Engine
	Adapters       adapter.Registry
	Deployments    []Deployment
	CostCalculator *costaccounting.Calculator
	Upstream       UpstreamCaller
	Logger         *slog.Logger
	CacheTTL       time.Duration
	// CacheL2TTL defaults to 75 seconds when unset — shorter than
	// CacheTTL's 5-minute default, as defense-in-depth per the RFC's TTL
	// rationale (not a substitute for the normalization allowlist's own
	// collision-freedom guarantee).
	CacheL2TTL time.Duration
	// CacheL3TTL defaults to 5 minutes when unset — this is a raw
	// storage TTL (how long an entry can be found at all), distinct from
	// l3StalenessBudget (how old a FOUND candidate is allowed to be
	// before the freshness/risk model rejects it); the storage TTL is
	// deliberately longer than the staleness budget so the risk-model
	// check, not silent expiry, is what usually decides staleness.
	CacheL3TTL time.Duration
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
	cacheL2            cache.Cache
	cacheL3            cache.LexicalCache
	guardrails         *guardrail.Engine
	adapters           adapter.Registry
	deploymentsByModel map[string][]Deployment
	rrMu               sync.Mutex
	rrCounters         map[string]*atomic.Uint64
	costCalc           *costaccounting.Calculator
	upstream           UpstreamCaller
	upstreamStream     UpstreamStreamCaller
	logger             *slog.Logger
	cacheTTL           time.Duration
	cacheL2TTL         time.Duration
	cacheL3TTL         time.Duration
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
	case cfg.CacheL2 == nil:
		return nil, fmt.Errorf("dataplane: Config.CacheL2 is required")
	case cfg.CacheL3 == nil:
		return nil, fmt.Errorf("dataplane: Config.CacheL3 is required")
	case cfg.Guardrails == nil:
		return nil, fmt.Errorf("dataplane: Config.Guardrails is required")
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
	l2TTL := cfg.CacheL2TTL
	if l2TTL <= 0 {
		l2TTL = 75 * time.Second
	}
	l3TTL := cfg.CacheL3TTL
	if l3TTL <= 0 {
		l3TTL = 5 * time.Minute
	}

	return &Pipeline{
		verifier:           cfg.Verifier,
		limiter:            cfg.Limiter,
		budget:             cfg.Budget,
		cache:              cfg.Cache,
		cacheL2:            cfg.CacheL2,
		cacheL3:            cfg.CacheL3,
		guardrails:         cfg.Guardrails,
		adapters:           cfg.Adapters,
		deploymentsByModel: byModel,
		rrCounters:         map[string]*atomic.Uint64{},
		costCalc:           cfg.CostCalculator,
		upstream:           cfg.Upstream,
		upstreamStream:     cfg.UpstreamStream,
		logger:             logger,
		cacheTTL:           ttl,
		cacheL2TTL:         l2TTL,
		cacheL3TTL:         l3TTL,
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

// checkRateLimit reports whether vk may proceed, and whether that answer
// was a fail-open (rate limiter backend errored, request let through
// anyway) rather than a genuine rate-limit decision — failedOpen is
// surfaced to the caller specifically so it can reach
// GatewayDecisionEvent.RateLimitFailOpen, per
// docs/rfcs/2026-09-03-gatewayevents-decision-enrichment.md. A Redis
// backend error (network failure, timeout) is logged and the request is
// allowed through rather than rejected — see
// docs/rfcs/2026-09-03-distributed-rate-limiting.md's "Fail-open, not
// fail-closed" section for why that's the right default specifically for
// Kelvran: internal/budget.Tracker's per-key USD cap is a second,
// independent control that never touches Redis, so a rate-limiter
// outage alone does not remove every spending control at once. In
// in-memory mode, p.limiter.Allow never returns an error at all, so this
// fail-open path is only ever exercised when a Redis backend is
// configured.
func (p *Pipeline) checkRateLimit(ctx context.Context, vk *identity.VirtualKey) (ok bool, failedOpen bool) {
	allowed, err := p.limiter.Allow(ctx, vk.ID)
	if err != nil {
		p.logger.Warn("ratelimit_backend_unavailable", "key_id", vk.ID, "error", err.Error())
		return true, true
	}
	return allowed, false
}

// fallbackInfo captures whether a request fell back to a second
// deployment, and if so which one and why — captured at the one point in
// the fallback block where the original dep/err are still available,
// before they're overwritten, per
// docs/rfcs/2026-09-03-gatewayevents-decision-enrichment.md. Kelvran's
// fallback logic attempts at most one fallback per request, never a
// chain, so this is a fixed 3-field record, not a repeated/list shape.
type fallbackInfo struct {
	happened bool
	from     string // Deployment.Name first tried and abandoned.
	reason   string // err.Error() from the first attempt.
}

// checkCache checks L1 (exact) then L2 (normalized) in that order, per
// docs/rfcs/2026-09-03-cache-l2-normalized-match.md. An L2 hit is
// promoted into L1 (best-effort — a promotion failure never affects the
// response already found) so the next byte-identical repeat becomes an
// L1 hit.
func (p *Pipeline) checkCache(ctx context.Context, l1Key, l2Key string) (cached []byte, hit bool) {
	if cached, ok, getErr := p.cache.Get(ctx, l1Key); getErr == nil && ok {
		return cached, true
	}
	if cached, ok, getErr := p.cacheL2.Get(ctx, l2Key); getErr == nil && ok {
		_ = p.cache.Put(ctx, l1Key, cached, p.cacheTTL)
		return cached, true
	}
	return nil, false
}

// writeCache writes encoded to all three cache layers, eagerly and
// best-effort, on a genuine miss — gateway/ARCHITECTURE.md's Request
// Lifecycle says write-back covers "all layers." No lazy/async
// population: the response is already in hand.
func (p *Pipeline) writeCache(ctx context.Context, tenantID, l1Key, l2Key string, l3Signature []uint64, l3Fingerprint map[string]struct{}, modelID string, encoded []byte) {
	_ = p.cache.Put(ctx, l1Key, encoded, p.cacheTTL)
	_ = p.cacheL2.Put(ctx, l2Key, encoded, p.cacheL2TTL)
	_ = p.cacheL3.Put(ctx, tenantID, l3Signature, encoded, l3Fingerprint, modelID, p.guardrails.Version(), p.cacheL3TTL)
}

// l3ShingleWords, l3SignatureSize, and l3SearchK are Cache L3-lite's own
// tuning constants — standard textbook MinHash defaults (see
// docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md's Unresolved
// Questions: not yet tuned against real traffic, which doesn't exist).
const (
	l3ShingleWords  = 3
	l3SignatureSize = 128
	l3SearchK       = 5
	// l3MinSimilarity is THREAT_MODEL.md's "~0.9" floor, applied here to
	// a Jaccard estimate rather than the embedding-cosine similarity it
	// was originally specified for — an unvalidated transfer, stated
	// plainly in the RFC's own Unresolved Questions, not assumed safe.
	l3MinSimilarity = 0.9
	// l3StalenessBudget is a single, global staleness budget for this
	// first pass — the RFC's own freshness-risk-model checklist calls
	// for per-content-type budgets; this ships one bucket now and defers
	// real tiering to when calibration data exists.
	l3StalenessBudget = 24 * time.Hour
)

// volatileQueryPattern matches queries whose correct answer changes over
// time in a way no cache TTL can honestly bound — per
// docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md's freshness/
// risk-model checklist item 2. A hard bypass straight to upstream, never
// a soft risk-score adjustment.
var volatileQueryPattern = regexp.MustCompile(`(?i)\b(weather|price|stock|score|today|current|currently|now|latest)\b`)

// isVolatileQuery reports whether any message matches the volatility
// keyword list above.
func isVolatileQuery(messages []adapter.Message) bool {
	for _, m := range messages {
		if volatileQueryPattern.MatchString(m.Content) {
			return true
		}
	}
	return false
}

// freshnessRiskModel implements docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md's
// checklist items 1 (staleness budget), 3 (similarity floor), and 5
// (model-version exact match) — item 2 (volatility bypass) and item 4
// (entity/number/date hard-gate) are checked separately in
// checkLexicalCache, since they apply BEFORE and INDEPENDENTLY of this
// function respectively. A mismatch on any check here is an outright
// rejection, never partial credit.
func freshnessRiskModel(writtenAt time.Time, storedModelID, currentModelID string, similarity float64) bool {
	if time.Since(writtenAt) > l3StalenessBudget {
		return false
	}
	if similarity < l3MinSimilarity {
		return false
	}
	if storedModelID != currentModelID {
		return false
	}
	return true
}

// checkLexicalCache is Cache L3-lite's own check, run after an L1/L2
// miss and before the router — per
// docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md. Every
// rejection path (volatile bypass, search error, entity/number/date
// mismatch, freshness/risk-model failure) falls through to a real
// upstream call, never an unchecked serve — this is Kelvran's single
// highest-consequence cache check, per AGENTS.md's explicit "Never" rule
// against weakening it, so every branch here is a hard, visible
// rejection, not a soft score.
func (p *Pipeline) checkLexicalCache(ctx context.Context, vk *identity.VirtualKey, req adapter.ChatRequest, signature []uint64) (cached []byte, hit bool) {
	if isVolatileQuery(req.Messages) {
		return nil, false
	}
	candidates, err := p.cacheL3.Search(ctx, vk.ID, signature, l3SearchK)
	if err != nil {
		p.logger.Warn("lexical_cache_search_failed", "key_id", vk.ID, "error", err.Error())
		return nil, false // fail-closed: a search error skips L3, never bypasses the gate
	}
	queryFingerprint := Fingerprint(req.Messages)
	for _, c := range candidates {
		if !fingerprintsEqual(queryFingerprint, c.Fingerprint) {
			continue
		}
		if !freshnessRiskModel(c.WrittenAt, c.ModelID, req.Model, c.Similarity) {
			continue
		}
		// A new, separate gate from freshnessRiskModel — per
		// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md, never
		// folded into that function, which stays scoped to Cache
		// L3-lite's own checklist. A candidate written under a since-
		// changed guardrail policy/detector set is a forced miss, never
		// a silent, unchecked serve.
		if c.GuardrailPolicyVersion != p.guardrails.Version() {
			continue
		}
		return c.Resp, true
	}
	return nil, false
}

// fingerprintsEqual reports whether two entity/number/date fingerprints
// are identical sets — an exact match, never a subset/overlap check, per
// docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md's "deliberately
// blunt rather than clever" hard-gate design.
func fingerprintsEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
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
		cacheHit              bool
		vk                    *identity.VirtualKey
		dep                   Deployment
		rateLimitFailedOpen   bool
		fallback              fallbackInfo
		budgetSpentAtDecision decimal.Decimal
	)

	ctx, span := telemetry.Tracer.Start(ctx, "chat "+req.Model)
	defer func() {
		p.finalize(ctx, span, vk, dep, req, resp, cacheHit, rateLimitFailedOpen, fallback, budgetSpentAtDecision, err)
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

	var rateLimitOK bool
	rateLimitOK, rateLimitFailedOpen = p.checkRateLimit(ctx, vk)
	if !rateLimitOK {
		err = ErrRateLimited
		return
	}

	budgetSpentAtDecision = p.budget.SpentUSD(vk.ID)
	if !p.budget.Allow(vk.ID, vk.BudgetUSD) {
		err = ErrBudgetExceeded
		return
	}

	l1Key := cache.Key(vk.ID, req.Model, serializeMessages(req.Messages), req.Temperature, req.MaxTokens, p.guardrails.Version())
	l2Key := cache.NormalizedKey(vk.ID, req.Model, normalizeMessages(req.Messages), req.Temperature, req.MaxTokens, p.guardrails.Version())
	l3Signature := cache.MinHashSignature(cache.Shingles(normalizeMessages(req.Messages), l3ShingleWords), l3SignatureSize)

	if cached, ok := p.checkCache(ctx, l1Key, l2Key); ok {
		var cachedResp adapter.ChatResponse
		if unmarshalErr := json.Unmarshal(cached, &cachedResp); unmarshalErr == nil {
			resp = cachedResp
			cacheHit = true
			return
		}
		// A corrupt cache entry is treated as a miss, not a request
		// failure — fall through to the upstream path below.
	}

	if cached, ok := p.checkLexicalCache(ctx, vk, req, l3Signature); ok {
		var cachedResp adapter.ChatResponse
		if unmarshalErr := json.Unmarshal(cached, &cachedResp); unmarshalErr == nil {
			resp = cachedResp
			cacheHit = true
			return
		}
		// A corrupt cache entry is treated as a miss, not a request
		// failure — fall through to the upstream path below.
	}

	// Guardrail pre-call: after L1/L2/L3 all miss, before the router — per
	// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md, matching
	// gateway/ARCHITECTURE.md's Request Lifecycle exactly. A cache hit
	// above never reaches this check at all — its provenance was already
	// checked under the current policy at write time, per this same RFC's
	// cache-key/GuardrailPolicyVersion mechanism.
	if verdict := p.guardrails.Check(ctx, serializeMessages(req.Messages)); verdict.Blocked {
		p.logger.Warn("guardrail_blocked_precall", "key_id", vk.ID, "finding_count", len(verdict.Findings))
		err = ErrGuardrailBlocked
		return
	}

	var found bool
	dep, found = p.nextDeployment(req.Model)
	if !found {
		err = fmt.Errorf("%w: %q", ErrNoDeployment, req.Model)
		return
	}

	resp, err = p.callDeployment(ctx, dep, req)
	if err != nil {
		// Single fallback to the next deployment for the same model, per
		// gateway/ARCHITECTURE.md's router step.
		if fallbackDep, hasFallback := p.nextDeployment(req.Model); hasFallback && fallbackDep.Name != dep.Name {
			fallback = fallbackInfo{happened: true, from: dep.Name, reason: err.Error()}
			dep = fallbackDep
			resp, err = p.callDeployment(ctx, dep, req)
		}
	}
	if err != nil {
		err = fmt.Errorf("dataplane: upstream call failed for model %q: %w", req.Model, err)
		return
	}

	// Guardrail post-call, buffered path: resp is guaranteed fully
	// populated here and nothing downstream (cache write, return to
	// client) has happened yet — a Block verdict can still refuse both.
	if postVerdict := p.guardrails.Check(ctx, serializeResponse(resp)); postVerdict.Blocked {
		p.logger.Warn("guardrail_blocked_postcall", "key_id", vk.ID, "finding_count", len(postVerdict.Findings))
		err = ErrGuardrailBlocked
		return
	}

	if encoded, marshalErr := json.Marshal(resp); marshalErr == nil {
		p.writeCache(ctx, vk.ID, l1Key, l2Key, l3Signature, Fingerprint(req.Messages), req.Model, encoded)
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
// hits, which never touch a deployment at all). rateLimitFailedOpen,
// fallback, and budgetSpentAtDecision feed GatewayDecisionEvent's 3
// enrichment fields per
// docs/rfcs/2026-09-03-gatewayevents-decision-enrichment.md — all three
// are zero-valued whenever the corresponding check never ran (e.g. auth
// failed before the rate-limit check), which is the correct, intentional
// "not applicable" representation for those fields.
func (p *Pipeline) finalize(ctx context.Context, span trace.Span, vk *identity.VirtualKey, dep Deployment, req adapter.ChatRequest, resp adapter.ChatResponse, cacheHit bool, rateLimitFailedOpen bool, fallback fallbackInfo, budgetSpentAtDecision decimal.Decimal, err error) {
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

	event := &gatewayeventsv1.GatewayDecisionEvent{
		TraceId:                span.SpanContext().TraceID().String(),
		SpanId:                 span.SpanContext().SpanID().String(),
		OccurredAt:             timestamppb.Now(),
		VirtualKeyId:           virtualKeyID,
		RequestedModel:         req.Model,
		Outcome:                outcomeFor(err),
		RateLimitFailOpen:      rateLimitFailedOpen,
		FallbackHappened:       fallback.happened,
		FallbackFromDeployment: fallback.from,
		FallbackReason:         fallback.reason,
		BudgetSpentUsd:         budgetSpentAtDecision.String(),
	}
	span.End()

	p.logRequest(vk, req, resp, cacheHit, cost, err, event)
}

// outcomeFor derives a GatewayDecisionEvent's structured Outcome from
// the same sentinel errors HandleChatCompletion/HandleChatCompletionStream
// already return — per docs/rfcs/2026-09-03-api-gatewayevents-contract.md,
// no new rejection categories, no changes to either method's control
// flow, only classification of what err already is.
func outcomeFor(err error) gatewayeventsv1.GatewayDecisionEvent_Outcome {
	switch {
	case err == nil:
		return gatewayeventsv1.GatewayDecisionEvent_OUTCOME_OK
	case errors.Is(err, identity.ErrMissingHeader), errors.Is(err, identity.ErrInvalidKey):
		return gatewayeventsv1.GatewayDecisionEvent_OUTCOME_AUTH_FAILED
	case errors.Is(err, ErrModelNotAllowed):
		return gatewayeventsv1.GatewayDecisionEvent_OUTCOME_MODEL_NOT_ALLOWED
	case errors.Is(err, ErrRateLimited):
		return gatewayeventsv1.GatewayDecisionEvent_OUTCOME_RATE_LIMITED
	case errors.Is(err, ErrBudgetExceeded):
		return gatewayeventsv1.GatewayDecisionEvent_OUTCOME_BUDGET_EXCEEDED
	case errors.Is(err, ErrNoDeployment):
		return gatewayeventsv1.GatewayDecisionEvent_OUTCOME_NO_DEPLOYMENT
	case errors.Is(err, ErrGuardrailBlocked):
		return gatewayeventsv1.GatewayDecisionEvent_OUTCOME_GUARDRAIL_BLOCKED
	default:
		return gatewayeventsv1.GatewayDecisionEvent_OUTCOME_UPSTREAM_ERROR
	}
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
func (p *Pipeline) logRequest(vk *identity.VirtualKey, req adapter.ChatRequest, resp adapter.ChatResponse, cacheHit bool, cost decimal.Decimal, err error, event *gatewayeventsv1.GatewayDecisionEvent) {
	fields := []any{"model", req.Model, "cache_hit", cacheHit}
	if vk != nil {
		fields = append(fields, "virtual_key_id", vk.ID)
	}
	// gatewayevents_v1 is added on BOTH the error and success paths below
	// — Outcome is exactly as meaningful for a rejection as for a
	// success, per docs/rfcs/2026-09-03-api-gatewayevents-contract.md. A
	// marshal failure (never expected for a validly-constructed proto3
	// message with no required fields, but the API can still return one)
	// is logged and the field is simply omitted — it must never block the
	// rest of this already-real log line.
	if encoded, marshalErr := protojson.Marshal(event); marshalErr != nil {
		p.logger.Warn("gatewayevents_marshal_failed", "error", marshalErr.Error())
	} else {
		fields = append(fields, "gatewayevents_v1", string(encoded))
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

// serializeResponse extracts a response's text content for the
// guardrail post-call check, per
// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md. Deliberately
// minimal, mirroring serializeMessages' own scope — every choice's
// message content, newline-joined, not a full JSON re-encoding (the
// guardrail scans text, not structure).
func serializeResponse(resp adapter.ChatResponse) string {
	contents := make([]string, 0, len(resp.Choices))
	for _, c := range resp.Choices {
		contents = append(contents, c.Message.Content)
	}
	return strings.Join(contents, "\n")
}

// trailingTerminalPunctuation is the exact, closed set
// normalizeMessages's third allowlist operation strips — nothing else,
// ever, without a new RFC revision. See
// docs/rfcs/2026-09-03-cache-l2-normalized-match.md's "narrower than this
// RFC's own grounding research recommended, and why" for why this list
// is deliberately small.
const trailingTerminalPunctuation = ".!?"

// normalizeMessages produces the L2 (normalized-match) input to
// cache.NormalizedKey, applying EXACTLY the 3-operation allowlist
// docs/rfcs/2026-09-03-cache-l2-normalized-match.md specifies — outer
// whitespace trim, Unicode NFC, and a single trailing terminal
// punctuation mark stripped from the last message only. Every other
// field (Role, ToolCalls, ToolCallID) and every other message's Content
// is passed through unchanged: a real difference there means a real
// different request, never something to normalize away.
//
// Deliberately does NOT collapse internal whitespace or fold case — both
// were considered and rejected for v1 because Kelvran serves agent
// traffic that plausibly includes pasted code, where indentation and
// identifier case can be genuinely meaningful; see the RFC's Detailed
// Design section for the concrete collision examples that motivated
// this.
func normalizeMessages(messages []adapter.Message) string {
	normalized := make([]adapter.Message, len(messages))
	copy(normalized, messages)

	for i := range normalized {
		content := strings.TrimSpace(normalized[i].Content)
		content = norm.NFC.String(content)
		if i == len(normalized)-1 && content != "" {
			last := content[len(content)-1]
			if strings.IndexByte(trailingTerminalPunctuation, last) >= 0 {
				content = content[:len(content)-1]
			}
		}
		normalized[i].Content = content
	}

	b, err := json.Marshal(normalized)
	if err != nil {
		panic(fmt.Sprintf("dataplane: marshaling normalized messages for L2 cache key: %v", err))
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
