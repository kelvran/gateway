package dataplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/ingress/anthropicmsgs"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// ErrCountTokensUnavailable is returned by HandleCountTokens when the
// model's deployment cannot count tokens: every provider but anthropic (the
// gateway translates no other provider's count call), and an anthropic
// deployment when no
// CountTokensUpstream is wired. The route answers it as 404
// not_found_error with code count_tokens_unavailable (Claude Code reads
// that as an absent endpoint and estimates locally; the Anthropic SDKs
// raise their not-found error). Item 11 slices S10b and S11c.
var ErrCountTokensUnavailable = errors.New("dataplane: token counting is not available for this model's deployment")

// HandleCountTokens serves POST /v1/messages/count_tokens: the chat route's
// gates in the chat route's order -- Verify, the source-IP and model
// allowlists, one RPM token from the key's own per-model bucket (taken
// before the body is parsed, as on every route: a body the parser rejects
// still costs the tenant one token and the operator nothing), then the
// sticky first pick -- and, on an anthropic deployment with CountTokensUpstream wired
// (RFC-1 §9, item 11 slice S11c), the deployment's own count: the body is
// parsed into the canonical shadow for the pre-call guardrail, forwarded as
// received with model rewritten to the deployment's upstream id and the
// client's anthropic-* headers, and the answer handed back as received
// with its relayable headers (CountTokensResult). No budget moves
// (Anthropic bills nothing for a count), no cache layer or idempotency
// store is touched, no GatewayDecisionEvent is emitted. Every other
// deployment answers ErrCountTokensUnavailable with the body unread. A
// body the parser rejects is a *CountTokensBodyError; an upstream failure
// is the caller's *UpstreamHTTPError (Provider set) or a plain error.
func (p *Pipeline) HandleCountTokens(ctx context.Context, authorizationHeader, remoteAddr, model string, body []byte, forward http.Header) (*CountTokensResult, error) {
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
	if dep.Provider != "anthropic" || p.countTokensUpstream == nil {
		// Every other provider: no shadow, no guardrail scan, no upstream byte,
		// as before slice S11c -- Claude Code reads the 404 and estimates.
		p.logger.Info("count_tokens", append(fields, "deployment", dep.Name, "provider", dep.Provider, "available", false)...)
		return nil, fmt.Errorf("%w (model %q)", ErrCountTokensUnavailable, model)
	}
	// The anthropic branch (RFC-1 §9, item 11 slice S11c): the body is parsed
	// into the canonical shadow for the pre-call guardrail -- the same scan a
	// turn gets, before any upstream byte -- then forwarded as received, model
	// rewritten to the deployment's upstream id, with the client's anthropic-*
	// headers, to the deployment's own count_tokens. No budget moves (Anthropic
	// bills nothing for a count), no cache layer is read or written, and the
	// deployment's answer comes back as received with its relayable headers.
	req, _, parseErr := anthropicmsgs.ParseCountTokens(body)
	if parseErr == nil {
		parseErr = validateCountTokensRequest(req)
	}
	if parseErr != nil {
		p.logger.Info("count_tokens_invalid_body", append(fields, "deployment", dep.Name, "error", parseErr.Error())...)
		return nil, &CountTokensBodyError{Err: parseErr}
	}
	verdict := p.guardrails.Check(ctx, guardrailScanMessages(req.Messages))
	if verdict.Blocked {
		p.logger.Warn("guardrail_blocked_precall", append(fields, "deployment", dep.Name, "route", "count_tokens", "finding_count", len(verdict.Findings), "finding_detectors", verdict.DetectorNames())...)
		return nil, ErrGuardrailBlocked
	}
	p.noteGuardrailFailOpen(ctx, vk.ID, telemetry.GuardrailStagePrecall, verdict)
	pr, err := anthropic.NewCountTokensPassthrough(body, dep.UpstreamModel, forward)
	if err != nil {
		return nil, &CountTokensBodyError{Err: err}
	}
	result, err := p.countTokensUpstream(ctx, dep, pr)
	if err != nil {
		p.logger.Warn("count_tokens_upstream_failed", append(fields, "deployment", dep.Name, "provider", dep.Provider, "error", clientSafeLogText(err))...)
		// Wrapped as the chat path wraps every caller error: a transport failure
		// or the answer cap carries the deployment name and the upstream URL,
		// which the envelope redacts to the same "upstream call failed" text as
		// /v1/messages; errors.As still finds an *UpstreamHTTPError through it,
		// so the verbatim 400/422 and the relay headers are unchanged.
		return nil, WrapUpstreamCallFailed(model, false, err)
	}
	p.logger.Info("count_tokens", append(fields, "deployment", dep.Name, "provider", dep.Provider, "passthrough", true, "available", true)...)
	return result, nil
}

