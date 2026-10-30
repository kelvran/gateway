package dataplane

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/admission"
	"github.com/kelvran/gateway/gateway/internal/redisfused"
	"github.com/kelvran/gateway/gateway/internal/spendledger"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// fakeFused is the FusedSettler the pipeline is handed in these tests: it
// records each request and the error its context carried, and answers with a
// configured result.
type fakeFused struct {
	mu        sync.Mutex
	reqs      []redisfused.Request
	ctxErrs   []error
	deadlines []bool // the context carried a deadline
	counters  []bool // the context carried the request's Redis round-trip counter
	result    redisfused.Result
	closed    int
}

func (f *fakeFused) Settle(ctx context.Context, req redisfused.Request) redisfused.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	_, hasDeadline := ctx.Deadline()
	_, _, hasCounter := telemetry.RedisRoundTripsFromContext(ctx)
	f.deadlines = append(f.deadlines, hasDeadline)
	f.counters = append(f.counters, hasCounter)
	res := f.result
	res.Sent = true
	return res
}

func (f *fakeFused) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

// fusedCountingLedger counts the standalone ledger writes the fused path must not make.
type fusedCountingLedger struct {
	spendledger.Ledger
	records atomic.Int64
}

func (l *fusedCountingLedger) Record(ctx context.Context, e spendledger.Entry) error {
	l.records.Add(1)
	return l.Ledger.Record(ctx, e)
}

// fusedBudgetBackend is a Redis-mode budget backend that reserves the whole
// cap, counts its standalone writes, and counts the warn-mark scripts.
type fusedBudgetBackend struct {
	settlementCapturingBudgetBackend
	warnMarks atomic.Int64
}

func (b *fusedBudgetBackend) MarkWarnAlerted(context.Context, string, int64) (bool, error) {
	b.warnMarks.Add(1)
	return false, nil
}

func (b *fusedBudgetBackend) adjusts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.adjustCalls
}

type fusedRig struct {
	p      *Pipeline
	fused  *fakeFused
	budget *fusedBudgetBackend
	gate   *gateBackend
	ledger *fusedCountingLedger
	logs   *bytes.Buffer
}

const fusedStreamSSE = gateTestSSE

func newFusedRig(t *testing.T, upstream UpstreamCaller, embedding UpstreamCaller) *fusedRig {
	t.Helper()
	return newFusedRigWithStream(t, upstream, embedding, nil)
}

