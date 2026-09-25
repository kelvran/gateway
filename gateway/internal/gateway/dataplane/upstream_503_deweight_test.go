package dataplane

// Tests for wrapUpstreamCallerFor503Deweight/
// wrapUpstreamStreamCallerFor503Deweight/reportUpstream503Deweight, per
// docs/upgrade-research/self-hosted-inference-serving-landscape-2026-09-25.md
// Finding 1 -- see dataplane.go's own doc comment on those functions for
// the full design rationale. Mirrors latency_deweight_wiring_test.go's
// own end-to-end rigor exactly: a real (mocked) upstream, the real
// wrapped Config.Upstream closure NewPipeline builds, and the real
// router.Select path (via nextDeployment), never a direct/bypassing call
// into router.SetLatencyFactor itself (that's internal/router's own
// latency_deweight_test.go job).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// upstreamReturningStatusFor is a two-deployment UpstreamCaller fixture:
// every call routed to targetDeployment fails with an *UpstreamHTTPError
// at the given statusCode; every other deployment always succeeds.
func upstreamReturningStatusFor(targetDeployment string, statusCode int) UpstreamCaller {
	return func(_ context.Context, dep Deployment, _ any) (any, error) {
		if dep.Name == targetDeployment {
			return nil, &UpstreamHTTPError{StatusCode: statusCode, Body: "simulated"}
		}
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}
}

// probeChatRequest is the minimal adapter.ChatRequest callDeployment
// needs for these tests -- content is irrelevant, since
// upstreamReturningStatusFor never inspects it.
func probeChatRequest() adapter.ChatRequest {
	return adapter.ChatRequest{Messages: []adapter.Message{{Role: "user", Content: "ping"}}}
}

// selectCounts drives n real nextDeployment calls (the same real
// router.Select path HandleChatCompletion's own routing decision uses)
// and returns how many times each deployment name was chosen.
func selectCounts(t *testing.T, p *Pipeline, model string, n int) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		dep, ok := p.nextDeployment(model, nil)
		if !ok {
			t.Fatalf("call %d: nextDeployment returned ok=false, want true", i)
		}
		counts[dep.Name]++
	}
	return counts
}

// TestReal503UpstreamCallDeweightsFutureRouting is the headline proof
// required by this task: a REAL (non-probe) upstream call that fails
// with a 503 must measurably reduce the deployment's share of FUTURE
// routing decisions -- mirroring internal/router/latency_deweight_test.go's
// TestSelectNeverFullyStarvesALatencyDeweightedDeployment exactly (same
// percent=1 -> clamped to latencyFactorFloorPercent, same two-equal-
// weight-deployment setup, same 600-call exact-count rigor), since that
// is precisely what reportUpstream503Deweight's SetLatencyFactor(name, 1)
// call produces here too, just reached via one real failed request
// instead of a direct router-level call.
func TestReal503UpstreamCallDeweightsFutureRouting(t *testing.T) {
	deployments := twoDeploymentsSameModel() // "good", "bad"
	p := newTestPipeline(t, upstreamReturningStatusFor("bad", http.StatusServiceUnavailable), deployments)
	ctx := context.Background()

	badDep, ok := p.deploymentsByName["bad"]
	if !ok {
		t.Fatal("setup: \"bad\" deployment not found")
	}

	// One real, non-probe upstream call that fails with a 503.
	if _, err := p.callDeployment(ctx, badDep, probeChatRequest()); err == nil {
		t.Fatal("expected callDeployment to return an error for the simulated 503 response")
	}

	const totalCalls = 600
	counts := selectCounts(t, p, "gpt-4o", totalCalls)

	if got := counts["bad"]; got != 54 {
		t.Errorf(`counts["bad"] = %d, want 54 (600 calls, de-weighted to latencyFactorFloorPercent by one real 503) -- a dramatically reduced but never-fully-starved share`, got)
	}
	if got := counts["good"]; got != 546 {
		t.Errorf(`counts["good"] = %d, want 546`, got)
	}
}

