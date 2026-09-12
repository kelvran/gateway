package dataplane

// Error-classified, multi-hop fallback chains, per
// docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md. This
// file is deliberately kept separate from dataplane.go/streaming.go: both
// runMissPath and streamDeploymentWithFallback call into it, but neither
// needs to know how classification or chain-walking works internally.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// FallbackClassContentPolicy, FallbackClassContextWindowExceeded, and
// FallbackClassGeneric are the three coexisting fallback-chain buckets a
// Deployment.FallbackChains map may key on, mirroring
// controlplane.DeploymentConfig's own independently-defined constants of
// the same values — the two packages are siblings (see
// gateway/ARCHITECTURE.md's dependency rules; neither may import the
// other), so this is a deliberately duplicated, plain-string convention
// rather than a shared type, matching every provider-name string
// elsewhere in this codebase (no bespoke enum type exists for those
// either). FallbackClassGeneric is also the catch-all for rate limits and
// every other non-content-policy, non-context-window condition, matching
// LiteLLM's own "generic errors (rate limits, etc.)" bucket.
const (
	FallbackClassContentPolicy         = "content_policy"
	FallbackClassContextWindowExceeded = "context_window_exceeded"
	FallbackClassGeneric               = "generic"
)

// UpstreamHTTPError is returned by NewHTTPUpstreamCaller/
// NewHTTPUpstreamStreamCaller when an upstream provider responds with a
// non-2xx status. It is the one place real, classifiable information
// about an upstream failure exists — errors from ToProvider/FromProvider
// (local marshaling/mapping problems) or a network-level failure carry no
// equivalent structured data and always classify as
// FallbackClassGeneric via classifyFallbackError below.
type UpstreamHTTPError struct {
	StatusCode int
	Body       string
}

// Error implements the error interface. callDeployment wraps this with
// "%w" (never "%v"), so errors.As still finds the concrete type through
// that wrap.
func (e *UpstreamHTTPError) Error() string {
	return fmt.Sprintf("upstream returned status %d: %s", e.StatusCode, e.Body)
}

// DeploymentCapacityError is returned when a shared deployment's own
// aggregate rate-limit or concurrency ceiling rejects a call, per
// docs/upgrade-research/gateway-per-deployment-concurrency-2026-09-09.md
// — a server/backend-capacity condition (HTTP 503 semantics), never a
// client-facing rate-limit decision: the caller may be nowhere near ITS
// OWN rate limit or concurrency cap (ErrRateLimited/
// ErrConcurrencyLimitExceeded, both client-facing 429s); the deployment
// it happened to route or fall back to is simply, aggregately, at
// capacity across every virtual key currently converging on it. Reason
// is "concurrency" or "rate_limit", for logging only.
type DeploymentCapacityError struct {
	Deployment string
	Reason     string
}

func (e *DeploymentCapacityError) Error() string {
	return fmt.Sprintf("deployment %q at capacity (%s)", e.Deployment, e.Reason)
}

// contextWindowExceededKeywords and contentPolicyKeywords are checked,
// lowercased, against an UpstreamHTTPError's Body — a best-effort,
// provider-agnostic heuristic over free text, not a structured field.
// Verified directly against live provider documentation (not assumed
// from training-data memory, per this codebase's own established
// discipline) that no provider exposes a clean, stable error-code enum
// for exactly these two conditions across the board: Anthropic's real,
// current API-errors docs confirm every 4xx validation failure collapses
// to the single generic "invalid_request_error" type, distinguished only
// by this same kind of free-text message. Context-window keywords are
// checked before content-policy ones by classifyFallbackError, since a
// "too long"/"too many tokens" message is unambiguous where a
// content-policy message might otherwise share generic words.
var contextWindowExceededKeywords = []string{
	"context_length_exceeded",
	"context window",
	"maximum context length",
	"context length exceeded",
	"too many tokens",
	"prompt is too long",
	"input is too long",
	"reduce the length of the messages",
}

var contentPolicyKeywords = []string{
	"content_policy_violation",
	"content policy violation",
	"content management policy",
	"content filter",
	"flagged by our content",
	"violates our usage policies",
}

