package dataplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// ErrCountTokensUnavailable is returned by HandleCountTokens when the
// deployment picked for the model cannot count tokens: every deployment
// today -- the anthropic branch (RFC-1 §9: pre-call guardrail on the shadow,
// then the provider's own count_tokens endpoint with the raw body) lands with
// the passthrough leg, item 11 slice S11. The handler answers 404
// not_found_error, which Claude Code reads as "estimate locally" (the
// protocol page: a character-based estimate when the endpoint is absent).
// Decided before any upstream call; the request paid one RPM token and
// nothing else.
var ErrCountTokensUnavailable = errors.New("dataplane: token counting is not available for this model's deployment")

// HandleCountTokens serves POST /v1/messages/count_tokens (item 11 slice
// S10b): the chat route's own gates in the chat route's order -- Verify, the
// source-IP allowlist, the model allowlist, one RPM token from the key's own
// limiter (never TPM, never a budget reservation: Anthropic documents the
// call as free but RPM-limited) -- then the sticky first pick for the model.
// It writes no cache entry and emits no GatewayDecisionEvent: like
// HandleListModels it logs one line per call (count_tokens with
// available=false until then; count_tokens_auth_failed,
// count_tokens_source_ip_not_allowed, count_tokens_model_not_allowed,
// count_tokens_rate_limited, count_tokens_no_deployment on the gates) with
// the trace fields and the key id, never a credential value. The returned body is the provider's count_tokens answer
// once the anthropic branch exists; today it is always nil with
// ErrCountTokensUnavailable (or an earlier gate's error).
func (p *Pipeline) HandleCountTokens(ctx context.Context, authorizationHeader, remoteAddr, model string) ([]byte, error) {
	fields := traceLogFields(ctx)
	vk, verifyErr := p.verifier.Load().Verify(authorizationHeader)
	if verifyErr != nil {
		fields = append(fields, "error", verifyErr.Error())
		if expired := expiredKeyFromErr(verifyErr); expired != nil {
			fields = append(fields, "virtual_key_id", expired.ID, "key_expired_at", expired.ExpiresAt.UTC().Format(time.RFC3339))
		}
		p.logger.Warn("count_tokens_auth_failed", fields...)
		return nil, fmt.Errorf("dataplane: auth: %w", verifyErr)
	}
	fields = append(fields, "virtual_key_id", vk.ID, "model", boundedModelForTelemetry(model))
	if !isSourceIPAllowed(vk, resolveClientIP(remoteAddr)) {
		p.logger.Warn("count_tokens_source_ip_not_allowed", fields...)
		return nil, fmt.Errorf("%w: %q", ErrSourceIPNotAllowed, remoteAddr)
	}
	if !isModelAllowed(vk, model) {
		p.logger.Warn("count_tokens_model_not_allowed", fields...)
		return nil, fmt.Errorf("%w: %q", ErrModelNotAllowed, model)
	}
	// The RPM dimension only, through the same per-key, model-scoped
	// bucket a turn draws from (checkRateLimit also reserves TPM, which a
	// count has no tokens for), with the same fail-open policy on a
	// backend error. A rejection carries the same Retry-After a turn's
	// would (attachRetryAfter), so a client throttled here backs off the
	// same way.
	allowed, err := p.limiter.AllowForModel(ctx, vk.ID, model)
	if err != nil {
		p.logger.Warn("count_tokens_ratelimit_backend_unavailable", append(fields, "error", err.Error())...)
		telemetry.RecordRateLimitFailOpen(ctx, vk.ID)
		allowed = true
	}
	if !allowed {
		p.logger.Warn("count_tokens_rate_limited", fields...)
		return nil, p.attachRetryAfter(vk, ErrRateLimited)
	}
	dep, found := p.nextDeploymentSticky(model, nil, vk.ID)
	if !found {
		p.logger.Warn("count_tokens_no_deployment", fields...)
		return nil, fmt.Errorf("%w: %q", ErrNoDeployment, model)
	}
	// Every provider, anthropic included, until slice S11 wires the
	// anthropic branch (guardrail on the shadow, then the provider's
	// count_tokens with the raw body and forwarded headers).
	p.logger.Info("count_tokens", append(fields, "deployment", dep.Name, "provider", dep.Provider, "available", false)...)
	return nil, fmt.Errorf("%w (model %q)", ErrCountTokensUnavailable, model)
}
