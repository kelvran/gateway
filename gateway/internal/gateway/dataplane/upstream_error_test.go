package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"absent", "", 0},
		{"delta seconds", "5", 5 * time.Second},
		{"delta seconds with whitespace", " 7 ", 7 * time.Second},
		{"zero is no wait", "0", 0},
		{"negative is malformed", "-3", 0},
		{"garbage is malformed", "soon", 0},
		{"fractional seconds are not RFC 9110 delta-seconds", "1.5", 0},
		{"http-date in the future", now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second},
		{"http-date in the past", now.Add(-10 * time.Second).Format(http.TimeFormat), 0},
		{"absurd delta is clamped, not overflowed", "99999999999999", maxParsedRetryAfter},
		{"far-future http-date is clamped", now.Add(400 * 24 * time.Hour).Format(http.TimeFormat), maxParsedRetryAfter},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseRetryAfter(tt.value, now); got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestSanitizeAWSErrorType(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"ThrottlingException:http://internal.amazon.com/coral/com.amazon.bedrock/", "ThrottlingException"},
		{"com.amazon.bedrock#ValidationException", "ValidationException"},
		{"com.amazon.bedrock#ModelNotReadyException:http://x/", "ModelNotReadyException"},
		{"ServiceUnavailableException", "ServiceUnavailableException"},
		{"", ""},
		{strings.Repeat("A", 500), strings.Repeat("A", maxUpstreamErrorTypeLen)},
		// Log-safety bounds on upstream-authored text: control characters
		// (newline, tab, CR, ESC) are dropped so the value can neither
		// forge a log line nor carry an ANSI escape; the bracketed digits
		// that follow a stripped ESC are harmless printable text.
		{"Throttling\nFAKE_ERROR\x1b[31mREDACTED\x1b[0m\t\rException", "ThrottlingFAKE_ERROR[31mREDACTED[0mException"},
		// The length cut is in runes on a rune boundary: 200 four-byte
		// emoji become exactly 128 valid runes, never a split sequence.
		{strings.Repeat("🔥", 200), strings.Repeat("🔥", maxUpstreamErrorTypeLen)},
		// 43 three-byte runes (129 bytes) are under the rune bound and must
		// pass through untouched — the old byte-based cut mangled these.
		{strings.Repeat("中", 43), strings.Repeat("中", 43)},
		// Invalid UTF-8 bytes are dropped rather than passed through.
		{"Throttling\xffException", "ThrottlingException"},
	}
	for _, tt := range tests {
		got := sanitizeAWSErrorType(tt.in)
		if got != tt.want {
			t.Errorf("sanitizeAWSErrorType(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("sanitizeAWSErrorType(%q) produced invalid UTF-8: %q", tt.in, got)
		}
	}
}

