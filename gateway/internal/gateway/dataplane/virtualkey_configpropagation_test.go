package dataplane

// Tests for push-based cross-instance propagation of live virtual-key
// mutations (Upsert/Delete/Rotate) — the gap found necessary while
// migrating identity's own hot state to Redis (see
// internal/identity/redisstore's own doc comment): identity.Store only
// ever answers "what does a freshly (re)started replica load," never
// "how does an already-running replica learn about another instance's
// live admin mutation." Mirrors configpropagation_test.go's own
// established test shapes for the identical deployment-weight gap
// exactly — real Redis pub/sub integration tests for the end-to-end
// proof, fast in-process unit tests for the ordering/edge-case logic.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
)

// newVirtualKeyPropagationTestPipeline mirrors
// newConfigPropagationTestPipeline's exact shape (configpropagation_test.go)
// but takes the initial virtual-key set as a parameter — Delete-based
// tests need at least 2 keys configured (identity.NewVerifier refuses to
// go to zero keys), unlike that helper's fixed single "test-key".
func newVirtualKeyPropagationTestPipeline(t *testing.T, keys []identity.VirtualKey, publisher configpropagation.Publisher) *Pipeline {
	t.Helper()
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
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
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			return fakeOpenAIResponse(dep.UpstreamModel), nil
		},
		ConfigPublisher: publisher,
		Logger:          discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// canAuthenticate reports whether secret currently resolves to a valid
// virtual key against p — a real HandleChatCompletion call, not a
// Verifier-internal peek, so this proves the full live-request path,
// not just Verifier state in isolation.
func canAuthenticate(p *Pipeline, secret string) bool {
	_, err := p.HandleChatCompletion(context.Background(), "Bearer "+secret, adapter.ChatRequest{Model: "gpt-4o"}, "")
	return err == nil
}

// subscribeVirtualKeyEvents mirrors cmd/gateway/main.go's real
// subscriber closure for TypeVirtualKeyUpsert/TypeVirtualKeyDelete
// exactly, applying every received event to target — so these tests
// prove the same code path production wiring uses, not a test-only
// shortcut (mirrors configpropagation_test.go's own identical
// rationale for TypeDeploymentWeight).
func subscribeVirtualKeyEvents(ctx context.Context, sub *configpropagation.PubSub, target *Pipeline, onErr func(error)) {
	_ = sub.Subscribe(ctx, func(event configpropagation.MutationEvent) {
		switch event.Type {
		case configpropagation.TypeVirtualKeyUpsert:
			var payload configpropagation.VirtualKeyUpsertPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				onErr(err)
				return
			}
			if err := target.ApplyVirtualKeyUpsertFromEvent(payload, event.PublishedAtUnixNano); err != nil {
				onErr(err)
			}
		case configpropagation.TypeVirtualKeyDelete:
			var payload configpropagation.VirtualKeyDeletePayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				onErr(err)
				return
			}
			if err := target.ApplyVirtualKeyDeleteFromEvent(payload.ID, event.PublishedAtUnixNano); err != nil {
				onErr(err)
			}
		}
	})
}

func openTestRedis(t *testing.T) string {
	t.Helper()
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
	return strings.TrimPrefix(connStr, "redis://")
}

