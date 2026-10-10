package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/idempotency"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

// apiError is the OpenAI-shaped error envelope every data-plane error
// response carries:
//
//	{"error":{"message":"...","type":"...","param":null,"code":"..."}}
//
// The official OpenAI SDKs choose the exception class from the HTTP status
// and read these fields for the message and the machine-readable code;
// LangChain and LiteLLM additionally branch on type == "insufficient_quota"
// to stop retrying a permanent condition. All four keys are always present
// (null when unknown): the official SDKs tolerate absence, and presence is
// strictly more compatible with any stricter parser. Status codes are
// decided elsewhere (errorStatus) and are NOT changed by this envelope; it
// only adds type and code to what used to be a text/plain body.
type apiError struct {
	Error apiErrorDetail `json:"error"`
}

type apiErrorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// OpenAI error type vocabulary used by the envelope.
const (
	errTypeInvalidRequest = "invalid_request_error"
	errTypeAuthentication = "authentication_error"
	errTypePermission     = "permission_error"
	errTypeRateLimit      = "rate_limit_error"
	errTypeQuota          = "insufficient_quota"
	errTypeServer         = "server_error"
)

// fallbackEnvelope is the hand-written body used if marshalling an apiError
// ever fails. encoding/json cannot fail on a struct of strings and pointers
// to strings (invalid UTF-8 is replaced, not rejected), so this is a
// belt-and-braces guarantee that a client always receives a parseable
// envelope rather than an empty body or an empty SSE frame.
const fallbackEnvelope = `{"error":{"message":"internal error while encoding the error response","type":"server_error","param":null,"code":null}}`

// marshalEnvelope renders an apiError, falling back to fallbackEnvelope so
// callers never have to handle a marshal error on an error path.
func marshalEnvelope(e apiError) []byte {
	body, err := json.Marshal(e)
	if err != nil {
		return []byte(fallbackEnvelope)
	}
	return body
}

// writeAPIError writes the JSON envelope with the given status. It replaces
// http.Error for every client-facing data-plane error: same status, same
// message text, but application/json instead of text/plain. nosniff is kept
// because http.Error set it and a client could notice its absence. The body
// is written with one Write and no trailing newline (OpenAI's own byte
// shape). That Write can fail when the client has gone away mid-response,
// but the status and headers are already on the wire by then, so there is
// nothing better to do than let the partial body stand.
//
// Callers must not have written headers already: like http.Error, this
// cannot change a status net/http has already committed. The streaming
// handler uses writeTracker + writeStreamErrorFrame for that case.
func writeAPIError(w http.ResponseWriter, status int, errType string, code, param *string, message string) {
	body := marshalEnvelope(apiError{Error: apiErrorDetail{Message: message, Type: errType, Param: param, Code: code}})
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeStreamErrorFrame reports a failure that happened AFTER the first SSE
// chunk reached the client. The status is already committed (200), so the
// only honest channel left is the stream itself: one in-band
//
//	data: {"error":{...}}
//
// frame — the same convention OpenAI and Anthropic use for mid-stream
// errors, which the OpenAI SDKs' stream parsers surface as an APIError
// instead of a parse failure — and then the stream ends WITHOUT a [DONE]
// sentinel, so a client never mistakes the truncated completion for a
// complete one. The envelope carries the same type, code and redacted
// message a buffered error would; param is always null in a stream frame
// (this function never names a request field -- the one pipeline error
// whose buffered envelope sets param, ErrEmptyMessages, fails before the
// first chunk and so goes through writeErrorResponse instead). It is only
// reachable before [DONE]: every dataplane path that fails after the first
// chunk returns before finishStreamedResponse writes the sentinel.
func writeStreamErrorFrame(w http.ResponseWriter, err error) {
	status := errorStatus(err)
	errType, code := errorTypeAndCode(err, status)
	body := marshalEnvelope(apiError{Error: apiErrorDetail{Message: clientSafeMessage(err), Type: errType, Code: code}})
	_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// writeTracker wraps an http.ResponseWriter and records whether any header
// or body byte has been committed, so an error path can tell a pre-stream
// failure (status still changeable, write a normal envelope) from a
// mid-stream one (status committed, write an in-band frame). It forwards
// the optional interfaces the stdlib writer may implement -- Flush (which
// streaming.NewWriter requires), Hijack, and Unwrap for
// http.ResponseController -- so wrapping is transparent to the dataplane.
// It is used by exactly one goroutine: the handler creates it, hands it to
// the pipeline, and reads `written` only after the pipeline has returned.
type writeTracker struct {
	http.ResponseWriter
	written bool
}

func (t *writeTracker) WriteHeader(status int) {
	t.written = true
	t.ResponseWriter.WriteHeader(status)
}

func (t *writeTracker) Write(p []byte) (int, error) {
	t.written = true
	return t.ResponseWriter.Write(p)
}

// Flush satisfies http.Flusher. Flushing commits the headers too. The
// underlying writer is always the stdlib *http.response here, which
// flushes; if a non-flushing writer were ever wrapped, streaming.NewWriter
// would have rejected it before the first chunk, so the no-op branch is
// unreachable in production and exists only so the method is total.
func (t *writeTracker) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		t.written = true
		f.Flush()
	}
}

