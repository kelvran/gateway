// Package dataplane implements the gateway's request pipeline exactly as
// gateway/ARCHITECTURE.md's Request Lifecycle describes, minus streaming
// (every response is buffered, non-streaming, this pass) and minus
// guardrails/MCP (not built yet — Phase 1+ per PRD.md):
//
//	auth -> rate-limit -> cache lookup (L1 exact) -> hit? return
//	     -> miss -> router (weighted round-robin, via internal/router, per
//	        docs/rfcs/2026-09-04-weighted-routing.md) + error-classified,
//	        multi-hop fallback (per-deployment fallback_chains config, or
//	        the pre-existing single-fallback-via-router behavior when a
//	        deployment has none configured — see
//	        docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md)
//	     -> adapter -> upstream HTTP call -> adapter (response)
//	     -> cache write-back -> structured JSON log (incl. cost) -> response
//
// Cost/observability finalization (the log line) always runs, even on
// error, via a deferred closure over named return values — mirroring
// gateway/ARCHITECTURE.md's note that this step must always execute,
// since a partial generation still consumed billable output tokens.
package dataplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/singleflight"
	"golang.org/x/text/unicode/norm"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	gatewayeventsv1 "github.com/kelvran/gateway/gateway/api/gatewayevents/v1"
	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/gateway/internal/adapter/gemini"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/adapter/openaicompat"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/router"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// ErrRateLimited is returned by HandleChatCompletion when the caller's
// virtual key has exhausted its own rate-limit token bucket.
var ErrRateLimited = errors.New("dataplane: rate limit exceeded")

// ErrBudgetExceeded is returned when the caller's virtual key has spent at
// least its configured BudgetUSD cap. See internal/budget.
var ErrBudgetExceeded = errors.New("dataplane: budget exceeded")

// ErrConcurrencyLimitExceeded is returned when the caller's virtual key
// already has its configured MaxInFlight requests outstanding, per
// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md. Classified
// alongside ErrRateLimited (OUTCOME_RATE_LIMITED, HTTP 429) rather than
// given its own GatewayDecisionEvent_Outcome value — both are, from a
// client's perspective, the same kind of decision ("you are being
// throttled, back off and retry"), and avoiding a new outcome value
// avoids a protobuf schema change to api/gatewayevents/v1 for this pass,
// mirroring fallback.go's own FallbackClassGeneric precedent of folding
// rate limits into an existing bucket rather than minting a new one.
var ErrConcurrencyLimitExceeded = errors.New("dataplane: concurrency limit exceeded")

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
	// AccessKeyID/SecretAccessKey/SessionToken are resolved AWS
	// credential values, populated only for Provider == "bedrock" —
	// see docs/rfcs/2026-09-04-bedrock-adapter.md. SessionToken is empty
	// for the common case of long-lived IAM user credentials.
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// Region is the AWS region SigV4 signing is computed against. Not a
	// secret — populated directly from config, never resolved from an
	// env var the way the credential fields above are.
	Region string
	// FallbackChains maps an error class (FallbackClassContentPolicy,
	// FallbackClassContextWindowExceeded, FallbackClassGeneric) to an
	// ordered list of OTHER deployments' Names to attempt, in order, when
	// a call to THIS deployment fails with that class of error — per
	// docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md.
	// Empty/nil (the default) means this deployment has not opted into
	// explicit fallback-chain configuration at all: runMissPath/
	// streamDeploymentWithFallback fall through to the pre-existing,
	// router-based single-fallback behavior for it instead, preserving
	// every config written before this feature existed exactly as-is.
	FallbackChains map[string][]string
	// DisableCacheControlAutoPopulate opts this deployment OUT of
	// Kelvran's default auto-population of a CacheControl marker on an
	// otherwise-unmarked system message, per
	// docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md. False
	// (the default) means auto-populate stays ON. Threaded onto the
	// canonical adapter.ChatRequest's own field of the same name at
	// every ToProvider call site (callDeployment here;
	// streamDeployment/streamDeploymentBedrock in streaming.go),
	// mirroring how UpstreamModel is already threaded onto Model.
	//
	// Never read directly at those call sites — see
	// effectiveCacheControlAutoDisabled, which also composes this field
	// with SharedAcrossTenants below.
	DisableCacheControlAutoPopulate bool
	// SharedAcrossTenants declares that this deployment's own upstream
	// credential (APIKey, or AccessKeyID/SecretAccessKey for Bedrock) is
	// deliberately shared by more than one tenant's virtual key(s) — a
	// real, structural possibility since routing (internal/router)
	// selects purely by canonical model name with no tenant awareness at
	// all, and a config can legitimately point multiple virtual keys at
	// deployments sharing one "model" value. Per
	// docs/rfcs/2026-09-09-gateway-cache-shared-tenant-flag.md: when
	// true, CacheControl auto-populate is force-disabled for this
	// deployment regardless of DisableCacheControlAutoPopulate's own
	// value, closing a cross-tenant provider-side prompt-cache-leakage
	// risk (structurally the same shared-credential mechanism a 2026
	// CacheProbe/SAGAI'26 study measured live against OpenRouter) that
	// the 2026-09-07 auto-populate default-ON change otherwise exposes
	// by default on every shared deployment. An operator-declared fact,
	// not something Kelvran can detect itself — which virtual keys route
	// to a given deployment is a config-time property, not a per-request
	// signal checkable at cache-lookup time. False (the default) means
	// this deployment is not declared shared, and behaves exactly as
	// before this field existed.
	SharedAcrossTenants bool
}

