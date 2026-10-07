package dataplane

// Deployment-scoped TPM gate — the per-hop reserve/reconcile the
// 2026-09-09 per-deployment-concurrency research deferred, wired
// 2026-10-07 per docs/upgrade-research/kelvran-deep-research-round3-
// 2026-10-07.md (ranked item 1). These tests use only the public handlers
// and the existing capacity wrappers, so they compile against the code
// BEFORE the gate existed and fail there (no third call is ever rejected)
// — the sanity-check-by-breaking for the feature.

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// deploymentTPMLimiter builds a deployment limiter with an effectively
// unlimited RPM ceiling and the given TPM bucket for depName — so only
// the TPM dimension can ever reject.
func deploymentTPMLimiter(depName string, tpmCapacity, tpmRefill float64) *ratelimit.KeyLimiter {
	return ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{
		{ID: depName, Capacity: 1000, RefillPerSecond: 1000, TPMCapacity: tpmCapacity, TPMRefillPerSecond: tpmRefill},
	})
}

func assertDeploymentCapacityTPM(t *testing.T, err error, depName string) {
	t.Helper()
	var capErr *DeploymentCapacityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v, want a *DeploymentCapacityError (Reason tpm) for %q", err, depName)
	}
	if capErr.Deployment != depName || capErr.Reason != "tpm" {
		t.Errorf("capErr = %+v, want Deployment=%s Reason=tpm", capErr, depName)
	}
}

// distinctChatRequest varies the content per call so every call is a
// genuine cache MISS that reaches the upstream — identical or lexically
// near-identical requests would otherwise be served from L1/L3 and never
// touch the deployment's own TPM bucket at all.
func distinctChatRequest(model string, n int) adapter.ChatRequest {
	content := "deployment tpm gate request " + string(rune('a'+n%26)) + string(rune('A'+(n/26)%26))
	return adapter.ChatRequest{Model: model, Messages: []adapter.Message{{Role: "user", Content: content}}}
}

