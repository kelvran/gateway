package dataplane

// End-to-end proofs for upstream_error.go that go through the REAL HTTP
// caller closures and the real public handlers, closing the gap between
// the constructor unit tests in upstream_error_test.go and production:
// an httptest.Server returning a Bedrock-shaped 429 proves all three
// closures hand the headers to newUpstreamHTTPError, and the embeddings
// handler proves logEmbeddingsRequest carries the new fields.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// bedrockShapedThrottleServer is an httptest.Server that answers every
// request the way Bedrock answers a throttled Converse call: a 429 whose
// exception name lives in the X-Amzn-ErrorType header (with the URI
// suffix AWS appends), a Retry-After, and a body carrying only a message.
func bedrockShapedThrottleServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Amzn-ErrorType", "ThrottlingException:http://internal.amazon.com/coral/com.amazon.bedrock/")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"Too many requests, please wait before trying again."}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func assertThrottledUpstreamHTTPError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error for a 429 upstream response, got nil")
	}
	var httpErr *UpstreamHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %v (%T), want a *UpstreamHTTPError", err, err)
	}
	if httpErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want 429", httpErr.StatusCode)
	}
	if httpErr.ErrorType != "ThrottlingException" {
		t.Errorf("ErrorType = %q, want ThrottlingException (sanitised from the X-Amzn-ErrorType header)", httpErr.ErrorType)
	}
	if httpErr.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v, want 7s (parsed from the Retry-After header)", httpErr.RetryAfter)
	}
	if !strings.Contains(httpErr.Body, "Too many requests") {
		t.Errorf("Body = %q, want the upstream body captured verbatim", httpErr.Body)
	}
}

// TestHTTPUpstreamCallersCaptureErrorTypeAndRetryAfterFromRealResponses
// drives each of the three real HTTP caller closures against a real HTTP
// server, so a regression in any one of them (wrong argument order, a
// literal &UpstreamHTTPError{} reintroduced, the body reader consumed
// before the constructor) fails here rather than in production.
func TestHTTPUpstreamCallersCaptureErrorTypeAndRetryAfterFromRealResponses(t *testing.T) {
	srv := bedrockShapedThrottleServer(t)
	dep := Deployment{Name: "d1", Model: "m", Provider: "openai", UpstreamModel: "m", BaseURL: srv.URL, APIKey: embeddingsTestFakeCredential("upstream-error")}
	ctx := context.Background()

	t.Run("buffered chat caller", func(t *testing.T) {
		_, err := NewHTTPUpstreamCaller(srv.Client(), nil)(ctx, dep, map[string]any{"model": "m"})
		assertThrottledUpstreamHTTPError(t, err)
	})
	t.Run("embedding caller", func(t *testing.T) {
		_, err := NewHTTPEmbeddingUpstreamCaller(srv.Client(), nil)(ctx, dep, map[string]any{"model": "m", "input": []string{"x"}})
		assertThrottledUpstreamHTTPError(t, err)
	})
	t.Run("streaming caller", func(t *testing.T) {
		rc, err := NewHTTPUpstreamStreamCaller(srv.Client(), nil, 5*time.Second)(ctx, dep, map[string]any{"model": "m", "stream": true})
		if rc != nil {
			_ = rc.Close()
		}
		assertThrottledUpstreamHTTPError(t, err)
	})
}

// findJSONLogLine returns the last JSON log entry in buf whose msg equals
// msg, or fails the test.
func findJSONLogLine(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == msg {
			found = entry
		}
	}
	if found == nil {
		t.Fatalf("no %q log line found in:\n%s", msg, buf.String())
	}
	return found
}

