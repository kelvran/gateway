package dataplane

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

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

	if _, err := p2.HandleChatCompletion(context.Background(), "Bearer config-secret", "", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Errorf("HandleChatCompletion with the config-derived key after restart: %v", err)
	}
	if _, err := p2.HandleChatCompletion(context.Background(), "Bearer admin-secret", "", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
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

// blockingOnceStore wraps a real identity.Store, letting a test hold its
// FIRST Save call open indefinitely (blocked on release) after signaling
// entered -- deterministically forcing a specific persistence-call
// interleaving, rather than hoping a fixed sleep happens to land in the
// right place. Only Save is intercepted; every other method (including
// Delete) passes straight through to the real store.
type blockingOnceStore struct {
	identity.Store
	entered atomic.Bool
	// signalOnce/release are set by the test AFTER construction; both
	// are only ever touched by this single blocked Save call and the
	// test's own goroutine, never concurrently with each other.
	signalEntered chan struct{}
	release       chan struct{}
}

func (b *blockingOnceStore) Save(ctx context.Context, vk identity.VirtualKey) error {
	if b.entered.CompareAndSwap(false, true) {
		close(b.signalEntered)
		<-b.release
	}
	return b.Store.Save(ctx, vk)
}

// TestConcurrentRotateAndDeleteSameVirtualKeySurvivesRestart is the
// regression proof for the real bug fixed in Pipeline.virtualKeyMutationMu's
// own doc comment: Upsert/Delete/RotateVirtualKey's in-memory CAS loop was
// already safe, but each method's own persistence call was a separate,
// uncoordinated I/O call with no ordering guarantee relative to a
// DIFFERENT method's own persistence call for the SAME key. Deterministically
// forces the exact adverse interleaving the fix closes: Rotate's CAS
// commits in-memory, then its OWN persist call is held open; while it's
// held, Delete runs to full completion (CAS + persist, removing the key
// both in-memory and on disk); only THEN is Rotate's held persist call
// released, writing the stale rotated hash back to disk AFTER Delete's
// own disk write already removed it. Without virtualKeyMutationMu
// serializing the two methods' entire mutate-then-persist sequences,
// Delete's own DeleteVirtualKey call would never even reach its own CAS
// while Rotate's Save is blocked -- proving the fix directly, not by
// chance timing.
func TestConcurrentRotateAndDeleteSameVirtualKeySurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.db")
	baseKeys := []identity.VirtualKey{
		{ID: "config-key", KeyHash: testHashOf("config-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		{ID: "racing-key", KeyHash: testHashOf("racing-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}

	realStore1, err := identityboltstore.Open(path)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	store1 := &blockingOnceStore{Store: realStore1, signalEntered: make(chan struct{}), release: make(chan struct{})}
	p1 := newTestPipelineWithIdentityStore(t, baseKeys, store1)

	rotateErrCh := make(chan error, 1)
	go func() {
		rotateErrCh <- p1.RotateVirtualKey("racing-key", testHashOf("rotated-secret"), time.Minute)
	}()

	select {
	case <-store1.signalEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Rotate's own Save call never entered — virtualKeyMutationMu may be blocking it before it even reaches persistence")
	}

	// At this point Rotate's CAS has ALREADY committed in-memory (the
	// key now has the rotated hash) and its own persist call is
	// deliberately held open. If virtualKeyMutationMu is doing its job,
	// this Delete call cannot even START its own CAS loop until Rotate's
	// entire method — including the still-blocked Save above — returns.
	deleteDone := make(chan struct{})
	go func() {
		defer close(deleteDone)
		if err := p1.DeleteVirtualKey("racing-key"); err != nil {
			t.Errorf("DeleteVirtualKey: %v", err)
		}
	}()

	select {
	case <-deleteDone:
		t.Fatal("DeleteVirtualKey returned while Rotate's own persist call was still blocked — virtualKeyMutationMu did NOT serialize the two methods")
	case <-time.After(200 * time.Millisecond):
		// Expected: Delete is still blocked waiting for the mutex Rotate
		// is holding.
	}

	close(store1.release) // let Rotate's held Save call, and the rest of RotateVirtualKey, finish.
	if err := <-rotateErrCh; err != nil {
		t.Fatalf("RotateVirtualKey: %v", err)
	}
	<-deleteDone

	// Final state, whatever it settled on, in-memory.
	var finalInMemory *identity.VirtualKey
	for _, k := range p1.verifier.Load().Keys() {
		if k.ID == "racing-key" {
			kk := k
			finalInMemory = &kk
		}
	}

	if err := p1.Close(); err != nil { // closes store1 too
		t.Fatalf("Close (first): %v", err)
	}

	store2, err := identityboltstore.Open(path)
	if err != nil {
		t.Fatalf("Open (second, simulating a restart): %v", err)
	}
	defer func() { _ = store2.Close() }()
	persisted, err := store2.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	persistedEntry, stillPersisted := persisted["racing-key"]

	if finalInMemory == nil && stillPersisted {
		t.Fatalf("racing-key is absent in-memory but STILL PERSISTED on disk (%+v) -- a restart would resurrect it", persistedEntry)
	}
	if finalInMemory != nil && !stillPersisted {
		t.Fatalf("racing-key is present in-memory (%+v) but MISSING from the persisted store -- a restart would silently drop it", *finalInMemory)
	}
	if finalInMemory != nil && stillPersisted && finalInMemory.KeyHash != persistedEntry.KeyHash {
		t.Fatalf("racing-key's persisted KeyHash %q does not match its final in-memory KeyHash %q", persistedEntry.KeyHash, finalInMemory.KeyHash)
	}
}