// TestReal500UpstreamCallDoesNotTriggerDeweight is the load-bearing
// proof that the new check is genuinely NARROW to 503, not "any 5xx":
// an otherwise-identical real upstream failure at 500 must leave FUTURE
// routing completely unaffected -- the deployment keeps its full,
// equal, undeweighted share, byte-for-byte like before this feature
// existed (mirroring
// internal/router/latency_deweight_test.go's own
// TestSelectUnaffectedByLatencyFactorWhenNeverSet).
func TestReal500UpstreamCallDoesNotTriggerDeweight(t *testing.T) {
	deployments := twoDeploymentsSameModel() // "good", "bad"
	p := newTestPipeline(t, upstreamReturningStatusFor("bad", http.StatusInternalServerError), deployments)
	ctx := context.Background()

	badDep, ok := p.deploymentsByName["bad"]
	if !ok {
		t.Fatal("setup: \"bad\" deployment not found")
	}

	if _, err := p.callDeployment(ctx, badDep, probeChatRequest()); err == nil {
		t.Fatal("expected callDeployment to return an error for the simulated 500 response")
	}

	const totalCalls = 600
	counts := selectCounts(t, p, "gpt-4o", totalCalls)

	if got := counts["bad"]; got != 300 {
		t.Errorf(`counts["bad"] = %d, want 300 (equal 50%% share -- a 500 must never trigger de-weighting, only 503 does)`, got)
	}
	if got := counts["good"]; got != 300 {
		t.Errorf(`counts["good"] = %d, want 300`, got)
	}
}

// TestProbeUpstream503DoesNotTriggerDeweight is the required proof that
// probe traffic (probeOneDeployment) never reaches
// reportUpstream503Deweight's own de-weighting side effect --
// wrapUpstreamCallerFor503Deweight's own doc comment scopes this feature
// to REAL (non-probe) upstream calls ONLY, but probeOneDeployment routes
// through the identical wrapped p.upstream closure real traffic uses
// (via p.callDeployment); withProbeContext/isProbeContext is what
// actually enforces that documented scoping. Uses a SINGLE probe --
// never enough on its own to trip UnhealthyThreshold's default-3
// consecutive-failure floor (router.HealthConfig's own doc comment) --
// so this test isolates the de-weighting side effect specifically, never
// conflating it with health-exclusion.
func TestProbeUpstream503DoesNotTriggerDeweight(t *testing.T) {
	deployments := twoDeploymentsSameModel() // "good", "bad"
	p := newTestPipeline(t, upstreamReturningStatusFor("bad", http.StatusServiceUnavailable), deployments)
	ctx := context.Background()

	badDep, ok := p.deploymentsByName["bad"]
	if !ok {
		t.Fatal(`setup: "bad" deployment not found`)
	}

	// One probe call -- not real traffic -- that fails with a 503.
	p.probeOneDeployment(ctx, badDep)

	const totalCalls = 600
	counts := selectCounts(t, p, "gpt-4o", totalCalls)

	if got := counts["bad"]; got != 300 {
		t.Errorf(`counts["bad"] = %d, want 300 (equal 50%% share -- a probe's own 503 must never de-weight future routing, only a REAL call's 503 does)`, got)
	}
	if got := counts["good"]; got != 300 {
		t.Errorf(`counts["good"] = %d, want 300`, got)
	}
}

// TestReal503UpstreamCallLeavesCurrentRequestErrorAndFallbackUnchanged
// is the required no-regression proof: the CURRENT request's own
// *UpstreamHTTPError (StatusCode, Body, Error() string) and its
// classifyFallbackError bucket for a 503 must be byte-for-byte identical
// to what they were before this change -- reportUpstream503Deweight is
// purely an additional side effect, never a rewrite of the returned
// error. classifyFallbackError itself is untouched by this change; this
// pins its 503 behavior down permanently as a regression guard for the
// exact status code this feature newly cares about.
func TestReal503UpstreamCallLeavesCurrentRequestErrorAndFallbackUnchanged(t *testing.T) {
	deployments := twoDeploymentsSameModel() // "good", "bad"
	p := newTestPipeline(t, upstreamReturningStatusFor("bad", http.StatusServiceUnavailable), deployments)
	ctx := context.Background()

	badDep, ok := p.deploymentsByName["bad"]
	if !ok {
		t.Fatal("setup: \"bad\" deployment not found")
	}

	_, err := p.callDeployment(ctx, badDep, probeChatRequest())
	if err == nil {
		t.Fatal("expected callDeployment to return an error for the simulated 503 response")
	}

	var httpErr *UpstreamHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("errors.As failed to find *UpstreamHTTPError in %v", err)
	}
	if httpErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("httpErr.StatusCode = %d, want %d", httpErr.StatusCode, http.StatusServiceUnavailable)
	}
	if httpErr.Body != "simulated" {
		t.Errorf("httpErr.Body = %q, want %q", httpErr.Body, "simulated")
	}
	if want := "upstream returned status 503: simulated"; httpErr.Error() != want {
		t.Errorf("httpErr.Error() = %q, want %q", httpErr.Error(), want)
	}
	if got := classifyFallbackError(err); got != FallbackClassGeneric {
		t.Errorf("classifyFallbackError(503) = %q, want %q -- unchanged by this feature", got, FallbackClassGeneric)
	}

	// isCandidateHealthFailure (fallback.go, untouched by this change) is
	// NOT wired to any real call site today -- per its own doc comment,
	// "Deliberately NOT WIRED to any real call site or to Router.
	// ReportProbeResult anywhere in this codebase" -- confirmed via grep,
	// its only callers are its own unit test and this one. Pinning its
	// pure-function output for a 503 here is purely a regression guard
	// for that (currently dead) classifier's own behavior, proving this
	// feature's narrow 503 check would compose correctly with it WHENEVER
	// it is eventually wired up -- not proof of anything about a
	// currently-live traffic path.
	if !isCandidateHealthFailure(err) {
		t.Error("isCandidateHealthFailure(503) = false, want true -- unchanged by this feature")
	}
}