// Hijack forwards connection hijacking (HTTP/1.x) so a future handler that
// upgrades the connection still can; hijacking commits the response. An
// underlying writer that cannot hijack (HTTP/2) yields an error, exactly as
// the stdlib does.
func (t *writeTracker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := t.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("writeTracker: underlying ResponseWriter does not implement http.Hijacker")
	}
	t.written = true
	return h.Hijack()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (t *writeTracker) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// codePtr is a small helper so call sites can pass literal codes.
func codePtr(code string) *string { return &code }

// invalidRequest writes a handler-level validation failure as an envelope
// of type invalid_request_error. The TYPE is fixed; the status is the
// caller's (400 for malformed bodies and missing fields, 405 for a wrong
// method, 413 for an oversized body), because OpenAI uses
// invalid_request_error for every client-side request problem regardless
// of status. param names the offending request field when there is one.
func invalidRequest(w http.ResponseWriter, status int, code string, param *string, message string) {
	writeAPIError(w, status, errTypeInvalidRequest, codePtr(code), param, message)
}

// methodNotAllowed is the 405 variant: RFC 9110 §15.5.6 requires an Allow
// header listing the permitted methods, which http.Error never set. allowed
// is the header value verbatim, i.e. a comma-separated method list such as
// "GET, POST" when a route accepts more than one.
func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	invalidRequest(w, http.StatusMethodNotAllowed, "method_not_allowed", nil, "method not allowed")
}