func newFusedRigWithStream(t *testing.T, upstream UpstreamCaller, embedding UpstreamCaller, stream UpstreamStreamCaller) *fusedRig {
	t.Helper()
	if stream == nil {
		stream = func(context.Context, Deployment, any) (io.ReadCloser, error) {
			return nopCloserReader{strings.NewReader(fusedStreamSSE)}, nil
		}
	}
	keys := []identity.VirtualKey{
		{
			ID: "fz", KeyHash: testHashOf("fz"), RateLimitBurst: 100, RateLimitRefill: 100,
			BudgetUSD: decimal.RequireFromString("10"), BudgetWarnPercent: 0.5,
		},
		{ID: "fu", KeyHash: testHashOf("fu"), RateLimitBurst: 100, RateLimitRefill: 100}, // uncapped
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if upstream == nil {
		upstream = func(_ context.Context, dep Deployment, _ any) (any, error) {
			return fakeOpenAIResponse(dep.UpstreamModel), nil
		}
	}
	if embedding == nil {
		embedding = func(context.Context, Deployment, any) (any, error) {
			return &openai.EmbeddingResponseWire{
				Model: "text-embedding-3-small",
				Data:  []openai.EmbeddingDataWire{{Index: 0, Embedding: []float64{0.1}}},
				Usage: openai.EmbeddingUsageWire{PromptTokens: 20, TotalTokens: 20},
			}, nil
		}
	}
	r := &fusedRig{
		fused: &fakeFused{result: redisfused.Result{
			Budget: redisfused.Part{Status: redisfused.StatusApplied},
			Tokens: redisfused.Part{Status: redisfused.StatusApplied},
			Ledger: redisfused.Part{Status: redisfused.StatusApplied},
		}},
		budget: &fusedBudgetBackend{},
		gate:   &gateBackend{result: admission.Result{Allowed: true, TPMCost: 1000, TPDCost: 1000, TPDColdStart: 1000}},
		ledger: &fusedCountingLedger{Ledger: spendledger.NewMemory(spendledger.Options{})},
		logs:   &bytes.Buffer{},
	}
	logger := slog.New(slog.NewJSONHandler(r.logs, nil))
	deployments := []Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "emb1", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"},
	}
	p, err := NewPipeline(Config{
		Verifier: verifier,
		Limiter: ratelimit.NewRedisKeyLimiter([]ratelimit.KeyConfig{
			{ID: "fz", Capacity: 100, RefillPerSecond: 100, TPMCapacity: 1000, TPMRefillPerSecond: 50, TokensPerDay: 86400},
			{ID: "fu", Capacity: 100, RefillPerSecond: 100, TPMCapacity: 1000, TPMRefillPerSecond: 50, TokensPerDay: 86400},
		}, r.gate),
		Budget:      budget.NewRedisTracker(r.budget, logger),
		Ledger:      r.ledger,
		Fused:       r.fused,
		Cache:       inprocess.New(0),
		CacheL2:     inprocess.New(0),
		CacheL3:     inprocess.NewLexicalCache(0),
		Guardrails:  guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:    adapter.Registry{"openai": openai.New()},
		Router:      testRouter(deployments),
		Deployments: deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{
			"gpt-4o":                 {PromptPerToken: decimal.NewFromFloat(0.001), CompletionPerToken: decimal.NewFromFloat(0.002)},
			"text-embedding-3-small": {PromptPerToken: decimal.NewFromFloat(0.0001)},
		}),
		Upstream:          upstream,
		EmbeddingUpstream: embedding,
		UpstreamStream:    stream,
		Logger:            logger,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	r.p = p
	return r
}

var fusedChatReq = adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}

// One chat completion is 5 prompt and 3 completion tokens at 0.001 and 0.002
// USD: 0.011 USD, against a reservation of the whole 10 USD cap taken at epoch 7.
const (
	fusedCostNano     = 11_000_000
	fusedReservedNano = 10_000_000_000
)

func (r *fusedRig) onlyRequest(t *testing.T) redisfused.Request {
	t.Helper()
	r.fused.mu.Lock()
	defer r.fused.mu.Unlock()
	if len(r.fused.reqs) != 1 {
		t.Fatalf("fused settlements = %d, want exactly 1", len(r.fused.reqs))
	}
	return r.fused.reqs[0]
}

func (r *fusedRig) assertNoStandaloneSettlement(t *testing.T) {
	t.Helper()
	if n := r.budget.adjusts(); n != 0 {
		t.Errorf("standalone budget Adjust calls = %d, want 0: the fused settlement carried it", n)
	}
	if n := len(r.gate.settles); n != 0 {
		t.Errorf("standalone SettleTokens calls = %d, want 0", n)
	}
	if n := r.ledger.records.Load(); n != 0 {
		t.Errorf("standalone ledger Record calls = %d, want 0", n)
	}
}