// classifyFallbackError maps err onto one of the three FallbackClass*
// constants. Anything that isn't a (possibly wrapped) *UpstreamHTTPError
// — a local adapter error, a network failure, a context-cancellation
// error — classifies as FallbackClassGeneric, matching LiteLLM's own
// generic/rate-limit bucket.
func classifyFallbackError(err error) string {
	var capacityErr *DeploymentCapacityError
	if errors.As(err, &capacityErr) {
		// Explicit, not incidental: bucketed alongside rate limits under
		// FallbackClassGeneric — never ContentPolicy/ContextWindowExceeded,
		// which exist to route to a DIFFERENTLY-SHAPED target (bigger
		// context, different moderation) for a reason capacity exhaustion
		// shares nothing with. A real branch, not left to the
		// UpstreamHTTPError fallthrough below, so this classification
		// can't silently break if that branch is ever restructured.
		return FallbackClassGeneric
	}

	// A Round-5 backlog-audit finding: Gemini's prompt-level safety block
	// (a 200 OK response with an empty candidates array and
	// promptFeedback.blockReason set — see adapter.ErrProviderContentPolicyBlocked's
	// own doc comment) is a real, local FromProvider error, never an
	// UpstreamHTTPError — the call succeeded with a 2xx status. Checked
	// via errors.Is BEFORE the UpstreamHTTPError branch below, which can
	// never fire for this condition at all, so a Gemini deployment's own
	// configured content_policy fallback chain can finally route around
	// this exactly the way any other provider's 4xx content-policy
	// rejection already does.
	if errors.Is(err, adapter.ErrProviderContentPolicyBlocked) {
		return FallbackClassContentPolicy
	}

	var httpErr *UpstreamHTTPError
	if !errors.As(err, &httpErr) {
		return FallbackClassGeneric
	}

	body := strings.ToLower(httpErr.Body)
	if containsAnyKeyword(body, contextWindowExceededKeywords) {
		return FallbackClassContextWindowExceeded
	}
	if containsAnyKeyword(body, contentPolicyKeywords) {
		return FallbackClassContentPolicy
	}
	return FallbackClassGeneric
}

// isCandidateHealthFailure reports whether err is even ELIGIBLE to count
// as a real-request backend-health signal, per docs/upgrade-research/
// gateway-router-health-real-traffic-2026-09-09.md's Finding 2/3 and
// Recommendation item 1 — the status-class/origin filter every production
// system studied (Envoy, Linkerd, Istio) applies before any passive
// signal is allowed to influence health state at all: a genuine backend
// problem is a real 5xx response, or a local/connection-level failure
// that never got a response at all (a dial error, a timeout) — never an
// ordinary 4xx, which is client-caused (a bad request, a content-policy
// rejection, a context-window overrun) and must never be conflated with
// the deployment itself being unhealthy.
//
// Reuses UpstreamHTTPError rather than inventing a second classifier:
// classifyFallbackError already proves a content-policy or context-
// window-exceeded condition is a (necessarily 4xx-shaped) client-caused
// one, so this function doesn't need to duplicate that keyword logic —
// it only needs the numeric status class, which is coarser and doesn't
// require inspecting the response body at all.
//
// A *DeploymentCapacityError (fallback.go, same file) is explicitly
// EXCLUDED, never a candidate: it is Kelvran's own rate-limit/concurrency
// throttling rejecting a call before ever reaching the deployment at
// all — a self-inflicted, caller-side condition, not evidence the
// deployment itself is unhealthy. Treating it as a health signal would
// let Kelvran's own configured ceiling talk itself into marking a
// perfectly healthy, merely-busy deployment as failing.
//
// Deliberately NOT WIRED to any real call site or to Router.
// ReportProbeResult anywhere in this codebase — see this function's own
// package-level callers (or lack thereof) and the research doc's own
// explicit recommendation: build the filter now, as real, tested,
// inert code, but leave the decision of whether/when to connect it to
// live health state gated on a real production traffic-volume floor
// this project does not have yet (see SampleWindow, gateway/internal/
// router/samplewindow.go, that same doc's Recommendation item 2).
func isCandidateHealthFailure(err error) bool {
	if err == nil {
		return false
	}

	var capacityErr *DeploymentCapacityError
	if errors.As(err, &capacityErr) {
		return false
	}

	var httpErr *UpstreamHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode >= 500 && httpErr.StatusCode <= 599
	}

	// Anything else — a local adapter/network/dial/timeout error — never
	// got a real HTTP response from the deployment at all, the
	// "local-origin" half of Finding 3's split. Real backend-health
	// signal, same as a real 5xx.
	return true
}

func containsAnyKeyword(haystack string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(haystack, kw) {
			return true
		}
	}
	return false
}

