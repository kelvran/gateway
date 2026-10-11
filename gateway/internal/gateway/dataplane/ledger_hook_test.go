package dataplane

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/spendledger"
	"github.com/kelvran/gateway/gateway/internal/spendledger/boltstore"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// ledgerTestPipeline is pricedTestPipeline with a spend ledger and two
// virtual keys, for the item 13d slice-3 write-hook tests: `test-key` has a
// real cap, so the budget tracker records its spend and the ledger's nano-USD
// can be compared with it (a capped key reserves its full headroom cold, so
// it is used sequentially); `other-key` is unlimited, for concurrent bursts.
// A nil upstream fails the test if it is ever called.
func ledgerTestPipeline(t *testing.T, upstream UpstreamCaller, embeddingUpstream UpstreamCaller, ledger spendledger.Ledger) (*Pipeline, *budget.Tracker) {
	t.Helper()
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("1000")},
		{ID: "other-key", KeyHash: testHashOf("other-key"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	if upstream == nil {
		upstream = func(context.Context, Deployment, any) (any, error) {
			t.Fatal("the chat upstream must not be called by this test")
			return nil, nil
		}
	}
	if embeddingUpstream == nil {
		embeddingUpstream = func(context.Context, Deployment, any) (any, error) {
			t.Fatal("the embeddings upstream must not be called by this test")
			return nil, nil
		}
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	tracker := budget.NewTracker()
	deployments := []Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "emb1", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"},
	}
	p, err := NewPipeline(Config{
		Verifier:    verifier,
		Limiter:     ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:      tracker,
		Ledger:      ledger,
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
		EmbeddingUpstream: embeddingUpstream,
		Logger:            discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p, tracker
}

func ledgerRows(t *testing.T, l spendledger.Ledger, groupBy, key string) []spendledger.Row {
	t.Helper()
	rows, err := l.Query(context.Background(), spendledger.Query{GroupBy: groupBy, Key: key, Since: time.Now().Add(-2 * time.Hour), Until: time.Now().Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func describeLedger(rows []spendledger.Row) string {
	var parts []string
	for _, r := range rows {
		parts = append(parts, r.Key+"/"+r.Model+"/"+r.Tool+"/"+r.Session+"="+decimal.NewFromInt(r.SpendNanoUSD).String()+":"+decimal.NewFromInt(r.Requests).String())
	}
	return strings.Join(parts, " ")
}

var ledgerChatReq = adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}

// TestFinalizeRecordsSpendInTheLedgerOnlyWhenBillable: a real upstream call
// writes one entry whose spend equals the budget tracker's charge, with the
// served model, the client tool and the session; ten coalesced identical
// misses write one entry; a cache hit writes nothing — the ledger is fed
// under the exact `billable` gate that already feeds kelvran.llm.spend_usd.
func TestFinalizeRecordsSpendInTheLedgerOnlyWhenBillable(t *testing.T) {
	ledger := spendledger.NewMemory(spendledger.Options{})
	var upstreamCalls atomic.Int64
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	p, tracker := ledgerTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if upstreamCalls.Add(1) == 2 { // the burst's one real call; the first call below is sequential
			once.Do(func() { close(started) })
			<-release
		}
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, nil, ledger)
	ctx := telemetry.WithAttribution(context.Background(), telemetry.Attribution{ClientTool: "curl", SessionID: "sess-1"})

	// One sequential real call on the capped key: the ledger's nano-USD is
	// the tracker's charge, keyed by served model, tool and session.
	if _, err := p.HandleChatCompletion(ctx, "Bearer test-key", "", "", ledgerChatReq, ""); err != nil {
		t.Fatal(err)
	}
	spent := tracker.SpentUSD(context.Background(), "test-key", 0)
	if spent.IsZero() {
		t.Fatal("the budget tracker recorded no spend; the price table fixture is broken")
	}
	nano := decimal.NewFromInt(budget.USDToNanoUSD(spent)).String()
	for groupBy, want := range map[string]string{
		spendledger.GroupBySession: "test-key///sess-1=" + nano + ":1",
		spendledger.GroupByModel:   "/gpt-4o//=" + nano + ":1",
		spendledger.GroupByTool:    "//curl/=" + nano + ":1",
	} {
		if got := describeLedger(ledgerRows(t, ledger, groupBy, "")); got != want {
			t.Fatalf("ledger by %s after one real call = %q, want %q", groupBy, got, want)
		}
	}

	// A cache hit replays no spend.
	if _, err := p.HandleChatCompletion(ctx, "Bearer test-key", "", "", ledgerChatReq, ""); err != nil {
		t.Fatal(err)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstreamCalls after the cache hit = %d, want still 1", upstreamCalls.Load())
	}
	if got := describeLedger(ledgerRows(t, ledger, spendledger.GroupBySession, "test-key")); got != "test-key///sess-1="+nano+":1" {
		t.Fatalf("ledger after a cache hit = %q, want unchanged", got)
	}

	// Ten concurrent identical misses on the unlimited key: one upstream
	// call, one ledger entry.
	burstReq := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "burst"}}}
	const n = 10
	var ready, done sync.WaitGroup
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer done.Done()
			ready.Done()
			if _, err := p.HandleChatCompletion(ctx, "Bearer other-key", "", "", burstReq, ""); err != nil {
				t.Errorf("coalesced call: %v", err)
			}
		}()
	}
	ready.Wait()
	<-started
	time.Sleep(20 * time.Millisecond) // let the followers queue behind the leader
	close(release)
	done.Wait()
	if upstreamCalls.Load() != 2 {
		t.Fatalf("upstreamCalls after the burst = %d, want 2 (one sequential, one for the burst)", upstreamCalls.Load())
	}
	if got := describeLedger(ledgerRows(t, ledger, spendledger.GroupBySession, "other-key")); got != "other-key///sess-1="+nano+":1" {
		t.Fatalf("ledger for the burst key = %q, want one entry of one request", got)
	}
}