// effectiveCacheControlAutoDisabled reports whether CacheControl
// auto-populate should be treated as disabled for this deployment —
// either the operator's own DisableCacheControlAutoPopulate opt-out, or
// because SharedAcrossTenants declares this deployment's credential is
// shared across tenants, which forces the same effective behavior
// regardless of DisableCacheControlAutoPopulate's own value. Every real
// call site (callDeployment here; streamDeployment/
// streamDeploymentBedrock in streaming.go) calls this method, never the
// two raw fields directly, so a future fourth call site can't forget
// the OR. Per docs/rfcs/2026-09-09-gateway-cache-shared-tenant-flag.md.
func (d Deployment) effectiveCacheControlAutoDisabled() bool {
	return d.DisableCacheControlAutoPopulate || d.SharedAcrossTenants
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
	// Concurrency bounds each virtual key's own in-flight-request count,
	// per docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md.
	// Deliberately OPTIONAL (nil-safe throughout, exactly like
	// UpstreamStream below) rather than required like every other
	// dependency in this Config — a nil Concurrency correctly behaves as
	// "concurrency limiting not configured," matching this codebase's
	// existing "zero/nil means disabled" convention for optional
	// subsystems (Redis rate-limiting, budget persistence, health-
	// probing), and keeps this feature's diff from forcing every existing
	// Pipeline test construction to change for a control most of those
	// tests aren't exercising.
	Concurrency *ratelimit.ConcurrencyLimiter
	// DeploymentConcurrency bounds each shared deployment's own aggregate
	// in-flight-request count, across every virtual key and every
	// fallback hop that routes to it — a genuinely different scope from
	// Concurrency above (per virtual key), per docs/upgrade-research/
	// gateway-per-deployment-concurrency-2026-09-09.md. Deliberately
	// optional (nil-safe), matching Concurrency's own convention.
	DeploymentConcurrency *ratelimit.ConcurrencyLimiter
	// DeploymentLimiter enforces each shared deployment's own aggregate
	// RPM ceiling, checked on every call to it regardless of which
	// virtual key initiated the request. Optional (nil-safe) — unlike
	// Limiter below (required; every virtual key always has SOME rate
	// limit), "no deployment-level throughput ceiling configured at all"
	// is the common, valid v1 state.
	DeploymentLimiter *ratelimit.KeyLimiter
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
	Guardrails *guardrail.Engine
	Adapters   adapter.Registry
	// Router selects which Deployment serves the next request for a
	// given model — weighted round-robin, per
	// docs/rfcs/2026-09-04-weighted-routing.md. Required, like every
	// other dependency here; built by the caller (cmd/gateway) from the
	// same Deployments list below.
	Router         *router.Router
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
//
// verifier is an atomic.Pointer, not a bare *identity.Verifier, so a
// virtual-key mutation via internal/admin (docs/rfcs/2026-09-05-gateway-
// admin-api.md) can swap it live: identity.Verifier is itself immutable
// once constructed, so UpsertVirtualKey/DeleteVirtualKey build a whole
// new Verifier from the current key set plus the one change, then Store
// it — every in-flight request that already Load'd the old pointer keeps
// running against a consistent, unchanged key set; every new request
// sees the new one. No lock, no partial-update window within one request.
type Pipeline struct {
	verifier atomic.Pointer[identity.Verifier]
	limiter  *ratelimit.KeyLimiter
	// concurrency is nil whenever Config.Concurrency was left unset — see
	// that field's own doc comment; checkConcurrency/releaseConcurrency
	// treat a nil concurrency as "no cap configured," never panicking.
	concurrency *ratelimit.ConcurrencyLimiter
	// deploymentConcurrency/deploymentLimiter are nil whenever
	// Config.DeploymentConcurrency/DeploymentLimiter were left unset —
	// checkDeploymentConcurrency/checkDeploymentRateLimit treat a nil
	// value as "no cap configured," never panicking, mirroring
	// concurrency's own convention above.
	deploymentConcurrency *ratelimit.ConcurrencyLimiter
	deploymentLimiter     *ratelimit.KeyLimiter
	// retryBackoff tracks each virtual key's own consecutive-rejection
	// streak for the client-facing Retry-After signal, per
	// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md's design
	// (a). Always non-nil (constructed unconditionally by NewPipeline,
	// unlike concurrency above) — it is pure in-memory bookkeeping with
	// no external resource to make optional, and every request needs
	// SOME answer to "reset or record this key's streak," never a
	// nil-check branch.
	retryBackoff      *ratelimit.RetryBackoff
	budget            *budget.Tracker
	cache             cache.Cache
	cacheL2           cache.Cache
	cacheL3           cache.LexicalCache
	guardrails        *guardrail.Engine
	adapters          adapter.Registry
	router            *router.Router
	deploymentsByName map[string]Deployment
	costCalc          *costaccounting.Calculator
	upstream          UpstreamCaller
	upstreamStream    UpstreamStreamCaller
	logger            *slog.Logger
	cacheTTL          time.Duration
	cacheL2TTL        time.Duration
	cacheL3TTL        time.Duration
	// missGroup deduplicates concurrent identical cache misses — see
	// runMissPath's own doc comment. Zero value is ready to use, per
	// golang.org/x/sync/singleflight's own documented contract; no
	// construction needed in NewPipeline.
	missGroup singleflight.Group
	// probeMu guards probeSchedule — see probeDueDeployments/
	// rescheduleDeployment, per
	// docs/rfcs/2026-09-08-gateway-health-probe-backoff.md. A separate
	// mutex from every other lock in this struct: probeSchedule is
	// mutated from each deployment's own probe goroutine concurrently
	// (mirroring healthMu's own per-concern-scoped-lock precedent in
	// internal/router/health.go), never touched by request-handling
	// code at all.
	probeMu sync.Mutex
	// probeSchedule tracks each deployment's own next-eligible-probe
	// time and current backoff magnitude. Absent entry (never probed
	// yet) is always due immediately — matching RunHealthProbeLoop's
	// pre-existing "first pass happens after the first interval
	// elapses" contract exactly, since every deployment starts with no
	// entry at process start.
	probeSchedule map[string]*healthProbeSchedule
	// now returns the current time — real time.Now in production
	// (NewPipeline's default), overridden directly by white-box tests
	// needing a fake clock (mirroring internal/budget.Tracker.now's own
	// identical convention) so probeDueDeployments' backoff growth can
	// be proven deterministically, without sleeping on real elapsed
	// time.
	now func() time.Time
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
	case cfg.Router == nil:
		return nil, fmt.Errorf("dataplane: Config.Router is required")
	case len(cfg.Deployments) == 0:
		return nil, fmt.Errorf("dataplane: Config.Deployments must be non-empty")
	case cfg.CostCalculator == nil:
		return nil, fmt.Errorf("dataplane: Config.CostCalculator is required")
	case cfg.Upstream == nil:
		return nil, fmt.Errorf("dataplane: Config.Upstream is required")
	}

	byName := map[string]Deployment{}
	for _, d := range cfg.Deployments {
		byName[d.Name] = d
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

	p := &Pipeline{
		limiter:               cfg.Limiter,
		concurrency:           cfg.Concurrency,
		deploymentConcurrency: cfg.DeploymentConcurrency,
		deploymentLimiter:     cfg.DeploymentLimiter,
		retryBackoff:          ratelimit.NewRetryBackoff(),
		budget:                cfg.Budget,
		cache:                 cfg.Cache,
		cacheL2:               cfg.CacheL2,
		cacheL3:               cfg.CacheL3,
		guardrails:            cfg.Guardrails,
		adapters:              cfg.Adapters,
		router:                cfg.Router,
		deploymentsByName:     byName,
		costCalc:              cfg.CostCalculator,
		upstream:              cfg.Upstream,
		upstreamStream:        cfg.UpstreamStream,
		logger:                logger,
		cacheTTL:              ttl,
		cacheL2TTL:            l2TTL,
		cacheL3TTL:            l3TTL,
		probeSchedule:         map[string]*healthProbeSchedule{},
		now:                   time.Now,
	}
	p.verifier.Store(cfg.Verifier)
	return p, nil
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

// ErrCannotDeleteLastVirtualKey is returned by DeleteVirtualKey when name
// is the only remaining configured virtual key — refused, rather than
// leaving the gateway with no possible way for any client to
// authenticate and no remaining way back in via this same admin API.
var ErrCannotDeleteLastVirtualKey = errors.New("dataplane: cannot delete the last remaining virtual key")

// ErrVirtualKeyNotFound is returned by DeleteVirtualKey when name does
// not match any currently-configured virtual key.
var ErrVirtualKeyNotFound = errors.New("dataplane: virtual key not found")

// UpsertVirtualKey adds vk as a new virtual key, or replaces the existing
// one with the same ID, live — no process restart, per
// docs/rfcs/2026-09-05-gateway-admin-api.md. rateLimit configures vk's own
// token-bucket parameters for internal/ratelimit.KeyLimiter.
//
// rateLimit is registered with the rate limiter BEFORE the new Verifier
// is swapped in — never the other way around. p.verifier.Load() is what
// makes an ID resolvable to callers of HandleChatCompletion at all, and
// checkRateLimit calls p.limiter.Allow(ctx, vk.ID) with no burst/refill
// parameters of its own; an ID that became resolvable before its rate
// limiter entry existed would hit the exact nil-bucket/zero-capacity
// hazard docs/rfcs/2026-09-05-gateway-admin-api.md's Design section
// found and named, not a hypothetical.
func (p *Pipeline) UpsertVirtualKey(vk identity.VirtualKey, rateLimit ratelimit.KeyConfig) error {
	current := p.verifier.Load().Keys()
	updated := make([]identity.VirtualKey, 0, len(current)+1)
	replaced := false
	for _, k := range current {
		if k.ID == vk.ID {
			updated = append(updated, vk)
			replaced = true
			continue
		}
		updated = append(updated, k)
	}
	if !replaced {
		updated = append(updated, vk)
	}

	newVerifier, err := identity.NewVerifier(updated)
	if err != nil {
		return fmt.Errorf("dataplane: UpsertVirtualKey: %w", err)
	}

	p.limiter.Register(rateLimit)
	p.verifier.Store(newVerifier)
	return nil
}

// DeleteVirtualKey removes the virtual key identified by name, live — no
// process restart. Returns ErrCannotDeleteLastVirtualKey, changing
// nothing, if name is the only remaining key (identity.NewVerifier's own
// "at least one virtual key is required" rule, reused directly rather
// than duplicating a second check here).
//
// The removed key's internal/ratelimit.KeyLimiter bucket/config entry is
// deliberately left in place rather than cleaned up — a bounded,
// per-key-sized amount of retained memory (one TokenBucket, or one
// KeyConfig struct) that is simply never accessed again, since the
// Verifier no longer resolves that ID to anything. Named explicitly as a
// real, self-limiting v1 gap rather than solved here — cleanup only
// matters if virtual-key churn ever becomes high-volume, which nothing
// about this feature's own use case implies.
func (p *Pipeline) DeleteVirtualKey(name string) error {
	current := p.verifier.Load().Keys()
	updated := make([]identity.VirtualKey, 0, len(current))
	found := false
	for _, k := range current {
		if k.ID == name {
			found = true
			continue
		}
		updated = append(updated, k)
	}
	if !found {
		return fmt.Errorf("%w: %q", ErrVirtualKeyNotFound, name)
	}

	newVerifier, err := identity.NewVerifier(updated)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCannotDeleteLastVirtualKey, err)
	}

	p.verifier.Store(newVerifier)
	return nil
}

// checkRateLimit reports whether vk may proceed against model, and
// whether that answer was a fail-open (rate limiter backend errored,
// request let through anyway) rather than a genuine rate-limit decision —
// failedOpen is surfaced to the caller specifically so it can reach
// GatewayDecisionEvent.RateLimitFailOpen, per
// docs/rfcs/2026-09-03-gatewayevents-decision-enrichment.md. A Redis
// backend error (network failure, timeout) is logged and the request is
// allowed through rather than rejected — see
// docs/rfcs/2026-09-03-distributed-rate-limiting.md's "Fail-open, not
// fail-closed" section for why that's the right default specifically for
// Kelvran: internal/budget.Tracker's per-key USD cap is a second,
// independent control that never touches Redis, so a rate-limiter
// outage alone does not remove every spending control at once. In
// in-memory mode, p.limiter.AllowForModel never returns an error at all,
// so this fail-open path is only ever exercised when a Redis backend is
// configured.
//
// model is threaded through to AllowForModel so a virtual key's own
// PerModel override (per
// docs/rfcs/2026-09-07-gateway-multi-dimensional-rate-limits.md), if one
// is configured for this specific model, is consulted before vk's default
// bucket — a key with no PerModel entries behaves exactly as if this
// parameter didn't exist.
//
// tpmReserved/tpmReservedTokens are ReserveTPM's own return values,
// per docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md: the
// caller MUST thread both through to finalize's ReconcileTPM call on
// every return path (including error), or a granted TPM reservation
// leaks permanently. Both are the harmless zero values whenever no real
// reservation was made — TPM not configured for vk, or the RPM check
// above already rejected the request before ReserveTPM was ever called.
func (p *Pipeline) checkRateLimit(ctx context.Context, vk *identity.VirtualKey, model string) (ok bool, failedOpen bool, tpmReserved bool, tpmReservedTokens float64) {
	allowed, err := p.limiter.AllowForModel(ctx, vk.ID, model)
	if err != nil {
		p.logger.Warn("ratelimit_backend_unavailable", "key_id", vk.ID, "error", err.Error())
		// A real, aggregate-friendly metric alongside the log line above,
		// per docs/rfcs/2026-09-05-gateway-ratelimit-fail-open-metric.md
		// — the structured log/GatewayDecisionEvent field already
		// existed per-request; this is what lets an operator alert on
		// "how many times has this happened recently" without scanning
		// every log line or trace.
		telemetry.RecordRateLimitFailOpen(ctx, vk.ID)
		return true, true, false, 0
	}
	if !allowed {
		return false, false, false, 0
	}
	// TPM dimension, per docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md,
	// now via ReserveTPM (docs/rfcs/2026-09-08-gateway-budget-ratelimit-
	// toctou-fix.md) rather than the old non-reserving AllowTPM: a plain,
	// non-erroring result — ReserveTPM never touches a backend in v1
	// (in-memory-only), so there's no fail-open case to handle here,
	// unlike Allow above. Checked only after the RPM check passes, so an
	// RPM-exhausted request is always rejected for that reason first,
	// matching this codebase's own existing check-ordering discipline
	// (model-allowed before rate-limit before budget).
	ok, tpmReserved, tpmReservedTokens = p.limiter.ReserveTPM(vk.ID, model)
	return ok, false, tpmReserved, tpmReservedTokens
}

// checkFallbackTargetRateLimit reports whether a fallback_chains hop to
// model may proceed, consulting ONLY the RPM dimension
// (p.limiter.AllowForModel) — never TPM, which stays scoped to the single
// pre-routing checkRateLimit/ReserveTPM call the caller already made
// against req.Model before this chain walk ever started. Closes
// evals/tests/fixtures/regression_corpus_cost_abuse.json's
// costabuse-permodel-ratelimit-bypassed-via-crossmodel-fallback-chain
// case: checkRateLimit itself only ever runs once, before
// routing/fallback resolution, keyed on the client's originally-requested
// model — with no equivalent re-check after a fallback_chains hop
// switches to a genuinely different model, a virtual key's PerModel RPM
// cap on an expensive model was entirely unenforced for traffic that
// reached it only via a cheaper model's own fallback hop.
// attemptFallbackChain calls this immediately before actually attempting
// each candidate target, mirroring its own pre-existing
// router.IsHealthy-skip pattern — a false return skips the target exactly
// like an unhealthy one, never wasting a real rate-limit consumption on a
// target that would be skipped for some other reason anyway.
//
// A rate-limiter backend error fails OPEN (returns true) rather than
// silently removing a fallback target from the chain for an infra reason
// unrelated to its real rate-limit status — the same "fail-open, not
// fail-closed" policy checkRateLimit's own doc comment documents for the
// identical reason (internal/budget.Tracker's per-key USD cap is a
// second, independent control that never touches Redis).
func (p *Pipeline) checkFallbackTargetRateLimit(ctx context.Context, keyID, model string) bool {
	allowed, err := p.limiter.AllowForModel(ctx, keyID, model)
	if err != nil {
		p.logger.Warn("ratelimit_backend_unavailable_fallback_hop", "key_id", keyID, "model", model, "error", err.Error())
		return true
	}
	return allowed
}

