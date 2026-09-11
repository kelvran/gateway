package dataplane

// A minimal, hypothesis-driven fault-injection harness proving Kelvran's
// real resilience mechanisms (fallback chains, the per-request circuit
// breaker, inter-hop backoff) behave as designed when a fault is injected
// through the ACTUAL dataplane code path (a real HandleChatCompletion
// call against a real Pipeline) -- distinct from both:
//
//   - fallback_test.go's existing unit tests, which call
//     attemptFallbackChain directly, bypassing HandleChatCompletion,
//     Guardrails, cache, budget, and every other real pipeline stage;
//   - evals' regression_corpus_routing_chaos.json, a fixed, pre-recorded
//     set of scenarios evals scores deterministically, never a live
//     fault injected through Kelvran's own real code during a test run.
//
// Each test below follows chaos engineering's own four-step live-
// experiment methodology (Basiri et al., Netflix, 2016 -- define steady
// state, hypothesize it holds under a fault, inject the fault, compare):
// this is the staging/test-level form of that methodology (an
// AWS/Netlify/Gremlin-documented, non-production-required starting point
// per docs/upgrade-research/gateway-reliability-resilience-round4-2026-09-11.md),
// not a live-production experiment.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// faultInjector is a stateful, controllable UpstreamCaller test double.
// For each deployment name, failUntilCall[N] real calls against it fail
// with failErr before the (N+1)th succeeds -- 0 means "always succeeds,"
// never configured means "always succeeds" too (the zero value for a
// name never added to failUntilCall). Every real call is recorded, in
// order, with its own wall-clock arrival time -- the record a chaos
// experiment's own "compare" step reads back.
type faultInjector struct {
	mu            sync.Mutex
	failUntilCall map[string]int
	failErr       error
	calls         []faultInjectorCall
}

type faultInjectorCall struct {
	deployment string
	at         time.Time
}

func newFaultInjector(failErr error) *faultInjector {
	return &faultInjector{failUntilCall: map[string]int{}, failErr: failErr}
}

// failDeploymentForCalls configures name to fail its first n real calls,
// then succeed from call n+1 onward.
func (f *faultInjector) failDeploymentForCalls(name string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failUntilCall[name] = n
}

func (f *faultInjector) callCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.deployment == name {
			n++
		}
	}
	return n
}

func (f *faultInjector) callTimes(name string) []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	var times []time.Time
	for _, c := range f.calls {
		if c.deployment == name {
			times = append(times, c.at)
		}
	}
	return times
}

// caller returns the UpstreamCaller a test's Pipeline is built with. Every
// real call is recorded before the fail/succeed decision is made, so
// callCount/callTimes reflect the truth even on a call this injector fails.
func (f *faultInjector) caller() UpstreamCaller {
	return func(ctx context.Context, dep Deployment, providerReq any) (any, error) {
		f.mu.Lock()
		thisCallNum := 1
		for _, c := range f.calls {
			if c.deployment == dep.Name {
				thisCallNum++
			}
		}
		f.calls = append(f.calls, faultInjectorCall{deployment: dep.Name, at: time.Now()})
		remaining := f.failUntilCall[dep.Name]
		f.mu.Unlock()

		if thisCallNum <= remaining {
			return nil, f.failErr
		}
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}
}