// TestCallDeploymentWithCapacityCheckRejectsOnceDeploymentTPMExhausted is
// the direct gate proof, with the same arithmetic as the per-key
// TestHandleChatCompletionRejectsOnceTPMBucketExhausted: a 10-token
// bucket with no refill and 8-token responses admits exactly two calls
// (cold start reserves the full balance and reconciles to 8; the 2 left
// admit one more, reconciled to -6) and rejects the third with
// Reason "tpm".
func TestCallDeploymentWithCapacityCheckRejectsOnceDeploymentTPMExhausted(t *testing.T) {
	dep := Deployment{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o"}
	calls := 0
	p := &Pipeline{
		deploymentLimiter: deploymentTPMLimiter("d1", 10, 0),
		logger:            discardLogger(),
		adapters:          adapter.Registry{"openai": openai.New()},
		upstream: func(ctx context.Context, d Deployment, req any) (any, error) {
			calls++
			return fakeOpenAIResponse(d.UpstreamModel), nil
		},
	}
	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	for i := 1; i <= 2; i++ {
		if _, err := p.callDeploymentWithCapacityCheck(context.Background(), dep, req); err != nil {
			t.Fatalf("call %d: %v, want admitted", i, err)
		}
	}
	_, err := p.callDeploymentWithCapacityCheck(context.Background(), dep, req)
	assertDeploymentCapacityTPM(t, err, "d1")
	if calls != 2 {
		t.Errorf("upstream calls = %d, want 2 (the rejected third call must never reach the upstream)", calls)
	}
}

// TestCallDeploymentWithCapacityCheckReleasesDeploymentTPMOnUpstreamError
// is the release guard: a failed hop must undo its reservation (the
// per-hop rule the config doc named as the reason deployment TPM was
// deferred), so repeated upstream failures never eat the bucket.
func TestCallDeploymentWithCapacityCheckReleasesDeploymentTPMOnUpstreamError(t *testing.T) {
	dep := Deployment{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o"}
	fail := true
	p := &Pipeline{
		deploymentLimiter: deploymentTPMLimiter("d1", 10, 0),
		logger:            discardLogger(),
		adapters:          adapter.Registry{"openai": openai.New()},
		upstream: func(ctx context.Context, d Deployment, req any) (any, error) {
			if fail {
				return nil, errors.New("simulated upstream failure")
			}
			return fakeOpenAIResponse(d.UpstreamModel), nil
		},
	}
	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	for i := 1; i <= 5; i++ {
		_, err := p.callDeploymentWithCapacityCheck(context.Background(), dep, req)
		var capErr *DeploymentCapacityError
		if err == nil || errors.As(err, &capErr) {
			t.Fatalf("failing call %d: err = %v, want the simulated upstream failure (never a capacity rejection)", i, err)
		}
	}
	fail = false
	if _, err := p.callDeploymentWithCapacityCheck(context.Background(), dep, req); err != nil {
		t.Fatalf("first successful call after 5 failed ones: %v, want admitted (every failed hop released its reservation)", err)
	}
}

// TestCallDeploymentWithCapacityCheckReleasesDeploymentTPMOnPanic proves
// the reconcile is deferred: a panicking adapter/upstream (recovered here,
// as a caller higher up the stack would) must not leak its reservation —
// the ratelimit.TokenBucket contract holds on EVERY return path, panics
// included, exactly as the deferred concurrency release already does.
func TestCallDeploymentWithCapacityCheckReleasesDeploymentTPMOnPanic(t *testing.T) {
	dep := Deployment{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o"}
	panicking := true
	p := &Pipeline{
		deploymentLimiter: deploymentTPMLimiter("d1", 10, 0),
		logger:            discardLogger(),
		adapters:          adapter.Registry{"openai": openai.New()},
		upstream: func(ctx context.Context, d Deployment, req any) (any, error) {
			if panicking {
				panic("simulated adapter panic")
			}
			return fakeOpenAIResponse(d.UpstreamModel), nil
		},
	}
	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	callRecovering := func() (recovered any) {
		defer func() { recovered = recover() }()
		_, _ = p.callDeploymentWithCapacityCheck(context.Background(), dep, req)
		return nil
	}
	for i := 1; i <= 5; i++ {
		if got := callRecovering(); got == nil {
			t.Fatalf("panicking call %d did not panic", i)
		}
	}
	panicking = false
	if _, err := p.callDeploymentWithCapacityCheck(context.Background(), dep, req); err != nil {
		t.Fatalf("first successful call after 5 panicking ones: %v, want admitted (every panicking hop released its reservation)", err)
	}
}

// TestHandleChatCompletionFailedFallbackHopReleasesDeploymentTPM: a hop
// that is selected by the chain and then FAILS upstream must release the
// reservation it took — otherwise five failing requests would leave the
// hop permanently "at capacity (tpm)" without ever having served one.
func TestHandleChatCompletionFailedFallbackHopReleasesDeploymentTPM(t *testing.T) {
	primary := Deployment{
		Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused",
		FallbackChains: map[string][]string{FallbackClassGeneric: {"hop"}},
	}
	hop := Deployment{Name: "hop", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}
	keys := []identity.VirtualKey{{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100}}
	hopCalls := 0
	p := deploymentCapacityTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if dep.Name == "hop" {
			hopCalls++
		}
		return nil, &UpstreamHTTPError{StatusCode: 500, Body: dep.Name + " down"}
	}, []Deployment{primary, hop}, keys, nil, deploymentTPMLimiter("hop", 10, 0))

	for i := 1; i <= 5; i++ {
		_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", distinctChatRequest("gpt-4o", i), "")
		var capErr *DeploymentCapacityError
		if err == nil || errors.As(err, &capErr) {
			t.Fatalf("request %d: err = %v, want the hop's own 500 (never a capacity rejection — a failed hop must release its reservation)", i, err)
		}
	}
	if hopCalls != 5 {
		t.Errorf("hop upstream calls = %d, want 5 (the hop must be attempted on every request; a leaked reservation would have TPM-rejected it from the second request on)", hopCalls)
	}
}

// TestCallDeploymentWithCapacityCheckWithoutDeploymentTPMNeverRejects is
// the zero-config regression guard: a deployment with only an RPM
// ceiling (every config written before this feature) is never TPM-gated.
func TestCallDeploymentWithCapacityCheckWithoutDeploymentTPMNeverRejects(t *testing.T) {
	dep := Deployment{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o"}
	p := &Pipeline{
		deploymentLimiter: ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{{ID: "d1", Capacity: 1000, RefillPerSecond: 1000}}),
		logger:            discardLogger(),
		adapters:          adapter.Registry{"openai": openai.New()},
		upstream: func(ctx context.Context, d Deployment, req any) (any, error) {
			return fakeOpenAIResponse(d.UpstreamModel), nil
		},
	}
	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	for i := 1; i <= 20; i++ {
		if _, err := p.callDeploymentWithCapacityCheck(context.Background(), dep, req); err != nil {
			t.Fatalf("call %d: %v, want admitted (no deployment TPM configured)", i, err)
		}
	}
}

// TestHandleChatCompletionFallbackHopIsGatedByDeploymentTPM proves the
// gate applies to a fallback_chains hop — the path that calls
// callDeployment from inside attemptFallbackChain's closure, NOT through
// callDeploymentWithCapacityCheck — so the per-hop reservation genuinely
// covers every call to a deployment, as the config doc promised.
func TestHandleChatCompletionFallbackHopIsGatedByDeploymentTPM(t *testing.T) {
	primary := Deployment{
		Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused",
		FallbackChains: map[string][]string{FallbackClassGeneric: {"hop"}},
	}
	hop := Deployment{Name: "hop", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}
	keys := []identity.VirtualKey{{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100}}
	hopCalls := 0
	p := deploymentCapacityTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if dep.Name == "primary" {
			return nil, &UpstreamHTTPError{StatusCode: 500, Body: "primary down"}
		}
		hopCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{primary, hop}, keys, nil, deploymentTPMLimiter("hop", 10, 0))

	for i := 1; i <= 2; i++ {
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", distinctChatRequest("gpt-4o", i), ""); err != nil {
			t.Fatalf("request %d: %v, want served by the hop", i, err)
		}
	}
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", distinctChatRequest("gpt-4o", 3), "")
	assertDeploymentCapacityTPM(t, err, "hop")
	if hopCalls != 2 {
		t.Errorf("hop upstream calls = %d, want 2 (the TPM-rejected third hop attempt must never reach the upstream)", hopCalls)
	}
}