// TestLedgerSessionPrefersClaudeCodeSessionThenAgentRunIDThatPassesTheGrammar:
// the session dimension is the Claude Code session id when present, else the
// agent_run_id Baggage member when it satisfies the identifier grammar (a
// value that does not is dropped and counted under `baggage.agent_run_id`),
// else none; a wire session equal to the fold target's name is dropped too
// (counted under its header), so `other` stays derived state only.
func TestLedgerSessionPrefersClaudeCodeSessionThenAgentRunIDThatPassesTheGrammar(t *testing.T) {
	withRun := func(ctx context.Context, id string) context.Context {
		m, err := baggage.NewMember("agent_run_id", id)
		if err != nil {
			t.Fatal(err)
		}
		b, err := baggage.New(m)
		if err != nil {
			t.Fatal(err)
		}
		return baggage.ContextWithBaggage(ctx, b)
	}
	cases := []struct {
		name        string
		ctx         context.Context
		wantSession string
		wantDropped string // header name counted, "" for none
	}{
		{"claude code session id wins", withRun(telemetry.WithAttribution(context.Background(), telemetry.Attribution{SessionID: "cc-1"}), "run-1"), "cc-1", ""},
		{"agent_run_id when no session id", withRun(context.Background(), "run-1"), "run-1", ""},
		{"agent_run_id outside the grammar is dropped", withRun(context.Background(), "bad%20id"), "", "baggage.agent_run_id"},
		{"a session id naming the fold target is dropped", telemetry.WithAttribution(context.Background(), telemetry.Attribution{SessionID: spendledger.OtherSession}), "", "x-claude-code-session-id"},
		{"nothing attributed", context.Background(), "", ""},
	}
	p, _ := ledgerTestPipeline(t, nil, nil, spendledger.NewMemory(spendledger.Options{}))
	reader := dataplaneTelemetryMetricsReaderForTest()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var before, after metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &before); err != nil {
				t.Fatal(err)
			}
			got := p.sessionFor(tc.ctx, telemetry.AttributionFromContext(tc.ctx))
			if got != tc.wantSession {
				t.Fatalf("sessionFor = %q, want %q", got, tc.wantSession)
			}
			if err := reader.Collect(context.Background(), &after); err != nil {
				t.Fatal(err)
			}
			b, a := snapshotDataplaneTelemetry(t, before), snapshotDataplaneTelemetry(t, after)
			for _, header := range []string{"baggage.agent_run_id", "x-claude-code-session-id"} {
				want := int64(0)
				if header == tc.wantDropped {
					want = 1
				}
				if delta := a.attributionDroppedByHeader[header] - b.attributionDroppedByHeader[header]; delta != want {
					t.Errorf("kelvran.attribution.dropped{%s} delta = %d, want %d", header, delta, want)
				}
			}
		})
	}
}