// faultInjectionDeployments builds a straight-line fallback chain
// primary -> hop2 -> hop3 -> hop4 -> hop5, all serving the same canonical
// model (so a client's request never needs to know which hop actually
// served it) -- the shared steady-state topology for every experiment
// below. Four fallback targets (hop2..hop5), one more than
// maxConsecutiveChainFailures (3), deliberately: Experiment 2 needs a
// target the circuit breaker can prove it never reaches, not just one
// the chain runs out of targets to try anyway.
func faultInjectionDeployments() []Deployment {
	return []Deployment{
		{
			Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused",
			FallbackChains: map[string][]string{FallbackClassGeneric: {"hop2", "hop3", "hop4", "hop5"}},
		},
		{Name: "hop2", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "hop3", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "hop4", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "hop5", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
}

func newFaultInjectionRequest() adapter.ChatRequest {
	return adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hello"}}}
}

// TestFaultInjectionFallbackChainSurvivesAPrimaryOutage is Experiment 1.
//
// Steady state: a request against "primary" succeeds on the first hop.
// Hypothesis: if "primary" is genuinely down (every call fails), the
// request still succeeds end-to-end, served by the next healthy hop in
// its own fallback_chains -- Kelvran's real fallback mechanism, not a
// pre-recorded expectation of it.
// Inject: primary fails every call, indefinitely.
// Compare: HandleChatCompletion still returns success, and "hop2" (the
// first fallback target) is the one that actually served it.
func TestFaultInjectionFallbackChainSurvivesAPrimaryOutage(t *testing.T) {
	injector := newFaultInjector(&UpstreamHTTPError{StatusCode: 503, Body: "upstream down"})
	injector.failDeploymentForCalls("primary", 1_000_000) // "genuinely down," not a transient blip

	p := newTestPipeline(t, injector.caller(), faultInjectionDeployments())
	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", newFaultInjectionRequest())
	if err != nil {
		t.Fatalf("HandleChatCompletion: %v, want success via fallback", err)
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("resp.Model = %q, want %q", resp.Model, "gpt-4o")
	}
	if got := injector.callCount("primary"); got != 1 {
		t.Errorf("primary call count = %d, want 1 (tried once, then abandoned for the chain)", got)
	}
	if got := injector.callCount("hop2"); got != 1 {
		t.Errorf("hop2 call count = %d, want 1 (the fallback that actually served the request)", got)
	}
	if got := injector.callCount("hop3"); got != 0 {
		t.Errorf("hop3 call count = %d, want 0 (never reached -- hop2 already succeeded)", got)
	}
}

// TestFaultInjectionCircuitBreakerStopsAtMaxConsecutiveFailures is
// Experiment 2.
//
// Steady state: a chain with more fallback targets than it needs tries
// only as many hops as it needs to succeed.
// Hypothesis: if EVERY hop in the chain is down, the per-request circuit
// breaker (maxConsecutiveChainFailures = 3, fallback.go) stops the WALK
// after exactly 3 real attempts WITHIN attemptFallbackChain itself --
// never trying a 4th chain target ("hop5") it has budget left in the
// chain's own target list to reach, and never silently retrying forever.
// primary's own very first, pre-chain attempt is a separate call, not
// counted by this breaker at all (attemptFallbackChain's own
// consecutiveFailures counter starts at 0 on every call) -- so the real,
// total call count across the whole request is 1 (primary) + 3 (the
// breaker's own ceiling, spent on hop2/hop3/hop4) = 4, with hop5 the
// specific, provable "never reached" proof.
// Inject: primary, hop2, hop3, hop4, and hop5 all fail every call.
// Compare: exactly 4 real upstream calls happen in total, hop5 is never
// called at all, and HandleChatCompletion returns a real, non-nil error
// -- the failure is surfaced, never silently swallowed into a fake
// success.
func TestFaultInjectionCircuitBreakerStopsAtMaxConsecutiveFailures(t *testing.T) {
	injector := newFaultInjector(&UpstreamHTTPError{StatusCode: 503, Body: "upstream down"})
	for _, name := range []string{"primary", "hop2", "hop3", "hop4", "hop5"} {
		injector.failDeploymentForCalls(name, 1_000_000)
	}

	p := newTestPipeline(t, injector.caller(), faultInjectionDeployments())
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", newFaultInjectionRequest())
	if err == nil {
		t.Fatal("HandleChatCompletion: err = nil, want a real error -- every hop in the chain is down")
	}

	totalCalls := injector.callCount("primary") + injector.callCount("hop2") +
		injector.callCount("hop3") + injector.callCount("hop4") + injector.callCount("hop5")
	wantTotal := 1 + maxConsecutiveChainFailures
	if totalCalls != wantTotal {
		t.Errorf("total real upstream calls = %d, want %d (primary's own first attempt + the "+
			"circuit breaker's own ceiling spent on the chain) -- either it tried fewer (gave up "+
			"too early) or more (the breaker didn't trip)", totalCalls, wantTotal)
	}
	if got := injector.callCount("hop5"); got != 0 {
		t.Errorf("hop5 call count = %d, want 0 -- the breaker must trip before a 4th real chain attempt is ever made", got)
	}
}

// TestFaultInjectionInsertsRealMeasurableBackoffBeforeSecondChainHop is
// Experiment 3.
//
// Steady state: the FIRST real attempt WITHIN a fallback chain walk
// (attemptFallbackChain's own first successfully-health/rate/capacity-
// checked candidate) incurs no artificial delay of its own.
// Hypothesis: once a fault forces a SECOND real attempt within that same
// chain walk, Kelvran's real inter-hop backoff
// (fallbackChainInterHopBackoffBase, fallback.go) inserts a genuine,
// measurable delay before that second call actually happens -- not just
// a documented intention, a real elapsed-wall-clock proof through the
// live pipeline. Note this is deliberately NOT "primary retries itself":
// runMissPath pre-seeds attemptFallbackChain's own tried map with the
// original deployment's name (dataplane.go's `tried :=
// map[string]bool{dep.Name: true}`), so a chain that lists its own
// origin as a target would be silently skipped, never actually
// retried -- confirmed by first attempting exactly that design and
// finding it produces zero real second attempts, before landing on the
// two-DISTINCT-targets design below.
// Inject: "primary" (the request's first-ever attempt, outside the
// chain walk) fails once; its own chain then walks hop2 (fails, the
// chain's own first real attempt) then hop3 (succeeds, the chain's own
// second real attempt -- the one backoff applies before).
// Compare: the measured gap between hop2's and hop3's own call
// timestamps is at least half of fallbackChainInterHopBackoffBase -- a
// generous lower bound (not an exact-millisecond match) chosen
// specifically to avoid CI-timing flakiness while still failing
// decisively if the real delay were removed entirely (which would
// produce a gap close to zero). hop4 is never reached at all, a bonus
// proof that the chain stopped as soon as hop3 succeeded.
func TestFaultInjectionInsertsRealMeasurableBackoffBeforeSecondChainHop(t *testing.T) {
	injector := newFaultInjector(&UpstreamHTTPError{StatusCode: 503, Body: "transient"})
	injector.failDeploymentForCalls("primary", 1_000_000)
	injector.failDeploymentForCalls("hop2", 1_000_000)
	// hop3 left unconfigured -- succeeds immediately.

	p := newTestPipeline(t, injector.caller(), faultInjectionDeployments())
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", newFaultInjectionRequest())
	if err != nil {
		t.Fatalf("HandleChatCompletion: %v, want success via hop3", err)
	}

	hop2Times := injector.callTimes("hop2")
	hop3Times := injector.callTimes("hop3")
	if len(hop2Times) != 1 || len(hop3Times) != 1 {
		t.Fatalf("hop2 calls = %d, hop3 calls = %d, want 1 and 1", len(hop2Times), len(hop3Times))
	}
	if got := injector.callCount("hop4"); got != 0 {
		t.Errorf("hop4 call count = %d, want 0 -- the chain must stop once hop3 succeeds", got)
	}

	gap := hop3Times[0].Sub(hop2Times[0])
	minExpected := fallbackChainInterHopBackoffBase / 2
	if gap < minExpected {
		t.Errorf("gap between hop2's and hop3's real calls = %v, want >= %v (real backoff, not a no-op)", gap, minExpected)
	}
}
