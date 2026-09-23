package main

// Integration tests for Phase 9's own unblock: budget.RedisAddr/
// Admin.RedisAddr (internal/budget/redisbudget, internal/identity/
// redisstore) wired through the REAL buildPipeline path, not just the
// lower-level Tracker/Store unit tests from Phases 7-8 — and, per the
// plan's own Phase 9 test requirement, a real proof that two LIVE,
// concurrently-running gateway replicas sharing one Redis instance
// enforce combined budget/identity state correctly, the exact scenario
// deploy/k8s/base/deployment.yaml's now-removed replicas: 1 was
// preventing.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/configpropagation"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// openTestRedisForMain starts a real Redis container for this package's
// own integration tests — mirrors internal/gateway/dataplane's identical
// openTestRedis helper (Phase 8), duplicated here rather than exported
// cross-package, matching this file's own doc comment on why cmd/gateway
// tests stay self-contained package main.
func openTestRedisForMain(t *testing.T) string {
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

// testConfigPropagationSigningSecretForMain generates a fresh random
// HMAC secret for one test's own config-propagation setup — mirrors
// internal/gateway/dataplane's identical testConfigPropagationSigningSecret
// helper, duplicated here rather than exported cross-package for the
// same "cmd/gateway tests stay self-contained package main" reason
// openTestRedisForMain's own doc comment gives. configpropagation.Open
// now refuses to Publish/Subscribe without one (see
// configpropagation.MutationEvent.Signature's own doc comment), and
// buildPipeline's newConfigPublisher fails startup entirely when
// ConfigPropagation.RedisAddr is set but the resolved secret is empty —
// every call to newRedisBackedIntegrationServer below needs a real one.
func testConfigPropagationSigningSecretForMain(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generating test signing secret: %v", err)
	}
	return hex.EncodeToString(b)
}

// newRedisBackedIntegrationServer builds the same real buildPipeline +
// chatCompletionsHandler wiring as newIntegrationServerWithBudgetPersistence
// (budget_persistence_integration_test.go), but with Budget/Admin/
// ConfigPropagation ALL pointed at redisAddr — proving newBudgetTracker's
// and buildPipeline's own identity-store construction branch genuinely
// route through internal/budget/redisbudget and internal/identity/
// redisstore, via the exact config fields an operator would set, not a
// lower-level constructor call. signingSecret is the config-propagation
// HMAC secret — callers that need two instances to interoperate (e.g.
// a real cross-instance convergence test) must pass the SAME value to
// both; callers that don't care can pass independently generated ones.
func newRedisBackedIntegrationServer(t *testing.T, upstreamURL, upstreamKeyEnvVar, redisAddr, signingSecret string, keys []controlplane.VirtualKeyConfig) (*httptest.Server, *dataplane.Pipeline) {
	t.Helper()
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")
	signingSecretEnvVar := upstreamKeyEnvVar + "_CFGPROP_SECRET"
	t.Setenv(signingSecretEnvVar, signingSecret)

	cfg := &controlplane.Config{
		ListenAddr:  ":0",
		VirtualKeys: keys,
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "gpt4o-primary",
				Model:         "gpt-4o",
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       upstreamURL,
				APIKeyEnv:     upstreamKeyEnvVar,
			},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"gpt-4o": {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
		},
		Budget:            controlplane.BudgetConfig{RedisAddr: redisAddr},
		Admin:             controlplane.AdminConfig{RedisAddr: redisAddr},
		ConfigPropagation: controlplane.ConfigPropagationConfig{RedisAddr: redisAddr, SigningSecretEnv: signingSecretEnvVar},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler(pipeline))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, pipeline
}

// subscribeVirtualKeyAndWeightEvents mirrors run()'s own real
// config-propagation subscriber closure (cmd/gateway/main.go) exactly —
// the piece buildPipeline itself does NOT wire (only the Publish side),
// since the subscriber goroutine's lifecycle belongs to run(), not
// pipeline construction. Tests that need live cross-instance
// convergence must start this themselves, against the same target
// Pipeline production's own subscriber goroutine would apply events to.
func subscribeVirtualKeyAndWeightEvents(ctx context.Context, sub *configpropagation.PubSub, target *dataplane.Pipeline, logger *slog.Logger) {
	_ = sub.Subscribe(ctx, "test-instance", func(event configpropagation.MutationEvent) {
		switch event.Type {
		case configpropagation.TypeVirtualKeyUpsert:
			var payload configpropagation.VirtualKeyUpsertPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				logger.Warn("configpropagation_payload_unmarshal_failed", "type", event.Type, "error", err)
				return
			}
			if err := target.ApplyVirtualKeyUpsertFromEvent(payload, event.PublishedAtUnixNano); err != nil {
				logger.Warn("configpropagation_apply_failed", "type", event.Type, "error", err)
			}
		case configpropagation.TypeVirtualKeyDelete:
			var payload configpropagation.VirtualKeyDeletePayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				logger.Warn("configpropagation_payload_unmarshal_failed", "type", event.Type, "error", err)
				return
			}
			if err := target.ApplyVirtualKeyDeleteFromEvent(payload.ID, event.PublishedAtUnixNano); err != nil {
				logger.Warn("configpropagation_apply_failed", "type", event.Type, "error", err)
			}
		}
	})
}

