package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/idempotency"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

// apiErrorBody is the OpenAI-shaped envelope every data-plane error now
// carries: {"error":{"message","type","param","code"}}. The four keys are
// always present (null when unknown).
type apiErrorBody struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    *string `json:"code"`
	} `json:"error"`
}

func decodeAPIError(t *testing.T, rec *httptest.ResponseRecorder) apiErrorBody {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	raw := rec.Body.Bytes()
	if bytes.HasSuffix(raw, []byte("\n")) {
		t.Errorf("error body ends with a newline; OpenAI's own bodies do not, and a byte-exact client fixture would notice")
	}
	var body apiErrorBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("error body is not the JSON envelope: %v\nbody: %s", err, rec.Body.String())
	}
	return body
}

func strPtr(s string) *string { return &s }

// TestWriteErrorResponseEnvelopeTable enumerates EVERY sentinel the status
// switch knows, bare and %w-wrapped, and pins status, type, code and that
// the message still carries the sentinel's own text. If a future sentinel
// is added without a row here, the default-502 branch is the only thing
// that catches it, which is exactly the regression this table exists to
// make visible.
func TestWriteErrorResponseEnvelopeTable(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		status  int
		typ     string
		code    *string
		wantMsg string // substring the message must contain
	}{
		{"missing header", identity.ErrMissingHeader, http.StatusUnauthorized, "authentication_error", nil, identity.ErrMissingHeader.Error()},
		{"invalid key", identity.ErrInvalidKey, http.StatusUnauthorized, "authentication_error", strPtr("invalid_api_key"), identity.ErrInvalidKey.Error()},
		{"key expired", &identity.KeyExpiredError{ID: "team-expired-fixture", ExpiresAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, http.StatusUnauthorized, "authentication_error", strPtr("key_expired"), identity.ErrKeyExpired.Error()},
		{"rate limited", dataplane.ErrRateLimited, http.StatusTooManyRequests, "rate_limit_error", strPtr("rate_limit_exceeded"), dataplane.ErrRateLimited.Error()},
		{"budget exceeded", dataplane.ErrBudgetExceeded, http.StatusTooManyRequests, "insufficient_quota", strPtr("insufficient_quota"), dataplane.ErrBudgetExceeded.Error()},
		{"concurrency", dataplane.ErrConcurrencyLimitExceeded, http.StatusTooManyRequests, "rate_limit_error", strPtr("concurrency_limit_exceeded"), dataplane.ErrConcurrencyLimitExceeded.Error()},
		{"deployment capacity", &dataplane.DeploymentCapacityError{Deployment: "d", Reason: "rpm"}, http.StatusServiceUnavailable, "server_error", strPtr("deployment_capacity_exceeded"), "capacity"},
		{"model not allowed", dataplane.ErrModelNotAllowed, http.StatusForbidden, "permission_error", strPtr("model_not_allowed"), dataplane.ErrModelNotAllowed.Error()},
		{"source ip", dataplane.ErrSourceIPNotAllowed, http.StatusForbidden, "permission_error", strPtr("source_ip_not_allowed"), dataplane.ErrSourceIPNotAllowed.Error()},
		{"no deployment", dataplane.ErrNoDeployment, http.StatusBadRequest, "invalid_request_error", strPtr("model_not_found"), dataplane.ErrNoDeployment.Error()},
		{"guardrail", dataplane.ErrGuardrailBlocked, http.StatusBadRequest, "invalid_request_error", strPtr("content_policy_violation"), dataplane.ErrGuardrailBlocked.Error()},
		{"streaming unsupported", dataplane.ErrStreamingNotSupported, http.StatusBadRequest, "invalid_request_error", strPtr("streaming_not_supported"), dataplane.ErrStreamingNotSupported.Error()},
		{"not an embedding deployment", dataplane.ErrNotAnEmbeddingDeployment, http.StatusBadRequest, "invalid_request_error", strPtr("not_an_embedding_model"), dataplane.ErrNotAnEmbeddingDeployment.Error()},
		{"empty messages", dataplane.ErrEmptyMessages, http.StatusBadRequest, "invalid_request_error", strPtr("empty_messages"), dataplane.ErrEmptyMessages.Error()},
		{"streaming not configured", dataplane.ErrStreamingNotConfigured, http.StatusNotImplemented, "server_error", strPtr("streaming_not_configured"), dataplane.ErrStreamingNotConfigured.Error()},
		{"embeddings not configured", dataplane.ErrEmbeddingsNotConfigured, http.StatusNotImplemented, "server_error", strPtr("embeddings_not_configured"), dataplane.ErrEmbeddingsNotConfigured.Error()},
		{"upstream http", &dataplane.UpstreamHTTPError{StatusCode: 503, Body: "x"}, http.StatusBadGateway, "server_error", strPtr("upstream_error"), "503"},
		{"unknown", errors.New("something else"), http.StatusBadGateway, "server_error", strPtr("upstream_error"), "something else"},
		// G16 (docs/rfcs/2026-10-10-gateway-owner-gate-decisions.md): both are
		// faults known from the request alone, so 4xx with their own codes, not
		// the 502 default they fell to before.
		{"idempotency key reused", idempotency.ErrFingerprintMismatch, http.StatusUnprocessableEntity, "invalid_request_error", strPtr("idempotency_key_reused"), idempotency.ErrFingerprintMismatch.Error()},
		{"response_format unsupported", adapter.ErrStructuredOutputUnsupported, http.StatusBadRequest, "invalid_request_error", strPtr("response_format_unsupported"), adapter.ErrStructuredOutputUnsupported.Error()},
		{"tool result parts unsupported", adapter.ErrToolResultPartsUnsupported, http.StatusBadRequest, "invalid_request_error", strPtr("tool_result_parts_unsupported"), adapter.ErrToolResultPartsUnsupported.Error()},
	}
	for _, tc := range cases {
		for _, wrapped := range []bool{false, true} {
			err := tc.err
			name := tc.name
			if wrapped {
				err = fmt.Errorf("outer: %w", err)
				name += " (wrapped)"
			}
			t.Run(name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				writeErrorResponse(rec, err)
				if rec.Code != tc.status {
					t.Fatalf("status = %d, want %d", rec.Code, tc.status)
				}
				body := decodeAPIError(t, rec)
				if body.Error.Type != tc.typ {
					t.Errorf("type = %q, want %q", body.Error.Type, tc.typ)
				}
				switch {
				case tc.code == nil && body.Error.Code != nil:
					t.Errorf("code = %q, want null", *body.Error.Code)
				case tc.code != nil && (body.Error.Code == nil || *body.Error.Code != *tc.code):
					t.Errorf("code = %v, want %q", body.Error.Code, *tc.code)
				}
				if !strings.Contains(body.Error.Message, tc.wantMsg) {
					t.Errorf("message = %q, want it to contain %q", body.Error.Message, tc.wantMsg)
				}
				if strings.Contains(body.Error.Message, "team-expired-fixture") {
					t.Errorf("message = %q names the expired key; the id belongs on the log line only", body.Error.Message)
				}
			})
		}
	}
}

