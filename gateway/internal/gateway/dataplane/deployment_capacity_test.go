package dataplane

// Load-bearing proofs for the per-deployment concurrency/rate-ceiling
// feature, per docs/upgrade-research/gateway-per-deployment-concurrency-
// 2026-09-09.md — a deployment-scoped ceiling on aggregate load (across
// every virtual key, and every fallback hop, that converges on one shared
// deployment), never merged into the pre-existing per-key rate-limit/
// concurrency mechanisms.

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// TestCheckDeploymentRateLimitTreatsUnconfiguredDeploymentAsUnlimited is
// the direct regression proof for the zero-capacity-TokenBucket "always
// deny" bug found and fixed in internal/ratelimit/limiter.go while
// building this feature: a deployment with NO rate_limit configured at
// all — every deployment in every config file written before this
// feature existed — must never be denied, even once deploymentLimiter is
// wired to a real, non-nil *ratelimit.KeyLimiter. Before the HasLimit
// fix, KeyLimiter.Allow's own zero-capacity/never-registered TokenBucket
// returned false for BOTH states indistinguishably, which would have
// made this exact case fail.
func TestCheckDeploymentRateLimitTreatsUnconfiguredDeploymentAsUnlimited(t *testing.T) {
	// "configured-elsewhere" proves the fix isn't merely "an empty
	// limiter always allows" — the SAME *KeyLimiter instance has a real,
	// configured, already-exhausted entry for a DIFFERENT deployment.
	limiter := ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{
		{ID: "configured-elsewhere", Capacity: 1, RefillPerSecond: 0},
	})
	if allowed, err := limiter.Allow(context.Background(), "configured-elsewhere"); err != nil || !allowed {
		t.Fatalf("setup: first Allow(configured-elsewhere) = (%v, %v), want (true, nil)", allowed, err)
	}
	if allowed, err := limiter.Allow(context.Background(), "configured-elsewhere"); err != nil || allowed {
		t.Fatalf("setup: second Allow(configured-elsewhere) = (%v, %v), want (false, nil) (capacity 1, already consumed)", allowed, err)
	}

	p := &Pipeline{deploymentLimiter: limiter, logger: discardLogger()}

	for i := 0; i < 5; i++ {
		if !p.checkDeploymentRateLimit(context.Background(), "never-configured") {
			t.Fatalf("checkDeploymentRateLimit(never-configured) call %d = false, want true — an unconfigured deployment must never be denied", i)
		}
	}
}

// TestCheckDeploymentRateLimitEnforcesConfiguredCeiling proves the
// opposite direction: a deployment that DOES have a real, configured
// rate_limit is genuinely enforced, not silently bypassed by HasLimit's
// own gate.
func TestCheckDeploymentRateLimitEnforcesConfiguredCeiling(t *testing.T) {
	limiter := ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{
		{ID: "capped", Capacity: 1, RefillPerSecond: 0},
	})
	p := &Pipeline{deploymentLimiter: limiter, logger: discardLogger()}

	if !p.checkDeploymentRateLimit(context.Background(), "capped") {
		t.Fatal("first checkDeploymentRateLimit(capped) = false, want true")
	}
	if p.checkDeploymentRateLimit(context.Background(), "capped") {
		t.Fatal("second checkDeploymentRateLimit(capped) = true, want false — capacity 1, already consumed")
	}
}

// TestCheckDeploymentConcurrencyEnforcesConfiguredCeiling is
// checkDeploymentRateLimit's sibling proof for the concurrency dimension.
func TestCheckDeploymentConcurrencyEnforcesConfiguredCeiling(t *testing.T) {
	cc := ratelimit.NewConcurrencyLimiter([]ratelimit.ConcurrencyConfig{
		{ID: "capped", MaxInFlight: 1},
	})
	p := &Pipeline{deploymentConcurrency: cc}

	if !p.checkDeploymentConcurrency("capped") {
		t.Fatal("first checkDeploymentConcurrency(capped) = false, want true")
	}
	if p.checkDeploymentConcurrency("capped") {
		t.Fatal("second checkDeploymentConcurrency(capped) = true, want false — MaxInFlight 1, slot already held")
	}
	p.releaseDeploymentConcurrency("capped")
	if !p.checkDeploymentConcurrency("capped") {
		t.Fatal("checkDeploymentConcurrency(capped) after release = false, want true")
	}
}