// TestIntegrationTwoPipelinesConvergeOnVirtualKeyUpsertViaRedisPubSub is
// the real end-to-end proof for the upsert side: instance A's
// UpsertVirtualKey call — a brand-new key, never present on either
// instance — makes that key authenticate on instance B too, via a real
// Redis pub/sub round trip, without B's own Admin API ever being called
// directly.
func TestIntegrationTwoPipelinesConvergeOnVirtualKeyUpsertViaRedisPubSub(t *testing.T) {
	redisAddr := openTestRedis(t)
	keys := []identity.VirtualKey{{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100}}

	pubA := configpropagation.Open(redisAddr)
	t.Cleanup(func() { _ = pubA.Close() })
	pipelineA := newVirtualKeyPropagationTestPipeline(t, keys, pubA)
	pipelineB := newVirtualKeyPropagationTestPipeline(t, keys, nil)

	subB := configpropagation.Open(redisAddr)
	t.Cleanup(func() { _ = subB.Close() })
	subCtx, cancelSub := context.WithCancel(context.Background())
	t.Cleanup(cancelSub)
	go subscribeVirtualKeyEvents(subCtx, subB, pipelineB, func(err error) { t.Errorf("subscriber apply error: %v", err) })
	time.Sleep(100 * time.Millisecond) // let the subscription actually register.

	if canAuthenticate(pipelineB, "brand-new-secret") {
		t.Fatal("setup: pipelineB already authenticates a key that was never added")
	}

	if err := pipelineA.UpsertVirtualKey(
		identity.VirtualKey{ID: "team-gamma", KeyHash: testHashOf("brand-new-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		ratelimit.KeyConfig{ID: "team-gamma", Capacity: 100, RefillPerSecond: 100},
	); err != nil {
		t.Fatalf("UpsertVirtualKey on instance A: %v", err)
	}

	if !canAuthenticate(pipelineA, "brand-new-secret") {
		t.Fatal("pipelineA's own authentication did not reflect its own UpsertVirtualKey immediately")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if canAuthenticate(pipelineB, "brand-new-secret") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("pipelineB never converged to accept A's newly-upserted key within 5s -- cross-instance propagation did not converge")
}

// TestIntegrationTwoPipelinesConvergeOnVirtualKeyDeleteViaRedisPubSub is
// the identical proof for the delete side: instance A's DeleteVirtualKey
// call revokes access on instance B too, via the real Redis round trip
// — the security-critical direction, since a stale replica that never
// learns about a revocation would keep authenticating a credential its
// own operator believes is already dead.
func TestIntegrationTwoPipelinesConvergeOnVirtualKeyDeleteViaRedisPubSub(t *testing.T) {
	redisAddr := openTestRedis(t)
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
		{ID: "team-doomed", KeyHash: testHashOf("doomed-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}

	pubA := configpropagation.Open(redisAddr)
	t.Cleanup(func() { _ = pubA.Close() })
	pipelineA := newVirtualKeyPropagationTestPipeline(t, keys, pubA)
	pipelineB := newVirtualKeyPropagationTestPipeline(t, keys, nil)

	subB := configpropagation.Open(redisAddr)
	t.Cleanup(func() { _ = subB.Close() })
	subCtx, cancelSub := context.WithCancel(context.Background())
	t.Cleanup(cancelSub)
	go subscribeVirtualKeyEvents(subCtx, subB, pipelineB, func(err error) { t.Errorf("subscriber apply error: %v", err) })
	time.Sleep(100 * time.Millisecond)

	if !canAuthenticate(pipelineB, "doomed-secret") {
		t.Fatal("setup: pipelineB does not yet authenticate the key that is about to be deleted")
	}

	if err := pipelineA.DeleteVirtualKey("team-doomed"); err != nil {
		t.Fatalf("DeleteVirtualKey on instance A: %v", err)
	}
	if canAuthenticate(pipelineA, "doomed-secret") {
		t.Fatal("pipelineA's own authentication did not reflect its own DeleteVirtualKey immediately")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !canAuthenticate(pipelineB, "doomed-secret") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("pipelineB never converged to revoke A's deleted key within 5s -- a stale replica would keep authenticating a revoked credential")
}

// TestIntegrationTwoPipelinesConvergeOnVirtualKeyRotateViaRedisPubSub
// proves RotateVirtualKey's own propagation, via the SAME
// TypeVirtualKeyUpsert event Upsert uses (see that constant's own doc
// comment) — instance B must accept the NEW secret after A's rotation,
// via the real Redis round trip.
func TestIntegrationTwoPipelinesConvergeOnVirtualKeyRotateViaRedisPubSub(t *testing.T) {
	redisAddr := openTestRedis(t)
	keys := []identity.VirtualKey{
		{ID: "team-rotating", KeyHash: testHashOf("old-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}

	pubA := configpropagation.Open(redisAddr)
	t.Cleanup(func() { _ = pubA.Close() })
	pipelineA := newVirtualKeyPropagationTestPipeline(t, keys, pubA)
	pipelineB := newVirtualKeyPropagationTestPipeline(t, keys, nil)

	subB := configpropagation.Open(redisAddr)
	t.Cleanup(func() { _ = subB.Close() })
	subCtx, cancelSub := context.WithCancel(context.Background())
	t.Cleanup(cancelSub)
	go subscribeVirtualKeyEvents(subCtx, subB, pipelineB, func(err error) { t.Errorf("subscriber apply error: %v", err) })
	time.Sleep(100 * time.Millisecond)

	if canAuthenticate(pipelineB, "new-secret") {
		t.Fatal("setup: pipelineB already authenticates the not-yet-issued new secret")
	}

	if err := pipelineA.RotateVirtualKey("team-rotating", testHashOf("new-secret"), time.Hour); err != nil {
		t.Fatalf("RotateVirtualKey on instance A: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if canAuthenticate(pipelineB, "new-secret") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("pipelineB never converged to accept A's rotated key's new secret within 5s")
}

// TestApplyVirtualKeyUpsertFromEventDiscardsAnOutOfOrderStaleUpdate
// mirrors TestApplyDeploymentWeightFromEventDiscardsAnOutOfOrderStaleUpdate's
// identical ordering-regression shape, for the virtual-key dimension: a
// stale, out-of-order event must never overwrite a genuinely newer
// state.
func TestApplyVirtualKeyUpsertFromEventDiscardsAnOutOfOrderStaleUpdate(t *testing.T) {
	keys := []identity.VirtualKey{{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100}}
	p := newVirtualKeyPropagationTestPipeline(t, keys, nil)

	newer := int64(2000)
	older := int64(1000)

	newerVK := identity.VirtualKey{ID: "team-gamma", KeyHash: testHashOf("newer-secret"), RateLimitBurst: 100, RateLimitRefill: 100}
	if err := p.ApplyVirtualKeyUpsertFromEvent(configpropagation.VirtualKeyUpsertPayload{
		VirtualKey:      virtualKeyToPayload(newerVK),
		RateLimitConfig: keyConfigToPayload(ratelimit.KeyConfig{ID: "team-gamma", Capacity: 100, RefillPerSecond: 100}),
	}, newer); err != nil {
		t.Fatalf("ApplyVirtualKeyUpsertFromEvent(newer): %v", err)
	}
	if !canAuthenticate(p, "newer-secret") {
		t.Fatal("setup: the genuinely newer upsert did not apply")
	}

	// A STALE, out-of-order upsert for the SAME ID, with an OLDER
	// timestamp than what's already applied — a different KeyHash, so a
	// wrongful apply is directly observable via authentication.
	staleVK := identity.VirtualKey{ID: "team-gamma", KeyHash: testHashOf("stale-secret"), RateLimitBurst: 100, RateLimitRefill: 100}
	if err := p.ApplyVirtualKeyUpsertFromEvent(configpropagation.VirtualKeyUpsertPayload{
		VirtualKey:      virtualKeyToPayload(staleVK),
		RateLimitConfig: keyConfigToPayload(ratelimit.KeyConfig{ID: "team-gamma", Capacity: 100, RefillPerSecond: 100}),
	}, older); err != nil {
		t.Fatalf("ApplyVirtualKeyUpsertFromEvent(stale): %v", err)
	}

	if canAuthenticate(p, "stale-secret") {
		t.Error("the STALE, out-of-order upsert was applied -- team-gamma now authenticates the older secret")
	}
	if !canAuthenticate(p, "newer-secret") {
		t.Error("the genuinely newer state was overwritten by the stale, out-of-order upsert")
	}
}

// TestApplyVirtualKeyDeleteFromEventDiscardsAnOutOfOrderStaleUpdate is
// the identical ordering proof for the delete side: a stale delete
// event, timestamped BEFORE a newer upsert this instance already
// applied for the same ID, must not resurrect-then-immediately-remove
// (or otherwise incorrectly act on) state that the newer event already
// superseded.
func TestApplyVirtualKeyDeleteFromEventDiscardsAnOutOfOrderStaleUpdate(t *testing.T) {
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
		{ID: "team-gamma", KeyHash: testHashOf("gamma-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	p := newVirtualKeyPropagationTestPipeline(t, keys, nil)

	// A genuinely newer upsert for team-gamma (e.g. a concurrent update
	// on another instance) applies at t=2000.
	newerVK := identity.VirtualKey{ID: "team-gamma", KeyHash: testHashOf("updated-secret"), RateLimitBurst: 100, RateLimitRefill: 100}
	if err := p.ApplyVirtualKeyUpsertFromEvent(configpropagation.VirtualKeyUpsertPayload{
		VirtualKey:      virtualKeyToPayload(newerVK),
		RateLimitConfig: keyConfigToPayload(ratelimit.KeyConfig{ID: "team-gamma", Capacity: 100, RefillPerSecond: 100}),
	}, 2000); err != nil {
		t.Fatalf("ApplyVirtualKeyUpsertFromEvent(newer): %v", err)
	}

	// A STALE delete for the SAME ID at an OLDER timestamp (t=1000) --
	// e.g. delivered out of order relative to the upsert above -- must
	// be discarded, never removing the genuinely newer state.
	if err := p.ApplyVirtualKeyDeleteFromEvent("team-gamma", 1000); err != nil {
		t.Fatalf("ApplyVirtualKeyDeleteFromEvent(stale): %v", err)
	}

	if !canAuthenticate(p, "updated-secret") {
		t.Error("the stale, out-of-order delete removed a genuinely newer upsert's state")
	}
}

// TestRotateVirtualKeyPropagatedEventNeverRegistersARateLimit is the
// direct regression proof for TypeVirtualKeyUpsert's own doc comment:
// a rotation-originated apply must pass rateLimit=nil, never
// overwriting a remote replica's own, already-registered rate-limit
// config with a reconstructed guess. Proven here by driving
// ApplyVirtualKeyUpsertFromEvent directly with RateLimitConfig
// deliberately nil (exactly what RotateVirtualKey's own publish call
// produces) against a key with a DIFFERENT, already-registered
// Capacity, and confirming that registration survives unchanged.
func TestRotateVirtualKeyPropagatedEventNeverRegistersARateLimit(t *testing.T) {
	keys := []identity.VirtualKey{
		{ID: "team-gamma", KeyHash: testHashOf("gamma-secret"), RateLimitBurst: 1, RateLimitRefill: 0}, // capacity 1, zero refill: exhausts after one call
	}
	p := newVirtualKeyPropagationTestPipeline(t, keys, nil)

	rotatedVK := identity.VirtualKey{ID: "team-gamma", KeyHash: testHashOf("rotated-secret"), RateLimitBurst: 1, RateLimitRefill: 0}
	if err := p.ApplyVirtualKeyUpsertFromEvent(configpropagation.VirtualKeyUpsertPayload{
		VirtualKey:      virtualKeyToPayload(rotatedVK),
		RateLimitConfig: nil, // exactly what RotateVirtualKey's own publish call sends
	}, 1000); err != nil {
		t.Fatalf("ApplyVirtualKeyUpsertFromEvent(rateLimit=nil): %v", err)
	}

	if !canAuthenticate(p, "rotated-secret") {
		t.Fatal("the rotated secret does not authenticate at all")
	}
	// The pre-existing capacity=1 registration must still be in effect --
	// a SECOND call must be rate-limited, exactly as it would have been
	// before this rotation-shaped event was ever applied. If
	// ApplyVirtualKeyUpsertFromEvent had wrongly called Register with a
	// zero-value ratelimit.KeyConfig for a nil rateLimit, this key's
	// bucket would have capacity 0 and EVERY call (including the one
	// just above) would already be rejected.
	if canAuthenticate(p, "rotated-secret") {
		t.Error("a second call succeeded despite capacity=1 -- the pre-existing rate-limit registration was overwritten by the rotation-shaped event")
	}
}
