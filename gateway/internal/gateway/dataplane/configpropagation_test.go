package dataplane

// Integration tests for push-based cross-instance config propagation
// (see gateway/internal/configpropagation's own doc comment): a live
// deployment-weight mutation applied via ONE Pipeline's
// UpdateDeploymentWeight is published over a real Redis connection and,
// once a subscriber applies it via ApplyDeploymentWeightFromEvent,
// visibly changes ANOTHER Pipeline's own routing decisions -- not just
// that Publish/Subscribe succeed in isolation (configpropagation's own
// package tests already prove that).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/configpropagation"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/router"
)

// testConfigPropagationSigningSecret generates a fresh random HMAC
// secret for one test's own pub/sub pair -- configpropagation.Open now
// refuses to Publish/Subscribe without one (see
// configpropagation.MutationEvent.Signature's own doc comment); every
// Open call in a single test that must talk to EACH OTHER needs the
// SAME secret, so callers generate one and pass it to every Open in
// that test, never a package-wide shared constant.
func testConfigPropagationSigningSecret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generating test signing secret: %v", err)
	}
	return hex.EncodeToString(b)
}

// newConfigPropagationTestPipeline builds a *Pipeline with a real
// weighted router and, when publisher is non-nil, wires it as
// Config.ConfigPublisher -- deliberately its own small constructor
// (unlike newEmbeddingTestPipeline, testRouter's own helper always
// builds a weight-0-for-every-deployment router, which can't exercise a
// real weight change at all).
func newConfigPropagationTestPipeline(t *testing.T, deployments []router.Deployment, publisher configpropagation.Publisher) *Pipeline {
	t.Helper()
	depRouter := router.New(deployments, router.HealthConfig{})

	dpDeployments := make([]Deployment, 0, len(deployments))
	for _, d := range deployments {
		dpDeployments = append(dpDeployments, Deployment{Name: d.Name, Model: d.Model, Provider: "openai", UpstreamModel: d.Model, BaseURL: "http://unused"})
	}

	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
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
		Router:         depRouter,
		Deployments:    dpDeployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			return fakeOpenAIResponse("gpt-4o"), nil
		},
		ConfigPublisher: publisher,
		Logger:          discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// countStableVsCanary calls Select n times against model and counts how
// many landed on canaryName -- a plain WRR count, not sticky, so a
// weight change is observable via a real distribution shift.
func countStableVsCanary(p *Pipeline, model, canaryName string, n int) int {
	count := 0
	for i := 0; i < n; i++ {
		dep, ok := p.nextDeployment(model, nil)
		if ok && dep.Name == canaryName {
			count++
		}
	}
	return count
}

