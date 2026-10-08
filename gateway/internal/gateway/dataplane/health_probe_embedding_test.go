package dataplane

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// embeddingProbeUpstream is a mutex-guarded EmbeddingUpstream fixture (the
// probe loop runs one goroutine per deployment, so these tests run under
// -race): it records every provider-native request and either succeeds with
// a tiny vector or fails deterministically.
type embeddingProbeUpstream struct {
	mu    sync.Mutex
	calls []*openai.EmbeddingRequest
	fail  bool
}

func (u *embeddingProbeUpstream) call(_ context.Context, _ Deployment, req any) (any, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	native, _ := req.(*openai.EmbeddingRequest)
	u.calls = append(u.calls, native)
	if u.fail {
		return nil, errors.New("embeddingProbeUpstream: simulated failure")
	}
	return &openai.EmbeddingResponseWire{
		Model: "text-embedding-3-small",
		Data:  []openai.EmbeddingDataWire{{Index: 0, Embedding: []float64{0.1}}},
		Usage: openai.EmbeddingUsageWire{PromptTokens: 1, TotalTokens: 1},
	}, nil
}

func (u *embeddingProbeUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

func (u *embeddingProbeUpstream) lastInput() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.calls) == 0 || u.calls[len(u.calls)-1] == nil {
		return nil
	}
	return u.calls[len(u.calls)-1].Input
}

// lastModel is the model name the last provider-native request carried
// on the wire -- must be the deployment's upstream_model, never the
// canonical alias (see TestProbeEmbeddingDeploymentUsesEmbeddingAdapter).
func (u *embeddingProbeUpstream) lastModel() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.calls) == 0 || u.calls[len(u.calls)-1] == nil {
		return ""
	}
	return u.calls[len(u.calls)-1].Model
}