// TestWriteErrorResponseParamNamesTheOffendingField pins the envelope's
// param for the three errors that have one: the request field or header the
// client must change. Every other error keeps param null.
func TestWriteErrorResponseParamNamesTheOffendingField(t *testing.T) {
	for _, tc := range []struct {
		err   error
		param string
	}{
		{dataplane.ErrEmptyMessages, "messages"},
		{adapter.ErrStructuredOutputUnsupported, "response_format"},
		{fmt.Errorf("dataplane: idempotency: %w", idempotency.ErrFingerprintMismatch), "Idempotency-Key"},
	} {
		rec := httptest.NewRecorder()
		writeErrorResponse(rec, tc.err)
		body := decodeAPIError(t, rec)
		if body.Error.Param == nil || *body.Error.Param != tc.param {
			t.Errorf("%v: param = %v, want %q", tc.err, body.Error.Param, tc.param)
		}
	}
	rec := httptest.NewRecorder()
	writeErrorResponse(rec, dataplane.ErrRateLimited)
	if body := decodeAPIError(t, rec); body.Error.Param != nil {
		t.Errorf("rate limited: param = %q, want null", *body.Error.Param)
	}
}

// TestWriteErrorResponseBudgetIsInsufficientQuotaNotRateLimit is the one
// row that changes client behaviour: LangChain and LiteLLM stop retrying on
// insufficient_quota but keep retrying rate_limit_error, so a budget
// rejection must never be labelled as a transient rate limit.
func TestWriteErrorResponseBudgetIsInsufficientQuotaNotRateLimit(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErrorResponse(rec, dataplane.ErrBudgetExceeded)
	body := decodeAPIError(t, rec)
	if body.Error.Type == "rate_limit_error" {
		t.Fatal("budget exhaustion labelled rate_limit_error; clients would retry a permanent condition")
	}
	if body.Error.Type != "insufficient_quota" {
		t.Errorf("type = %q, want insufficient_quota", body.Error.Type)
	}
}

// Retry-After preservation is NOT re-tested here on purpose: RetryAfterError
// has no exported constructor (the dataplane attaches it internally), and the
// existing integration tests in this package already assert the header on real
// 429/503 responses through the pipeline; they must stay green after this change.