// TestIntegrationTwoPipelinesConvergeOnDeploymentWeightViaRedisPubSub is
// the real end-to-end proof: instance A's UpdateDeploymentWeight call
// changes instance A's OWN routing immediately (already proven at the
// router-package level) AND, via a real Redis pub/sub round trip,
// instance B's routing too -- without B's own Admin API ever being
// called directly.
func TestIntegrationTwoPipelinesConvergeOnDeploymentWeightViaRedisPubSub(t *testing.T) {
	ctx := context.Background()
	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Fatalf("starting test Redis container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	connStr, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("getting test Redis connection string: %v", err)
	}
	redisAddr := strings.TrimPrefix(connStr, "redis://")

	deployments := []router.Deployment{
		{Name: "stable", Model: "gpt-4o", Weight: 99},
		{Name: "canary", Model: "gpt-4o", Weight: 1},
	}

	signingSecret := testConfigPropagationSigningSecret(t)
	pubA := configpropagation.Open(redis.Options{Addr: redisAddr}, signingSecret)
	t.Cleanup(func() { _ = pubA.Close() })
	pipelineA := newConfigPropagationTestPipeline(t, deployments, pubA)

	// Instance B has NO publisher of its own (it only ever receives, in
	// this test) -- deliberately proving the propagation is one-way for
	// this call, not that B rebroadcasts A's own event.
	pipelineB := newConfigPropagationTestPipeline(t, deployments, nil)

	// B's own subscriber, applying every received event via
	// ApplyDeploymentWeightFromEvent -- mirrors cmd/gateway/main.go's
	// real subscriber closure exactly, so this proves the same code
	// path production wiring uses, not a test-only shortcut.
	subB := configpropagation.Open(redis.Options{Addr: redisAddr}, signingSecret)
	t.Cleanup(func() { _ = subB.Close() })
	subCtx, cancelSub := context.WithCancel(context.Background())
	t.Cleanup(cancelSub)
	go func() {
		_ = subB.Subscribe(subCtx, "instance-b", func(event configpropagation.MutationEvent) {
			if event.Type != configpropagation.TypeDeploymentWeight {
				return
			}
			var payload configpropagation.DeploymentWeightPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Errorf("unmarshaling received payload: %v", err)
				return
			}
			if err := pipelineB.ApplyDeploymentWeightFromEvent(payload.Model, payload.DeploymentName, payload.Weight, event.PublishedAtUnixNano); err != nil {
				t.Errorf("ApplyDeploymentWeightFromEvent: %v", err)
			}
		})
	}()
	time.Sleep(100 * time.Millisecond) // let the subscription actually register.

	// Before the mutation: both instances split ~99:1 in favor of stable.
	const samples = 1000
	beforeB := countStableVsCanary(pipelineB, "gpt-4o", "canary", samples)
	if beforeB > 50 {
		t.Fatalf("setup: pipelineB's canary count before any mutation = %d/%d, want close to 1%% (~10)", beforeB, samples)
	}

	// The mutation: instance A raises canary's weight to 50 via its real
	// Admin-facing method -- this is the exact call
	// admin.setPromptLabelHandler's sibling, updateDeploymentWeightHandler,
	// makes in production.
	if err := pipelineA.UpdateDeploymentWeight(ctx, "canary", 50); err != nil {
		t.Fatalf("UpdateDeploymentWeight on instance A: %v", err)
	}

	// Stable's own weight is unchanged (99), so the real post-mutation
	// ratio is 50/(99+50) ~= 33.6%, not 50% -- generous +/-8 percentage
	// point band for sampling noise at this sample size.
	const wantMin, wantMax = 250, 420

	// Instance A's own routing already reflects it immediately (no Redis
	// round trip needed for the instance that made the call).
	afterA := countStableVsCanary(pipelineA, "gpt-4o", "canary", samples)
	if afterA < wantMin || afterA > wantMax {
		t.Errorf("pipelineA's own canary count after weight=50 = %d/%d, want ~33.6%% (%d-%d)", afterA, samples, wantMin, wantMax)
	}

	// Instance B's routing must ALSO converge, via the real Redis pub/sub
	// round trip -- bounded wait, since delivery is asynchronous.
	deadline := time.Now().Add(5 * time.Second)
	var afterB int
	for time.Now().Before(deadline) {
		afterB = countStableVsCanary(pipelineB, "gpt-4o", "canary", samples)
		if afterB >= wantMin {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if afterB < wantMin || afterB > wantMax {
		t.Errorf("pipelineB's canary count after A's mutation = %d/%d, want ~33.6%% (%d-%d) -- cross-instance propagation did not converge within 5s", afterB, samples, wantMin, wantMax)
	}
}

// TestApplyDeploymentWeightFromEventDiscardsAnOutOfOrderStaleUpdate is
// the regression proof for a real gap this session's own end-to-end
// audit found: neither MutationEvent nor DeploymentWeightPayload carried
// any ordering token at all, so a message delivered out of order (Redis
// pub/sub only guarantees per-publisher order, never a total order
// across multiple publishers/admin calls) could silently overwrite a
// genuinely NEWER weight with a genuinely OLDER one, with zero
// detection. Deliberately a fast, in-process unit test of
// applyWeightIfNewer's own ordering logic — no real Redis needed at
// all, unlike TestIntegrationTwoPipelinesConvergeOnDeploymentWeightViaRedisPubSub
// above, which proves the real end-to-end pub/sub wiring but never
// exercised out-of-order delivery (Redis pub/sub delivers a single
// publisher's own messages in order, so that test alone could never
// have caught this).
func TestApplyDeploymentWeightFromEventDiscardsAnOutOfOrderStaleUpdate(t *testing.T) {
	deployments := []router.Deployment{
		{Name: "stable", Model: "gpt-4o", Weight: 99},
		{Name: "canary", Model: "gpt-4o", Weight: 1},
	}
	p := newConfigPropagationTestPipeline(t, deployments, nil)

	const samples = 1000
	newer := int64(2000)
	older := int64(1000)

	// A genuinely newer update (weight=50, timestamp=2000) applies.
	if err := p.ApplyDeploymentWeightFromEvent("gpt-4o", "canary", 50, newer); err != nil {
		t.Fatalf("ApplyDeploymentWeightFromEvent(weight=50, t=%d): %v", newer, err)
	}
	afterNewer := countStableVsCanary(p, "gpt-4o", "canary", samples)

	// A STALE, out-of-order update (weight=1, timestamp=1000 — earlier
	// than the 2000 already applied above) must be silently discarded,
	// never overwriting the genuinely newer weight=50 state with this
	// older one.
	if err := p.ApplyDeploymentWeightFromEvent("gpt-4o", "canary", 1, older); err != nil {
		t.Fatalf("ApplyDeploymentWeightFromEvent(weight=1, t=%d) [stale]: %v", older, err)
	}
	afterStale := countStableVsCanary(p, "gpt-4o", "canary", samples)

	// Generous +/-8 percentage point band for sampling noise, matching
	// this file's own established convention above.
	const wantMin, wantMax = 250, 420
	if afterNewer < wantMin || afterNewer > wantMax {
		t.Fatalf("setup: canary count after the genuinely newer weight=50 update = %d/%d, want ~33.6%% (%d-%d)", afterNewer, samples, wantMin, wantMax)
	}
	if afterStale < wantMin || afterStale > wantMax {
		t.Errorf("canary count after a STALE, out-of-order weight=1 update = %d/%d, want it UNCHANGED at ~33.6%% (%d-%d) -- the stale update must be discarded, not applied", afterStale, samples, wantMin, wantMax)
	}
}

// TestApplyDeploymentWeightFromEventFailedApplyNeverConsumesTheVersionSlot
// proves a narrower, easy-to-miss correctness property of
// applyWeightIfNewer: an update whose router.SetWeight call itself
// FAILS (e.g. an unknown deployment name) must NOT be recorded as the
// latest-applied version for that key — recording it regardless (or
// recording it BEFORE calling SetWeight, rather than after a confirmed
// success) would incorrectly "consume" that version slot despite no
// real state change ever happening, wrongly causing a LATER, genuinely
// valid update with an OLDER timestamp than the failed attempt to be
// rejected as stale even though it's actually the first real update
// this key has ever seen.
func TestApplyDeploymentWeightFromEventFailedApplyNeverConsumesTheVersionSlot(t *testing.T) {
	deployments := []router.Deployment{
		{Name: "stable", Model: "gpt-4o", Weight: 99},
		{Name: "canary", Model: "gpt-4o", Weight: 1},
	}
	p := newConfigPropagationTestPipeline(t, deployments, nil)

	// A high-timestamp update against an UNKNOWN deployment name --
	// router.SetWeight itself must return an error for this, and that
	// error must propagate back out.
	const highTimestamp = int64(9_000_000_000)
	if err := p.ApplyDeploymentWeightFromEvent("gpt-4o", "does-not-exist", 50, highTimestamp); err == nil {
		t.Fatal("ApplyDeploymentWeightFromEvent against an unknown deployment name returned nil error, want a real error")
	}

	// A genuinely real, valid update against "canary" -- at a LOWER
	// timestamp than the failed attempt above -- must still apply. If
	// the failed attempt above had wrongly consumed canary's own version
	// slot, this would be incorrectly rejected as "stale" relative to
	// a timestamp that was never really associated with canary's state
	// at all.
	const lowTimestamp = int64(1000)
	if err := p.ApplyDeploymentWeightFromEvent("gpt-4o", "canary", 50, lowTimestamp); err != nil {
		t.Fatalf("ApplyDeploymentWeightFromEvent(canary, weight=50, t=%d): %v", lowTimestamp, err)
	}

	const samples = 1000
	const wantMin, wantMax = 250, 420
	got := countStableVsCanary(p, "gpt-4o", "canary", samples)
	if got < wantMin || got > wantMax {
		t.Errorf("canary count after its own first real update = %d/%d, want ~33.6%% (%d-%d) -- the earlier FAILED update (against a different, unknown deployment) must never have consumed canary's own version slot", got, samples, wantMin, wantMax)
	}
}
