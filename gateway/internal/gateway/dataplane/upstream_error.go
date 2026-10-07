package dataplane

// Upstream HTTP-error enrichment: the provider's machine-readable error
// type and its Retry-After header, captured at the one place they still
// exist (the HTTP caller closures in dataplane.go hold the *http.Response;
// nothing downstream does) and surfaced as structured log fields and as a
// capped floor on the client-facing Retry-After. Per
// docs/upgrade-research/kelvran-deep-research-round3-2026-10-07.md (ranked
// item 3): AWS reports ThrottlingException and ModelNotReadyException under
// one 429, and ServiceUnavailableException/InternalServerException under
// 503/500, and documents "honor Retry-After" as the first thing a client
// should do. Kept out of fallback.go/retry_after.go for the same reason
// those files exist — one independently-reasoned-about concern each.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

const (
	// awsErrorTypeHeader is where AWS services (Bedrock included) put the
	// exception name, e.g.
	// "ThrottlingException:http://internal.amazon.com/coral/com.amazon.bedrock/".
	// http.Header.Get is case-insensitive, so the on-the-wire casing
	// ("x-amzn-ErrorType") does not matter.
	awsErrorTypeHeader = "X-Amzn-ErrorType"

	// maxUpstreamErrorTypeLen bounds ErrorType, in RUNES, cut on a rune
	// boundary — it is upstream-authored text headed for a structured log
	// field, and a hostile or broken openaicompat upstream must not be
	// able to pad every error log line with kilobytes of "error type".
	// Rune-based (not byte-based) so the cut can never split a multi-byte
	// UTF-8 sequence and leave invalid UTF-8 in the field; the byte bound
	// is therefore 4x this value in the worst case, still well under a
	// line of log.
	maxUpstreamErrorTypeLen = 128

	// maxParsedRetryAfter bounds what parseRetryAfter will ever return, so
	// an absurd or overflowing delta-seconds value ("99999999999999") can
	// neither wrap time.Duration nor claim a multi-year wait in a log
	// field. Deliberately far above maxUpstreamRetryAfter: this is the
	// honest "what the upstream said" value for logging; the client-facing
	// cap is applied separately in upstreamRetryAfter.
	maxParsedRetryAfter = 24 * time.Hour

	// maxUpstreamRetryAfter caps how far an upstream's Retry-After may push
	// out the Retry-After Kelvran hands its own clients. An upstream must
	// not be able to tell every tenant to wait an hour; 60 s is twice the
	// local backoff's own 30 s cap (ratelimit.retryBackoffCap) and well
	// past every Retry-After AWS documents for throttling.
	maxUpstreamRetryAfter = 60 * time.Second
)

// newUpstreamHTTPError builds the typed non-2xx error from a response whose
// body has already been fully read. It is the ONLY constructor the three
// HTTP caller closures use, so every UpstreamHTTPError carries whatever
// error type and Retry-After the provider sent.
func newUpstreamHTTPError(resp *http.Response, body []byte) *UpstreamHTTPError {
	return &UpstreamHTTPError{
		StatusCode: resp.StatusCode,
		Body:       string(body),
		ErrorType:  resolveAWSErrorType(resp.Header, body),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}
}

// resolveAWSErrorType mirrors smithy-go's restjson error resolution
// (transport/http/protocol/internal/json: ResolveProtocolErrorType, then
// SanitizeErrorCode) — verified against the SDK source in the module
// cache, not recalled: the X-Amzn-ErrorType header wins; else the body's
// top-level "__type"; else its top-level "code" when that is a string.
// Non-AWS providers set none of these (OpenAI nests "code" under "error",
// Anthropic's top-level key is "type"), so they resolve to "". A non-JSON
// body (an HTML 502 page) resolves to "" as well.
func resolveAWSErrorType(h http.Header, body []byte) string {
	if v := h.Get(awsErrorTypeHeader); v != "" {
		return sanitizeAWSErrorType(v)
	}
	var info struct {
		Type string `json:"__type"`
		Code any    `json:"code"`
	}
	if len(body) == 0 || json.Unmarshal(body, &info) != nil {
		return ""
	}
	if info.Type != "" {
		return sanitizeAWSErrorType(info.Type)
	}
	if code, ok := info.Code.(string); ok && code != "" {
		return sanitizeAWSErrorType(code)
	}
	return ""
}