// TestWriteErrorResponseRedactsUpstreamBodyInsideEnvelope: the redaction
// introduced on 2026-09-26 (raw upstream bodies leaked AWS account ids) must
// survive the move from text/plain to JSON.
func TestWriteErrorResponseRedactsUpstreamBodyInsideEnvelope(t *testing.T) {
	arn := "arn:aws:iam::123456789012:role/kelvran-gateway"
	rec := httptest.NewRecorder()
	writeErrorResponse(rec, &dataplane.UpstreamHTTPError{StatusCode: 403, Body: fmt.Sprintf(`{"message":"User: %s is not authorized"}`, arn)})
	body := decodeAPIError(t, rec)
	if strings.Contains(body.Error.Message, arn) || strings.Contains(body.Error.Message, "123456789012") {
		t.Fatalf("envelope leaks the upstream body: %q", body.Error.Message)
	}
	if !strings.Contains(body.Error.Message, "403") {
		t.Errorf("message %q should still name the upstream status", body.Error.Message)
	}
}

// TestChatCompletionsHandlerMalformedJSONReturnsEnvelope covers the handler-
// level validation path that never reaches the pipeline (the pipeline is
// nil here on purpose): the F4 symptom ("tool_choice":"auto" → 400) lands
// exactly on this branch, so it must speak JSON too.
func TestChatCompletionsHandlerMalformedJSONReturnsEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":`))
	chatCompletionsHandler(nil)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := decodeAPIError(t, rec)
	if body.Error.Type != "invalid_request_error" || body.Error.Code == nil || *body.Error.Code != "invalid_json" {
		t.Errorf("type/code = %q/%v, want invalid_request_error/invalid_json", body.Error.Type, body.Error.Code)
	}
}

// TestChatCompletionsHandlerMethodNotAllowedReturnsEnvelope: 405 is a
// handler-level decision too; it must speak JSON and, per RFC 9110
// §15.5.6, carry Allow.
func TestChatCompletionsHandlerMethodNotAllowedReturnsEnvelope(t *testing.T) {
	for _, h := range []struct {
		name string
		fn   http.HandlerFunc
		path string
	}{{"chat", chatCompletionsHandler(nil), "/v1/chat/completions"}, {"embeddings", embeddingsHandler(nil), "/v1/embeddings"}} {
		t.Run(h.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.fn(rec, httptest.NewRequest(http.MethodGet, h.path, nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", rec.Code)
			}
			if got := rec.Header().Get("Allow"); got != http.MethodPost {
				t.Errorf("Allow = %q, want POST", got)
			}
			body := decodeAPIError(t, rec)
			if body.Error.Code == nil || *body.Error.Code != "method_not_allowed" {
				t.Errorf("code = %v, want method_not_allowed", body.Error.Code)
			}
		})
	}
}

// TestWriteTrackerRecordsCommit: the streaming handler relies on the tracker
// to tell "nothing sent yet" from "status already committed"; every path
// that commits (WriteHeader, Write, Flush) must flip the flag, and the
// wrapper must still flush (streaming.NewWriter requires http.Flusher).
func TestWriteTrackerRecordsCommit(t *testing.T) {
	for name, commit := range map[string]func(*writeTracker){
		"WriteHeader": func(tw *writeTracker) { tw.WriteHeader(http.StatusOK) },
		"Write":       func(tw *writeTracker) { _, _ = tw.Write([]byte("x")) },
		"Flush":       func(tw *writeTracker) { tw.Flush() },
	} {
		t.Run(name, func(t *testing.T) {
			tw := &writeTracker{ResponseWriter: httptest.NewRecorder()}
			if tw.written {
				t.Fatal("fresh tracker reports written")
			}
			commit(tw)
			if !tw.written {
				t.Errorf("%s did not mark the response as committed", name)
			}
		})
	}
	var _ http.Flusher = &writeTracker{}
}

// TestWriteStreamErrorFrameIsInBandAndRedacted: a post-first-chunk failure
// is one SSE data frame carrying the same envelope a buffered error would,
// with the upstream body redacted and no [DONE] sentinel after it.
func TestWriteStreamErrorFrameIsInBandAndRedacted(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusOK) // the first chunk already committed 200
	arn := "arn:aws:iam::123456789012:role/kelvran-gateway"
	writeStreamErrorFrame(rec, &dataplane.UpstreamHTTPError{StatusCode: 500, Body: arn})
	out := rec.Body.String()
	if !strings.HasPrefix(out, "data: {\"error\":") || !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("not a single SSE data frame: %q", out)
	}
	if strings.Contains(out, "[DONE]") {
		t.Error("an error frame must not be followed by [DONE]: the completion is not complete")
	}
	if strings.Contains(out, arn) {
		t.Errorf("frame leaks the upstream body: %q", out)
	}
	var body apiErrorBody
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(out, "data: "))), &body); err != nil {
		t.Fatalf("frame payload is not the envelope: %v", err)
	}
	if body.Error.Type != "server_error" || body.Error.Code == nil || *body.Error.Code != "upstream_error" {
		t.Errorf("type/code = %q/%v, want server_error/upstream_error", body.Error.Type, body.Error.Code)
	}
}