// TestHandleChatCompletionRoutesAroundReal503ViaConfiguredFallbackChain
// is a second, full-pipeline angle on the same no-regression
// requirement, mirroring
// fallback_chain_integration_test.go's own
// TestHandleChatCompletionRoutesEachErrorClassToItsOwnFallbackList
// pattern exactly (same shape, same assertions), just with a real 503
// substituted for that test's 400-with-keyword-body cases: a
// fallback_chains-configured deployment that fails with a 503 must
// still fall back to its OWN configured generic target and succeed,
// exactly like any other FallbackClassGeneric-classified error already
// does -- proving the new de-weighting side effect never changes WHICH
// deployment serves the request or WHETHER the request succeeds.
func TestHandleChatCompletionRoutesAroundReal503ViaConfiguredFallbackChain(t *testing.T) {
	primary := Deployment{
		Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused",
		FallbackChains: map[string][]string{
			FallbackClassGeneric: {"generic-alt"},
		},
	}
	deployments := []Deployment{
		primary,
		{Name: "generic-alt", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}

	var calls []string
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		calls = append(calls, dep.Name)
		if dep.Name == "primary" {
			return nil, &UpstreamHTTPError{StatusCode: http.StatusServiceUnavailable, Body: "simulated"}
		}
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, deployments)

	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{Model: "gpt-4o"}, "")
	if err != nil {
		t.Fatalf("expected the configured fallback to succeed on a real 503, got error: %v", err)
	}
	if len(calls) != 2 || calls[0] != "primary" || calls[1] != "generic-alt" {
		t.Fatalf("calls = %v, want [primary generic-alt]", calls)
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("resp.Model = %q, want %q (echoed canonical model)", resp.Model, "gpt-4o")
	}
}

// upstreamStreamReturningStatusFor mirrors upstreamReturningStatusFor,
// one level over, for UpstreamStreamCaller -- every call routed to
// targetDeployment fails with an *UpstreamHTTPError at the given
// statusCode; every other deployment always succeeds with a real,
// minimal SSE stream (realOpenAISSEStream, streaming_test.go).
func upstreamStreamReturningStatusFor(targetDeployment string, statusCode int) UpstreamStreamCaller {
	return func(_ context.Context, dep Deployment, _ any) (io.ReadCloser, error) {
		if dep.Name == targetDeployment {
			return nil, &UpstreamHTTPError{StatusCode: statusCode, Body: "simulated"}
		}
		return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
	}
}

// newStreamDeploymentTestArgs builds the minimal, real streaming.Writer/
// firstChunkSent/blocked argument set streamDeployment needs beyond
// (ctx, dep, req) -- msr is left at its zero value, safe here because
// every case below returns from streamDeployment before its own
// checkMidStreamReservationTopup call is ever reached (p.upstreamStream
// itself already failed).
func newStreamDeploymentTestArgs(t *testing.T) (*streaming.Writer, *bool, *bool) {
	t.Helper()
	sw, err := streaming.NewWriter(httptest.NewRecorder())
	if err != nil {
		t.Fatalf("streaming.NewWriter: %v", err)
	}
	return sw, new(bool), new(bool)
}