// TestCallDeploymentWithCapacityCheckReleasesSlotOnSuccessAndError proves
// callDeploymentWithCapacityCheck's own deferred release fires on BOTH
// the success and the error path of the wrapped callDeployment — a
// leaked slot on the error path specifically would silently and
// permanently shrink a deployment's real capacity every time its
// upstream call fails, which is exactly the condition this ceiling
// exists to protect against, not cause.
func TestCallDeploymentWithCapacityCheckReleasesSlotOnSuccessAndError(t *testing.T) {
	cc := ratelimit.NewConcurrencyLimiter([]ratelimit.ConcurrencyConfig{
		{ID: "d1", MaxInFlight: 1},
	})
	dep := Deployment{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o"}

	t.Run("success path releases", func(t *testing.T) {
		p := &Pipeline{
			deploymentConcurrency: cc,
			adapters:              adapter.Registry{"openai": openai.New()},
			upstream: func(ctx context.Context, d Deployment, req any) (any, error) {
				return fakeOpenAIResponse(d.UpstreamModel), nil
			},
		}
		if _, err := p.callDeploymentWithCapacityCheck(context.Background(), dep, adapter.ChatRequest{Model: "gpt-4o"}); err != nil {
			t.Fatalf("callDeploymentWithCapacityCheck: %v", err)
		}
		if got := cc.InFlight("d1"); got != 0 {
			t.Fatalf("InFlight(d1) after a successful call = %d, want 0 (slot released)", got)
		}
	})

	t.Run("error path still releases", func(t *testing.T) {
		p := &Pipeline{
			deploymentConcurrency: cc,
			adapters:              adapter.Registry{"openai": openai.New()},
			upstream: func(ctx context.Context, d Deployment, req any) (any, error) {
				return nil, errors.New("simulated upstream failure")
			},
		}
		if _, err := p.callDeploymentWithCapacityCheck(context.Background(), dep, adapter.ChatRequest{Model: "gpt-4o"}); err == nil {
			t.Fatal("expected the simulated upstream failure to propagate, got nil error")
		}
		if got := cc.InFlight("d1"); got != 0 {
			t.Fatalf("InFlight(d1) after a FAILED call = %d, want 0 (slot released even on error)", got)
		}
	})
}

// TestCallDeploymentWithCapacityCheckRejectsWithDeploymentCapacityError
// proves the rejection path's own error type and Reason field, for both
// the rate-limit and concurrency sub-checks — the caller-facing contract
// cmd/gateway's writeErrorResponse and classifyFallbackError both key
// off of.
func TestCallDeploymentWithCapacityCheckRejectsWithDeploymentCapacityError(t *testing.T) {
	dep := Deployment{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o"}
	neverCall := func(ctx context.Context, d Deployment, req any) (any, error) {
		t.Fatal("upstream must never be called once capacity is already rejected")
		return nil, nil
	}

	t.Run("concurrency", func(t *testing.T) {
		cc := ratelimit.NewConcurrencyLimiter([]ratelimit.ConcurrencyConfig{{ID: "d1", MaxInFlight: 1}})
		if !cc.Acquire("d1") {
			t.Fatal("setup: could not pre-acquire d1's only slot")
		}
		p := &Pipeline{deploymentConcurrency: cc, adapters: adapter.Registry{"openai": openai.New()}, upstream: neverCall}
		_, err := p.callDeploymentWithCapacityCheck(context.Background(), dep, adapter.ChatRequest{Model: "gpt-4o"})
		var capErr *DeploymentCapacityError
		if !errors.As(err, &capErr) {
			t.Fatalf("err = %v, want a *DeploymentCapacityError", err)
		}
		if capErr.Deployment != "d1" || capErr.Reason != "concurrency" {
			t.Errorf("capErr = %+v, want Deployment=d1 Reason=concurrency", capErr)
		}
	})

	t.Run("rate_limit", func(t *testing.T) {
		limiter := ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{{ID: "d1", Capacity: 1, RefillPerSecond: 0}})
		if allowed, err := limiter.Allow(context.Background(), "d1"); err != nil || !allowed {
			t.Fatalf("setup: could not pre-consume d1's only token: allowed=%v err=%v", allowed, err)
		}
		p := &Pipeline{deploymentLimiter: limiter, logger: discardLogger(), adapters: adapter.Registry{"openai": openai.New()}, upstream: neverCall}
		_, err := p.callDeploymentWithCapacityCheck(context.Background(), dep, adapter.ChatRequest{Model: "gpt-4o"})
		var capErr *DeploymentCapacityError
		if !errors.As(err, &capErr) {
			t.Fatalf("err = %v, want a *DeploymentCapacityError", err)
		}
		if capErr.Deployment != "d1" || capErr.Reason != "rate_limit" {
			t.Errorf("capErr = %+v, want Deployment=d1 Reason=rate_limit", capErr)
		}
	})
}

// TestAttemptFallbackChainSkipsCapacityConstrainedTargetsWithoutAttemptingThem
// mirrors fallback_test.go's own
// TestAttemptFallbackChainSkipsRateLimitedTargetsWithoutAttemptingThem,
// but for a REAL deploymentCapacityOK closure backed by real limiter
// state (not the alwaysAllowDeploymentCapacity stub every other test in
// fallback_test.go passes) — proving the wiring between
// attemptFallbackChain's own skip-gate and checkDeploymentCapacity is
// correct end to end, not just that the gate parameter exists.
func TestAttemptFallbackChainSkipsCapacityConstrainedTargetsWithoutAttemptingThem(t *testing.T) {
	cc := ratelimit.NewConcurrencyLimiter([]ratelimit.ConcurrencyConfig{{ID: "b", MaxInFlight: 1}})
	if !cc.Acquire("b") {
		t.Fatal("setup: could not pre-acquire b's only slot")
	}
	p := &Pipeline{
		deploymentConcurrency: cc,
		deploymentsByName: map[string]Deployment{
			"b": {Name: "b"},
			"c": {Name: "c"},
		},
	}

	var calls []string
	call := func(d Deployment) (adapter.ChatResponse, error) {
		calls = append(calls, d.Name)
		return adapter.ChatResponse{Model: "served-by-" + d.Name}, nil
	}

	start := time.Now()
	_, resp, err, attempted := p.attemptFallbackChain(context.Background(), []string{"b", "c"}, map[string]bool{"a": true}, call, func() bool { return false }, alwaysAllowRateLimit, func(depName string) bool { return p.checkDeploymentCapacity(context.Background(), depName) }, alwaysAllowCapability)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !attempted {
		t.Fatal("attempted = false, want true")
	}
	if resp.Model != "served-by-c" {
		t.Errorf("resp.Model = %q, want served-by-c", resp.Model)
	}
	if len(calls) != 1 || calls[0] != "c" {
		t.Fatalf("calls = %v, want exactly [c] — 'b' skipped as capacity-constrained, never attempted", calls)
	}
	if elapsed >= fallbackChainInterHopBackoffBase {
		t.Errorf("elapsed = %v, want well under %v — a capacity-constrained skip must never charge the inter-hop backoff delay", elapsed, fallbackChainInterHopBackoffBase)
	}
}

// deploymentCapacityTestPipeline builds a Pipeline exactly like
// newCrossModelFallbackPipeline (crossmodel_fallback_billing_test.go),
// but with the deployment-scoped DeploymentConcurrency/DeploymentLimiter
// also wired — no other helper in this package threads those two Config
// fields through.
func deploymentCapacityTestPipeline(t *testing.T, upstream UpstreamCaller, deployments []Deployment, keys []identity.VirtualKey, deploymentConcurrency *ratelimit.ConcurrencyLimiter, deploymentLimiter *ratelimit.KeyLimiter) *Pipeline {
	t.Helper()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	p, err := NewPipeline(Config{
		Verifier:              verifier,
		Limiter:               ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		DeploymentConcurrency: deploymentConcurrency,
		DeploymentLimiter:     deploymentLimiter,
		Budget:                budget.NewTracker(),
		Cache:                 inprocess.New(0),
		CacheL2:               inprocess.New(0),
		CacheL3:               inprocess.NewLexicalCache(0),
		Guardrails:            guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:              adapter.Registry{"openai": openai.New()},
		Router:                testRouter(deployments),
		Deployments:           deployments,
		CostCalculator:        costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream:              upstream,
		Logger:                discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestHandleChatCompletionTwoVirtualKeysShareDeploymentCapacityCeiling is
// the core, full-stack integration proof: two entirely different virtual
// keys, each with their own effectively-unlimited per-key rate limit and
// concurrency (so neither key's OWN controls are what's being tested),
// both routing to the SAME single deployment — capped at
// MaxInFlight: 1 — the deployment-scoped ceiling that protects the
// deployment's own aggregate load, per docs/upgrade-research/gateway-
// per-deployment-concurrency-2026-09-09.md. Key A's call is held open by
// a blocking channel; key B's concurrent request must be rejected with a
// *DeploymentCapacityError (Reason "concurrency") while key A's slot is
// held, then key A's own call completes successfully once released.
func TestHandleChatCompletionTwoVirtualKeysShareDeploymentCapacityCeiling(t *testing.T) {
	deployments := []Deployment{
		{Name: "shared", Model: "shared-model", Provider: "openai", UpstreamModel: "shared-model", BaseURL: "http://unused"},
	}
	keys := []identity.VirtualKey{
		{ID: "key-a", KeyHash: testHashOf("key-a"), RateLimitBurst: 100, RateLimitRefill: 100},
		{ID: "key-b", KeyHash: testHashOf("key-b"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	cc := ratelimit.NewConcurrencyLimiter([]ratelimit.ConcurrencyConfig{{ID: "shared", MaxInFlight: 1}})

	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once

	p := deploymentCapacityTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		startedOnce.Do(func() { close(started) })
		<-release
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, deployments, keys, cc, nil)

	var keyAResp adapter.ChatResponse
	var keyAErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		keyAResp, keyAErr = p.HandleChatCompletion(context.Background(), "Bearer key-a", adapter.ChatRequest{Model: "shared-model"})
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for key-a's call to reach the (blocking) upstream")
	}

	_, keyBErr := p.HandleChatCompletion(context.Background(), "Bearer key-b", adapter.ChatRequest{Model: "shared-model"})
	var capErr *DeploymentCapacityError
	if !errors.As(keyBErr, &capErr) {
		t.Fatalf("key-b's err = %v, want a *DeploymentCapacityError (key-a is holding the deployment's only slot)", keyBErr)
	}
	if capErr.Deployment != "shared" || capErr.Reason != "concurrency" {
		t.Errorf("capErr = %+v, want Deployment=shared Reason=concurrency", capErr)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for key-a's call to complete after release")
	}
	if keyAErr != nil {
		t.Fatalf("key-a's err = %v, want nil (its call should succeed once the slot is released)", keyAErr)
	}
	if keyAResp.Model != "shared-model" {
		t.Errorf("key-a's resp.Model = %q, want shared-model", keyAResp.Model)
	}
	if got := cc.InFlight("shared"); got != 0 {
		t.Errorf("InFlight(shared) after both requests finished = %d, want 0", got)
	}
}

// TestHandleChatCompletionCapacityConstrainedHopTriesNextHopBeforeFailing
// is the plan's own "a capacity-constrained hop tries the next configured
// hop before failing the whole request" proof — mirroring
// TestHandleChatCompletionFallbackHopSkipsRateLimitedTargetButChainStillSucceeds
// (crossmodel_fallback_billing_test.go) but for the deployment-capacity
// skip-gate instead of the per-key PerModel one: "primary" always fails,
// its generic fallback_chains names ["capacity-constrained", "backup"] in
// that order — "capacity-constrained" is pre-exhausted (its own slot held
// by a separate, blocked in-flight call) and must be skipped without ever
// being attempted, while "backup" is healthy and ultimately serves the
// response.
func TestHandleChatCompletionCapacityConstrainedHopTriesNextHopBeforeFailing(t *testing.T) {
	deployments := []Deployment{
		{
			Name: "primary", Model: "m", Provider: "openai", UpstreamModel: "m", BaseURL: "http://unused",
			FallbackChains: map[string][]string{FallbackClassGeneric: {"capacity-constrained", "backup"}},
		},
		{Name: "capacity-constrained", Model: "m2", Provider: "openai", UpstreamModel: "m2", BaseURL: "http://unused"},
		{Name: "backup", Model: "m3", Provider: "openai", UpstreamModel: "m3", BaseURL: "http://unused"},
	}
	cc := ratelimit.NewConcurrencyLimiter([]ratelimit.ConcurrencyConfig{{ID: "capacity-constrained", MaxInFlight: 1}})
	if !cc.Acquire("capacity-constrained") {
		t.Fatal("setup: could not pre-acquire capacity-constrained's only slot")
	}

	var calls []string
	p := deploymentCapacityTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		calls = append(calls, dep.Name)
		if dep.Name == "primary" {
			return nil, &UpstreamHTTPError{StatusCode: 500, Body: "primary unavailable"}
		}
		if dep.Name == "capacity-constrained" {
			t.Fatal("capacity-constrained must never be called — its own concurrency slot is already held")
		}
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, deployments, defaultTestVirtualKeys(), cc, nil)

	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("expected the chain to still succeed at backup, got error: %v", err)
	}
	if resp.Model != "m" {
		t.Errorf("resp.Model = %q, want m (echoed client-facing canonical model)", resp.Model)
	}
	if len(calls) != 2 || calls[0] != "primary" || calls[1] != "backup" {
		t.Fatalf("calls = %v, want [primary backup] — capacity-constrained skipped entirely", calls)
	}
}

// TestHandleChatCompletionStreamSkipsCapacityConstrainedHopAndReleasesSlot
// is streaming's symmetry proof for the same skip/try-next-hop behavior,
// mirroring TestHandleChatCompletionStreamMultiHopFallbackChainRoutesByClassAndHop
// (streaming_test.go), plus proving streamDeploymentWithCapacityCheck
// releases the slot it acquires for the hop that DOES run, even though
// that hop is reached only after a skip.
func TestHandleChatCompletionStreamSkipsCapacityConstrainedHopAndReleasesSlot(t *testing.T) {
	primary := Deployment{
		Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused",
		FallbackChains: map[string][]string{FallbackClassGeneric: {"capacity-constrained", "hop-2"}},
	}
	deployments := []Deployment{
		primary,
		{Name: "capacity-constrained", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "hop-2", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	cc := ratelimit.NewConcurrencyLimiter([]ratelimit.ConcurrencyConfig{{ID: "capacity-constrained", MaxInFlight: 1}})
	if !cc.Acquire("capacity-constrained") {
		t.Fatal("setup: could not pre-acquire capacity-constrained's only slot")
	}

	keys := []identity.VirtualKey{{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	var calls []string
	p, err := NewPipeline(Config{
		Verifier:              verifier,
		Limiter:               ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		DeploymentConcurrency: cc,
		Budget:                budget.NewTracker(),
		Cache:                 inprocess.New(0),
		CacheL2:               inprocess.New(0),
		CacheL3:               inprocess.NewLexicalCache(0),
		Guardrails:            guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:              adapter.Registry{"openai": openai.New()},
		Router:                testRouter(deployments),
		Deployments:           deployments,
		CostCalculator:        costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream should never be called by a streaming test")
			return nil, nil
		},
		UpstreamStream: func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
			calls = append(calls, dep.Name)
			switch dep.Name {
			case "primary":
				return nil, &UpstreamHTTPError{StatusCode: 500, Body: "primary unavailable"}
			case "capacity-constrained":
				t.Fatal("capacity-constrained must never be called — its own concurrency slot is already held")
			}
			return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	rec := httptest.NewRecorder()
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o", Stream: true}, rec); err != nil {
		t.Fatalf("expected the chain to eventually succeed at hop-2, got error: %v", err)
	}
	if len(calls) != 2 || calls[0] != "primary" || calls[1] != "hop-2" {
		t.Fatalf("calls = %v, want [primary hop-2] — capacity-constrained skipped entirely", calls)
	}
	if got := cc.InFlight("hop-2"); got != 0 {
		t.Errorf("InFlight(hop-2) after the stream completed = %d, want 0 (slot released)", got)
	}
}