// sanitizeAWSErrorType is smithy-go's SanitizeErrorCode (strip a ":"-URI
// suffix, then a "#"-namespace prefix), followed by this package's own two
// log-safety bounds, because the value is upstream-authored text headed
// for a structured log field:
//
//   - every control character is dropped (unicode.IsControl: C0, DEL and
//     C1), so an embedded newline, tab or ESC can neither forge a second
//     log line in a plain-text sink nor recolour an operator's terminal
//     through an ANSI-rendering log viewer — slog's JSON handler already
//     escapes them, but the plain-text handler does not. Invalid UTF-8
//     bytes are dropped too, so the field is always valid UTF-8;
//   - the result is cut to maxUpstreamErrorTypeLen runes on a rune
//     boundary (see truncateRunes), never mid-sequence.
//
// Real AWS exception names are short ASCII identifiers and pass through
// both bounds untouched.
func sanitizeAWSErrorType(code string) string {
	if idx := strings.Index(code, ":"); idx != -1 {
		code = code[:idx]
	}
	if idx := strings.Index(code, "#"); idx != -1 {
		code = code[idx+1:]
	}
	code = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, code)
	return truncateRunes(strings.TrimSpace(code), maxUpstreamErrorTypeLen)
}

// truncateRunes returns s cut to at most n runes, on a rune boundary.
func truncateRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// parseRetryAfter parses an RFC 9110 §10.2.3 Retry-After value — either a
// non-negative integer number of seconds or an HTTP-date — relative to
// now. Returns 0 for an absent, malformed, zero, negative or already-past
// value, and clamps to maxParsedRetryAfter.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(value, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		if secs > int64(maxParsedRetryAfter/time.Second) {
			return maxParsedRetryAfter
		}
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := at.Sub(now); d > 0 {
			return min(d, maxParsedRetryAfter)
		}
	}
	return 0
}

// upstreamRetryAfter returns the client-facing-capped Retry-After the
// (possibly wrapped) final upstream error carried, or 0 when it carried
// none. The 60 s cap is applied here, not in parseRetryAfter, so the
// logged upstream_retry_after_ms stays the value the provider sent (as
// parsed — clamped only at parseRetryAfter's 24 h overflow guard), and an
// operator can see a provider asking for a 10-minute wait even though
// clients are told 60 s.
func upstreamRetryAfter(err error) time.Duration {
	var httpErr *UpstreamHTTPError
	if !errors.As(err, &httpErr) || httpErr.RetryAfter <= 0 {
		return 0
	}
	return min(httpErr.RetryAfter, maxUpstreamRetryAfter)
}

// upstreamErrorLogFields returns the extra structured fields logRequest/
// logEmbeddingsRequest attach to their error log line when err carries
// typed upstream information: the status, error type and Retry-After of
// an *UpstreamHTTPError (through any number of %w wraps), and the provider
// plus typed sentinel of a mid-stream *adapter.UpstreamStreamError (the
// stream path already classifies Bedrock exception frames by name in
// internal/adapter/bedrock/stream.go — this lifts that into a field an
// operator can filter on, instead of a substring of the error text). Nil
// for any other error, so callers can append it unconditionally.
func upstreamErrorLogFields(err error) []any {
	var fields []any
	var httpErr *UpstreamHTTPError
	if errors.As(err, &httpErr) {
		fields = append(fields, "upstream_status", httpErr.StatusCode)
		if httpErr.ErrorType != "" {
			fields = append(fields, "upstream_error_type", httpErr.ErrorType)
		}
		if httpErr.RetryAfter > 0 {
			fields = append(fields, "upstream_retry_after_ms", httpErr.RetryAfter.Milliseconds())
		}
	}
	var streamErr *adapter.UpstreamStreamError
	if errors.As(err, &streamErr) {
		fields = append(fields, "upstream_provider", streamErr.Provider)
		if streamErr.Cause != nil {
			fields = append(fields, "upstream_stream_error", streamErr.Cause.Error())
		}
	}
	return fields
}
