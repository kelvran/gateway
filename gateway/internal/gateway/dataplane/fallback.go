package dataplane

// Error-classified, multi-hop fallback chains, per
// docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md. This
// file is deliberately kept separate from dataplane.go/streaming.go: both
// runMissPath and streamDeploymentWithFallback call into it, but neither
// needs to know how classification or chain-walking works internally.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kelvran/gateway/gateway/internal/adapter"
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
// p.deploymentsByName, or stop was already true before the first hop).
func (p *Pipeline) attemptFallbackChain(targets []string, tried map[string]bool, call func(Deployment) (adapter.ChatResponse, error), stop func() bool) (dep Deployment, resp adapter.ChatResponse, err error, attempted bool) {
	for _, name := range targets {
		if stop() {
			break
		}
		if tried[name] {
			continue
		}
		tried[name] = true

		nextDep, ok := p.deploymentsByName[name]
		if !ok {
			// Referential integrity is already validated at startup
			// (cmd/gateway.buildPipeline) — this is a defensive-only
			// branch, never expected to be reachable in production.
			continue
		}

		attempted = true
		dep = nextDep
		resp, err = call(dep)
		if err == nil {
			return dep, resp, nil, true
		}
	}
	return dep, resp, err, attempted
}
