package dataplane

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

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

type countTokensCall struct {
	dep  Deployment
	body string
	hdrs http.Header
}

// newCountTokensTestPipeline wires an anthropic and an openai deployment with
// a scripted CountTokensUpstream that records what it was handed.
func newCountTokensTestPipeline(t *testing.T, calls *[]countTokensCall, result *CountTokensResult, callErr error) *Pipeline {
	t.Helper()
	keys := []identity.VirtualKey{{ID: "ct-key", KeyHash: testHashOf("ct-secret"), RateLimitBurst: 100, RateLimitRefill: 100}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatal(err)
	}
	deployments := []Deployment{
		{Name: "claude", Model: "claude-sys", Provider: "anthropic", UpstreamModel: "claude-upstream-id", BaseURL: "http://unused/v1/messages"},
		{Name: "gpt", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
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
			t.Fatal("the chat Upstream must not be called by count_tokens")
			return nil, nil
		},
		CountTokensUpstream: func(ctx context.Context, dep Deployment, req *anthropic.PassthroughRequest) (*CountTokensResult, error) {
			*calls = append(*calls, countTokensCall{dep: dep, body: string(req.Body()), hdrs: req.Headers.Clone()})
			return result, callErr
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const countTokensRawBody = `{"model":"claude-sys","messages":[{"role":"user","content":"how many tokens"}],"future":{ "x" : 1}}`

// On an anthropic deployment the token count is the deployment's own answer:
// the body reaches the caller as received with model rewritten to the
// upstream id and no stream member, the client's anthropic-* headers ride
// along, and the upstream's body and relayable headers come back untouched
// (item 11 slice S11c).
func TestHandleCountTokensRelaysTheAnthropicDeploymentsAnswer(t *testing.T) {
	var calls []countTokensCall
	want := &CountTokensResult{Body: []byte(`{ "input_tokens" : 42 }`), Header: http.Header{"Anthropic-Ratelimit-Unified-Status": {"allowed"}}}
	p := newCountTokensTestPipeline(t, &calls, want, nil)
	forward := http.Header{"Anthropic-Version": {"2024-06-01"}, "Anthropic-Beta": {"beta-a"}}
	got, err := p.HandleCountTokens(context.Background(), "Bearer ct-secret", "", "claude-sys", []byte(countTokensRawBody), forward)
	if err != nil {
		t.Fatalf("HandleCountTokens: %v", err)
	}
	if got != want {
		t.Errorf("result = %+v, want the caller's result handed back as is", got)
	}
	if len(calls) != 1 || calls[0].dep.Name != "claude" {
		t.Fatalf("calls = %+v, want one call to the anthropic deployment", calls)
	}
	wantBody := `{"model":"claude-upstream-id","messages":[{"role":"user","content":"how many tokens"}],"future":{ "x" : 1}}`
	if calls[0].body != wantBody {
		t.Errorf("upstream body = %s\nwant          %s", calls[0].body, wantBody)
	}
	if strings.Contains(calls[0].body, `"stream"`) {
		t.Errorf("a stream member reached count_tokens: %s", calls[0].body)
	}
	if calls[0].hdrs.Get("Anthropic-Version") != "2024-06-01" || calls[0].hdrs.Get("Anthropic-Beta") != "beta-a" {
		t.Errorf("forwarded headers = %v", calls[0].hdrs)
	}
}

// The pre-call guardrail scans the canonical shadow before any upstream
// byte: a blocked body is a 400 and the caller is never invoked.
func TestHandleCountTokensRunsTheGuardrailBeforeTheUpstream(t *testing.T) {
	var calls []countTokensCall
	p := newCountTokensTestPipeline(t, &calls, &CountTokensResult{Body: []byte(`{"input_tokens":1}`)}, nil)
	body := []byte(`{"model":"claude-sys","messages":[{"role":"user","content":"my card is 4111111111111111"}]}`)
	_, err := p.HandleCountTokens(context.Background(), "Bearer ct-secret", "", "claude-sys", body, nil)
	if !errors.Is(err, ErrGuardrailBlocked) {
		t.Fatalf("err = %v, want ErrGuardrailBlocked", err)
	}
	if len(calls) != 0 {
		t.Errorf("upstream called %d times after a guardrail block", len(calls))
	}
}

// A body the Messages parser rejects is the client's error (400 with the
// parser's code), surfaced as CountTokensBodyError; the upstream is never
// called. A deployment that is not anthropic still answers 404 whatever the
// body (the S10b contract).
func TestHandleCountTokensBodyErrorsAndNonAnthropicDeployments(t *testing.T) {
	var calls []countTokensCall
	p := newCountTokensTestPipeline(t, &calls, &CountTokensResult{Body: []byte(`{"input_tokens":1}`)}, nil)
	_, err := p.HandleCountTokens(context.Background(), "Bearer ct-secret", "", "claude-sys", []byte(`{"model":"claude-sys"}`), nil)
	var bodyErr *CountTokensBodyError
	if !errors.As(err, &bodyErr) {
		t.Fatalf("err = %v, want *CountTokensBodyError", err)
	}
	if len(calls) != 0 {
		t.Errorf("upstream called on a body error")
	}
	_, err = p.HandleCountTokens(context.Background(), "Bearer ct-secret", "", "gpt-4o", []byte(`{"model":"gpt-4o"}`), nil)
	if !errors.Is(err, ErrCountTokensUnavailable) {
		t.Errorf("openai deployment: err = %v, want ErrCountTokensUnavailable", err)
	}
	if len(calls) != 0 {
		t.Errorf("upstream called for a non-anthropic deployment")
	}
}

// An upstream failure reaches the handler as the caller's error unchanged,
// so the envelope writer applies the usual rules (verbatim 400/422, redacted
// otherwise) and the relay headers it carries.
func TestHandleCountTokensPassesTheUpstreamErrorThrough(t *testing.T) {
	var calls []countTokensCall
	upErr := &UpstreamHTTPError{StatusCode: 400, Body: `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`, Provider: "anthropic"}
	p := newCountTokensTestPipeline(t, &calls, nil, upErr)
	_, err := p.HandleCountTokens(context.Background(), "Bearer ct-secret", "", "claude-sys", []byte(countTokensRawBody), nil)
	var got *UpstreamHTTPError
	if !errors.As(err, &got) || got.StatusCode != 400 || got.Provider != "anthropic" {
		t.Fatalf("err = %v, want the caller's UpstreamHTTPError", err)
	}
}