// checkConcurrency reserves one in-flight slot for vk.ID, per
// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md's design (b).
// Always true when Config.Concurrency was left unset (nil) — concurrency
// limiting not configured at all, matching this codebase's "unconfigured
// means unlimited" convention. Checked immediately after checkRateLimit
// succeeds, before the budget check — the same call site the task's own
// design targets, and a natural extension of this codebase's existing
// "model-allowed before rate-limit before budget" check-ordering
// discipline (checkRateLimit's own doc comment) with one new throttling
// dimension inserted between rate-limit and budget.
//
// Every true return MUST be paired with exactly one releaseConcurrency
// call — HandleChatCompletion/HandleChatCompletionStream both do this via
// defer immediately after a successful acquire.
func (p *Pipeline) checkConcurrency(vk *identity.VirtualKey) bool {
	if p.concurrency == nil {
		return true
	}
	return p.concurrency.Acquire(vk.ID)
}

// releaseConcurrency frees the in-flight slot checkConcurrency reserved.
// A no-op when Config.Concurrency was left unset, mirroring checkConcurrency.
func (p *Pipeline) releaseConcurrency(vk *identity.VirtualKey) {
	if p.concurrency == nil {
		return
	}
	p.concurrency.Release(vk.ID)
}

// checkDeploymentConcurrency reserves one in-flight slot against
// depName's own aggregate cap, per docs/upgrade-research/gateway-per-
// deployment-concurrency-2026-09-09.md. Always true when
// Config.DeploymentConcurrency is nil. Every true return MUST be paired
// with exactly one releaseDeploymentConcurrency call for the SAME
// depName once THIS HOP's own call finishes (success or error) — unlike
// per-key concurrency (checkConcurrency, held for the whole request),
// this is acquired/released PER HOP: once a request moves on to a
// different deployment, the original one is no longer receiving load
// from it and must not keep holding its slot.
func (p *Pipeline) checkDeploymentConcurrency(depName string) bool {
	if p.deploymentConcurrency == nil {
		return true
	}
	return p.deploymentConcurrency.Acquire(depName)
}

// releaseDeploymentConcurrency frees the slot checkDeploymentConcurrency
// reserved for depName. A no-op when Config.DeploymentConcurrency was
// left unset, mirroring releaseConcurrency.
func (p *Pipeline) releaseDeploymentConcurrency(depName string) {
	if p.deploymentConcurrency == nil {
		return
	}
	p.deploymentConcurrency.Release(depName)
}

// checkDeploymentRateLimit reports whether depName's own aggregate RPM
// ceiling allows one more call. Deployment-scoped, never per-model in
// v1 — model is deliberately never threaded through, unlike
// checkFallbackTargetRateLimit's own AllowForModel call. Fails OPEN on a
// backend error, mirroring checkFallbackTargetRateLimit's identical
// policy and rationale: an infra problem with the rate limiter itself
// must never silently remove a deployment from consideration.
//
// Checks KeyLimiter.HasLimit before ever calling Allow — a REAL,
// load-bearing check, not a redundant optimization: unlike a virtual key
// (which always resolves to a positive fallback capacity before
// construction), a deployment with no configured rate_limit is meant to
// be genuinely unlimited. Allow's own TokenBucket has no way to
// distinguish "never configured" from "configured with Capacity 0" —
// both have zero tokens and both would make Allow return false, which
// would silently deny every call to every deployment that never
// configured a rate_limit at all. HasLimit is what makes "0/unconfigured
// means unlimited" (per docs/upgrade-research/gateway-per-deployment-
// concurrency-2026-09-09.md) actually true rather than the opposite.
func (p *Pipeline) checkDeploymentRateLimit(ctx context.Context, depName string) bool {
	if p.deploymentLimiter == nil || !p.deploymentLimiter.HasLimit(depName) {
		return true
	}
	allowed, err := p.deploymentLimiter.Allow(ctx, depName)
	if err != nil {
		p.logger.Warn("deployment_ratelimit_backend_unavailable", "deployment", depName, "error", err.Error())
		return true
	}
	return allowed
}

// checkDeploymentCapacity is the combined skip-gate attemptFallbackChain
// checks immediately before attempting each candidate target, in the
// SAME position rateLimitOK already occupies — see
// attemptFallbackChain's own doc comment. A false return means depName
// is skipped exactly like an unhealthy or per-key-rate-limited target:
// no realAttempts/consecutiveFailures increment, no backoff sleep
// charged, and NOTHING is acquired. A true return has ALREADY acquired
// depName's concurrency slot — the caller MUST release it, exactly
// once, via releaseDeploymentConcurrency, on every path once its call to
// depName returns; callDeploymentWithCapacityCheck/
// streamDeploymentWithCapacityCheck do this via defer.
func (p *Pipeline) checkDeploymentCapacity(ctx context.Context, depName string) bool {
	if !p.checkDeploymentRateLimit(ctx, depName) {
		return false
	}
	return p.checkDeploymentConcurrency(depName)
}

// callDeploymentWithCapacityCheck wraps callDeployment with dep's own
// checkDeploymentCapacity gate and guaranteed release — used for EVERY
// call to a deployment, hop 1 included, unlike the per-key rate-limit/
// concurrency checks (checked once before routing, held for the whole
// request). A rejection returns a *DeploymentCapacityError (fallback.go),
// never a bare ErrRateLimited/ErrConcurrencyLimitExceeded, since this is
// a backend-capacity condition, not a per-caller one.
func (p *Pipeline) callDeploymentWithCapacityCheck(ctx context.Context, dep Deployment, req adapter.ChatRequest) (adapter.ChatResponse, error) {
	if !p.checkDeploymentRateLimit(ctx, dep.Name) {
		return adapter.ChatResponse{}, &DeploymentCapacityError{Deployment: dep.Name, Reason: "rate_limit"}
	}
	if !p.checkDeploymentConcurrency(dep.Name) {
		return adapter.ChatResponse{}, &DeploymentCapacityError{Deployment: dep.Name, Reason: "concurrency"}
	}
	defer p.releaseDeploymentConcurrency(dep.Name)
	return p.callDeployment(ctx, dep, req)
}

// fallbackInfo captures whether a request fell back away from its first
// deployment, and if so which one and why — captured at the one point in
// the fallback block where the original dep/err are still available,
// before they're overwritten, per
// docs/rfcs/2026-09-03-gatewayevents-decision-enrichment.md. A single
// request may now walk a multi-hop fallback chain (per
// docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md), but
// this stays a fixed 3-field record on purpose: from/reason always
// capture the FIRST (originally abandoned) deployment/error, never an
// intermediate hop — api/gatewayevents/v1's wire schema is unchanged by
// that RFC, deliberately; which hop ultimately served the request is
// already visible via the existing per-request OTel DeploymentName/
// Provider attributes at the point of success.
type fallbackInfo struct {
	happened bool
	from     string // Deployment.Name first tried and abandoned.
	reason   string // err.Error() from the first attempt.
}

// cacheProvenance records which cache layer (if any) served this request,
// and the similarity score and age of the served entry, per
// docs/rfcs/2026-09-05-gateway-cache-hit-provenance.md and
// docs/upgrade-research/cache-2026-09-06.md Finding 6. Layer == "" means
// no cache hit (a real upstream call happened, possibly coalesced via
// runMissPath). Similarity is only ever populated for Layer == "L3" — L1
// and L2 are exact/normalized byte matches, no similarity concept
// applies. AgeMs is populated for every hit layer (L1/L2 via
// cache.Cache.Get's writtenAt, L3 via LexicalCandidate.WrittenAt) —
// uniform across all three layers, closing the asymmetry this struct's
// doc comment used to name as a real, separate future change.
type cacheProvenance struct {
	Layer      string
	Similarity float64
	AgeMs      float64
}

// Hit reports whether any cache layer served this request.
func (c cacheProvenance) Hit() bool { return c.Layer != "" }

// checkCache checks L1 (exact) then L2 (normalized) in that order, per
// docs/rfcs/2026-09-03-cache-l2-normalized-match.md. An L2 hit is
// promoted into L1 (best-effort — a promotion failure never affects the
// response already found) so the next byte-identical repeat becomes an
// L1 hit. writtenAt is the served entry's OWN write time (L2's, on an L2
// hit — never the promotion write's fresh timestamp), per
// docs/upgrade-research/cache-2026-09-06.md Finding 6.
//
// tenantID feeds logCacheCrossInstanceCheck (per
// docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md) for each of L1
// and L2's own check — but only when the backend actually answered
// (getErr == nil): a backend I/O error tells this telemetry nothing
// about whether the key was really present, so it must never be
// misreported as a definite miss.
func (p *Pipeline) checkCache(ctx context.Context, tenantID, l1Key, l2Key string) (cached []byte, layer string, writtenAt time.Time, hit bool) {
	l1Cached, l1WrittenAt, l1OK, l1Err := p.cache.Get(ctx, l1Key)
	if l1Err == nil {
		p.logCacheCrossInstanceCheck(tenantID, l1Key, "L1", l1OK, p.cacheTTL)
	}
	if l1Err == nil && l1OK {
		return l1Cached, "L1", l1WrittenAt, true
	}

	l2Cached, l2WrittenAt, l2OK, l2Err := p.cacheL2.Get(ctx, l2Key)
	if l2Err == nil {
		p.logCacheCrossInstanceCheck(tenantID, l2Key, "L2", l2OK, p.cacheL2TTL)
	}
	if l2Err == nil && l2OK {
		_ = p.cache.Put(ctx, l1Key, l2Cached, p.cacheTTL)
		return l2Cached, "L2", l2WrittenAt, true
	}

	return nil, "", time.Time{}, false
}