func TestResolveAWSErrorType(t *testing.T) {
	withHeader := http.Header{}
	withHeader.Set("x-amzn-ErrorType", "ThrottlingException:http://internal.amazon.com/coral/com.amazon.bedrock/")
	tests := []struct {
		name string
		h    http.Header
		body string
		want string
	}{
		{"header wins over body", withHeader, `{"__type":"ValidationException","message":"x"}`, "ThrottlingException"},
		{"header alone (Bedrock's usual shape: body has only message)", withHeader, `{"message":"Too many requests, please wait before trying again."}`, "ThrottlingException"},
		{"body __type", http.Header{}, `{"__type":"com.amazon.bedrock#ValidationException","message":"x"}`, "ValidationException"},
		{"body code string", http.Header{}, `{"code":"ModelNotReadyException","message":"x"}`, "ModelNotReadyException"},
		{"body code number is not a type", http.Header{}, `{"code":429,"message":"x"}`, ""},
		{"openai nests code under error, so no false pick-up", http.Header{}, `{"error":{"code":"rate_limit_exceeded","type":"requests","message":"x"}}`, ""},
		{"anthropic uses type, not __type", http.Header{}, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, ""},
		{"html error page", http.Header{}, `<html><body>502 Bad Gateway</body></html>`, ""},
		{"empty body", http.Header{}, ``, ""},
		{"json array body", http.Header{}, `[1,2,3]`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveAWSErrorType(tt.h, []byte(tt.body)); got != tt.want {
				t.Errorf("resolveAWSErrorType = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNewUpstreamHTTPErrorCapturesHeadersAndKeepsStringsUnchanged pins the
// contract the three HTTP caller closures rely on: headers are captured,
// and neither Error() (server-side log text) nor ClientSafeMessage() (the
// only client-facing text) changes shape — the error type and Retry-After
// are additive fields, not new disclosure.
func TestNewUpstreamHTTPErrorCapturesHeadersAndKeepsStringsUnchanged(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	resp.Header.Set("Retry-After", "7")
	resp.Header.Set("X-Amzn-ErrorType", "ThrottlingException:http://internal.amazon.com/coral/com.amazon.bedrock/")
	body := []byte(`{"message":"Too many requests, please wait before trying again."}`)

	got := newUpstreamHTTPError(resp, body)

	if got.StatusCode != 429 || got.Body != string(body) {
		t.Fatalf("StatusCode/Body = %d/%q, want 429/%q", got.StatusCode, got.Body, body)
	}
	if got.ErrorType != "ThrottlingException" {
		t.Errorf("ErrorType = %q, want ThrottlingException", got.ErrorType)
	}
	if got.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v, want 7s", got.RetryAfter)
	}
	if want := "upstream returned status 429: " + string(body); got.Error() != want {
		t.Errorf("Error() = %q, want %q (unchanged shape)", got.Error(), want)
	}
	if want := "upstream provider returned status 429"; got.ClientSafeMessage() != want {
		t.Errorf("ClientSafeMessage() = %q, want %q (must not disclose the error type)", got.ClientSafeMessage(), want)
	}

	plain := newUpstreamHTTPError(&http.Response{StatusCode: 502, Header: http.Header{}}, []byte("<html>bad gateway</html>"))
	if plain.ErrorType != "" || plain.RetryAfter != 0 {
		t.Errorf("header-less response: ErrorType=%q RetryAfter=%v, want empty/0", plain.ErrorType, plain.RetryAfter)
	}
}

func TestUpstreamErrorLogFields(t *testing.T) {
	httpErr := &UpstreamHTTPError{StatusCode: 429, Body: "x", ErrorType: "ThrottlingException", RetryAfter: 5 * time.Second}
	wrapped := fmt.Errorf("dataplane: upstream call failed for model %q: %w", "m", fmt.Errorf("upstream call to deployment %q: %w", "d1", httpErr))
	streamErr := &adapter.UpstreamStreamError{Provider: "bedrock", Cause: errors.New("bedrock: request throttled"), Raw: "{...}"}

	tests := []struct {
		name string
		err  error
		want []any
	}{
		{"nil error", nil, nil},
		{"plain error", errors.New("dial tcp: connection refused"), nil},
		{"wrapped http error carries all three", wrapped, []any{"upstream_status", 429, "upstream_error_type", "ThrottlingException", "upstream_retry_after_ms", int64(5000)}},
		{"http error without type or retry-after carries status only", &UpstreamHTTPError{StatusCode: 500, Body: "x"}, []any{"upstream_status", 500}},
		{"wrapped stream error carries provider and sentinel", fmt.Errorf("decoding stream from deployment %q: %w", "d1", streamErr), []any{"upstream_provider", "bedrock", "upstream_stream_error", "bedrock: request throttled"}},
		{"stream error without cause carries provider only", &adapter.UpstreamStreamError{Provider: "openai", Raw: "x"}, []any{"upstream_provider", "openai"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := upstreamErrorLogFields(tt.err)
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("upstreamErrorLogFields(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestAttachRetryAfterHonoursUpstreamRetryAfterAsACappedFloor: when the
// final upstream error says how long to wait, the client-facing
// Retry-After is at least that (a client told 500 ms by the local backoff
// when Bedrock said 5 s just gets throttled again), but never more than
// maxUpstreamRetryAfter — an upstream cannot make Kelvran tell every
// tenant to wait an hour. Without an upstream value the local backoff is
// unchanged (first consecutive rejection: 500 ms base with equal jitter).
func TestAttachRetryAfterHonoursUpstreamRetryAfterAsACappedFloor(t *testing.T) {
	vk := &identity.VirtualKey{ID: "test-key"}
	wrap := func(ra time.Duration) error {
		return fmt.Errorf("upstream call to deployment %q: %w", "d1", &UpstreamHTTPError{StatusCode: 429, Body: "x", RetryAfter: ra})
	}
	retryAfterOf := func(t *testing.T, err error) time.Duration {
		t.Helper()
		var retryErr *RetryAfterError
		if !errors.As(err, &retryErr) {
			t.Fatalf("attachRetryAfter(%v) = %v, want a *RetryAfterError", err, err)
		}
		return retryErr.RetryAfter
	}

	t.Run("upstream 5s raises a sub-second local backoff to 5s", func(t *testing.T) {
		p := &Pipeline{retryBackoff: ratelimit.NewRetryBackoff()}
		if got := retryAfterOf(t, p.attachRetryAfter(vk, wrap(5*time.Second))); got != 5*time.Second {
			t.Errorf("RetryAfter = %v, want exactly 5s (upstream floor above a first-attempt local backoff)", got)
		}
	})
	t.Run("upstream 3600s is capped", func(t *testing.T) {
		p := &Pipeline{retryBackoff: ratelimit.NewRetryBackoff()}
		if got := retryAfterOf(t, p.attachRetryAfter(vk, wrap(time.Hour))); got != maxUpstreamRetryAfter {
			t.Errorf("RetryAfter = %v, want %v (cap)", got, maxUpstreamRetryAfter)
		}
	})
	// The local-backoff bounds below are the exact deterministic ranges of
	// ratelimit.EqualJitterBackoff (base 500 ms, cap 30 s, equal jitter =
	// half fixed + half random): a first consecutive rejection is always in
	// [250 ms, 500 ms); a ninth is 500 ms * 2^8 = 128 s, capped to 30 s,
	// jittered into [15 s, 30 s). Tight on purpose — a loose "> 0" would
	// let a broken backoff pass.
	t.Run("no upstream value leaves the local backoff alone", func(t *testing.T) {
		p := &Pipeline{retryBackoff: ratelimit.NewRetryBackoff()}
		got := retryAfterOf(t, p.attachRetryAfter(vk, wrap(0)))
		if got < 250*time.Millisecond || got >= 500*time.Millisecond {
			t.Errorf("RetryAfter = %v, want the first-attempt local backoff in [250ms, 500ms)", got)
		}
	})
	t.Run("local backoff above the upstream value is kept", func(t *testing.T) {
		p := &Pipeline{retryBackoff: ratelimit.NewRetryBackoff()}
		// Drive the per-key streak to 8 so the ninth rejection's local
		// backoff sits at the 30 s cap, far above the 1 s upstream hint.
		for i := 0; i < 8; i++ {
			_ = p.attachRetryAfter(vk, wrap(0))
		}
		if got := retryAfterOf(t, p.attachRetryAfter(vk, wrap(time.Second))); got < 15*time.Second || got >= 30*time.Second {
			t.Errorf("RetryAfter = %v, want the ninth-rejection local backoff in [15s, 30s), not the 1s upstream value", got)
		}
	})
}

// TestHandleChatCompletionLogsUpstreamErrorTypeAndHonoursRetryAfter is the
// end-to-end proof through the public handler: a Bedrock-shaped 429 from
// the upstream (X-Amzn-ErrorType + Retry-After headers, body with only a
// message — exactly what Bedrock sends) reaches the chat_completion error
// log line as structured upstream_status / upstream_error_type /
// upstream_retry_after_ms fields, and the error the client gets carries
// the upstream's 7 s as its Retry-After, not the sub-second local backoff.
func TestHandleChatCompletionLogsUpstreamErrorTypeAndHonoursRetryAfter(t *testing.T) {
	var logBuf bytes.Buffer
	keys := []identity.VirtualKey{
		{ID: "upstream-error-key", KeyHash: testHashOf("upstream-error-key"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{
		{Name: "d1", Model: "claude-bedrock", Provider: "openai", UpstreamModel: "claude-bedrock", BaseURL: "http://unused"},
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
			resp.Header.Set("Retry-After", "7")
			resp.Header.Set("x-amzn-ErrorType", "ThrottlingException:http://internal.amazon.com/coral/com.amazon.bedrock/")
			return nil, newUpstreamHTTPError(resp, []byte(`{"message":"Too many requests, please wait before trying again."}`))
		},
		Logger: slog.New(slog.NewJSONHandler(&logBuf, nil)),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	_, err = p.HandleChatCompletion(context.Background(), "Bearer upstream-error-key", "", "", adapter.ChatRequest{Model: "claude-bedrock", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}, "")
	if err == nil {
		t.Fatal("HandleChatCompletion returned nil error, want the upstream 429 to propagate")
	}
	var httpErr *UpstreamHTTPError
	if !errors.As(err, &httpErr) || httpErr.ErrorType != "ThrottlingException" {
		t.Fatalf("returned error = %v, want a wrapped *UpstreamHTTPError with ErrorType ThrottlingException", err)
	}
	var retryErr *RetryAfterError
	if !errors.As(err, &retryErr) {
		t.Fatalf("returned error = %v, want a *RetryAfterError wrap", err)
	}
	if retryErr.RetryAfter != 7*time.Second {
		t.Errorf("client-facing RetryAfter = %v, want 7s (the upstream's own Retry-After as a floor)", retryErr.RetryAfter)
	}

	var logged map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "chat_completion" {
			logged = entry
		}
	}
	if logged == nil {
		t.Fatalf("no chat_completion log line found in:\n%s", logBuf.String())
	}
	for field, want := range map[string]any{
		"upstream_status":         float64(429),
		"upstream_error_type":     "ThrottlingException",
		"upstream_retry_after_ms": float64(7000),
	} {
		if got := logged[field]; got != want {
			t.Errorf("chat_completion log field %s = %v (%T), want %v", field, got, got, want)
		}
	}
}
