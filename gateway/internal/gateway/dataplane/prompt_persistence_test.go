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
	"github.com/kelvran/gateway/gateway/internal/prompt"
	promptboltstore "github.com/kelvran/gateway/gateway/internal/prompt/boltstore"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// newTestPipelineWithPrompts mirrors newTestPipelineWithIdentityStore's own
// shape, wiring Prompts instead of IdentityStore -- the only existing test
// helper this repo has for a *prompt.Store built via
// prompt.NewStoreWithPersister.
func newTestPipelineWithPrompts(t *testing.T, prompts *prompt.Store) *Pipeline {
	t.Helper()
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Prompts:        prompts,
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

// TestPromptCreatedViaAdminAPISurvivesAPipelineRestart is the
// load-bearing proof that prompt.Persister's first real implementation
// (boltstore) actually closes the "admin-mutated state is in-memory-only"
// gap for prompts, mirroring identity's own
// TestVirtualKeyCreatedViaAdminAPISurvivesAPipelineRestart.
func TestPromptCreatedViaAdminAPISurvivesAPipelineRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompts.db")

	store1, err := promptboltstore.Open(path)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	prompts1, err := prompt.NewStoreWithPersister(context.Background(), store1)
	if err != nil {
		t.Fatalf("NewStoreWithPersister (first): %v", err)
	}
	p1 := newTestPipelineWithPrompts(t, prompts1)
	if _, err := p1.UpsertPrompt("greeting", []adapter.Message{{Role: "user", Content: "hello from before the restart"}}); err != nil {
		t.Fatalf("UpsertPrompt: %v", err)
	}
	if err := p1.Close(); err != nil { // closes store1 too, via prompts1.Close()
		t.Fatalf("Close (first): %v", err)
	}

	store2, err := promptboltstore.Open(path)
	if err != nil {
		t.Fatalf("Open (second, simulating a restart): %v", err)
	}
	prompts2, err := prompt.NewStoreWithPersister(context.Background(), store2)
	if err != nil {
		t.Fatalf("NewStoreWithPersister (second): %v", err)
	}
	p2 := newTestPipelineWithPrompts(t, prompts2)
	defer func() { _ = p2.Close() }()

	got, ok := p2.GetPrompt("greeting", 0)
	if !ok {
		t.Fatal("GetPrompt(\"greeting\") after restart: not found")
	}
	if got.Messages[0].Content != "hello from before the restart" {
		t.Errorf("greeting content after restart = %q, want %q", got.Messages[0].Content, "hello from before the restart")
	}
}

// TestDeletedPromptStaysGoneAfterAPipelineRestart proves the deletion
// half: a prompt removed via the admin API must not resurrect from a
// stale persisted entry after a restart.
func TestDeletedPromptStaysGoneAfterAPipelineRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompts.db")

	store1, err := promptboltstore.Open(path)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	prompts1, err := prompt.NewStoreWithPersister(context.Background(), store1)
	if err != nil {
		t.Fatalf("NewStoreWithPersister (first): %v", err)
	}
	p1 := newTestPipelineWithPrompts(t, prompts1)
	if _, err := p1.UpsertPrompt("doomed", []adapter.Message{{Role: "user", Content: "will be deleted"}}); err != nil {
		t.Fatalf("UpsertPrompt: %v", err)
	}
	if err := p1.DeletePrompt("doomed"); err != nil {
		t.Fatalf("DeletePrompt: %v", err)
	}
	if err := p1.Close(); err != nil {
		t.Fatalf("Close (first): %v", err)
	}

	store2, err := promptboltstore.Open(path)
	if err != nil {
		t.Fatalf("Open (second, simulating a restart): %v", err)
	}
	defer func() { _ = store2.Close() }()
	persisted, err := store2.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, stillPersisted := persisted["doomed"]; stillPersisted {
		t.Fatal("\"doomed\" is still present in the persisted store after DeletePrompt")
	}
}