// TestHandleChatCompletionRouterFallbackIsGatedByDeploymentTPM covers the
// third per-hop path: the router-based single fallback runMissPath takes
// when NO fallback_chains are configured (nextEligibleDeployment, then
// callDeploymentWithCapacityCheck). Two same-model deployments and no
// chains: "a" always fails upstream, "b" serves and carries a 10-token
// bucket. Whichever the router's round-robin picks first, every request
// that "b" serves reaches it through a TPM-gated path — directly when the
// router picked it, via the router fallback when it picked "a" — so after
// two served requests "b" is exhausted and the third never reaches its
// upstream. The assertions are deliberately independent of the router's
// pick order (which is an implementation detail of the WRR cursor): the
// served count and the gate hold either way, and "a" is always attempted
// at least once (either as the first pick, or as the fallback after "b"
// rejects), which is what exercises the router-fallback code path.
func TestHandleChatCompletionRouterFallbackIsGatedByDeploymentTPM(t *testing.T) {
	a := Deployment{Name: "a", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}
	b := Deployment{Name: "b", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}
	keys := []identity.VirtualKey{{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100}}
	aCalls, bCalls := 0, 0
	p := deploymentCapacityTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if dep.Name == "a" {
			aCalls++
			return nil, &UpstreamHTTPError{StatusCode: 500, Body: "a down"}
		}
		bCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{a, b}, keys, nil, deploymentTPMLimiter("b", 10, 0))

	for i := 1; i <= 2; i++ {
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", distinctChatRequest("gpt-4o", i), ""); err != nil {
			t.Fatalf("request %d: %v, want served by b (directly or via the router fallback)", i, err)
		}
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", distinctChatRequest("gpt-4o", 3), ""); err == nil {
		t.Fatal("request 3 succeeded, want failure: b is TPM-exhausted and a always fails")
	}
	if bCalls != 2 {
		t.Errorf("b upstream calls = %d, want exactly 2 (the third request must be TPM-rejected before reaching b, on either routing path)", bCalls)
	}
	if aCalls < 1 {
		t.Errorf("a upstream calls = %d, want >= 1 (a is attempted as first pick or as the router fallback after b's rejection)", aCalls)
	}
}

// TestHandleChatCompletionStreamRejectsOnceDeploymentTPMExhausted is the
// streaming twin: streamDeploymentWithCapacityCheck's hop reconciles the
// deployment bucket with the streamed response's real (or estimated)
// usage, so a small bucket is exhausted after a bounded number of streams
// and the next one is rejected with Reason "tpm" before any chunk flows.
func TestHandleChatCompletionStreamRejectsOnceDeploymentTPMExhausted(t *testing.T) {
	keys := []identity.VirtualKey{{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	streams := 0
	p, err := NewPipeline(Config{
		Verifier:          verifier,
		Limiter:           ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		DeploymentLimiter: deploymentTPMLimiter("d1", 10, 0),
		Budget:            budget.NewTracker(),
		Cache:             inprocess.New(0),
		CacheL2:           inprocess.New(0),
		CacheL3:           inprocess.NewLexicalCache(0),
		Guardrails:        guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:          adapter.Registry{"openai": openai.New()},
		Router:            testRouter(deployments),
		Deployments:       deployments,
		CostCalculator:    costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream must never be called by a streaming test")
			return nil, nil
		},
		UpstreamStream: func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
			streams++
			return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	var rejected error
	for i := 1; i <= 10 && rejected == nil; i++ {
		req := distinctChatRequest("gpt-4o", i)
		req.Stream = true
		if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, httptest.NewRecorder(), ""); err != nil {
			rejected = err
		}
	}
	if rejected == nil {
		t.Fatalf("10 streams against a 10-token deployment TPM bucket were all admitted (%d streams ran), want a Reason=tpm rejection", streams)
	}
	assertDeploymentCapacityTPM(t, rejected, "d1")
	if streams == 0 {
		t.Error("no stream ever reached the upstream, want at least one admitted before exhaustion")
	}
}