// TestHandleEmbeddingsLogsUpstreamErrorFields is the embeddings twin of
// TestHandleChatCompletionLogsUpstreamErrorTypeAndHonoursRetryAfter: the
// "embeddings" error log line written by logEmbeddingsRequest carries the
// structured upstream fields for a Bedrock-shaped 503.
func TestHandleEmbeddingsLogsUpstreamErrorFields(t *testing.T) {
	var logBuf bytes.Buffer
	const keyID = "emb-upstream-error-key"
	keys := []identity.VirtualKey{{ID: keyID, KeyHash: testHashOf(keyID), RateLimitBurst: 100, RateLimitRefill: 100}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{
		{Name: "emb1", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"},
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
			t.Fatal("chat Upstream must never be called by an embeddings test")
			return nil, nil
		},
		EmbeddingUpstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			resp := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}}
			resp.Header.Set("X-Amzn-ErrorType", "ServiceUnavailableException")
			resp.Header.Set("Retry-After", "3")
			return nil, newUpstreamHTTPError(resp, []byte(`{"message":"Bedrock is unable to process your request."}`))
		},
		Logger: slog.New(slog.NewJSONHandler(&logBuf, nil)),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	_, err = p.HandleEmbeddings(context.Background(), "Bearer "+keyID, "", adapter.EmbeddingRequest{Model: "text-embedding-3-small", Input: []string{"hello"}})
	if err == nil {
		t.Fatal("HandleEmbeddings returned nil error, want the upstream 503 to propagate")
	}
	var httpErr *UpstreamHTTPError
	if !errors.As(err, &httpErr) || httpErr.ErrorType != "ServiceUnavailableException" {
		t.Fatalf("returned error = %v, want a wrapped *UpstreamHTTPError with ErrorType ServiceUnavailableException", err)
	}

	logged := findJSONLogLine(t, &logBuf, "embeddings")
	for field, want := range map[string]any{
		"upstream_status":         float64(503),
		"upstream_error_type":     "ServiceUnavailableException",
		"upstream_retry_after_ms": float64(3000),
	} {
		if got := logged[field]; got != want {
			t.Errorf("embeddings log field %s = %v (%T), want %v", field, got, got, want)
		}
	}
}

// TestHandleChatCompletionStreamLogsUpstreamStreamErrorFields proves the
// mid-stream half of upstreamErrorLogFields through the REAL streaming
// handler: a wire-accurate Bedrock exception frame (built with the same
// eventstream.Encoder fixture helpers bedrock_stream_test.go uses) is
// decoded by the real adapter into an *adapter.UpstreamStreamError wrapping
// bedrock.ErrBedrockThrottled, and the chat_completion error log line
// carries upstream_provider / upstream_stream_error — which only holds if
// every wrap on the streaming error path is a %w that errors.As can see.
func TestHandleChatCompletionStreamLogsUpstreamStreamErrorFields(t *testing.T) {
	var logBuf bytes.Buffer
	wire := encodeBedrockWireFixture(t, []eventstream.Message{
		bedrockWireEvent("messageStart", `{"role":"assistant"}`),
		{
			Headers: eventstream.Headers{
				{Name: ":message-type", Value: eventstream.StringValue("exception")},
				{Name: ":exception-type", Value: eventstream.StringValue("throttlingException")},
			},
			Payload: []byte(`{"message":"rate exceeded"}`),
		},
	})

	const keyID = "stream-upstream-error-key"
	keys := []identity.VirtualKey{{ID: keyID, KeyHash: testHashOf(keyID), RateLimitBurst: 100, RateLimitRefill: 100}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{
		{Name: "d1", Model: "claude-bedrock", Provider: "bedrock", UpstreamModel: "anthropic.claude-3-5-sonnet-20241022-v2:0", BaseURL: "http://unused"},
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"bedrock": bedrock.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream must never be called by a streaming test")
			return nil, nil
		},
		UpstreamStream: func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(wire)), nil
		},
		Logger: slog.New(slog.NewJSONHandler(&logBuf, nil)),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	err = p.HandleChatCompletionStream(context.Background(), "Bearer "+keyID, "", "", adapter.ChatRequest{
		Model: "claude-bedrock", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, httptest.NewRecorder(), "")
	if !errors.Is(err, bedrock.ErrBedrockThrottled) {
		t.Fatalf("HandleChatCompletionStream error = %v, want errors.Is(err, bedrock.ErrBedrockThrottled)", err)
	}

	logged := findJSONLogLine(t, &logBuf, "chat_completion")
	if got := logged["upstream_provider"]; got != "bedrock" {
		t.Errorf("chat_completion log field upstream_provider = %v, want bedrock", got)
	}
	if got := logged["upstream_stream_error"]; got != bedrock.ErrBedrockThrottled.Error() {
		t.Errorf("chat_completion log field upstream_stream_error = %v, want %q", got, bedrock.ErrBedrockThrottled.Error())
	}
	if _, has := logged["upstream_status"]; has {
		t.Errorf("chat_completion log carries upstream_status for a mid-stream exception frame, want none (no HTTP status applies)")
	}
}