// clientSafeLogText is the text a count_tokens upstream failure is logged
// with: the typed error's own redaction when it has one, else the message.
func clientSafeLogText(err error) string {
	var upErr *UpstreamHTTPError
	if errors.As(err, &upErr) {
		return fmt.Sprintf("upstream status %d", upErr.StatusCode)
	}
	return err.Error()
}

// CountTokensResult is an anthropic deployment's count_tokens answer as
// received: the body bytes and the relayable response headers
// (relayResponseHeaders), for the handler to write unchanged.
type CountTokensResult struct {
	Body   []byte
	Header http.Header
}

// CountTokensCaller calls an anthropic deployment's count_tokens with the
// passthrough body and returns its 2xx answer, or an *UpstreamHTTPError
// (Provider set) for a non-2xx.
type CountTokensCaller func(ctx context.Context, dep Deployment, req *anthropic.PassthroughRequest) (*CountTokensResult, error)

// CountTokensBodyError wraps a count_tokens body the Messages parser
// rejected; the handler maps Err to the parser's 400 code.
type CountTokensBodyError struct{ Err error }

func (e *CountTokensBodyError) Error() string {
	return "dataplane: count_tokens body: " + e.Err.Error()
}
func (e *CountTokensBodyError) Unwrap() error { return e.Err }

// NewHTTPCountTokensCaller posts the passthrough body to the deployment's
// count_tokens URL (its base URL -- the Messages endpoint -- plus
// /count_tokens) with the same credential and forwarded headers a turn
// gets (setUpstreamAuthHeaders), through the deployment's buffered client.
func NewHTTPCountTokensCaller(defaultClient *http.Client, perDeployment map[string]*http.Client) CountTokensCaller {
	return func(ctx context.Context, dep Deployment, req *anthropic.PassthroughRequest) (*CountTokensResult, error) {
		body := req.Body()
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, countTokensURL(dep.BaseURL), bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("building count_tokens request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if err := setUpstreamAuthHeaders(ctx, httpReq, dep, req, body); err != nil {
			return nil, fmt.Errorf("setting auth headers for deployment %q: %w", dep.Name, err)
		}
		httpResp, err := clientForDeployment(dep, defaultClient, perDeployment).Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("calling count_tokens upstream %q: %w", dep.Name, err)
		}
		defer func() { _ = httpResp.Body.Close() }()
		respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, maxCountTokensResponseBytes+1))
		if err != nil {
			return nil, fmt.Errorf("reading count_tokens response: %w", err)
		}
		if len(respBody) > maxCountTokensResponseBytes {
			return nil, fmt.Errorf("count_tokens response from %q exceeds %d bytes", dep.Name, maxCountTokensResponseBytes)
		}
		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			upErr := newUpstreamHTTPError(httpResp, respBody)
			upErr.Provider = dep.Provider
			return nil, upErr
		}
		return &CountTokensResult{Body: respBody, Header: relayResponseHeaders(httpResp.Header)}, nil
	}
}

// maxCountTokensResponseBytes bounds a count_tokens answer (Anthropic's is a
// few dozen bytes); a larger one is an upstream failure.
const maxCountTokensResponseBytes = 64 << 10

// countTokensURL appends /count_tokens to the deployment's Messages URL.
func countTokensURL(base string) string { return strings.TrimSuffix(base, "/") + "/count_tokens" }

// validateCountTokensRequest runs the six request validators the Messages
// handler runs before a turn reaches the pipeline (message count, tool
// definitions, tool_choice, field sizes, content parts, the output_config
// format schema), so a body /v1/messages refuses is refused here too rather
// than forwarded for counting.
func validateCountTokensRequest(req adapter.ChatRequest) error {
	for _, check := range []func() error{
		func() error { return adapter.ValidateMessageCount(req.Messages) },
		func() error { return adapter.ValidateToolDefs(req.Tools) },
		func() error { return adapter.ValidateToolChoice(req.ToolChoice, req.Tools) },
		func() error { return adapter.ValidateFieldSizes(req.Messages) },
		func() error { return adapter.ValidateContentParts(req.Messages) },
		func() error { return adapter.ValidateResponseFormatSchema(req.ResponseFormat) },
	} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}