// logCacheCrossInstanceCheck emits one line of the cross-instance
// cache-check telemetry stream, per
// docs/upgrade-research/cache-2026-09-06.md Finding 5 and
// docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md: exactly the
// (tenant, exact key, instance ID, hit-or-miss, timestamp, ttl) tuple
// telemetry/cachecorrelation.Analyze needs to retroactively estimate,
// once a real multi-instance deployment exists, how often a miss on one
// instance was actually a repeat of a request already servable from a
// sibling instance's own cache within its TTL window. slog's own record
// timestamp covers this tuple's "timestamp" field — never duplicated
// here.
//
// A structured LOG line, deliberately NOT an OTel metric: key is a
// per-request SHA256 hash (unbounded cardinality), and attaching an
// unbounded-cardinality value as a metric attribute is a well-documented
// cardinality-explosion anti-pattern — unlike the small, fixed
// vocabularies (gate name, pass/reject, instance ID) telemetry's other
// counters in this codebase use as attributes.
func (p *Pipeline) logCacheCrossInstanceCheck(tenantID, key, layer string, hit bool, ttl time.Duration) {
	p.logger.Info("cache_cross_instance_check",
		"tenant_id", tenantID,
		"cache_key", key,
		"cache_layer", layer,
		"instance_id", telemetry.InstanceID,
		"hit", hit,
		"ttl_ms", ttl.Milliseconds(),
	)
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

// isVolatileQuery reports whether any USER-role message matches the
// volatility keyword list above. Scoped to role == "user" only — never
// "system" — per docs/upgrade-research/gateway-cache-volatility-scope-
// 2026-09-09.md: a system prompt is static/repeated across every request
// through one deployment, so a system prompt that happens to contain a
// volatility keyword (e.g. "you may discuss current stock prices") must
// never drive a per-request cache-bypass decision — that would silently
// disable L3 for the entire deployment, not just requests that are
// actually about volatile content. This mirrors Portkey's Semantic Cache
// and GPTCache's own default skip_list, both of which independently
// converge on excluding system-role content from per-request cache
// eligibility decisions.
func isVolatileQuery(messages []adapter.Message) bool {
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
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
//
// l1Key (the caller's own already-computed exact-match key for this
// request) is threaded through solely for
// docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md's cross-instance
// check event, emitted only on the two paths that genuinely completed a
// real search (a full pass-through hit, or exhausting every candidate
// without one) — never on the volatile-bypass or search-error paths,
// which never learned anything about whether a valid entry existed. See
// that RFC's Design section for why L3 reuses l1Key rather than having
// its own exact-key concept: L3's own match is a fuzzy near-duplicate
// search with no single deterministic lookup key of its own, so l1Key
// here represents "was THIS exact request servable from any cache
// resource on this instance," not "l1Key is a real L3 storage key" — the
// same question checkCache's own L1/L2 events already answer for their
// own layers, extended here to L3's different mechanism.
func (p *Pipeline) checkLexicalCache(ctx context.Context, vk *identity.VirtualKey, req adapter.ChatRequest, l1Key string, signature []uint64) (cached []byte, similarity float64, ageMs float64, hit bool) {
	// Per-gate outcome counters, per docs/upgrade-research/cache-2026-09-06.md
	// Finding 1 — GroundedCache's own per-gate ablation methodology
	// applied to L3-lite's three existing gates. No new gate logic: every
	// branch below already existed, this only counts which way it went.
	volatile := isVolatileQuery(req.Messages)
	telemetry.RecordCacheL3GateOutcome(ctx, telemetry.CacheL3GateVolatileBypass, volatile)
	if volatile {
		return nil, 0, 0, false
	}
	candidates, err := p.cacheL3.Search(ctx, vk.ID, signature, l3SearchK)
	if err != nil {
		p.logger.Warn("lexical_cache_search_failed", "key_id", vk.ID, "error", err.Error())
		return nil, 0, 0, false // fail-closed: a search error skips L3, never bypasses the gate
	}
	queryFingerprint := Fingerprint(req.Messages)
	for _, c := range candidates {
		entityMismatch := !fingerprintsEqual(queryFingerprint, c.Fingerprint)
		telemetry.RecordCacheL3GateOutcome(ctx, telemetry.CacheL3GateEntityMismatch, entityMismatch)
		if entityMismatch {
			continue
		}
		freshnessRejected := !freshnessRiskModel(c.WrittenAt, c.ModelID, req.Model, c.Similarity)
		telemetry.RecordCacheL3GateOutcome(ctx, telemetry.CacheL3GateFreshnessRiskModel, freshnessRejected)
		if freshnessRejected {
			continue
		}
		// A new, separate gate from freshnessRiskModel — per
		// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md, never
		// folded into that function, which stays scoped to Cache
		// L3-lite's own checklist. A candidate written under a since-
		// changed guardrail policy/detector set is a forced miss, never
		// a silent, unchecked serve. Not one of Finding 1's three named
		// gates, so deliberately not counted alongside them.
		if c.GuardrailPolicyVersion != p.guardrails.Version() {
			continue
		}
		p.logCacheCrossInstanceCheck(vk.ID, l1Key, "L3", true, p.cacheL3TTL)
		return c.Resp, c.Similarity, float64(time.Since(c.WrittenAt).Milliseconds()), true
	}
	p.logCacheCrossInstanceCheck(vk.ID, l1Key, "L3", false, p.cacheL3TTL)
	return nil, 0, 0, false
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
		cacheInfo             cacheProvenance
		vk                    *identity.VirtualKey
		dep                   Deployment
		rateLimitFailedOpen   bool
		fallback              fallbackInfo
		budgetSpentAtDecision decimal.Decimal
		billable              bool
		// tpmReserved/tpmReservedTokens and budgetReserved/
		// budgetReservedUSD are checkRateLimit's/budget.Reserve's own
		// return values, threaded through to finalize's ReconcileTPM/
		// Reconcile calls on every return path — including error — per
		// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md.
		tpmReserved       bool
		tpmReservedTokens float64
		budgetReserved    bool
		budgetReservedUSD decimal.Decimal
	)

	start := time.Now()
	ctx, span := telemetry.Tracer.Start(ctx, "chat "+req.Model)
	defer func() {
		// attachRetryAfter runs BEFORE finalize, reassigning the named
		// return err, so finalize's own outcomeFor-based classification
		// and structured log line see the exact same wrapped error the
		// client ultimately receives — per
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

	// Per-identity concurrency cap, per
	// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md's design
	// (b) — checked immediately after the rate-limit check, before
	// budget, per that RFC's check-ordering rationale. The reserved slot
	// is released via defer the moment it's acquired, so it's freed on
	// EVERY subsequent return path (cache hit, guardrail block, upstream
	// success or failure) without needing a second release call at each
	// one.
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

	l1Key := cache.Key(vk.ID, req.Model, serializeMessages(req.Messages), req.Temperature, req.MaxTokens, p.guardrails.Version())
	l2Key := cache.NormalizedKey(vk.ID, req.Model, normalizeMessages(req.Messages), req.Temperature, req.MaxTokens, p.guardrails.Version())
	l3Signature := cache.MinHashSignature(cache.Shingles(normalizeMessages(req.Messages), l3ShingleWords), l3SignatureSize)

	if cached, layer, writtenAt, ok := p.checkCache(ctx, vk.ID, l1Key, l2Key); ok {
		var cachedResp adapter.ChatResponse
		if unmarshalErr := json.Unmarshal(cached, &cachedResp); unmarshalErr == nil {
			resp = cachedResp
			cacheInfo = cacheProvenance{Layer: layer, AgeMs: float64(time.Since(writtenAt).Milliseconds())}
			return
		}
		// A corrupt cache entry is treated as a miss, not a request
		// failure — fall through to the upstream path below.
	}

	if cached, similarity, ageMs, ok := p.checkLexicalCache(ctx, vk, req, l1Key, l3Signature); ok {
		var cachedResp adapter.ChatResponse
		if unmarshalErr := json.Unmarshal(cached, &cachedResp); unmarshalErr == nil {
			resp = cachedResp
			cacheInfo = cacheProvenance{Layer: "L3", Similarity: similarity, AgeMs: ageMs}
			return
		}
		// A corrupt cache entry is treated as a miss, not a request
		// failure — fall through to the upstream path below.
	}

	resp, dep, fallback, billable, err = p.runMissPath(ctx, vk, req, l1Key, l2Key, l3Signature)
	return
}

// cacheMissOutcome bundles everything HandleChatCompletion needs after a
// real cache-miss execution — returned as runMissPath's shared result so
// every coalesced waiter gets the same dep/fallback, not just the same
// resp.
type cacheMissOutcome struct {
	resp     adapter.ChatResponse
	dep      Deployment
	fallback fallbackInfo
}

// runMissPath executes the real, expensive cache-miss path (guardrail
// pre-call, router+fallback, upstream call, guardrail post-call, cache
// write-back) for l1Key, deduplicating concurrent identical misses via
// singleflight: N concurrent requests for the same (tenant, model, exact
// messages, temperature, max_tokens, guardrail policy version) never each
// independently hit the guardrail engine and upstream provider — only the
// first caller for a given l1Key actually runs this body; every other
// concurrent caller for the same key blocks and receives the same result,
// closing THREAT_MODEL.md's Cache Denial-of-Service row ("cache-miss
// storms causing redundant expensive upstream calls").
//
// Coalescing is keyed on l1Key (the EXACT-match cache key), not l2Key —
// deliberately narrower than it could be: two concurrent requests that
// are merely normalized-equivalent (would share an L2 hit once either one
// completes) are NOT coalesced with each other in v1, only byte-identical
// ones — a real, named scope limit, not an oversight; broadening this to
// l2Key needs writing into two different L1 keys from one shared
// execution, a genuinely separate design question. l1Key already bakes
// in the tenant ID (vk.ID), so this can never coalesce two different
// tenants' requests together — no cross-tenant call sharing, by
// construction, matching this codebase's existing tenant-isolation
// discipline elsewhere in Cache.
//
// A real, accepted tradeoff, not hidden: only the first (leader) caller's
// ctx is actually used for the shared guardrail checks and upstream call —
// if the leader's own request is canceled, every coalesced waiter's call
// fails too, even though their own individual contexts may still be live.
// This is the same tradeoff every production use of
// golang.org/x/sync/singleflight for HTTP request coalescing accepts
// (e.g. groupcache); a detached context outliving any single caller would
// need its own timeout policy this project has no need for yet.
// billable reports whether resp came from this specific call's own real,
// unshared execution of the closure below (true), or was a coalesced
// follower's copy of another caller's in-flight result (false) — per
// docs/rfcs/2026-09-05-gateway-cost-double-counting.md. Each caller's own
// stack-local billable is only ever written by that same caller's own
// closure, never shared across goroutines — singleflight.Group.Do simply
// never invokes a follower's closure at all, so a follower's billable
// stays false with no synchronization needed.
func (p *Pipeline) runMissPath(ctx context.Context, vk *identity.VirtualKey, req adapter.ChatRequest, l1Key, l2Key string, l3Signature []uint64) (resp adapter.ChatResponse, dep Deployment, fallback fallbackInfo, billable bool, err error) {
	result, doErr, _ := p.missGroup.Do(l1Key, func() (any, error) {
		billable = true

		// Guardrail pre-call: after L1/L2/L3 all miss, before the router — per
		// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md, matching
		// gateway/ARCHITECTURE.md's Request Lifecycle exactly. A cache hit
		// above never reaches this check at all — its provenance was already
		// checked under the current policy at write time, per this same RFC's
		// cache-key/GuardrailPolicyVersion mechanism.
		if verdict := p.guardrails.Check(ctx, serializeMessages(req.Messages)); verdict.Blocked {
			p.logger.Warn("guardrail_blocked_precall", "key_id", vk.ID, "finding_count", len(verdict.Findings))
			return nil, ErrGuardrailBlocked
		}

		dep, found := p.nextDeployment(req.Model)
		if !found {
			return nil, fmt.Errorf("%w: %q", ErrNoDeployment, req.Model)
		}

		resp, err := p.callDeploymentWithCapacityCheck(ctx, dep, req)
		var fallback fallbackInfo
		if err != nil {
			// Error-classified, multi-hop fallback, per
			// docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md
			// — or, when dep has no fallback_chains configured at all,
			// the pre-existing single-fallback-via-router behavior,
			// unchanged.
			originalDep, originalErr := dep, err
			if targets, configured := fallbackTargets(dep, err); configured {
				tried := map[string]bool{dep.Name: true}
				hopDep, hopResp, hopErr, attempted := p.attemptFallbackChain(ctx, targets, tried,
					func(d Deployment) (adapter.ChatResponse, error) {
						defer p.releaseDeploymentConcurrency(d.Name)
						return p.callDeployment(ctx, d, req)
					},
					func() bool { return false },
					func(model string) bool { return p.checkFallbackTargetRateLimit(ctx, vk.ID, model) },
					func(depName string) bool { return p.checkDeploymentCapacity(ctx, depName) },
				)
				if attempted {
					fallback = fallbackInfo{happened: true, from: originalDep.Name, reason: originalErr.Error()}
					dep, resp, err = hopDep, hopResp, hopErr
				}
			} else if fallbackDep, hasFallback := p.nextDeployment(req.Model); hasFallback && fallbackDep.Name != dep.Name {
				fallback = fallbackInfo{happened: true, from: dep.Name, reason: err.Error()}
				dep = fallbackDep
				resp, err = p.callDeploymentWithCapacityCheck(ctx, dep, req)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("dataplane: upstream call failed for model %q: %w", req.Model, err)
		}

		// Guardrail post-call, buffered path: resp is guaranteed fully
		// populated here and nothing downstream (cache write, return to
		// client) has happened yet — a Block verdict can still refuse both.
		if postVerdict := p.guardrails.Check(ctx, serializeResponse(resp)); postVerdict.Blocked {
			p.logger.Warn("guardrail_blocked_postcall", "key_id", vk.ID, "finding_count", len(postVerdict.Findings))
			return nil, ErrGuardrailBlocked
		}

		if encoded, marshalErr := json.Marshal(resp); marshalErr == nil {
			p.writeCache(ctx, vk.ID, l1Key, l2Key, l3Signature, Fingerprint(req.Messages), req.Model, encoded)
		}

		return cacheMissOutcome{resp: resp, dep: dep, fallback: fallback}, nil
	})
	if doErr != nil {
		return adapter.ChatResponse{}, Deployment{}, fallbackInfo{}, false, doErr
	}
	outcome := result.(cacheMissOutcome)
	return outcome.resp, outcome.dep, outcome.fallback, billable, nil
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
	// Per-deployment CacheControl-auto-populate opt-out, per
	// docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md — a
	// no-op field read only by the anthropic/bedrock adapters.
	upstreamReq.DisableCacheControlAutoPopulate = dep.effectiveCacheControlAutoDisabled()

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

// nextDeployment selects the next deployment for model via p.router
// (weighted round-robin, per docs/rfcs/2026-09-04-weighted-routing.md).
// The second return value is false if no deployment is configured for
// model at all.
func (p *Pipeline) nextDeployment(model string) (Deployment, bool) {
	name, ok := p.router.Select(model)
	if !ok {
		return Deployment{}, false
	}
	dep, ok := p.deploymentsByName[name]
	return dep, ok
}

// healthProbeCallTimeout bounds a single deployment's synthetic probe
// call, per docs/rfcs/2026-09-07-gateway-active-health-probing.md — a
// hung upstream must not stall the whole probe pass (ProbeDeployments
// runs every deployment concurrently, but a runaway one should still
// fail its own probe promptly rather than block RunHealthProbeLoop's
// next tick indefinitely). Not itself a config field — the plan's own
// configurable surface is interval/N/M, not this internal per-call
// timeout, matching the same "not every constant needs a YAML knob"
// judgment gracefulShutdownTimeout (cmd/gateway/main.go) already makes.
const healthProbeCallTimeout = 5 * time.Second

// healthProbeMaxTokens caps every synthetic probe request's completion
// length — a probe exists to prove the deployment is reachable and
// answering, not to generate a real completion a client would pay for.
const healthProbeMaxTokens = 1

// ProbeDeployments issues one lightweight, synthetic chat-completion
// request per configured deployment — concurrently, each bounded by
// healthProbeCallTimeout — and reports the outcome to p.router via
// ReportProbeResult, per
// docs/rfcs/2026-09-07-gateway-active-health-probing.md. This is the
// traffic-INDEPENDENT active-probe half of health-probing: it calls
// p.callDeployment directly, bypassing auth/cache/guardrail/budget/
// rate-limit entirely — a probe is not real client traffic, is never
// cached, never billed, and belongs to no virtual key.
//
// Exported (rather than only reachable via RunHealthProbeLoop) so tests
// can drive deterministic probe passes without depending on real
// elapsed time — see health_probe_test.go. Production wiring
// (cmd/gateway) only ever calls this indirectly, via RunHealthProbeLoop —
// which, per docs/rfcs/2026-09-08-gateway-health-probe-backoff.md, no
// longer calls this function at all: it drives the backoff-aware
// probeDueDeployments instead. This function deliberately keeps its
// original unconditional-every-deployment-every-call contract regardless
// — it stays a real, useful "force a full probe pass right now,
// ignoring any backoff" primitive (this file's own existing tests rely
// on exactly that), it is simply no longer what production's own
// scheduled loop uses tick-to-tick.
func (p *Pipeline) ProbeDeployments(ctx context.Context) {
	var wg sync.WaitGroup
	for _, dep := range p.deploymentsByName {
		wg.Add(1)
		go func(dep Deployment) {
			defer wg.Done()
			p.probeOneDeployment(ctx, dep)
		}(dep)
	}
	wg.Wait()
}

// probeOneDeployment issues and reports the outcome of a single
// deployment's synthetic probe request. Split out from ProbeDeployments
// purely so each deployment's own probeCtx/cancel pair stays scoped to
// its own goroutine.
func (p *Pipeline) probeOneDeployment(ctx context.Context, dep Deployment) {
	probeCtx, cancel := context.WithTimeout(ctx, healthProbeCallTimeout)
	defer cancel()

	maxTokens := healthProbeMaxTokens
	req := adapter.ChatRequest{
		Messages:  []adapter.Message{{Role: "user", Content: "ping"}},
		MaxTokens: &maxTokens,
	}
	_, err := p.callDeployment(probeCtx, dep, req)

	healthy, changed := p.router.ReportProbeResult(dep.Name, err == nil)
	if !changed {
		return
	}
	if healthy {
		p.logger.Info("health_probe_deployment_recovered", "deployment", dep.Name)
		return
	}
	p.logger.Warn("health_probe_deployment_unhealthy", "deployment", dep.Name, "error", err)
}

// healthProbeBackoffGrowthFactor doubles a persistently-unhealthy
// deployment's own probe interval on every consecutive unhealthy
// reschedule, per docs/rfcs/2026-09-08-gateway-health-probe-backoff.md —
// mirroring this codebase's own already-established exponential-backoff
// idiom (internal/ratelimit.EqualJitterBackoff/RetryBackoff, used there
// for the CLIENT-facing Retry-After signal on rejected requests) applied
// here to Kelvran's OWN background probe traffic instead. Deliberately
// NOT jittered, unlike RetryBackoff: jitter's whole value is
// desynchronizing many independent callers converging on one shared
// resource at once, which doesn't apply to a single per-instance
// background loop probing its own configured deployment list — a plain
// deterministic doubling is simpler and exactly provable against a fake
// clock (see health_probe_backoff_test.go), with no loss of real
// benefit.
const healthProbeBackoffGrowthFactor = 2

// healthProbeBackoffMaxMultiplier caps a backed-off probe interval at
// this many multiples of the OPERATOR'S OWN configured interval, not a
// fixed absolute duration — that interval is itself an operator-tunable
// value (300s default per
// docs/rfcs/2026-09-07-gateway-active-health-probing.md), so an absolute
// cap could land at or below an operator's own configured cadence for a
// longer-than-default interval, inverting the entire point of backing
// off. 8x means a deployment that has stayed unhealthy long enough to
// fully back off is checked roughly every 8 configured intervals instead
// of every 1 — at the documented 300s default, every 40 minutes instead
// of every 5 — materially cutting Kelvran's own probe load against an
// already-struggling dependency while still bounding how long a real
// recovery can go unnoticed to a human-reasonable window.
const healthProbeBackoffMaxMultiplier = 8

// healthProbeSchedule is one deployment's own next-eligible-probe time
// and current backoff magnitude, per
// docs/rfcs/2026-09-08-gateway-health-probe-backoff.md. backoff == 0
// means "not backed off" — this deployment is due at the plain
// configured interval, either because it has never gone unhealthy or
// because it just recovered (see rescheduleDeployment). backoff > 0 is
// this deployment's own current probe-interval override, grown by
// healthProbeBackoffGrowthFactor (capped at
// healthProbeBackoffMaxMultiplier * interval) on every consecutive
// unhealthy reschedule, and reset straight back to 0 the instant a probe
// reports healthy again — backoff must never linger past a real
// recovery.
type healthProbeSchedule struct {
	nextProbeAt time.Time
	backoff     time.Duration
}

// probeDueDeployments issues a probe (via probeOneDeployment — still
// concurrently, each independently bounded by healthProbeCallTimeout)
// for every configured deployment whose own next-eligible-probe time has
// arrived, per docs/rfcs/2026-09-08-gateway-health-probe-backoff.md's
// per-deployment backoff design. This is the backoff-AWARE entry point
// RunHealthProbeLoop actually drives — unlike ProbeDeployments (which
// always probes every deployment unconditionally; see its own doc
// comment for why that stays true), only deployments genuinely due this
// call are probed.
//
// A deployment with no schedule entry yet (never probed through this
// path before) is always due — matching RunHealthProbeLoop's
// pre-existing "first pass happens after the first interval elapses"
// contract exactly, since every deployment starts with no entry at
// process start; this function itself has no opinion about WHEN it is
// first called, only about which deployments are due AT the moment it
// IS called.
//
// Each due deployment's own next-eligible time and backoff are
// recomputed, via rescheduleDeployment, from the FRESH health verdict
// probeOneDeployment's own ReportProbeResult call just produced — never
// a stale pre-probe verdict — so a deployment that just recovered on
// THIS probe reverts to the plain interval starting with its very next
// scheduling decision, and one that just went unhealthy starts backing
// off starting with its very next probe, never the one that just ran
// (that one was scheduled under the state before this call, which was
// healthy right up until this probe's own result).
func (p *Pipeline) probeDueDeployments(ctx context.Context, interval time.Duration) {
	now := p.now()

	var due []Deployment
	p.probeMu.Lock()
	for name, dep := range p.deploymentsByName {
		if sched, ok := p.probeSchedule[name]; ok && now.Before(sched.nextProbeAt) {
			continue
		}
		due = append(due, dep)
	}
	p.probeMu.Unlock()

	var wg sync.WaitGroup
	for _, dep := range due {
		wg.Add(1)
		go func(dep Deployment) {
			defer wg.Done()
			p.probeOneDeployment(ctx, dep)
			p.rescheduleDeployment(dep.Name, interval)
		}(dep)
	}
	wg.Wait()
}

// rescheduleDeployment updates name's own next-eligible-probe time
// immediately after one of its probes just completed — see
// probeDueDeployments' own doc comment for the full contract. Currently
// healthy (per p.router.IsHealthy, read fresh — including a deployment
// that just recovered on the probe that preceded this call) always
// resets straight back to the plain configured interval with zero
// carried-over backoff: backoff must never apply to a currently-healthy
// deployment. Currently unhealthy grows the backoff
// (healthProbeBackoffGrowthFactor per step, capped at
// healthProbeBackoffMaxMultiplier*interval) and schedules against that
// instead.
func (p *Pipeline) rescheduleDeployment(name string, interval time.Duration) {
	p.probeMu.Lock()
	defer p.probeMu.Unlock()

	sched, ok := p.probeSchedule[name]
	if !ok {
		sched = &healthProbeSchedule{}
		p.probeSchedule[name] = sched
	}

	now := p.now()
	if p.router.IsHealthy(name) {
		sched.backoff = 0
		sched.nextProbeAt = now.Add(interval)
		return
	}

	if sched.backoff <= 0 {
		sched.backoff = interval
	}
	sched.backoff *= healthProbeBackoffGrowthFactor
	if maxDelay := interval * healthProbeBackoffMaxMultiplier; sched.backoff > maxDelay {
		sched.backoff = maxDelay
	}
	sched.nextProbeAt = now.Add(sched.backoff)
}

// RunHealthProbeLoop runs probeDueDeployments once per interval until
// ctx is canceled — the production wiring for
// docs/rfcs/2026-09-07-gateway-active-health-probing.md's background
// active/synthetic health-probing loop, now backoff-aware per
// docs/rfcs/2026-09-08-gateway-health-probe-backoff.md (probeDueDeployments'
// own doc comment covers the per-deployment scheduling this drives). A
// no-op if interval <= 0 (health probing not configured) — matching
// every other optional subsystem's "zero means disabled" convention
// (Redis, boltstore, OTel, admin). Deliberately does not run a probe
// pass immediately at start — the first pass happens after the first
// interval elapses, so a gateway restart storm never adds a synchronized
// burst of extra upstream calls on top of real traffic resuming.
func (p *Pipeline) RunHealthProbeLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.probeDueDeployments(ctx, interval)
		}
	}
}

// realServingModel returns dep.Model — the canonical model of the
// Deployment that GENUINELY served a response, after any routing/
// fallback_chains resolution — whenever dep is populated (dep.Model !=
// ""), falling back to fallbackModel otherwise. dep.Model != "" if and
// only if a real deployment actually, successfully produced the response
// being finalized: HandleChatCompletion/HandleChatCompletionStream leave
// dep at its zero value on every path that never resolves one at all (a
// cache hit, or any rejection before routing even runs), and runMissPath
// itself returns Deployment{} — discarding whatever the last attempted
// fallback target was — on a total upstream failure (every configured
// hop, or the pre-existing single-fallback, exhausted). fallbackModel is
// the correct value in every one of those cases: req.Model for cost (a
// cache hit's own price is the same model it was originally, genuinely
// billed under) and resp.Model for the ResponseModel telemetry field
// (which the caller additionally gates on err == nil, since resp.Model is
// always "" on an error path — see finalize's own responseModel
// computation).
func realServingModel(dep Deployment, fallbackModel string) string {
	if dep.Model != "" {
		return dep.Model
	}
	return fallbackModel
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
// "not applicable" representation for those fields. billable reports
// whether resp came from a genuine, unshared upstream call this specific
// request itself paid for — false for every cache hit (L1/L2/L3) and
// every coalesced singleflight follower — per
// docs/rfcs/2026-09-05-gateway-cost-double-counting.md: cost is still
// computed and reported via telemetry/the log line either way (real,
// informational "what this would have cost" data, e.g. for a
// cache-savings dashboard), but budget.Record is only ever called when
// billable is true, so a virtual key's tracked spend reflects genuine
// upstream cost, never a notional replay of it. billable also gates
// telemetry.RecordChatCompletionMetrics's token-usage histogram, per
// docs/rfcs/2026-09-07-gateway-genai-metrics.md, for the identical
// double-counting reason. duration is time.Since of a clock read at
// HandleChatCompletion/HandleChatCompletionStream's own entry, captured
// by the caller (trace.Span has no clean, provider-agnostic way to read
// back its own start time) — the gateway's full request boundary, fed to
// the same RecordChatCompletionMetrics call.
//
// budgetReserved/budgetReservedUSD and tpmReserved/tpmReservedTokens are
// budget.Tracker.Reserve's/KeyLimiter.ReserveTPM's own return values,
// captured by the caller at the point each check ran, per
// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md — finalize
// is where every granted reservation MUST be reconciled or released,
// unconditionally of err/billable, since this is the one hook guaranteed
// to run on every return path (including error, via defer). Both pairs
// are the harmless zero/false values whenever no real reservation was
// ever made (the corresponding Reserve/ReserveTPM call was never
// reached, or ran and was rejected) — see budget.Tracker.Reconcile's own
// doc comment for why calling it with a zero reservedUSD is always safe.
func (p *Pipeline) finalize(ctx context.Context, span trace.Span, vk *identity.VirtualKey, dep Deployment, req adapter.ChatRequest, resp adapter.ChatResponse, cacheInfo cacheProvenance, rateLimitFailedOpen bool, fallback fallbackInfo, budgetSpentAtDecision decimal.Decimal, billable bool, budgetReserved bool, budgetReservedUSD decimal.Decimal, tpmReserved bool, tpmReservedTokens float64, err error, duration time.Duration) {
	// Zero value (decimal.Decimal{}) is a valid, correct "no cost yet"
	// default on the err != nil path — verified explicitly in
	// internal/budget's own tests, not assumed here too.
	var cost decimal.Decimal
	if err == nil {
		// Priced against realServingModel(dep, req.Model), NOT req.Model
		// directly — closes
		// evals/tests/fixtures/regression_corpus_cost_abuse.json's
		// costabuse-crossmodel-fallback-billed-at-requested-not-served-model-price
		// case: dep is the Deployment that GENUINELY served the response,
		// after any routing/fallback_chains resolution, so its own
		// dep.Model is the correct price-table entry whenever a real
		// deployment resolved this response — never req.Model, the
		// client's originally-requested (possibly cheaper) model, which
		// callDeployment deliberately echoes back onto resp.Model for the
		// client-facing response body only (matching the convention of
		// OpenAI-shaped APIs) and must never double as the price-table
		// key too.
		cost = p.costCalc.Calculate(realServingModel(dep, req.Model), costaccounting.Usage{
			PromptTokens:        resp.Usage.PromptTokens,
			CompletionTokens:    resp.Usage.CompletionTokens,
			TotalTokens:         resp.Usage.TotalTokens,
			CacheReadTokens:     resp.Usage.CacheReadTokens,
			CacheCreationTokens: resp.Usage.CacheCreationTokens,
		})
	}

	// Reservation reconciliation/release, per
	// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md.
	// Deliberately NOT nested inside `if err == nil` the way the old
	// Record/RecordTokens calls were — a reservation made by an earlier
	// Reserve/ReserveTPM call must be released even when the request
	// later errors out, times out, or turns out non-billable (a cache
	// hit or coalesced singleflight follower), or that capacity leaks
	// permanently. realCost/realTokens stay nil (release-only) unless
	// err == nil && billable — the exact same gate the old billable-only
	// Record/RecordTokens calls used, so the cost-double-counting
	// invariant (docs/rfcs/2026-09-05-gateway-cost-double-counting.md)
	// carries over unchanged.
	if vk != nil {
		var realCost *decimal.Decimal
		if err == nil && billable {
			realCost = &cost
		}
		if budgetReserved || realCost != nil {
			p.budget.Reconcile(vk.ID, budgetReservedUSD, realCost, vk.BudgetResetInterval)
		}
		if realCost != nil {
			p.checkBudgetWarnThreshold(vk)
		}

		var realTokens *float64
		if err == nil && billable {
			rt := float64(resp.Usage.TotalTokens)
			realTokens = &rt
		}
		if tpmReserved || realTokens != nil {
			p.limiter.ReconcileTPM(vk.ID, req.Model, tpmReservedTokens, realTokens)
		}
	}

	var virtualKeyID string
	if vk != nil {
		virtualKeyID = vk.ID
	}
	// outcome is computed once and shared by GatewayDecisionEvent.Outcome
	// below and, via errorTypeFor, the GenAI error.type attribute — both
	// are the same classification of err, not two separately-maintained
	// taxonomies.
	outcome := outcomeFor(err)
	var errorType string
	if err != nil {
		errorType = errorTypeFor(outcome)
	}
	// responseModel reflects the model that GENUINELY served this
	// response, not resp.Model's own client-facing echo of req.Model —
	// closes the observability half of
	// costabuse-crossmodel-fallback-billed-at-requested-not-served-model-price's
	// sibling gap: before this, ResponseModel was a silent duplicate of
	// RequestModel on every request, cross-model fallback or not, since
	// callDeployment always overwrites resp.Model back to req.Model for
	// the response body. err == nil is required in addition to
	// realServingModel's own dep.Model != "" check: on any error path
	// (including one where dep is a fallback target that was actually
	// attempted but still failed), resp was never genuinely produced, so
	// ResponseModel must stay resp.Model's own "" zero value here — never
	// a deployment name that never served anything, preserving
	// ChatCompletionResult.ResponseModel's own existing documented
	// invariant ("" whenever no response was ever produced).
	responseModel := resp.Model
	if err == nil {
		responseModel = realServingModel(dep, resp.Model)
	}
	result := telemetry.ChatCompletionResult{
		VirtualKeyID:    virtualKeyID,
		Provider:        dep.Provider,
		DeploymentName:  dep.Name,
		RequestModel:    req.Model,
		ResponseModel:   responseModel,
		ResponseID:      resp.ID,
		FinishReasons:   finishReasons(resp),
		InputTokens:     resp.Usage.PromptTokens,
		OutputTokens:    resp.Usage.CompletionTokens,
		CacheHit:        cacheInfo.Hit(),
		CacheLayer:      cacheInfo.Layer,
		CacheSimilarity: cacheInfo.Similarity,
		CacheAgeMs:      cacheInfo.AgeMs,
		// telemetry stays a dependency-free leaf (no decimal.Decimal
		// import) per docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md —
		// the exact decimal string is formatted here, at the boundary,
		// per docs/rfcs/2026-09-02-decimal-cost-accounting.md.
		CostUSD:    cost.String(),
		AgentRunID: telemetry.AgentRunIDFromContext(ctx),
		Billable:   billable,
		Duration:   duration,
		ErrorType:  errorType,
		Err:        err,
	}
	telemetry.RecordChatCompletionResult(span, result)
	// Same result struct, per
	// docs/rfcs/2026-09-07-gateway-genai-metrics.md's "reuse the existing
	// per-request data capture point" design — not a second, independent
	// capture of the same fields.
	telemetry.RecordChatCompletionMetrics(ctx, result)
	// kelvran.cache.savings_usd, per
	// docs/rfcs/2026-09-10-gateway-cache-savings-metric.md: cost is
	// guaranteed populated here on every real hit (this branch only ever
	// leaves cost at its zero value when err != nil, and a cache hit path
	// never sets err) — a real, already-aggregatable exported counter,
	// not a second capture of new data.
	if cacheInfo.Hit() {
		savingsUSD, _ := cost.Float64()
		telemetry.RecordCacheSavings(ctx, cacheInfo.Layer, savingsUSD)
	}

	event := &gatewayeventsv1.GatewayDecisionEvent{
		TraceId:                span.SpanContext().TraceID().String(),
		SpanId:                 span.SpanContext().SpanID().String(),
		OccurredAt:             timestamppb.Now(),
		VirtualKeyId:           virtualKeyID,
		RequestedModel:         req.Model,
		Outcome:                outcome,
		RateLimitFailOpen:      rateLimitFailedOpen,
		FallbackHappened:       fallback.happened,
		FallbackFromDeployment: fallback.from,
		FallbackReason:         fallback.reason,
		BudgetSpentUsd:         budgetSpentAtDecision.String(),
	}
	span.End()

	p.logRequest(vk, req, resp, cacheInfo, cost, err, event)
}

// checkBudgetWarnThreshold logs a budget_warn_threshold_crossed warning
// once vk's spend (after the real charge finalize just recorded) is at
// or above vk.BudgetWarnPercent of its BudgetUSD cap — log-only, per
// docs/rfcs/2026-09-05-gateway-budget-warn-threshold.md: never rejects
// or alters the request, no new API surface. Re-logs on every billable
// completion while spend remains over threshold, rather than tracking
// "already warned this period" state — matches this codebase's existing
// ratelimit_backend_unavailable precedent of logging every occurrence
// rather than only the first.
func (p *Pipeline) checkBudgetWarnThreshold(vk *identity.VirtualKey) {
	if vk.BudgetWarnPercent <= 0 || !vk.BudgetUSD.IsPositive() {
		return
	}
	spent := p.budget.SpentUSD(vk.ID, vk.BudgetResetInterval)
	warnAt := vk.BudgetUSD.Mul(decimal.NewFromFloat(vk.BudgetWarnPercent))
	if spent.GreaterThanOrEqual(warnAt) {
		p.logger.Warn("budget_warn_threshold_crossed",
			"key_id", vk.ID,
			"spent_usd", spent.String(),
			"budget_usd", vk.BudgetUSD.String(),
			"warn_percent", vk.BudgetWarnPercent,
		)
	}
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
	case errors.Is(err, ErrRateLimited), errors.Is(err, ErrConcurrencyLimitExceeded):
		// Both classify as OUTCOME_RATE_LIMITED — see
		// ErrConcurrencyLimitExceeded's own doc comment for why this
		// deliberately avoids a new GatewayDecisionEvent_Outcome value.
		return gatewayeventsv1.GatewayDecisionEvent_OUTCOME_RATE_LIMITED
	case errors.Is(err, ErrBudgetExceeded):
		return gatewayeventsv1.GatewayDecisionEvent_OUTCOME_BUDGET_EXCEEDED
	case errors.Is(err, ErrNoDeployment):
		return gatewayeventsv1.GatewayDecisionEvent_OUTCOME_NO_DEPLOYMENT
	case errors.Is(err, ErrGuardrailBlocked):
		return gatewayeventsv1.GatewayDecisionEvent_OUTCOME_GUARDRAIL_BLOCKED
	default:
		var capErr *DeploymentCapacityError
		if errors.As(err, &capErr) {
			// A deployment-scoped rate-limit/concurrency ceiling rejected
			// this request — a backend-capacity condition, distinct from
			// OUTCOME_RATE_LIMITED (the caller's OWN per-key cap) and no
			// longer folded into the generic OUTCOME_UPSTREAM_ERROR
			// bucket it originally reused, per DeploymentCapacityError's
			// own doc comment (fallback.go) and docs/upgrade-research/
			// gateway-per-deployment-concurrency-2026-09-09.md.
			return gatewayeventsv1.GatewayDecisionEvent_OUTCOME_DEPLOYMENT_CAPACITY
		}
		return gatewayeventsv1.GatewayDecisionEvent_OUTCOME_UPSTREAM_ERROR
	}
}

// errorTypeFor derives the GenAI semantic-conventions error.type
// attribute value (per telemetry.RecordChatCompletionMetrics) from
// outcome — a low-cardinality string like "rate_limited" or
// "budget_exceeded", reusing outcomeFor's own existing classification of
// err rather than maintaining a second, parallel error taxonomy. Only
// ever called by finalize when err != nil; never called with OUTCOME_OK.
func errorTypeFor(outcome gatewayeventsv1.GatewayDecisionEvent_Outcome) string {
	return strings.ToLower(strings.TrimPrefix(outcome.String(), "OUTCOME_"))
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
func (p *Pipeline) logRequest(vk *identity.VirtualKey, req adapter.ChatRequest, resp adapter.ChatResponse, cacheInfo cacheProvenance, cost decimal.Decimal, err error, event *gatewayeventsv1.GatewayDecisionEvent) {
	fields := []any{"model", req.Model, "cache_hit", cacheInfo.Hit()}
	if cacheInfo.Hit() {
		fields = append(fields, "cache_layer", cacheInfo.Layer, "cache_age_ms", cacheInfo.AgeMs)
		if cacheInfo.Layer == "L3" {
			fields = append(fields, "cache_similarity", cacheInfo.Similarity)
		}
	}
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
// message content plus any tool-call arguments, newline-joined, not a
// full JSON re-encoding (the guardrail scans text, not structure).
//
// Tool-call arguments are included per
// docs/rfcs/2026-09-09-gateway-guardrail-toolcall-scanning.md: this
// function previously scanned only Content, leaving PII/secrets/
// injected content hidden inside a model-generated ToolCall's
// ArgumentsJSON invisible to the post-call check even though
// serializeMessages' full JSON marshal already covers tool_calls on the
// pre-call side — a real asymmetry, not an intentional scope narrowing.
func serializeResponse(resp adapter.ChatResponse) string {
	contents := make([]string, 0, len(resp.Choices))
	for _, c := range resp.Choices {
		contents = append(contents, c.Message.Content)
		for _, tc := range c.Message.ToolCalls {
			contents = append(contents, tc.ArgumentsJSON)
		}
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
	"openaicompat": func(b []byte) (any, error) {
		var r openaicompat.Response
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("unmarshaling openaicompat response: %w", err)
		}
		return &r, nil
	},
	"gemini": func(b []byte) (any, error) {
		var r gemini.Response
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("unmarshaling gemini response: %w", err)
		}
		return &r, nil
	},
	"bedrock": func(b []byte) (any, error) {
		var r bedrock.Response
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("unmarshaling bedrock response: %w", err)
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
		if err := setUpstreamAuthHeaders(ctx, httpReq, dep, body); err != nil {
			return nil, fmt.Errorf("setting auth headers for deployment %q: %w", dep.Name, err)
		}

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
			// A typed error, not a bare fmt.Errorf, so
			// classifyFallbackError (see fallback.go) can inspect the
			// real status/body via errors.As through callDeployment's
			// own "%w" wrap — per
			// docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md.
			return nil, &UpstreamHTTPError{StatusCode: httpResp.StatusCode, Body: string(respBody)}
		}

		unmarshal, ok := responseUnmarshalers[dep.Provider]
		if !ok {
			return nil, fmt.Errorf("no response unmarshaler registered for provider %q", dep.Provider)
		}
		return unmarshal(respBody)
	}
}

// streamUpstreamURL returns the URL to use for a streaming upstream call,
// deriving it from dep.BaseURL for providers whose streaming endpoint is a
// genuinely different URL (Gemini, Bedrock) rather than a body-flag
// difference (every other provider today) — per
// docs/rfcs/2026-09-04-gemini-adapter.md's "real architectural gap"
// finding: Gemini's REST API uses a distinct URL method suffix
// (:generateContent vs :streamGenerateContent?alt=sse), confirmed directly
// against Google's live API discovery document, not a body "stream" flag
// like OpenAI/Anthropic/openaicompat. Operators configure base_url as the
// buffered (:generateContent) endpoint; the streaming sibling is derived
// here, never separately configured.
//
// Bedrock's derivation is a real path-SEGMENT swap (/converse ->
// /converse-stream), confirmed directly against aws-sdk-go-v2's own
// serializers.go source — genuinely different in shape from Gemini's
// colon-suffix swap, not a copy-paste of it, per
// docs/rfcs/2026-09-04-bedrock-converse-stream.md.
func streamUpstreamURL(dep Deployment) (string, error) {
	switch dep.Provider {
	case "gemini":
		u, err := url.Parse(dep.BaseURL)
		if err != nil {
			return "", fmt.Errorf("parsing gemini base_url %q: %w", dep.BaseURL, err)
		}
		if !strings.HasSuffix(u.Path, ":generateContent") {
			return "", fmt.Errorf("gemini base_url %q must end in %q for streaming URL derivation", dep.BaseURL, ":generateContent")
		}
		u.Path = strings.TrimSuffix(u.Path, ":generateContent") + ":streamGenerateContent"
		q := u.Query()
		q.Set("alt", "sse")
		u.RawQuery = q.Encode()
		return u.String(), nil
	case "bedrock":
		if !strings.HasSuffix(dep.BaseURL, "/converse") {
			return "", fmt.Errorf("bedrock base_url %q must end in %q for streaming URL derivation", dep.BaseURL, "/converse")
		}
		return strings.TrimSuffix(dep.BaseURL, "/converse") + "/converse-stream", nil
	default:
		return dep.BaseURL, nil
	}
}

// NewHTTPUpstreamStreamCaller returns a real, working UpstreamStreamCaller
// that POSTs the marshaled provider-native (streaming) request to the
// provider's streaming URL (see streamUpstreamURL) and, on a successful
// (< 300) status, returns the raw response body for the caller to read
// incrementally as SSE frames — unlike NewHTTPUpstreamCaller, it does not
// drain or unmarshal the body itself, since that would defeat streaming's
// entire purpose.
//
// idleTimeout closes a real gap found by the routing-chaos regression
// corpus (evals/tests/fixtures/regression_corpus_routing_chaos.json's
// "chaos-streaming-no-upstream-timeout-gap" case, and the matching
// docs/agents/LOGS.md entry): this caller used to be built from a bare
// &http.Client{} with no Timeout at all, and this file had zero
// context.WithTimeout/WithDeadline/SetReadDeadline calls anywhere, so a
// stalled/delayed streaming upstream hung indefinitely — bounded only by
// the ORIGINAL inbound client disconnecting and canceling its own request
// context, never by the gateway itself.
//
// This is deliberately NOT a single client.Timeout-style deadline over
// the WHOLE call, the way NewHTTPUpstreamCaller's 60s bound is for the
// non-streaming path: SSE is a legitimately long-lived connection that
// can correctly run far longer than any single request/response
// round-trip (that's the entire point of streaming), so a fixed
// whole-call bound would kill a slow-but-healthy long stream exactly as
// readily as a genuinely stalled one — the wrong fix for this shape of
// gap. Instead, idleTimeout is an IDLE window that resets on every unit
// of real forward progress — the upstream responding with status/headers
// at all, or a later Read of the open body returning (data or EOF) — via
// a single cancelable context plus a timer idleTimeoutReader
// stops/resets on every real Read. A stream that keeps producing chunks,
// however slowly overall, never trips this; one that goes fully silent
// for idleTimeout — whether before ever responding or mid-stream — does.
//
// Canceling that context makes the blocked client.Do (pre-headers stall)
// or the blocked body Read (mid-stream stall) return a plain
// (non-*UpstreamHTTPError) error, deliberately: this is what lets a
// timed-out streaming call classify via classifyFallbackError exactly
// like the non-streaming path's own 60s timeout error already does (see
// fallback.go's FallbackClassGeneric), rather than inventing a second,
// streaming-only classification path.
func NewHTTPUpstreamStreamCaller(client *http.Client, idleTimeout time.Duration) UpstreamStreamCaller {
	return func(ctx context.Context, dep Deployment, providerReq any) (io.ReadCloser, error) {
		body, err := json.Marshal(providerReq)
		if err != nil {
			return nil, fmt.Errorf("marshaling provider stream request: %w", err)
		}

		streamURL, err := streamUpstreamURL(dep)
		if err != nil {
			return nil, fmt.Errorf("deriving stream URL for deployment %q: %w", dep.Name, err)
		}

		// streamCtx/cancel govern the WHOLE call (headers + body), but
		// idleTimer is what actually decides when cancel fires — reset on
		// every real Read below, never a fixed deadline set once here.
		streamCtx, cancel := context.WithCancel(ctx)
		idleTimer := time.AfterFunc(idleTimeout, cancel)

		httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, streamURL, bytes.NewReader(body))
		if err != nil {
			idleTimer.Stop()
			cancel()
			return nil, fmt.Errorf("building upstream stream request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if dep.Provider == "bedrock" {
			// Real, current binary event-stream content type — confirmed
			// against AWS's own docs/SDK, per
			// docs/rfcs/2026-09-04-bedrock-converse-stream.md. Every other
			// provider's streaming responses are text-based SSE.
			httpReq.Header.Set("Accept", "application/vnd.amazon.eventstream")
		} else {
			httpReq.Header.Set("Accept", "text/event-stream")
		}
		if err := setUpstreamAuthHeaders(streamCtx, httpReq, dep, body); err != nil {
			idleTimer.Stop()
			cancel()
			return nil, fmt.Errorf("setting auth headers for deployment %q: %w", dep.Name, err)
		}

		httpResp, err := client.Do(httpReq)
		if err != nil {
			idleTimer.Stop()
			cancel()
			return nil, fmt.Errorf("calling upstream %q: %w", streamURL, err)
		}
		// Headers arrived within idleTimeout — push the window out fresh
		// for the first body Read, exactly like every subsequent one.
		idleTimer.Reset(idleTimeout)

		if httpResp.StatusCode >= 300 {
			// An error response is not itself a stream — safe (and
			// necessary, to avoid leaking the connection) to drain and
			// close it here rather than handing an error body to a caller
			// expecting SSE frames. Read before stopping the timer/
			// canceling, so a slow-but-real error body isn't itself cut
			// short by the same idle window meant for the success path.
			defer func() { _ = httpResp.Body.Close() }()
			errBody, _ := io.ReadAll(httpResp.Body)
			idleTimer.Stop()
			cancel()
			// Same typed error as NewHTTPUpstreamCaller's buffered path,
			// for the same reason — see the comment there.
			return nil, &UpstreamHTTPError{StatusCode: httpResp.StatusCode, Body: string(errBody)}
		}

		return newIdleTimeoutReader(httpResp.Body, idleTimer, idleTimeout, cancel), nil
	}
}

// idleTimeoutReader wraps an already-open streaming response body,
// resetting its idle timer on every completed Read (successful or not —
// an error means no further Reads are coming anyway, so resetting then is
// harmless) so NewHTTPUpstreamStreamCaller's idle window restarts on
// every real byte of forward progress, never on mere elapsed wall-clock
// time. Close stops the timer for good and cancels the request context,
// releasing both promptly rather than waiting out whatever time remained
// on the window.
//
// timer.Reset here races, in principle, with the AfterFunc goroutine that
// may be calling cancel at the exact instant the window expires —
// accepted deliberately, not overlooked: context.CancelFunc is
// idempotent, so the only possible outcome of that race is one harmless
// extra cancel call, never a correctness problem. This is the same
// standard idle-timeout pattern used elsewhere in the ecosystem (e.g.
// net/http/httputil, gRPC keepalive) for exactly this reason.
type idleTimeoutReader struct {
	body    io.ReadCloser
	timer   *time.Timer
	timeout time.Duration
	cancel  context.CancelFunc
}

func newIdleTimeoutReader(body io.ReadCloser, timer *time.Timer, timeout time.Duration, cancel context.CancelFunc) *idleTimeoutReader {
	return &idleTimeoutReader{body: body, timer: timer, timeout: timeout, cancel: cancel}
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	r.timer.Reset(r.timeout)
	return n, err
}

func (r *idleTimeoutReader) Close() error {
	r.timer.Stop()
	r.cancel()
	return r.body.Close()
}

// bedrockSigningName is the real AWS SigV4 service-signing name for
// Bedrock Runtime, confirmed directly against aws-sdk-go-v2's own
// endpoint-resolution source (service/bedrockruntime/endpoints.go) — it
// is the unconditional fallback used because zero per-region SigningName
// overrides exist anywhere in that package's endpoint-resolution table,
// not "bedrock" as a plausible-sounding guess would suggest. Getting this
// exactly right matters: a wrong service name fails signing with a real
// AWS-side SignatureDoesNotMatch/InvalidSignatureException, not a local
// error this codebase's own tests can catch without a live AWS account.
// See docs/rfcs/2026-09-04-bedrock-adapter.md's Motivation section.
const bedrockSigningName = "amazonbedrockfrontendservice"

// setUpstreamAuthHeaders sets the provider-specific auth header(s) for an
// outgoing upstream request. Anthropic's Messages API uses an "x-api-key"
// header plus a required "anthropic-version" header rather than a Bearer
// token — this is exactly the kind of per-provider quirk
// gateway/ARCHITECTURE.md's adapter escape hatch exists for, applied here
// at the transport layer since it's about auth, not request-body shape.
//
// Bedrock is the one provider whose auth can genuinely fail: AWS SigV4
// signs over a hash of the request body itself (hence the body parameter,
// unused by every other provider), and the signer call can return an
// error — every other case below is infallible, matching this function's
// pre-existing (void) contract as closely as possible.
func setUpstreamAuthHeaders(ctx context.Context, httpReq *http.Request, dep Deployment, body []byte) error {
	switch dep.Provider {
	case "anthropic":
		httpReq.Header.Set("x-api-key", dep.APIKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	case "gemini":
		httpReq.Header.Set("x-goog-api-key", dep.APIKey)
	case "bedrock":
		payloadHash := sha256.Sum256(body)
		creds := aws.Credentials{
			AccessKeyID:     dep.AccessKeyID,
			SecretAccessKey: dep.SecretAccessKey,
			SessionToken:    dep.SessionToken,
		}
		signer := v4.NewSigner()
		if err := signer.SignHTTP(ctx, creds, httpReq, hex.EncodeToString(payloadHash[:]), bedrockSigningName, dep.Region, time.Now()); err != nil {
			return fmt.Errorf("signing bedrock request for deployment %q: %w", dep.Name, err)
		}
	default:
		httpReq.Header.Set("Authorization", "Bearer "+dep.APIKey)
	}
	return nil
}