// newMockStreamingUpstreamFailingMidStream sends one well-formed OpenAI chunk,
// flushes it (so the gateway commits status 200 to its client), then sends a
// frame that is not JSON and closes without [DONE] -- the shape of a provider
// dying mid-response.
func newMockStreamingUpstreamFailingMidStream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}
		var req openai.Request
		if err := json.Unmarshal(body, &req); err != nil || !req.Stream {
			http.Error(w, "expected a streaming request", http.StatusBadRequest)
			return
		}
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"id":"chatcmpl-midstream","model":%q,"choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`, req.Model))
		flusher.Flush()
		_, _ = fmt.Fprint(w, "data: {this is not json\n\n")
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestIntegrationMidStreamFailureEmitsSSEErrorFrame drives a real streaming
// request whose upstream dies after the first chunk: the client must receive
// status 200 (already committed), the chunk it was sent, then exactly one
// in-band `data: {"error":...}` frame and no [DONE].
func TestIntegrationMidStreamFailureEmitsSSEErrorFrame(t *testing.T) {
	upstream, calls := newMockStreamingUpstreamFailingMidStream(t)
	gw := newIntegrationServer(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_ENVELOPE")

	reqBody := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"mid-stream failure"}]}`
	httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer test-gateway-key")
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (the first chunk commits it); body: %s", resp.StatusCode, b)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, `"content":"partial"`) {
		t.Errorf("client never received the chunk the upstream sent before dying: %s", s)
	}
	if strings.Count(s, `data: {"error":`) != 1 {
		t.Errorf("want exactly one in-band error frame, got body: %s", s)
	}
	if strings.Contains(s, "[DONE]") {
		t.Errorf("a failed stream must not end with [DONE]: %s", s)
	}
	if strings.Contains(s, `"type":"server_error"`) == false {
		t.Errorf("error frame should carry type server_error: %s", s)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("upstream calls = %d, want 1", got)
	}
}

// TestWriteTrackerForwardsHijacker: the tracker must not hide the optional
// stdlib interfaces from a future upgrading handler. An httptest recorder
// cannot hijack, so the forwarded call must return an error (never panic)
// without marking the response committed; the type assertion itself must
// succeed.
func TestWriteTrackerForwardsHijacker(t *testing.T) {
	tw := &writeTracker{ResponseWriter: httptest.NewRecorder()}
	h, ok := interface{}(tw).(http.Hijacker)
	if !ok {
		t.Fatal("writeTracker does not satisfy http.Hijacker")
	}
	if _, _, err := h.Hijack(); err == nil {
		t.Error("hijacking a recorder should fail, got nil error")
	}
	// A REFUSED hijack committed nothing, so the tracker must still report
	// the response as uncommitted (a normal envelope is still possible).
	if tw.written {
		t.Error("a refused hijack must not mark the response as committed")
	}
}

// TestMarshalEnvelopeNeverEmpty: an error path must always produce a
// parseable envelope; the fallback constant must itself be valid JSON of
// the same shape.
func TestMarshalEnvelopeNeverEmpty(t *testing.T) {
	var body apiErrorBody
	if err := json.Unmarshal([]byte(fallbackEnvelope), &body); err != nil || body.Error.Type != "server_error" {
		t.Fatalf("fallbackEnvelope is not a valid envelope: %v", err)
	}
	out := marshalEnvelope(apiError{Error: apiErrorDetail{Message: "x\xff", Type: "server_error"}}) // invalid UTF-8 is replaced, not rejected
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("marshalEnvelope produced unparseable output %q: %v", out, err)
	}
}

// TestErrorEnvelopeLossyIngressRejected: RFC-1 §6's refusal is a 400
// invalid_request_error with its own code, decided before any upstream call,
// and param carries the pointers of the members that could not be translated.
func TestErrorEnvelopeLossyIngressRejected(t *testing.T) {
	err := &dataplane.ErrLossyIngressRejected{Pointers: []string{"/foo", "/thinking/display"}, TopLevelFields: 1, NestedCount: 1}
	if status := errorStatus(err); status != http.StatusBadRequest {
		t.Errorf("errorStatus = %d, want 400", status)
	}
	typ, code := errorTypeAndCode(err, http.StatusBadRequest)
	if typ != errTypeInvalidRequest || code == nil || *code != "lossy_ingress_rejected" {
		t.Errorf("errorTypeAndCode = %q/%v, want invalid_request_error/lossy_ingress_rejected", typ, code)
	}
	if param := errorParam(err); param == nil || *param != "/foo,/thinking/display" {
		t.Errorf("errorParam = %v, want the comma-joined pointers", param)
	}
}