// fallbackTargets resolves the ordered list of deployment names to
// attempt after dep's first call failed with err, per
// docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md's
// Design section. configured reports whether dep opted into explicit
// fallback_chains configuration at all:
//
//   - configured == false (dep.FallbackChains is empty/nil, the default):
//     callers MUST fall through to the pre-existing, router-based
//     single-fallback behavior — this is the backward-compatibility
//     guarantee for every config written before this feature existed.
//   - configured == true: targets is the classified error's own chain if
//     configured and non-empty, else the deployment's "generic" chain as
//     a catch-all fallthrough if THAT is configured and non-empty, else
//     nil (no fallback at all for this attempt — an explicit opt-in for
//     some classes is never a silent revert to the old behavior for an
//     unconfigured one).
func fallbackTargets(dep Deployment, err error) (targets []string, configured bool) {
	if len(dep.FallbackChains) == 0 {
		return nil, false
	}

	class := classifyFallbackError(err)
	if chain, ok := dep.FallbackChains[class]; ok && len(chain) > 0 {
		return chain, true
	}
	if class != FallbackClassGeneric {
		if chain, ok := dep.FallbackChains[FallbackClassGeneric]; ok && len(chain) > 0 {
			return chain, true
		}
	}
	return nil, true
}

// fallbackChainInterHopBackoffBase/Cap bound the pause
// attemptFallbackChain inserts before each hop after the first, per
// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md's design (c) —
// deliberately small (tens to a few hundred milliseconds, not seconds):
// this delay happens WITHIN one client request's own response-time
// budget, so it must stay small enough that a chain walk still completes
// in a reasonable time, while still being real, measurable spacing
// rather than an instant retry hammering the next struggling deployment
// immediately.
const (
	fallbackChainInterHopBackoffBase = 25 * time.Millisecond
	fallbackChainInterHopBackoffCap  = 400 * time.Millisecond
	// maxConsecutiveChainFailures is the per-request circuit breaker's
	// threshold, per the RFC's design (c): this many consecutive REAL
	// (non-skipped) fallback attempts failing in a row within ONE
	// chain-walk stops the walk immediately, even if configured targets
	// remain, rather than exhausting every remaining hop regardless of
	// how many have already failed. Deliberately reuses
	// router/health.go's own defaultUnhealthyThreshold value for
	// consistency of "how conservative is enough evidence" across this
	// codebase — but is a SEPARATE, hardcoded, per-request-scoped
	// counter: it shares no state and no config surface with
	// router.HealthConfig, and resets to zero on every new request.
	maxConsecutiveChainFailures = 3
)