// TestHandleEmbeddingsRecordsSpendInTheLedger: the embeddings site records
// the same way, with the served model.
func TestHandleEmbeddingsRecordsSpendInTheLedger(t *testing.T) {
	ledger := spendledger.NewMemory(spendledger.Options{})
	p, tracker := ledgerTestPipeline(t, nil, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return &openai.EmbeddingResponseWire{
			Model: "text-embedding-3-small",
			Data:  []openai.EmbeddingDataWire{{Index: 0, Embedding: []float64{0.1, 0.2}}},
			Usage: openai.EmbeddingUsageWire{PromptTokens: 20, TotalTokens: 20},
		}, nil
	}, ledger)
	ctx := telemetry.WithAttribution(context.Background(), telemetry.Attribution{ClientTool: "openai_python", SessionID: "sess-e"})
	if _, err := p.HandleEmbeddings(ctx, "Bearer test-key", "", adapter.EmbeddingRequest{Model: "text-embedding-3-small", Input: []string{"hello"}}); err != nil {
		t.Fatal(err)
	}
	spent := tracker.SpentUSD(context.Background(), "test-key", 0)
	if spent.IsZero() {
		t.Fatal("the budget tracker recorded no embeddings spend; the price table fixture is broken")
	}
	nano := decimal.NewFromInt(budget.USDToNanoUSD(spent)).String()
	if got := describeLedger(ledgerRows(t, ledger, spendledger.GroupBySession, "test-key")); got != "test-key///sess-e="+nano+":1" {
		t.Fatalf("ledger by session after an embeddings call = %q", got)
	}
	if got := describeLedger(ledgerRows(t, ledger, spendledger.GroupByModel, "test-key")); got != "test-key/text-embedding-3-small//="+nano+":1" {
		t.Fatalf("ledger by model after an embeddings call = %q, want the served model", got)
	}
	if got := describeLedger(ledgerRows(t, ledger, spendledger.GroupByTool, "test-key")); got != "test-key//openai_python/="+nano+":1" {
		t.Fatalf("ledger by tool after an embeddings call = %q", got)
	}
}