// newProbeTestPipeline builds a pipeline with BOTH upstream kinds wired, so
// the probe loop can be exercised against chat and embedding deployments at
// once. newTestPipeline leaves EmbeddingUpstream nil and
// newEmbeddingTestPipeline fails the test on any chat call, so neither fits.
func newProbeTestPipeline(t *testing.T, chatUpstream, embeddingUpstream UpstreamCaller, deployments []Deployment) *Pipeline {
	t.Helper()
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("1000")},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	p, err := NewPipeline(Config{
		Verifier:   verifier,
		Limiter:    ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:     budget.NewTracker(),
		Cache:      inprocess.New(0),
		CacheL2:    inprocess.New(0),
		CacheL3:    inprocess.NewLexicalCache(0),
		Guardrails: guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters: adapter.Registry{
			"openai":  openai.New(),
			"bedrock": bedrock.New(),
		},
		Router:            testRouter(deployments),
		Deployments:       deployments,
		CostCalculator:    costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream:          chatUpstream,
		EmbeddingUpstream: embeddingUpstream,
		Logger:            discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// chatAndEmbeddingDeployments: the embedding deployment deliberately uses
// an ALIASED canonical model ("fast-embed") that differs from its
// upstream_model, because every pre-existing embedding fixture used
// identical names and so never noticed that /v1/embeddings sent the
// canonical name on the OpenAI wire (fixed 2026-10-08 in
// callEmbeddingDeployment; chat always translated via callDeployment).
func chatAndEmbeddingDeployments() []Deployment {
	return []Deployment{
		{Name: "chat", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "titan-embed", Model: "fast-embed", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"},
	}
}

// chatUpstreamRejectingEmbeddingKind fails the test if the chat probe is
// ever sent to an embedding deployment — the F3 reproduction.
func chatUpstreamRejectingEmbeddingKind(t *testing.T) UpstreamCaller {
	return func(_ context.Context, dep Deployment, _ any) (any, error) {
		if dep.Kind == "embedding" {
			t.Errorf("chat Upstream received the embedding deployment %q: the probe is kind-unaware (live defect F3)", dep.Name)
			return nil, errors.New("chat probe sent to an embedding deployment")
		}
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}
}

// TestProbeEmbeddingDeploymentUsesEmbeddingAdapter is the F3 reproduction:
// before the fix every probe was a chat "ping", so an embedding deployment
// failed every probe, was marked unhealthy after the threshold, and /readyz
// reported its model as not ready even though real embedding traffic worked.
func TestProbeEmbeddingDeploymentUsesEmbeddingAdapter(t *testing.T) {
	emb := &embeddingProbeUpstream{}
	p := newProbeTestPipeline(t, chatUpstreamRejectingEmbeddingKind(t), emb.call, chatAndEmbeddingDeployments())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		p.ProbeDeployments(ctx)
	}
	if got := emb.count(); got != 3 {
		t.Fatalf("embedding upstream probed %d times, want 3 (one per ProbeDeployments pass)", got)
	}
	if in := emb.lastInput(); len(in) != 1 || in[0] == "" {
		t.Errorf("embedding probe input = %v, want one non-empty string", in)
	}
	if got := emb.lastModel(); got != "text-embedding-3-small" {
		t.Errorf("embedding probe wire model = %q, want the deployment's upstream_model %q (not the canonical alias)", got, "text-embedding-3-small")
	}
	if !p.router.IsHealthy("titan-embed") {
		t.Fatal("embedding deployment marked unhealthy although every embedding probe succeeded")
	}
	ready, perModel := p.ReadinessSummary()
	if !ready {
		t.Errorf("ReadinessSummary ready = false with every deployment healthy: %v", perModel)
	}
	if ok, present := perModel["fast-embed"]; !present || !ok {
		t.Errorf("per-model readiness for the embedding model = %v (present %v), want true", ok, present)
	}
}

// TestProbeEmbeddingDeploymentFailureStillTripsThreshold: a kind-aware probe
// must still detect a genuinely broken embedding deployment, otherwise the
// fix would have replaced a false negative with a false positive.
func TestProbeEmbeddingDeploymentFailureStillTripsThreshold(t *testing.T) {
	emb := &embeddingProbeUpstream{fail: true}
	p := newProbeTestPipeline(t, chatUpstreamRejectingEmbeddingKind(t), emb.call, chatAndEmbeddingDeployments())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		p.ProbeDeployments(ctx)
	}
	if p.router.IsHealthy("titan-embed") {
		t.Fatal("embedding deployment still healthy after 3 consecutive failed embedding probes")
	}
	_, perModel := p.ReadinessSummary()
	if ok, present := perModel["fast-embed"]; present && ok {
		t.Error("per-model readiness for the broken embedding model = true, want false")
	}
	if !p.router.IsHealthy("chat") {
		t.Error("chat deployment was marked unhealthy; the embedding failure must not leak across deployments")
	}
}

// TestProbeEmbeddingDeploymentWithoutEmbeddingUpstreamIsUnhealthy: a
// configuration with an embedding deployment but no embedding upstream
// would fail every real embedding request with ErrEmbeddingsNotConfigured,
// so the probe must report it unhealthy rather than skip it (skipping would
// leave router.IsHealthy at its optimistic default forever).
func TestProbeEmbeddingDeploymentWithoutEmbeddingUpstreamIsUnhealthy(t *testing.T) {
	deployments := []Deployment{
		{Name: "titan-embed", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"},
	}
	p := newProbeTestPipeline(t, chatUpstreamRejectingEmbeddingKind(t), nil, deployments)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		p.ProbeDeployments(ctx)
	}
	if p.router.IsHealthy("titan-embed") {
		t.Fatal("embedding deployment with no embedding upstream reads healthy; real traffic would get ErrEmbeddingsNotConfigured")
	}
}

// TestProbeDeploymentUnknownKindFailsProbe guards the future, not a
// reachable config: controlplane.Load rejects every kind other than
// "chat"/"embedding", so this can only happen once someone adds a third
// kind. When they do, probeDeployment must grow a probe shape for it
// rather than silently sending the chat probe -- the exact mechanism that
// made embedding deployments fail every probe (F3). Until then an unknown
// kind fails its probe loudly, with NEITHER upstream called.
func TestProbeDeploymentUnknownKindFailsProbe(t *testing.T) {
	var chatCalls atomic.Int32
	chatUpstream := func(_ context.Context, dep Deployment, _ any) (any, error) {
		chatCalls.Add(1)
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}
	emb := &embeddingProbeUpstream{}
	deployments := []Deployment{
		{Name: "rerank-1", Model: "rerank-v1", Provider: "openai", UpstreamModel: "rerank-v1", BaseURL: "http://unused", Kind: "rerank"},
	}
	p := newProbeTestPipeline(t, chatUpstream, emb.call, deployments)
	ctx := context.Background()

	err := p.probeDeployment(ctx, deployments[0])
	if err == nil || !strings.Contains(err.Error(), `kind "rerank"`) {
		t.Fatalf("probeDeployment(unknown kind) error = %v, want one naming the kind", err)
	}
	for i := 0; i < 3; i++ {
		p.ProbeDeployments(ctx)
	}
	if p.router.IsHealthy("rerank-1") {
		t.Error("deployment of an unknown kind still healthy after 3 failed probes")
	}
	if n := chatCalls.Load(); n != 0 {
		t.Errorf("chat upstream called %d times for an unknown-kind deployment, want 0", n)
	}
	if n := emb.count(); n != 0 {
		t.Errorf("embedding upstream called %d times for an unknown-kind deployment, want 0", n)
	}
}