// errorTypeAndCode maps a pipeline error to the envelope's type and code.
// The STATUS is decided by errorStatus and passed in; this function only
// enriches it, so a sentinel missing here still gets the right status and a
// type derived from that status. Sentinels are matched with
// errors.Is/errors.As so %w-wrapped errors classify the same way as bare
// ones (mirroring the status switch). The cases are grouped by type for
// readability; they are not the status switch and need not share its order.
func errorTypeAndCode(err error, status int) (errType string, code *string) {
	var capErr *dataplane.DeploymentCapacityError
	var lossyErr *dataplane.ErrLossyIngressRejected
	switch {
	case errors.Is(err, identity.ErrMissingHeader):
		return errTypeAuthentication, nil
	case errors.Is(err, identity.ErrInvalidKey):
		return errTypeAuthentication, codePtr("invalid_api_key")
	case errors.Is(err, identity.ErrKeyExpired):
		// The key's own expires_at has passed (RFC-3 decision 4): a distinct
		// code so the holder knows to ask for a new key rather than retype
		// this one. The message never names the key; the log line does.
		return errTypeAuthentication, codePtr("key_expired")
	case errors.Is(err, dataplane.ErrBudgetExceeded):
		// OpenAI's own vocabulary for "you are out of money", which clients
		// treat as permanent, unlike rate_limit_error which they retry.
		return errTypeQuota, codePtr("insufficient_quota")
	case errors.Is(err, dataplane.ErrRateLimited):
		return errTypeRateLimit, codePtr("rate_limit_exceeded")
	case errors.Is(err, dataplane.ErrConcurrencyLimitExceeded):
		return errTypeRateLimit, codePtr("concurrency_limit_exceeded")
	case errors.As(err, &capErr):
		return errTypeServer, codePtr("deployment_capacity_exceeded")
	case errors.Is(err, dataplane.ErrModelNotAllowed):
		return errTypePermission, codePtr("model_not_allowed")
	case errors.Is(err, dataplane.ErrSourceIPNotAllowed):
		return errTypePermission, codePtr("source_ip_not_allowed")
	case errors.Is(err, dataplane.ErrNoDeployment):
		// 400 (not OpenAI's 404) is the recorded 2026-09-28 decision: an
		// unknown model is a request-shape mistake here, not a missing
		// resource. The code still says what happened.
		return errTypeInvalidRequest, codePtr("model_not_found")
	case errors.Is(err, dataplane.ErrGuardrailBlocked):
		return errTypeInvalidRequest, codePtr("content_policy_violation")
	case errors.Is(err, dataplane.ErrStreamingNotSupported):
		return errTypeInvalidRequest, codePtr("streaming_not_supported")
	case errors.Is(err, dataplane.ErrNotAnEmbeddingDeployment):
		return errTypeInvalidRequest, codePtr("not_an_embedding_model")
	case errors.Is(err, dataplane.ErrEmptyMessages):
		return errTypeInvalidRequest, codePtr("empty_messages")
	case errors.Is(err, adapter.ErrStructuredOutputUnsupported):
		// G16 (2026-10-10): a request-shape fault, so a 400 with its own code
		// instead of the 502 upstream_error default it used to fall to.
		return errTypeInvalidRequest, codePtr("response_format_unsupported")
	case errors.Is(err, idempotency.ErrFingerprintMismatch):
		// G16: 422 -- the key says "same request", the body says otherwise.
		return errTypeInvalidRequest, codePtr("idempotency_key_reused")
	case errors.Is(err, adapter.ErrToolResultPartsUnsupported):
		// Item 11 slice S6: a tool message carries parts no deployment in the
		// pool can carry -- known from the request alone, so a 400.
		return errTypeInvalidRequest, codePtr("tool_result_parts_unsupported")
	case errors.As(err, &lossyErr):
		// Item 11 slice S9b: members no deployment in the pool can carry --
		// known from the request alone, so a 400; param lists the pointers.
		return errTypeInvalidRequest, codePtr("lossy_ingress_rejected")
	case errors.Is(err, dataplane.ErrPromptAndMessagesBothSet),
		errors.Is(err, dataplane.ErrPromptResolutionFailed),
		errors.Is(err, dataplane.ErrPromptLabelAndVersionBothSet),
		errors.Is(err, dataplane.ErrResolvedPromptContentInvalid):
		return errTypeInvalidRequest, codePtr("invalid_prompt_reference")
	case errors.Is(err, dataplane.ErrStreamingNotConfigured):
		return errTypeServer, codePtr("streaming_not_configured")
	case errors.Is(err, dataplane.ErrEmbeddingsNotConfigured):
		return errTypeServer, codePtr("embeddings_not_configured")
	}
	// Unknown sentinel: derive the type from the status the switch chose so
	// the envelope is never internally inconsistent, and label the default
	// 502 path honestly as an upstream error.
	switch {
	case status == http.StatusUnauthorized:
		return errTypeAuthentication, nil
	case status == http.StatusForbidden:
		return errTypePermission, nil
	case status == http.StatusTooManyRequests:
		return errTypeRateLimit, nil
	case status >= 400 && status < 500:
		return errTypeInvalidRequest, nil
	case status == http.StatusBadGateway:
		return errTypeServer, codePtr("upstream_error")
	default:
		return errTypeServer, nil
	}
}

// errorParam is the envelope's param member: the request field (or header)
// an error is about, nil when the error is not about one field. The
// vocabulary is OpenAI's where a field exists (messages, response_format);
// a header name for the idempotency contradiction; and, for a lossy-ingress
// rejection (item 11 slice S9b), every unknown member's RFC 6901 pointer,
// comma-joined and bounded like the dropped_fields summary
// (ErrLossyIngressRejected.Param), so a client sees which members the
// message only counted.
func errorParam(err error) *string {
	var lossyErr *dataplane.ErrLossyIngressRejected
	switch {
	case errors.Is(err, dataplane.ErrEmptyMessages):
		return codePtr("messages")
	case errors.Is(err, adapter.ErrStructuredOutputUnsupported):
		return codePtr("response_format")
	case errors.Is(err, idempotency.ErrFingerprintMismatch):
		return codePtr("Idempotency-Key")
	case errors.Is(err, adapter.ErrToolResultPartsUnsupported):
		return codePtr("messages")
	case errors.As(err, &lossyErr):
		return codePtr(lossyErr.Param())
	}
	return nil
}
