package dataplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// relayResponseHeaders keeps exactly the two classes the protocol page asks a
// gateway to pass through -- x-should-retry and anthropic-ratelimit-unified-*
// -- and drops everything else the upstream sent (request ids, organisation
// ids, internal headers).
func TestRelayResponseHeadersKeepsOnlyRetryAndUnifiedRateLimitHeaders(t *testing.T) {
	src := http.Header{}
	src.Set("X-Should-Retry", "false")
	src.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	src.Add("Anthropic-Ratelimit-Unified-Reset", "2026-10-11T00:00:00Z")
	src.Set("Anthropic-Organization-Id", "org_1")
	src.Set("Request-Id", "req_1")
	src.Set("X-Upstream-Internal", "nope")
	got := relayResponseHeaders(src)
	if got.Get("X-Should-Retry") != "false" || got.Get("Anthropic-Ratelimit-Unified-Status") != "allowed" || got.Get("Anthropic-Ratelimit-Unified-Reset") == "" {
		t.Errorf("relayResponseHeaders dropped a relayable header: %v", got)
	}
	for _, name := range []string{"Anthropic-Organization-Id", "Request-Id", "X-Upstream-Internal"} {
		if got.Get(name) != "" {
			t.Errorf("relayResponseHeaders kept %s", name)
		}
	}
	if relayResponseHeaders(nil) != nil {
		t.Error("relayResponseHeaders(nil) should be nil")
	}
}