func doChatRequest(t *testing.T, gw *httptest.Server, secret string) (int, string) {
	t.Helper()
	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`
	httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestIntegrationBudgetPersistsAcrossRestartViaRedis mirrors
// TestIntegrationBudgetPersistsAcrossRestart's identical proof
// (budget_persistence_integration_test.go), but with
// Budget.RedisAddr instead of PersistPath — the real-Redis,
// full-stack equivalent of internal/budget's own unit tests, proving
// newBudgetTracker's Redis branch actually gets exercised when an
// operator sets budget.redis_addr in config.yaml.
func TestIntegrationBudgetPersistsAcrossRestartViaRedis(t *testing.T) {
	redisAddr := openTestRedisForMain(t)
	upstream, calls := newMockUpstream(t)

	// Same arithmetic as the bbolt version: cap below the cost of even
	// one request lets the FIRST request through, rejects the next --
	// tight enough that instance #2's first request is already over
	// budget if (and only if) the Redis-shared spend actually survived.
	keys := []controlplane.VirtualKeyConfig{
		{Name: "team-durable", KeyHash: testKeyHash("durable-secret"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("0.00001")},
	}

	gw1, pipeline1 := newRedisBackedIntegrationServer(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_REDIS1", redisAddr, testConfigPropagationSigningSecretForMain(t), keys)
	status1, body1 := doChatRequest(t, gw1, "durable-secret")
	if status1 != http.StatusOK {
		t.Fatalf("instance #1 request status = %d, want 200; body: %s", status1, body1)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("mock upstream calls after instance #1's request = %d, want 1", got)
	}

	// Simulate a clean restart: close instance #1's pipeline before
	// instance #2 ever connects -- proves this is genuine Redis-backed
	// persistence, not merely an in-memory value both share by
	// accident within the same process.
	if err := pipeline1.Close(); err != nil {
		t.Fatalf("pipeline1.Close(): %v", err)
	}

	gw2, pipeline2 := newRedisBackedIntegrationServer(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_REDIS2", redisAddr, testConfigPropagationSigningSecretForMain(t), keys)
	t.Cleanup(func() { _ = pipeline2.Close() })

	status2, body2 := doChatRequest(t, gw2, "durable-secret")
	if status2 != http.StatusTooManyRequests {
		t.Fatalf("instance #2's first request status = %d, want %d (budget already exceeded via Redis, proving spend survived the restart); body: %s", status2, http.StatusTooManyRequests, body2)
	}
	if !bytes.Contains([]byte(body2), []byte("budget")) {
		t.Errorf("instance #2's rejection body = %q, want it to mention \"budget\"", body2)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("mock upstream calls after instance #2's (should be rejected) request = %d, want still 1", got)
	}
}

// TestIntegrationTwoLiveReplicasShareRedisBackedBudgetAcrossConcurrentInstances
// is the load-bearing proof the plan's own Phase 9 test section calls
// for: TWO independently-built gateway instances, BOTH already running
// (never restarted, never closed) at the moment the second request
// arrives, sharing one Redis instance for budget -- the exact scenario
// deploy/k8s/base/deployment.yaml's now-removed replicas: 1 constraint
// existed to prevent. Deliberately sequential (instance A's request,
// THEN instance B's), not truly concurrent goroutines racing each
// other -- this stays deterministic while still proving the real
// property: instance B, which never itself served the first request,
// still sees instance A's spend and enforces the SAME shared budget.
func TestIntegrationTwoLiveReplicasShareRedisBackedBudgetAcrossConcurrentInstances(t *testing.T) {
	redisAddr := openTestRedisForMain(t)
	upstreamA, callsA := newMockUpstream(t)
	upstreamB, callsB := newMockUpstream(t)

	keys := []controlplane.VirtualKeyConfig{
		{Name: "team-shared", KeyHash: testKeyHash("shared-secret"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("0.00001")},
	}

	// Instance A and instance B are built independently and stay open
	// side by side for the rest of this test -- neither is ever closed
	// before the other's request runs.
	gwA, pipelineA := newRedisBackedIntegrationServer(t, upstreamA.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_REPLICA_A", redisAddr, testConfigPropagationSigningSecretForMain(t), keys)
	t.Cleanup(func() { _ = pipelineA.Close() })
	gwB, pipelineB := newRedisBackedIntegrationServer(t, upstreamB.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_REPLICA_B", redisAddr, testConfigPropagationSigningSecretForMain(t), keys)
	t.Cleanup(func() { _ = pipelineB.Close() })

	statusA, bodyA := doChatRequest(t, gwA, "shared-secret")
	if statusA != http.StatusOK {
		t.Fatalf("instance A's request status = %d, want 200; body: %s", statusA, bodyA)
	}
	if got := callsA.Load(); got != 1 {
		t.Fatalf("instance A's own mock upstream calls = %d, want 1", got)
	}

	// Instance B, a completely separate, already-running Pipeline that
	// never itself served instance A's request, must ALSO see the
	// budget as exhausted -- the real cross-replica proof.
	statusB, bodyB := doChatRequest(t, gwB, "shared-secret")
	if statusB != http.StatusTooManyRequests {
		t.Fatalf("instance B's request status = %d, want %d (instance A's spend must be visible via shared Redis); body: %s", statusB, http.StatusTooManyRequests, bodyB)
	}
	if got := callsB.Load(); got != 0 {
		t.Fatalf("instance B's own mock upstream calls = %d, want 0 (rejected before ever reaching a deployment)", got)
	}
}

// TestIntegrationTwoLiveReplicasConvergeOnVirtualKeyUpsertViaConfigPropagation
// is the identity-dimension half of the same proof: instance A upserts a
// brand-new virtual key (never present in either instance's original
// config) and instance B -- an independent, already-running Pipeline,
// with its own real subscriber goroutine wired exactly like
// cmd/gateway's own run() does -- converges to accept it, via a real
// Redis pub/sub round trip through the FULL buildPipeline stack (not
// just the lower-level dataplane-package proof from Phase 8).
func TestIntegrationTwoLiveReplicasConvergeOnVirtualKeyUpsertViaConfigPropagation(t *testing.T) {
	redisAddr := openTestRedisForMain(t)
	upstreamA, _ := newMockUpstream(t)
	upstreamB, _ := newMockUpstream(t)

	keys := []controlplane.VirtualKeyConfig{
		{Name: "team-shared", KeyHash: testKeyHash("shared-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}

	signingSecret := testConfigPropagationSigningSecretForMain(t)
	_, pipelineA := newRedisBackedIntegrationServer(t, upstreamA.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_PROP_A", redisAddr, signingSecret, keys)
	t.Cleanup(func() { _ = pipelineA.Close() })
	gwB, pipelineB := newRedisBackedIntegrationServer(t, upstreamB.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_PROP_B", redisAddr, signingSecret, keys)
	t.Cleanup(func() { _ = pipelineB.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	subB := configpropagation.Open(redis.Options{Addr: redisAddr}, signingSecret)
	t.Cleanup(func() { _ = subB.Close() })
	subCtx, cancelSub := context.WithCancel(context.Background())
	t.Cleanup(cancelSub)
	go subscribeVirtualKeyAndWeightEvents(subCtx, subB, pipelineB, logger)
	time.Sleep(100 * time.Millisecond) // let the subscription actually register.

	if status, _ := doChatRequest(t, gwB, "brand-new-secret"); status == http.StatusOK {
		t.Fatal("setup: instance B already authenticates a key that was never added")
	}

	if err := pipelineA.UpsertVirtualKey(
		identity.VirtualKey{ID: "team-gamma", KeyHash: testKeyHash("brand-new-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		ratelimit.KeyConfig{ID: "team-gamma", Capacity: 100, RefillPerSecond: 100},
	); err != nil {
		t.Fatalf("UpsertVirtualKey on instance A: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if status, _ := doChatRequest(t, gwB, "brand-new-secret"); status == http.StatusOK {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("instance B never converged to accept instance A's newly-upserted key within 5s -- cross-instance propagation did not converge through the full buildPipeline stack")
}
