package main

import (
	"errors"
	"io"
	"net/http"

	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/ingress/anthropicmsgs"
)

// The Anthropic Messages error envelope (item 11 slice S10a, RFC-1 §9):
//
//	{"type":"error","error":{"type":"...","message":"...","code":"...","param":"..."}}
//
// Kelvran's status comes from the same errorStatus switch as the OpenAI
// envelope and its code from the same errorTypeAndCode vocabulary; only the
// type names change to Anthropic's. code and param are Kelvran additions
// Anthropic's own envelope lacks -- kelvran connect's probe reads
// error.code model_not_found, and the lossy-ingress 400 lists its pointers
// in param -- so every Anthropic-format client sees exactly what an
// OpenAI-format client sees, under the names its SDK expects.

// promptTooLongMarker is the stable marker Claude Code's recovery matches on
// when a gateway wraps an upstream over-long-input rejection in its own
// envelope (the protocol page names the Claude apps gateway substituting it
// for cloud providers' wording). Prefixed to the redacted message.
const promptTooLongMarker = "capability_rejected: prompt_too_long"

// anthropicErrorType maps a status the pipeline or the handler decided to
// Anthropic's error type vocabulary. 400, 405 and 422 are
// invalid_request_error; 401 authentication_error; 403 permission_error;
// 404 not_found_error (count_tokens on a deployment that cannot count, slice S10b);
// 413 request_too_large; 429 rate_limit_error -- a budget 429 too, Anthropic
// having no quota type, so the code (insufficient_quota) tells them apart;
// every 5xx api_error (Kelvran never emits 529, so overloaded_error is
// unused).
func anthropicErrorType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// writeAnthropicStatus writes the envelope for a status the caller decided
// (handler-level failures: 405, 413, a body Parse refused) -- the Anthropic
// twin of invalidRequest/writeAPIError. Same headers, one Write, no trailing
// newline. code and param are omitted from the body when empty.
func writeAnthropicStatus(w http.ResponseWriter, status int, code, param, message string) {
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(anthropicmsgs.EncodeError(anthropicErrorType(status), message, code, param))
}

// writeAnthropicError is writeErrorResponse's twin for /v1/messages: the
// same status (errorStatus), the same Retry-After rule (setRetryAfterHeader),
// the same code and param, the same redacted message (plus the
// prompt_too_long marker, anthropicClientMessage) -- under Anthropic's type
// names. The one exception is RFC-1 §9's verbatim path below: an anthropic
// deployment's own 400 or 422 whose body is Anthropic's error object is
// written as received, with its status, and its x-should-retry and
// anthropic-ratelimit-unified-* headers travel on every anthropic error.
// Callers must not have written headers already; the streaming
// handler uses the encoder's own error event once the stream has started.
func writeAnthropicError(w http.ResponseWriter, err error) {
	// count_tokens on a deployment that cannot count (every deployment but
	// anthropic, slice S11c) is Anthropic's 404 not_found_error -- the status Claude Code
	// reads as "estimate locally". HandleCountTokens has one caller, this
	// route, so the sentinel never reaches errorStatus's table and the OpenAI
	// envelope stays untouched (item 11 slice S10b).
	if errors.Is(err, dataplane.ErrCountTokensUnavailable) {
		writeAnthropicStatus(w, http.StatusNotFound, "count_tokens_unavailable", "", anthropicClientMessage(err))
		return
	}
	// An anthropic deployment's own answer (RFC-1 §9, slice S11b): its
	// x-should-retry and anthropic-ratelimit-unified-* headers are forwarded
	// on every error, and a 400 or 422 whose body IS Anthropic's error
	// object -- the classes Claude Code's recovery matches on (a rejected
	// thinking field, a mid-conversation system message, a preserved-
	// thinking mismatch) -- is written verbatim with its own status instead
	// of the redacted 502. The exception is exactly {anthropic, 400|422,
	// parsed Anthropic error object} (THREAT_MODEL's Tampering row); every
	// other upstream body, status or provider stays redacted below, and
	// clientSafeMessage/ClientSafeMessage are untouched.
	var upstreamErr *dataplane.UpstreamHTTPError
	if errors.As(err, &upstreamErr) && upstreamErr.Provider == "anthropic" {
		copyRelayHeaders(w.Header(), upstreamErr.RelayHeaders)
		if upstreamErr.StatusCode == http.StatusBadRequest || upstreamErr.StatusCode == http.StatusUnprocessableEntity {
			if _, _, ok := anthropicmsgs.ParseErrorEnvelope([]byte(upstreamErr.Body)); ok {
				h := w.Header()
				h.Del("Content-Length")
				h.Set("Content-Type", "application/json; charset=utf-8")
				h.Set("X-Content-Type-Options", "nosniff")
				w.WriteHeader(upstreamErr.StatusCode)
				_, _ = io.WriteString(w, upstreamErr.Body)
				return
			}
		}
	}
	status := errorStatus(err)
	setRetryAfterHeader(w, err)
	_, code := errorTypeAndCode(err, status)
	writeAnthropicStatus(w, status, derefString(code), derefString(errorParam(err)), anthropicClientMessage(err))
}

// anthropicClientMessage is clientSafeMessage with one addition for this
// envelope: an upstream context-window rejection (dataplane.
// IsContextWindowExceeded -- Bedrock's over-long ValidationException,
// OpenAI's context_length_exceeded, ...) is prefixed with
// promptTooLongMarker so Claude Code can recognise the condition through
// Kelvran's envelope; the redacted text follows, the upstream body never
// does.
func anthropicClientMessage(err error) string {
	message := clientSafeMessage(err)
	if dataplane.IsContextWindowExceeded(err) {
		return promptTooLongMarker + ": " + message
	}
	return message
}

// copyRelayHeaders adds every header of src (already filtered to
// x-should-retry and anthropic-ratelimit-unified-* by the dataplane) onto
// the client response, so Claude Code's retry decision and usage display
// see the upstream's own values (protocol page).
// copyRelayHeaders puts src's headers on dst, replacing any value dst already
// holds under the same name rather than accumulating beside it, so the
// client sees exactly the upstream's values.
func copyRelayHeaders(dst, src http.Header) {
	for name, values := range src {
		dst.Del(name)
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