func anthropicBufferedUpstream(t *testing.T, status int, body string, headers map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const anthropicOKBody = `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`

// The buffered caller records the upstream's 2xx bytes and relayable headers
// only on a passthrough request to an anthropic deployment (RFC-1 §9, slice
// S11b); a translate-hop request to the same deployment records nothing, so
// the handler re-encodes the shadow as before.
func TestHTTPUpstreamCallerRecordsResponseMetaForAnthropicPassthroughOnly(t *testing.T) {
	srv := anthropicBufferedUpstream(t, http.StatusOK, anthropicOKBody, map[string]string{"x-should-retry": "false", "anthropic-ratelimit-unified-status": "allowed", "request-id": "req_1"})
	caller := NewHTTPUpstreamCaller(http.DefaultClient, nil)
	dep := Deployment{Name: "claude", Provider: "anthropic", BaseURL: srv.URL, APIKey: testCred("dep")}

	ctx, meta := WithUpstreamResponseMeta(context.Background())
	if _, err := caller(ctx, dep, &anthropic.PassthroughRequest{Raw: []byte(`{"model":"m"}`)}); err != nil {
		t.Fatalf("caller (passthrough): %v", err)
	}
	if !meta.Relayable() || string(meta.Body) != anthropicOKBody || meta.StatusCode != http.StatusOK || meta.Provider != "anthropic" {
		t.Fatalf("meta = %+v, want the upstream's 2xx body recorded", meta)
	}
	if meta.Header.Get("X-Should-Retry") != "false" || meta.Header.Get("Anthropic-Ratelimit-Unified-Status") != "allowed" || meta.Header.Get("Request-Id") != "" {
		t.Errorf("meta.Header = %v, want only the relayable headers", meta.Header)
	}

	ctx2, meta2 := WithUpstreamResponseMeta(context.Background())
	if _, err := caller(ctx2, dep, &anthropic.Request{Model: "m"}); err != nil {
		t.Fatalf("caller (translate hop): %v", err)
	}
	if meta2.Relayable() || meta2.Body != nil {
		t.Errorf("translate hop recorded meta %+v, want none", meta2)
	}

	// No carrier on the context: the caller must not panic and must still
	// return the decoded response (a test or probe driving it directly).
	if _, err := caller(context.Background(), dep, &anthropic.PassthroughRequest{Raw: []byte(`{"model":"m"}`)}); err != nil {
		t.Fatalf("caller (no carrier): %v", err)
	}
}

// Both HTTP callers tag an upstream error with the deployment's provider and
// the relayable response headers, so the Anthropic envelope can scope the
// verbatim 400/422 exception to anthropic deployments and forward
// x-should-retry / anthropic-ratelimit-unified-* on an error too.
func TestHTTPUpstreamCallersTagErrorsWithProviderAndRelayHeaders(t *testing.T) {
	body := `{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}`
	srv := anthropicBufferedUpstream(t, http.StatusBadRequest, body, map[string]string{"x-should-retry": "false", "anthropic-ratelimit-unified-status": "rejected", "request-id": "req_2"})
	dep := Deployment{Name: "claude", Provider: "anthropic", BaseURL: srv.URL, APIKey: testCred("dep")}
	check := func(name string, err error) {
		var up *UpstreamHTTPError
		if !errors.As(err, &up) {
			t.Fatalf("%s: err = %v, want *UpstreamHTTPError", name, err)
		}
		if up.Provider != "anthropic" || up.StatusCode != http.StatusBadRequest || up.Body != body {
			t.Errorf("%s: error = %+v, want provider anthropic, 400, the body", name, up)
		}
		if up.RelayHeaders.Get("X-Should-Retry") != "false" || up.RelayHeaders.Get("Anthropic-Ratelimit-Unified-Status") != "rejected" || up.RelayHeaders.Get("Request-Id") != "" {
			t.Errorf("%s: RelayHeaders = %v, want only the relayable headers", name, up.RelayHeaders)
		}
	}
	_, err := NewHTTPUpstreamCaller(http.DefaultClient, nil)(context.Background(), dep, &anthropic.PassthroughRequest{Raw: []byte(`{"model":"m"}`)})
	check("buffered", err)
	_, err = NewHTTPUpstreamStreamCaller(http.DefaultClient, nil, 5*time.Second)(context.Background(), dep, &anthropic.PassthroughRequest{Raw: []byte(`{"model":"m","stream":true}`)})
	check("stream", err)
}

// callDeployment empties the carrier on every error after the upstream call:
// an anthropic 2xx the caller could not decode has already recorded its
// bytes, and the fallback hop that serves the turn must not leave them for
// the handler to relay beside its own canonical response.
func TestCallDeploymentFailureClearsUpstreamResponseMetaBeforeFallback(t *testing.T) {
	verifier, err := identity.NewVerifier(defaultTestVirtualKeys())
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{
		{Name: "claude-primary", Model: "claude-sys", Provider: "anthropic", UpstreamModel: "claude-up", BaseURL: "http://unused",
			FallbackChains: map[string][]string{FallbackClassGeneric: {"gpt-fallback"}}},
		{Name: "gpt-fallback", Model: "gpt-fb", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	calls := map[string]int{}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(defaultTestVirtualKeys())),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"anthropic": anthropic.New(), "openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			calls[dep.Name]++
			if dep.Provider == "anthropic" {
				// What NewHTTPUpstreamCaller does with a 2xx it then cannot decode.
				recordUpstreamResponseMeta(ctx, "anthropic", &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Should-Retry": {"true"}}}, []byte("<html>proxy</html>"))
				return nil, errors.New("unmarshaling anthropic response: invalid character '<'")
			}
			return fakeOpenAIResponse("gpt-4o"), nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	ctx, meta := WithUpstreamResponseMeta(context.Background())
	resp, err := p.HandleChatCompletion(ctx, "Bearer test-key", "", "", adapter.ChatRequest{Model: "claude-sys", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}, "")
	if err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if calls["claude-primary"] != 1 || calls["gpt-fallback"] != 1 {
		t.Fatalf("upstream calls = %v, want one per deployment", calls)
	}
	if resp.Model != "gpt-fb" {
		t.Errorf("served model = %q, want the fallback's gpt-fb", resp.Model)
	}
	if meta.Relayable() || len(meta.Body) != 0 || meta.Provider != "" || meta.Header != nil {
		t.Fatalf("carrier still holds the failed hop's bytes: %+v", *meta)
	}
}