// TestDeleteVirtualKeyCascadesToTheLedger: revoking a key erases its ledger
// rows on this replica, like its budget state; the other key's rows stay.
func TestDeleteVirtualKeyCascadesToTheLedger(t *testing.T) {
	ledger := spendledger.NewMemory(spendledger.Options{})
	p, _ := ledgerTestPipeline(t, nil, nil, ledger)
	now := time.Now()
	for _, key := range []string{"test-key", "other-key"} {
		if err := ledger.Record(context.Background(), spendledger.Entry{Key: key, Model: "gpt-4o", Tool: "curl", Session: "s1", At: now, SpendNanoUSD: 5, Requests: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.DeleteVirtualKey("test-key"); err != nil {
		t.Fatal(err)
	}
	if got := describeLedger(ledgerRows(t, ledger, spendledger.GroupByKey, "")); got != "other-key///=5:1" {
		t.Fatalf("ledger after DeleteVirtualKey(test-key) = %q, want only other-key", got)
	}
}

// TestBackupStoresIncludesTheLedgerFile: a bbolt-backed ledger is a backup
// candidate named `ledger-<stamp>.bbolt`; the in-memory ledger is skipped;
// Pipeline.Close closes the ledger.
func TestBackupStoresIncludesTheLedgerFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	store, err := boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := ledgerTestPipeline(t, nil, nil, store)
	files, err := p.BackupStores(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || !strings.HasPrefix(files[0], "ledger-") || !strings.HasSuffix(files[0], ".bbolt") {
		t.Fatalf("BackupStores = %v, want exactly one ledger-<stamp>.bbolt", files)
	}
	// Pipeline.Close closes the ledger too: the file lock is released, so a
	// reopen succeeds instead of timing out after one second.
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := boltstore.Open(path)
	if err != nil {
		t.Fatalf("reopen after Pipeline.Close: %v (the ledger was not closed)", err)
	}
	_ = reopened.Close()
	pm, _ := ledgerTestPipeline(t, nil, nil, spendledger.NewMemory(spendledger.Options{}))
	if files, err := pm.BackupStores(t.TempDir()); err != nil || len(files) != 0 {
		t.Fatalf("BackupStores with an in-memory ledger = %v, %v; want nothing", files, err)
	}
}

// failingLedger is a Ledger whose writes fail, for the failure-posture test.
type failingLedger struct{ spendledger.Ledger }

func (failingLedger) Record(context.Context, spendledger.Entry) error {
	return errors.New("ledger down")
}

// TestLedgerRecordFailureIsCountedAndNeverFailsTheRequest: a ledger write
// error is one Warn plus kelvran.persistence.failed{store_kind=ledger}; the
// client still gets its response.
func TestLedgerRecordFailureIsCountedAndNeverFailsTheRequest(t *testing.T) {
	p, _ := ledgerTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, nil, failingLedger{spendledger.NewMemory(spendledger.Options{})})
	reader := dataplaneTelemetryMetricsReaderForTest()
	var before, after metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &before); err != nil {
		t.Fatal(err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", ledgerChatReq, ""); err != nil {
		t.Fatalf("a ledger failure must never fail the request: %v", err)
	}
	if err := reader.Collect(context.Background(), &after); err != nil {
		t.Fatal(err)
	}
	b, a := snapshotDataplaneTelemetry(t, before), snapshotDataplaneTelemetry(t, after)
	if delta := a.persistenceFailedByStoreKind["ledger"] - b.persistenceFailedByStoreKind["ledger"]; delta != 1 {
		t.Fatalf("kelvran.persistence.failed{store_kind=ledger} delta = %d, want 1", delta)
	}
}

// countingLedger counts Sweep calls, for the sweep-loop test.
type countingLedger struct {
	spendledger.Ledger
	sweeps atomic.Int64
}

func (c *countingLedger) Sweep(ctx context.Context, now time.Time) error {
	c.sweeps.Add(1)
	return c.Ledger.Sweep(ctx, now)
}

// TestRunLedgerSweepLoopSweepsOnStartThenEveryInterval: the loop sweeps
// once immediately (a restart after days must drop expired hours at once;
// with an hour-long interval that first sweep is the only one), then every
// interval, and returns when its context ends. No ledger: returns at once.
func TestRunLedgerSweepLoopSweepsOnStartThenEveryInterval(t *testing.T) {
	waitFor := func(t *testing.T, l *countingLedger, n int64) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for l.sweeps.Load() < n && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		if got := l.sweeps.Load(); got < n {
			t.Fatalf("sweeps = %d, want at least %d", got, n)
		}
	}
	run := func(t *testing.T, l *countingLedger, interval time.Duration) (context.CancelFunc, chan struct{}) {
		p, _ := ledgerTestPipeline(t, nil, nil, l)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			p.RunLedgerSweepLoop(ctx, interval)
			close(done)
		}()
		return cancel, done
	}
	stop := func(t *testing.T, cancel context.CancelFunc, done chan struct{}) {
		t.Helper()
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("RunLedgerSweepLoop did not return after its context ended")
		}
	}

	slow := &countingLedger{Ledger: spendledger.NewMemory(spendledger.Options{})}
	cancel, done := run(t, slow, time.Hour)
	waitFor(t, slow, 1) // the sweep at start, long before the first tick
	time.Sleep(30 * time.Millisecond)
	if n := slow.sweeps.Load(); n != 1 {
		t.Fatalf("sweeps with an hour-long interval = %d, want exactly the one at start", n)
	}
	stop(t, cancel, done)

	fast := &countingLedger{Ledger: spendledger.NewMemory(spendledger.Options{})}
	cancel, done = run(t, fast, 5*time.Millisecond)
	waitFor(t, fast, 3) // start plus at least two ticks
	stop(t, cancel, done)

	pn, _ := ledgerTestPipeline(t, nil, nil, nil)
	pn.RunLedgerSweepLoop(context.Background(), time.Millisecond) // no ledger: returns at once
}

