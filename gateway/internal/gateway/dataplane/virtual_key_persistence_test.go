package dataplane

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	identityboltstore "github.com/kelvran/gateway/gateway/internal/identity/boltstore"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// newTestPipelineWithIdentityStore mirrors newTestPipelineWithKeysAndBudget's
// own shape, additionally wiring IdentityStore -- the only existing test
// helper this repo has for that field.
func newTestPipelineWithIdentityStore(t *testing.T, keys []identity.VirtualKey, store identity.Store) *Pipeline {
	t.Helper()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		IdentityStore:  store,
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
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestVirtualKeyCreatedViaAdminAPISurvivesAPipelineRestart is the
// load-bearing proof for docs/upgrade-research/admin-operator-experience-2026-09-14.md
// Finding 2: a virtual key created purely via the live admin API (never
// present in the "config-derived" key set a real restart would rebuild
// from config.yaml) must still authenticate after the process restarts,
// as long as the same bbolt file is reopened.
func TestVirtualKeyCreatedViaAdminAPISurvivesAPipelineRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.db")
	baseKeys := []identity.VirtualKey{
		{ID: "config-key", KeyHash: testHashOf("config-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}

	// "Process 1": create a second key purely via the admin API.
	store1, err := identityboltstore.Open(path)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	p1 := newTestPipelineWithIdentityStore(t, baseKeys, store1)
	if err := p1.UpsertVirtualKey(
		identity.VirtualKey{ID: "admin-created-key", KeyHash: testHashOf("admin-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		ratelimit.KeyConfig{ID: "admin-created-key", Capacity: 100, RefillPerSecond: 100},
	); err != nil {
		t.Fatalf("UpsertVirtualKey: %v", err)
	}
	if err := p1.Close(); err != nil { // closes store1 too
		t.Fatalf("Close (first): %v", err)
	}

	// "Process 2, after a restart": the SAME bootstrap set as before
	// (config.yaml was never updated -- it never knows about
	// admin-created-key at all) merged against the persisted store,
	// mirroring exactly what cmd/gateway's own buildPipeline does via
	// mergePersistedVirtualKeys.
	store2, err := identityboltstore.Open(path)
	if err != nil {
		t.Fatalf("Open (second, simulating a restart): %v", err)
	}
	persisted, err := store2.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	mergedKeys := append([]identity.VirtualKey{}, baseKeys...)
	for _, vk := range persisted {
		mergedKeys = append(mergedKeys, vk)
	}
	p2 := newTestPipelineWithIdentityStore(t, mergedKeys, store2)
	defer func() { _ = p2.Close() }()

	if _, err := p2.HandleChatCompletion(context.Background(), "Bearer config-secret", adapter.ChatRequest{Model: "gpt-4o"}); err != nil {
		t.Errorf("HandleChatCompletion with the config-derived key after restart: %v", err)
	}
	if _, err := p2.HandleChatCompletion(context.Background(), "Bearer admin-secret", adapter.ChatRequest{Model: "gpt-4o"}); err != nil {
		t.Errorf("HandleChatCompletion with the admin-API-created key after restart: %v", err)
	}
}

// TestDeletedVirtualKeyStaysGoneAfterAPipelineRestart proves the deletion
// half: a key removed via the admin API must not resurrect from a stale
// persisted entry after a restart.
func TestDeletedVirtualKeyStaysGoneAfterAPipelineRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.db")
	baseKeys := []identity.VirtualKey{
		{ID: "config-key", KeyHash: testHashOf("config-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		{ID: "doomed-key", KeyHash: testHashOf("doomed-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}

	store1, err := identityboltstore.Open(path)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	p1 := newTestPipelineWithIdentityStore(t, baseKeys, store1)
	// Persist doomed-key first (as if an earlier admin action had rotated
	// it), then delete it -- the delete must remove the persisted entry
	// too, not just the in-memory Verifier.
	if err := p1.UpsertVirtualKey(
		identity.VirtualKey{ID: "doomed-key", KeyHash: testHashOf("doomed-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		ratelimit.KeyConfig{ID: "doomed-key", Capacity: 100, RefillPerSecond: 100},
	); err != nil {
		t.Fatalf("UpsertVirtualKey: %v", err)
	}
	if err := p1.DeleteVirtualKey("doomed-key"); err != nil {
		t.Fatalf("DeleteVirtualKey: %v", err)
	}
	if err := p1.Close(); err != nil {
		t.Fatalf("Close (first): %v", err)
	}

	store2, err := identityboltstore.Open(path)
	if err != nil {
		t.Fatalf("Open (second, simulating a restart): %v", err)
	}
	persisted, err := store2.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, stillPersisted := persisted["doomed-key"]; stillPersisted {
		t.Fatal("doomed-key is still present in the persisted store after DeleteVirtualKey")
	}
	if err := store2.Close(); err != nil {
		t.Fatalf("Close (second): %v", err)
	}
}