// TestReal503StreamingUpstreamCallDeweightsFutureRouting is
// TestReal503UpstreamCallDeweightsFutureRouting's streaming-path
// sibling -- the required regression proof for
// wrapUpstreamStreamCallerFor503Deweight specifically (dataplane.go),
// which had ZERO test coverage before this: same two-equal-weight-
// deployment setup, same percent=1 -> latencyFactorFloorPercent clamp,
// same 600-call exact-count rigor, reached via p.streamDeployment (the
// real per-deployment call streaming.go's HandleChatCompletionStream
// itself drives) instead of p.callDeployment.
func TestReal503StreamingUpstreamCallDeweightsFutureRouting(t *testing.T) {
	deployments := twoDeploymentsSameModel() // "good", "bad"
	p := newStreamingTestPipeline(t, upstreamStreamReturningStatusFor("bad", http.StatusServiceUnavailable), deployments, adapter.Registry{"openai": openai.New()})
	ctx := context.Background()

	badDep, ok := p.deploymentsByName["bad"]
	if !ok {
		t.Fatal(`setup: "bad" deployment not found`)
	}

	sw, firstChunkSent, blocked := newStreamDeploymentTestArgs(t)
	if _, _, err := p.streamDeployment(ctx, badDep, probeChatRequest(), sw, firstChunkSent, "test-key", midStreamReservation{}, blocked); err == nil {
		t.Fatal("expected streamDeployment to return an error for the simulated 503 response")
	}

	const totalCalls = 600
	counts := selectCounts(t, p, "gpt-4o", totalCalls)

	if got := counts["bad"]; got != 54 {
		t.Errorf(`counts["bad"] = %d, want 54 (600 calls, de-weighted to latencyFactorFloorPercent by one real streaming 503) -- a dramatically reduced but never-fully-starved share`, got)
	}
	if got := counts["good"]; got != 546 {
		t.Errorf(`counts["good"] = %d, want 546`, got)
	}
}

// TestReal500StreamingUpstreamCallDoesNotTriggerDeweight is
// TestReal500UpstreamCallDoesNotTriggerDeweight's streaming-path
// sibling: an otherwise-identical real streaming upstream failure at 500
// must leave future routing completely unaffected, proving
// wrapUpstreamStreamCallerFor503Deweight's own narrow 503-only check
// (never widened to every 5xx) reaches the streaming path too, not just
// the buffered one.
func TestReal500StreamingUpstreamCallDoesNotTriggerDeweight(t *testing.T) {
	deployments := twoDeploymentsSameModel() // "good", "bad"
	p := newStreamingTestPipeline(t, upstreamStreamReturningStatusFor("bad", http.StatusInternalServerError), deployments, adapter.Registry{"openai": openai.New()})
	ctx := context.Background()

	badDep, ok := p.deploymentsByName["bad"]
	if !ok {
		t.Fatal(`setup: "bad" deployment not found`)
	}

	sw, firstChunkSent, blocked := newStreamDeploymentTestArgs(t)
	if _, _, err := p.streamDeployment(ctx, badDep, probeChatRequest(), sw, firstChunkSent, "test-key", midStreamReservation{}, blocked); err == nil {
		t.Fatal("expected streamDeployment to return an error for the simulated 500 response")
	}

	const totalCalls = 600
	counts := selectCounts(t, p, "gpt-4o", totalCalls)

	if got := counts["bad"]; got != 300 {
		t.Errorf(`counts["bad"] = %d, want 300 (equal 50%% share -- a 500 must never trigger de-weighting, only 503 does)`, got)
	}
	if got := counts["good"]; got != 300 {
		t.Errorf(`counts["good"] = %d, want 300`, got)
	}
}

// TestReal503EmbeddingUpstreamCallDeweightsFutureRouting is
// TestReal503UpstreamCallDeweightsFutureRouting's embedding-path
// sibling -- the required regression proof for NewPipeline's own
// embeddingUpstream wiring line specifically (dataplane.go's
// "embeddingUpstream: wrapUpstreamCallerFor503Deweight(cfg.
// EmbeddingUpstream, cfg.Router)"), which had no dedicated test:
// wrapUpstreamCallerFor503Deweight itself is already covered via the
// chat path above, but that alone doesn't prove THIS wiring line
// actually applies the wrap rather than, say, assigning cfg.
// EmbeddingUpstream bare or wrapping the wrong field -- reached via
// p.embeddingUpstream directly, the same real closure HandleEmbeddings
// itself calls.
func TestReal503EmbeddingUpstreamCallDeweightsFutureRouting(t *testing.T) {
	deployments := []Deployment{
		{Name: "good", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"},
		{Name: "bad", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"},
	}
	p := newEmbeddingTestPipeline(t, upstreamReturningStatusFor("bad", http.StatusServiceUnavailable), deployments, nil)
	ctx := context.Background()

	badDep, ok := p.deploymentsByName["bad"]
	if !ok {
		t.Fatal(`setup: "bad" deployment not found`)
	}

	if _, err := p.embeddingUpstream(ctx, badDep, nil); err == nil {
		t.Fatal("expected p.embeddingUpstream to return an error for the simulated 503 response")
	}

	const totalCalls = 600
	counts := selectCounts(t, p, "text-embedding-3-small", totalCalls)

	if got := counts["bad"]; got != 54 {
		t.Errorf(`counts["bad"] = %d, want 54 (600 calls, de-weighted to latencyFactorFloorPercent by one real embedding-path 503)`, got)
	}
	if got := counts["good"]; got != 546 {
		t.Errorf(`counts["good"] = %d, want 546`, got)
	}
}