// attemptFallbackChain walks targets in order, calling call for each
// name not already present in tried (which the caller seeds with at
// least the already-failed deployment's own name, defending against a
// config that names it again, or repeats a name, exactly like the
// pre-existing single-fallback code's own "fallbackDep.Name != dep.Name"
// check), stopping at the first success or the first time stop reports
// true (streaming's "a chunk already reached the client" rule — checked
// before every hop, not only the first; buffered callers pass a stop
// that always returns false). Returns the last-attempted deployment,
// response, and error, plus whether any attempt actually ran at all
// (false if every name in targets was already in tried, missing from
// p.deploymentsByName, skipped as router-unhealthy, or stop was already
// true before the first hop).
//
// Three additions on top of that pre-existing walk, per
// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md's design (c) (the
// first two) and the cross-model fallback rate-limit-bypass fix (the
// third, closing the gap
// evals/tests/fixtures/regression_corpus_cost_abuse.json's
// costabuse-permodel-ratelimit-bypassed-via-crossmodel-fallback-chain case
// documents):
//
//   - A target router.IsHealthy already reports unhealthy (real,
//     cross-request active-probe failures — see
//     docs/rfcs/2026-09-07-gateway-active-health-probing.md) is skipped
//     without ever being attempted and without charging any backoff
//     delay — composing with that already-shipped mechanism rather than
//     bypassing it, closing a real gap this RFC's own grounding research
//     found: before this, a named fallback_chains target was reached
//     directly via p.deploymentsByName, never consulting IsHealthy at
//     all. p.router is nil-checked so every pre-existing unit test in
//     this file, which constructs a bare Pipeline{deploymentsByName:...}
//     with no router configured, keeps behaving exactly as before —
//     "no router at all" resolves to the same "always healthy" default
//     router.IsHealthy itself already applies to a deployment it has
//     never been told about.
//   - A per-request circuit breaker (maxConsecutiveChainFailures) and an
//     equal-jitter backoff delay (fallbackChainInterHopBackoffBase/Cap,
//     via ratelimit.EqualJitterBackoff) inserted before the SECOND and
//     later real attempts only — never before the first, so the common
//     single-fallback case keeps its exact pre-existing latency. ctx is
//     used solely to make that sleep cancellation-aware: a canceled
//     request never blocks on this delay.
//   - rateLimitOK, called with the candidate target's OWN Deployment.Model
//     immediately before it is actually called — after the health check
//     and the consecutiveFailures circuit breaker, so a target skipped
//     for either of those reasons never wastes a real rate-limit
//     consumption either. A false return skips this target exactly like
//     an unhealthy one (continue to the next, no consecutiveFailures/
//     realAttempts increment, no backoff sleep charged) — the pre-routing
//     checkRateLimit(ctx, vk, req.Model) call
//     (dataplane.go/streaming.go, before this chain walk ever starts) is
//     keyed exclusively on the client's ORIGINALLY-REQUESTED model, so
//     without this, a virtual key's PerModel RPM cap on an expensive
//     model was entirely unenforced for traffic that reached it only via
//     a cheaper model's own fallback_chains hop. Callers pass
//     p.limiter.AllowForModel(ctx, vk.ID, model) (RPM only — TPM stays
//     scoped to the single pre-routing ReserveTPM call the caller already
//     made against req.Model; see
//     docs/upgrade-research/evals-corpus-cost-abuse-patterns-2026-09-08.md's
//     TPM follow-on finding for why re-targeting TPM's own Reserve/
//     Reconcile bookkeeping mid-chain is a materially deeper, explicitly
//     out-of-scope change, not attempted here).
//   - deploymentCapacityOK, checked immediately after rateLimitOK, in the
//     same position — per docs/upgrade-research/gateway-per-deployment-
//     concurrency-2026-09-09.md: a false return skips this target
//     exactly like an unhealthy or per-key-rate-limited one (no
//     realAttempts/consecutiveFailures increment, no backoff charged). A
//     true return has ALREADY acquired the target's own deployment-
//     scoped concurrency slot — callers' own call closure MUST release
//     it via a defer, since attemptFallbackChain itself has no way to
//     know when call's real work against that deployment finishes.
//   - capabilityOK, checked immediately after deploymentCapacityOK, in
//     the same position and with the same skip-without-charging-backoff
//     semantics (no realAttempts/consecutiveFailures increment, no
//     backoff charged) — closes the structured-output/JSON-schema
//     capability gap for FALLBACK hops specifically: a target that
//     cannot satisfy the request's own adapter.ChatRequest.ResponseFormat
//     (e.g. a Bedrock deployment whose model isn't on
//     adapter.SupportsStructuredOutput's whitelist) is skipped entirely,
//     never attempted, never sent a request it would either reject or
//     silently under-enforce. Callers pass a closure over the ORIGINAL
//     request's own ResponseFormat, never the just-failed hop's — the
//     capability requirement travels with the client's request, not with
//     whichever deployment most recently failed. Deliberately scoped to
//     fallback hops only, not the first-attempt router pick — see
//     bedrock.additionalModelRequestFieldsFor's own doc comment for why
//     that narrower first-attempt gap is a named, accepted one, not
//     closed here.
func (p *Pipeline) attemptFallbackChain(ctx context.Context, targets []string, tried map[string]bool, call func(Deployment) (adapter.ChatResponse, error), stop func() bool, rateLimitOK func(model string) bool, deploymentCapacityOK func(depName string) bool, capabilityOK func(d Deployment) bool) (dep Deployment, resp adapter.ChatResponse, err error, attempted bool) {
	consecutiveFailures := 0
	realAttempts := 0
	for _, name := range targets {
		if stop() {
			break
		}
		if tried[name] {
			continue
		}
		tried[name] = true

		if p.router != nil && !p.router.IsHealthy(name) {
			continue
		}

		nextDep, ok := p.deploymentsByName[name]
		if !ok {
			// Referential integrity is already validated at startup
			// (cmd/gateway.buildPipeline) — this is a defensive-only
			// branch, never expected to be reachable in production.
			continue
		}

		if consecutiveFailures >= maxConsecutiveChainFailures {
			break
		}

		if !rateLimitOK(nextDep.Model) {
			continue
		}

		if !deploymentCapacityOK(nextDep.Name) {
			continue
		}

		if !capabilityOK(nextDep) {
			continue
		}

		realAttempts++
		if realAttempts > 1 {
			delay := ratelimit.EqualJitterBackoff(realAttempts-1, fallbackChainInterHopBackoffBase, fallbackChainInterHopBackoffCap, rand.Float64())
			if !sleepOrCanceled(ctx, delay) {
				break
			}
		}

		attempted = true
		dep = nextDep
		hopStart := time.Now()
		resp, err = call(dep)
		if err == nil {
			return dep, resp, nil, true
		}
		// Only a failed hop gets recorded here — see
		// telemetry.RecordFallbackHop's own doc comment for why a
		// successful terminal hop needs no separate event.
		telemetry.RecordFallbackHop(ctx, dep.Name, classifyFallbackError(err), time.Since(hopStart))
		consecutiveFailures++
	}
	return dep, resp, err, attempted
}

// sleepOrCanceled blocks for d (a no-op if d <= 0), returning true if it
// elapsed normally or false if ctx was canceled first — callers treat a
// false return exactly like an already-true stop() signal, never
// attempting the hop the delay was inserted before.
func sleepOrCanceled(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