// TestQuerySpendAndEraseSpendSessionWrapTheLedger: the admin routes read and
// erase through the pipeline, which converts nano-USD to decimal USD (admin
// cannot import budget); with no ledger both report ErrNoSpendLedger.
func TestQuerySpendAndEraseSpendSessionWrapTheLedger(t *testing.T) {
	ledger := spendledger.NewMemory(spendledger.Options{})
	p, _ := ledgerTestPipeline(t, nil, nil, ledger)
	ctx := context.Background()
	now := time.Now()
	if err := ledger.Record(ctx, spendledger.Entry{Key: "test-key", Model: "gpt-4o", Tool: "curl", Session: "s1", At: now, SpendNanoUSD: 1_250_000_000, Requests: 2}); err != nil {
		t.Fatal(err)
	}
	rows, err := p.QuerySpend(ctx, spendledger.Query{GroupBy: spendledger.GroupBySession, Since: now.Add(-time.Hour), Until: now.Add(time.Hour)})
	if err != nil || len(rows) != 1 || rows[0].Key != "test-key" || rows[0].Session != "s1" || rows[0].SpendUSD.String() != "1.25" || rows[0].Requests != 2 {
		t.Fatalf("QuerySpend = %+v, %v; want one row test-key/s1 1.25 USD, 2 requests", rows, err)
	}
	if _, err := p.QuerySpend(ctx, spendledger.Query{GroupBy: "hour"}); !errors.Is(err, spendledger.ErrInvalidGroupBy) {
		t.Fatalf("QuerySpend with an unknown group_by: %v, want ErrInvalidGroupBy", err)
	}
	n, err := p.EraseSpendSession(ctx, "s1")
	if err != nil || n != 1 {
		t.Fatalf("EraseSpendSession = %d, %v; want 1 cell", n, err)
	}
	if _, err := p.EraseSpendSession(ctx, spendledger.OtherSession); !errors.Is(err, spendledger.ErrSessionNotErasable) {
		t.Fatalf("EraseSpendSession(other): %v, want ErrSessionNotErasable", err)
	}
	none, _ := ledgerTestPipeline(t, nil, nil, nil)
	if _, err := none.QuerySpend(ctx, spendledger.Query{GroupBy: spendledger.GroupByKey}); !errors.Is(err, ErrNoSpendLedger) {
		t.Fatalf("QuerySpend without a ledger: %v, want ErrNoSpendLedger", err)
	}
	if _, err := none.EraseSpendSession(ctx, "s1"); !errors.Is(err, ErrNoSpendLedger) {
		t.Fatalf("EraseSpendSession without a ledger: %v, want ErrNoSpendLedger", err)
	}
}