func TestNewPipelineRefusesFusedOverInMemoryStores(t *testing.T) {
	keys := []identity.VirtualKey{{ID: "fz", KeyHash: testHashOf("fz"), RateLimitBurst: 1, RateLimitRefill: 1}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatal(err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	redisLimiter := ratelimit.NewRedisKeyLimiter(keyConfigsFromVirtualKeys(keys), &gateBackend{})
	redisTracker := budget.NewRedisTracker(&fusedBudgetBackend{}, discardLogger())
	for _, tc := range []struct {
		name    string
		limiter *ratelimit.KeyLimiter
		tracker *budget.Tracker
		fused   FusedSettler
		want    string
	}{
		{"in-memory limiter", ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)), redisTracker, &fakeFused{}, "dataplane: Config.Fused requires a Redis-backed Config.Limiter"},
		{"in-memory tracker", redisLimiter, budget.NewTracker(), &fakeFused{}, "dataplane: Config.Fused requires a Redis-backed Config.Budget"},
		{"both Redis-backed", redisLimiter, redisTracker, &fakeFused{}, ""},
		{"no fused settler over in-memory stores", ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)), budget.NewTracker(), nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewPipeline(Config{
				Verifier: verifier, Limiter: tc.limiter, Budget: tc.tracker, Fused: tc.fused,
				Cache: inprocess.New(0), CacheL2: inprocess.New(0), CacheL3: inprocess.NewLexicalCache(0),
				Guardrails: guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
				Adapters:   adapter.Registry{"openai": openai.New()}, Router: testRouter(deployments), Deployments: deployments,
				CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
				Upstream:       func(context.Context, Deployment, any) (any, error) { return nil, nil },
				Logger:         discardLogger(),
			})
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("NewPipeline = %v, want accepted", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Fatalf("NewPipeline error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFinalizeFusedBillableRequestMakesOneFusedCallAndNoStandaloneSettlement(t *testing.T) {
	r := newFusedRig(t, nil, nil)
	if _, err := r.p.HandleChatCompletion(context.Background(), "Bearer fz", "", "", fusedChatReq, ""); err != nil {
		t.Fatal(err)
	}
	req := r.onlyRequest(t)
	want := redisfused.BudgetPart{KeyID: "fz", DeltaNanoUSD: fusedCostNano - fusedReservedNano, Epoch: 7, Billed: true}
	if req.Budget == nil || *req.Budget != want {
		t.Errorf("budget part = %+v, want %+v", req.Budget, want)
	}
	if len(req.Tokens) != 2 || req.Tokens[0].Key != "fz" || req.Tokens[1].Key != "fz\x00dim=day" {
		t.Fatalf("token settlements = %+v, want the minute and the day dimension", req.Tokens)
	}
	for _, s := range req.Tokens {
		if s.RealTokens == nil || *s.RealTokens != 8 {
			t.Errorf("token settlement %+v, want the real count 8", s)
		}
	}
	if e := req.Ledger; e == nil || e.Key != "fz" || e.Model != "gpt-4o" || e.SpendNanoUSD != fusedCostNano || e.Requests != 1 {
		t.Errorf("ledger entry = %+v, want one request of %d nano-USD on gpt-4o for fz", e, fusedCostNano)
	}
	r.assertNoStandaloneSettlement(t)
}

// A request that reserved but never billed still releases, in the same one call:
// the budget reservation (not billed), the token reservations (no real count),
// and no ledger row.
func TestFinalizeFusedReleasesReservationsWhenTheRequestIsNotBillable(t *testing.T) {
	r := newFusedRig(t, func(context.Context, Deployment, any) (any, error) { return nil, errors.New("upstream down") }, nil)
	if _, err := r.p.HandleChatCompletion(context.Background(), "Bearer fz", "", "", fusedChatReq, ""); err == nil {
		t.Fatal("a failing upstream must surface as an error")
	}
	req := r.onlyRequest(t)
	if req.Budget == nil || req.Budget.Billed || req.Budget.DeltaNanoUSD != -fusedReservedNano || req.Budget.Epoch != 7 {
		t.Errorf("budget part = %+v, want an unbilled release of the whole reservation at epoch 7", req.Budget)
	}
	if len(req.Tokens) != 2 {
		t.Fatalf("token settlements = %+v, want both dimensions released", req.Tokens)
	}
	for _, s := range req.Tokens {
		if s.RealTokens != nil {
			t.Errorf("token settlement %+v carries a real count; a release has none", s)
		}
	}
	if req.Ledger != nil {
		t.Errorf("ledger entry = %+v, want none for a request that billed nothing", req.Ledger)
	}
	r.assertNoStandaloneSettlement(t)
}

// Each part's failure is attributed as it is today: the budget part to
// budget_redis_backend_unavailable plus the skipped threshold checks, the ledger
// part to ledger_record_failed, and the token part to nothing at all.
func TestFinalizeFusedAttributesEachPartsFailureAsTodayDoes(t *testing.T) {
	r := newFusedRig(t, nil, nil)
	r.fused.result = redisfused.Result{
		Budget: redisfused.Part{Status: redisfused.StatusFailed, Err: errors.New("budget part blew up")},
		Tokens: redisfused.Part{Status: redisfused.StatusFailed, Err: errors.New("token part blew up")},
		Ledger: redisfused.Part{Status: redisfused.StatusFailed, Err: errors.New("ledger part blew up")},
	}
	if _, err := r.p.HandleChatCompletion(context.Background(), "Bearer fz", "", "", fusedChatReq, ""); err != nil {
		t.Fatalf("a lost settlement must not fail the request: %v", err)
	}
	logs := r.logs.String()
	for _, want := range []string{`"msg":"budget_redis_backend_unavailable"`, `"msg":"budget_threshold_checks_skipped"`, `"msg":"ledger_record_failed"`, "budget part blew up", "ledger part blew up"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %s: %s", want, logs)
		}
	}
	if strings.Contains(logs, "token part blew up") {
		t.Errorf("the token part's failure is silent today and must stay silent: %s", logs)
	}
	if n := strings.Count(logs, `"msg":"ledger_record_failed"`); n != 1 {
		t.Errorf("ledger_record_failed lines = %d, want 1", n)
	}
}

// The threshold checks read the spend the fused settlement reports, and a
// ledger-only failure does not skip them.
func TestFinalizeFusedThresholdChecksReadTheFusedReply(t *testing.T) {
	r := newFusedRig(t, nil, nil)
	r.fused.result = redisfused.Result{
		Budget:            redisfused.Part{Status: redisfused.StatusApplied},
		SpentAfterNanoUSD: 6_000_000_000, // 6 of 10 USD, past the 50% warn mark
		Tokens:            redisfused.Part{Status: redisfused.StatusApplied},
		Ledger:            redisfused.Part{Status: redisfused.StatusFailed, Err: errors.New("ledger part blew up")},
	}
	if _, err := r.p.HandleChatCompletion(context.Background(), "Bearer fz", "", "", fusedChatReq, ""); err != nil {
		t.Fatal(err)
	}
	logs := r.logs.String()
	if !strings.Contains(logs, `"msg":"budget_warn_threshold_crossed"`) {
		t.Errorf("the warn check did not run on the fused reply's spend: %s", logs)
	}
	if strings.Contains(logs, "budget_threshold_checks_skipped") {
		t.Errorf("a ledger-only failure skipped the threshold checks: %s", logs)
	}
	if !strings.Contains(logs, `"msg":"ledger_record_failed"`) {
		t.Errorf("the ledger failure was not logged: %s", logs)
	}
	if n := r.budget.warnMarks.Load(); n != 1 {
		t.Errorf("warn-mark scripts = %d, want 1 (the mark stays a standalone script)", n)
	}
}

// Settlement runs under a context a client disconnect cannot cancel, as the
// standalone path's does.
func TestFusedSettlementRunsEvenWhenTheClientHasDisconnected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newFusedRig(t, func(_ context.Context, dep Deployment, _ any) (any, error) {
		cancel()
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, nil)
	_, _ = r.p.HandleChatCompletion(ctx, "Bearer fz", "", "", fusedChatReq, "")
	r.onlyRequest(t)
	if err := r.fused.ctxErrs[0]; err != nil {
		t.Errorf("the fused settlement ran under a context with Err() = %v; a client disconnect must not cancel it", err)
	}
	if !r.fused.deadlines[0] {
		t.Error("the fused settlement ran under a context with no deadline; the 5 s settlement bound is gone")
	}
	if !r.fused.counters[0] {
		t.Error("the fused settlement lost the request's Redis round-trip counter; the chat span would miss it")
	}
}

func TestFinalizeFusedServesStreamedRequestsToo(t *testing.T) {
	r := newFusedRig(t, nil, nil)
	rec := httptest.NewRecorder()
	req := adapter.ChatRequest{Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if err := r.p.HandleChatCompletionStream(context.Background(), "Bearer fz", "", "", req, rec, ""); err != nil {
		t.Fatal(err)
	}
	got := r.onlyRequest(t)
	if got.Budget == nil || !got.Budget.Billed || len(got.Tokens) != 2 || got.Ledger == nil {
		t.Errorf("streamed settlement = %+v, want all three parts", got)
	}
	r.assertNoStandaloneSettlement(t)
}

// Embeddings, which settle in their own deferred calls, stay on the standalone
// scripts even when a fused settler is configured.
func TestEmbeddingsStayOnTheStandaloneScriptsWhenFusedIsSet(t *testing.T) {
	r := newFusedRig(t, nil, nil)
	if _, err := r.p.HandleEmbeddings(context.Background(), "Bearer fz", "", adapter.EmbeddingRequest{Model: "text-embedding-3-small", Input: []string{"hello"}}); err != nil {
		t.Fatal(err)
	}
	r.fused.mu.Lock()
	n := len(r.fused.reqs)
	r.fused.mu.Unlock()
	if n != 0 {
		t.Errorf("fused settlements for an embeddings call = %d, want 0", n)
	}
	if got := r.budget.adjusts(); got != 1 {
		t.Errorf("standalone budget Adjust calls = %d, want 1", got)
	}
	if got := r.ledger.records.Load(); got != 1 {
		t.Errorf("standalone ledger Record calls = %d, want 1", got)
	}
}

func TestPipelineCloseReleasesTheFusedSettler(t *testing.T) {
	r := newFusedRig(t, nil, nil)
	if err := r.p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if r.fused.closed != 1 {
		t.Errorf("fused settler closed %d times, want 1", r.fused.closed)
	}
}

// An uncapped key never reserves, so a request that billed nothing has no
// budget write to make: the call carries the token release alone.
func TestFinalizeFusedSendsNoBudgetPartWhenNothingWasReservedOrBilled(t *testing.T) {
	r := newFusedRig(t, func(context.Context, Deployment, any) (any, error) { return nil, errors.New("upstream down") }, nil)
	if _, err := r.p.HandleChatCompletion(context.Background(), "Bearer fu", "", "", fusedChatReq, ""); err == nil {
		t.Fatal("a failing upstream must surface as an error")
	}
	req := r.onlyRequest(t)
	if req.Budget != nil {
		t.Errorf("budget part = %+v, want none: the key is uncapped and nothing billed", req.Budget)
	}
	if len(req.Tokens) != 2 || req.Ledger != nil {
		t.Errorf("request = %+v, want the two token releases and no ledger entry", req)
	}
}

// A billable request on an uncapped key still books its cost: the adjust adds
// the cost to a window that reserved nothing.
func TestFinalizeFusedBooksTheCostOfAnUncappedKey(t *testing.T) {
	r := newFusedRig(t, nil, nil)
	if _, err := r.p.HandleChatCompletion(context.Background(), "Bearer fu", "", "", fusedChatReq, ""); err != nil {
		t.Fatal(err)
	}
	req := r.onlyRequest(t)
	want := redisfused.BudgetPart{KeyID: "fu", DeltaNanoUSD: fusedCostNano, Epoch: 7, Billed: true}
	if req.Budget == nil || *req.Budget != want {
		t.Errorf("budget part = %+v, want %+v", req.Budget, want)
	}
}

// The token settlement is keyed on the model the client asked for, not the one
// that served it, exactly as the standalone SettleTokens call is: a per-model
// TPM bucket belongs to the requested model.
func TestSettleFusedPlansTokensForTheModelTheClientAsked(t *testing.T) {
	gate := &gateBackend{}
	limiter := ratelimit.NewRedisKeyLimiter([]ratelimit.KeyConfig{{
		ID: "fz", Capacity: 100, RefillPerSecond: 100,
		PerModel: map[string]ratelimit.ModelRateLimit{"asked-model": {Capacity: 10, RefillPerSecond: 10, TPMCapacity: 500, TPMRefillPerSecond: 5}},
	}}, gate)
	f := &fakeFused{}
	p := &Pipeline{limiter: limiter, budget: budget.NewRedisTracker(&fusedBudgetBackend{}, discardLogger()), fused: f, logger: discardLogger()}
	rt := 42.0
	p.settleFused(context.Background(), fusedInputs{
		vk: &identity.VirtualKey{ID: "fz"}, reqModel: "asked-model", servedModel: "serving-model",
		realTokens: &rt, tpmReserved: true, tpmReservedTokens: 100, tpmReservationEpoch: 3,
	})
	want := limiter.PlanSettle("fz", "asked-model", ratelimit.TokenReservation{Reserved: true, Tokens: 100, Epoch: 3}, ratelimit.TokenReservation{}, &rt)
	if len(want) == 0 {
		t.Fatal("control: the per-model bucket planned no settlement for the asked model")
	}
	if len(f.reqs) != 1 || !reflect.DeepEqual(f.reqs[0].Tokens, want) {
		t.Errorf("token settlements = %+v, want %+v (keyed on the asked model)", f.reqs, want)
	}
}

// A pipeline without a ledger (tests only; cmd/gateway always wires one) sends
// no ledger part, however the request billed.
func TestFinalizeFusedSendsNoLedgerPartWithoutALedger(t *testing.T) {
	r := newFusedRig(t, nil, nil)
	r.p.ledger = nil
	if _, err := r.p.HandleChatCompletion(context.Background(), "Bearer fz", "", "", fusedChatReq, ""); err != nil {
		t.Fatal(err)
	}
	if req := r.onlyRequest(t); req.Ledger != nil {
		t.Errorf("ledger part = %+v, want none: the pipeline has no ledger", req.Ledger)
	}
}

// The all-applied case leaves no failure trace anywhere and feeds the threshold
// checks the spend the settlement reported.
func TestFinalizeFusedAllPartsAppliedLeavesNoFailureTrace(t *testing.T) {
	r := newFusedRig(t, nil, nil)
	r.fused.result.SpentAfterNanoUSD = 6_000_000_000 // past the 50% warn mark of the 10 USD cap
	if _, err := r.p.HandleChatCompletion(context.Background(), "Bearer fz", "", "", fusedChatReq, ""); err != nil {
		t.Fatal(err)
	}
	logs := r.logs.String()
	for _, absent := range []string{"budget_redis_backend_unavailable", "ledger_record_failed", "budget_threshold_checks_skipped"} {
		if strings.Contains(logs, absent) {
			t.Errorf("a fully applied settlement logged %s: %s", absent, logs)
		}
	}
	if !strings.Contains(logs, `"msg":"budget_warn_threshold_crossed"`) {
		t.Errorf("the warn check did not read the reported spend: %s", logs)
	}
}

// A requested part the settler reports as skipped (a settler bug, never the
// real one) is a lost part with a generic cause, not a silent success.
func TestFinalizeFusedTreatsARequestedButSkippedPartAsLost(t *testing.T) {
	r := newFusedRig(t, nil, nil)
	r.fused.result = redisfused.Result{}
	if _, err := r.p.HandleChatCompletion(context.Background(), "Bearer fz", "", "", fusedChatReq, ""); err != nil {
		t.Fatal(err)
	}
	logs := r.logs.String()
	for _, want := range []string{"fused settlement did not apply the budget part", "fused settlement did not apply the ledger part", `"msg":"budget_threshold_checks_skipped"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q: %s", want, logs)
		}
	}
	if n := strings.Count(logs, `"msg":"ledger_record_failed"`); n != 1 {
		t.Errorf("ledger_record_failed lines = %d, want 1", n)
	}
}

// The ledger row carries the model that SERVED the request (not the one the
// client asked for, the cross-model fallback billing guard), the client tool and
// a valid session id.
func TestSettleFusedBuildsTheLedgerEntryFromTheServedModelAndTheAttribution(t *testing.T) {
	f := &fakeFused{result: redisfused.Result{Budget: redisfused.Part{Status: redisfused.StatusApplied}, Ledger: redisfused.Part{Status: redisfused.StatusApplied}}}
	limiter := ratelimit.NewRedisKeyLimiter([]ratelimit.KeyConfig{{ID: "fz", Capacity: 100, RefillPerSecond: 100}}, &gateBackend{})
	p := &Pipeline{limiter: limiter, budget: budget.NewRedisTracker(&fusedBudgetBackend{}, discardLogger()), fused: f, logger: discardLogger(),
		ledger: spendledger.NewMemory(spendledger.Options{}), now: func() time.Time { return time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC) }}
	cost := decimal.RequireFromString("0.011")
	p.settleFused(context.Background(), fusedInputs{
		vk: &identity.VirtualKey{ID: "fz"}, reqModel: "asked-model", servedModel: "serving-model",
		cost: cost, realCost: &cost, attribution: telemetry.Attribution{ClientTool: "curl", SessionID: "sess-1"},
	})
	if len(f.reqs) != 1 || f.reqs[0].Ledger == nil {
		t.Fatalf("requests = %+v, want one carrying a ledger entry", f.reqs)
	}
	e := f.reqs[0].Ledger
	if e.Key != "fz" || e.Model != "serving-model" || e.Tool != "curl" || e.Session != "sess-1" || e.Requests != 1 || e.SpendNanoUSD != fusedCostNano || !e.At.Equal(time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("ledger entry = %+v, want fz / serving-model / curl / sess-1 / 1 request / %d nano-USD at the pipeline clock", e, fusedCostNano)
	}
}

// A stream that fails after real content reached the client is still billed (the
// standalone path's guard, billable = firstChunkSent), and the fused call carries
// the bill: a billed budget part, the real token estimate and the ledger row.
func TestFinalizeFusedBillsAStreamThatEndsAfterRealContent(t *testing.T) {
	content := "" +
		`data: {"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"` + strings.Repeat("a", 20) + `"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"` + strings.Repeat("b", 20) + `"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	cut := strings.Index(content, strings.Repeat("b", 20)) + 20 + len(`"},"finish_reason":null}]}`) + len("\n\n") + 5
	r := newFusedRigWithStream(t, nil, nil, func(context.Context, Deployment, any) (io.ReadCloser, error) {
		return nopCloserReader{&failAfterNBytesReader{r: strings.NewReader(content), n: cut}}, nil
	})
	rec := httptest.NewRecorder()
	req := adapter.ChatRequest{Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	_ = r.p.HandleChatCompletionStream(context.Background(), "Bearer fz", "", "", req, rec, "")
	got := r.onlyRequest(t)
	if got.Budget == nil || !got.Budget.Billed {
		t.Errorf("budget part = %+v, want a billed adjust for the real content delivered", got.Budget)
	}
	if got.Ledger == nil {
		t.Error("no ledger entry for a stream that delivered real content")
	}
	for _, s := range got.Tokens {
		if s.RealTokens == nil {
			t.Errorf("token settlement %+v is a release; the delivered content bills an estimated count", s)
		}
	}
	r.assertNoStandaloneSettlement(t)
}
