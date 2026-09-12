package dataplane

// Full-pipeline load-bearing proofs for the mid-stream reservation
// top-up guard (streamrunaway.go's checkMidStreamReservationTopup),
// closing the streaming concurrent-sibling reservation gap per
// docs/upgrade-research/gateway-streaming-concurrent-sibling-
// reservation-gap-2026-09-09.md: a long-running stream's real cost can
// grow well past its own initial (historical-average) reservation over
// the stream's FULL DURATION, silently understating its true claim on
// the key's headroom to a concurrent sibling for far longer than one
// HTTP round-trip — the gap docs/rfcs/2026-09-08-gateway-streaming-
// runaway-completion-guard.md's own case text explicitly, honestly left
// open ("a live recheck against a concurrent sibling's real-time spend
// remains a separate, still-open limitation").

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// topupGateSSEReader is runawaySSEReader's gated sibling (streamrunaway_test.go):
// serves exactly totalFrames real SSE content frames, then signals
// started and BLOCKS on release before finally returning io.EOF —
// letting a test observe the pipeline's own internal state (budget
// reservation) while a stream is deliberately held open mid-flight,
// rather than only after it has fully completed.
type topupGateSSEReader struct {
	chunkChars  int
	totalFrames int
	started     chan struct{}
	release     chan struct{}

	mu           sync.Mutex
	framesServed int
	pending      []byte
	startedOnce  sync.Once
}

func newTopupGateSSEReader(chunkChars, totalFrames int) *topupGateSSEReader {
	return &topupGateSSEReader{
		chunkChars:  chunkChars,
		totalFrames: totalFrames,
		started:     make(chan struct{}),
		release:     make(chan struct{}),
	}
}

func (r *topupGateSSEReader) Read(p []byte) (int, error) {
	if len(r.pending) == 0 {
		r.mu.Lock()
		if r.framesServed >= r.totalFrames {
			r.mu.Unlock()
			r.startedOnce.Do(func() { close(r.started) })
			<-r.release
			return 0, io.EOF
		}
		r.framesServed++
		r.mu.Unlock()
		content := strings.Repeat("x", r.chunkChars)
		r.pending = []byte(`data: {"id":"chatcmpl-topup","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"` + content + `"},"finish_reason":null}]}` + "\n\n")
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *topupGateSSEReader) Close() error { return nil }

// newTopupTestPipeline builds a Pipeline with a real, non-empty price
// table (unlike every other streaming test helper in this package, which
// use an empty PriceTable — this test's whole point is a real dollar
// cost estimate) and the given tracker, so a test can seed billing
// history on that same tracker before ever calling HandleChatCompletionStream.
func newTopupTestPipeline(t *testing.T, tracker *budget.Tracker, capUSD decimal.Decimal, upstreamStream UpstreamStreamCaller, logger *slog.Logger) *Pipeline {
	t.Helper()
	keys := []identity.VirtualKey{
		{ID: "topup-key", KeyHash: testHashOf("topup-secret"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: capUSD},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := NewPipeline(Config{
		Verifier:    verifier,
		Limiter:     ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:      tracker,
		Cache:       inprocess.New(0),
		CacheL2:     inprocess.New(0),
		CacheL3:     inprocess.NewLexicalCache(0),
		Guardrails:  guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:    adapter.Registry{"openai": openai.New()},
		Router:      testRouter(deployments),
		Deployments: deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{
			"gpt-4o": {CompletionPerToken: decimal.NewFromFloat(0.01)},
		}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream should never be called by this test")
			return nil, nil
		},
		UpstreamStream: upstreamStream,
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestHandleChatCompletionStreamMidStreamTopupIncreasesReservationWhileStreamIsInFlight
// is the core, direct proof this fix exists for: while a long-running
// stream is still in flight, its own outstanding budget reservation is
// topped up to reflect its REAL, growing output — visible to a
// concurrent sibling as reduced remaining headroom — and, once the
// stream finishes (with no real usage frame from this mock, so its real
// cost settles at $0), the transient top-up is cleanly undone with no
// permanent over- or under-counting left behind.
func TestHandleChatCompletionStreamMidStreamTopupIncreasesReservationWhileStreamIsInFlight(t *testing.T) {
	tracker := budget.NewTracker()
	capUSD := decimal.NewFromFloat(100.00) // generous — this test proves top-up SUCCEEDS, not the rejection boundary (see the sibling exhaustion test for that)
	// Seed billing history directly (bypassing a real priming HTTP
	// round-trip): a single real $0.05 cost (5 completion tokens at
	// $0.01/token) establishes billedCount=1, so the TARGET request's own
	// Reserve call below sizes off that small historical average ($0.05)
	// rather than grabbing the full $100 cap outright.
	_, reserved, primingReservation, primingEpoch := tracker.Reserve("topup-key", capUSD, 0)
	if !reserved {
		t.Fatal("setup: priming Reserve did not reserve anything")
	}
	primingRealCost := decimal.NewFromFloat(0.05)
	tracker.Reconcile("topup-key", primingReservation, primingEpoch, &primingRealCost, 0)
	if spent := tracker.SpentUSD("topup-key", 0); !spent.Equal(decimal.NewFromFloat(0.05)) {
		t.Fatalf("setup: SpentUSD after priming = %s, want 0.05", spent)
	}

	const chunkChars = 40 // 10 estimated tokens/frame at streamRunawayCharsPerToken=4
	const gateAfterFrames = 10
	reader := newTopupGateSSEReader(chunkChars, gateAfterFrames)
	p := newTopupTestPipeline(t, tracker, capUSD, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		return reader, nil
	}, discardLogger())

	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		rec := httptest.NewRecorder()
		err := p.HandleChatCompletionStream(context.Background(), "Bearer topup-secret", adapter.ChatRequest{
			Model: "gpt-4o", Stream: true,
			Messages: []adapter.Message{{Role: "user", Content: "generate a long response"}},
		}, rec)
		done <- result{err: err}
	}()

	select {
	case <-reader.started:
	case <-done:
		t.Fatal("stream finished before the gate was ever reached")
	}

	// While the stream is held open at the gate: 10 frames * 40 chars =
	// 400 accumulated chars -> 100 estimated completion tokens -> $1.00
	// estimated cost, which exceeds the target's own initial $0.05
	// reservation on frame 1 and keeps growing every frame after —
	// topping the reservation up to exactly $1.00 by frame 10 (each
	// frame's own delta fits comfortably under the $1.00 cap, verified by
	// hand in this test's own accompanying commit). Priming's $0.05 (real,
	// permanent) + the target's now-$1.00 (still outstanding) reservation
	// = $1.05 total tracked spend — MORE than the target's own tiny
	// initial reservation would show, exactly the concurrent-sibling-
	// visible signal this fix exists to produce.
	gotSpent := tracker.SpentUSD("topup-key", 0)
	wantSpent := decimal.NewFromFloat(1.05)
	if !gotSpent.Equal(wantSpent) {
		t.Fatalf("SpentUSD while the stream is held open mid-flight = %s, want %s — the mid-stream top-up should have raised the reservation to reflect real, growing output", gotSpent, wantSpent)
	}

	close(reader.release)
	res := <-done
	if res.err != nil {
		t.Fatalf("HandleChatCompletionStream: %v, want nil", res.err)
	}

	// This mock never sends a usage frame, so the target's own REAL cost
	// settles at $0 (stream_missing_usage, per finishStreamedResponse) —
	// Reconcile must cleanly undo the transient $1.00 top-up, leaving
	// final spend at exactly the priming amount, never a permanent leak
	// or double-count from the top-up mechanism itself.
	finalSpent := tracker.SpentUSD("topup-key", 0)
	if !finalSpent.Equal(decimal.NewFromFloat(0.05)) {
		t.Fatalf("final SpentUSD after the stream completed = %s, want 0.05 — the transient top-up must be fully undone by Reconcile, leaving no permanent trace", finalSpent)
	}
}

// TestHandleChatCompletionStreamMidStreamTopupExhaustionGracefullyTruncatesStream
// proves the failure/rejection path: once the mid-stream top-up would
// exceed the key's own remaining budget headroom, the stream is stopped
// exactly like a runaway-completion-ceiling trip — gracefully, as a
// normal truncated-but-valid response, never as an error — and a real,
// distinctly-named log line records why.
func TestHandleChatCompletionStreamMidStreamTopupExhaustionGracefullyTruncatesStream(t *testing.T) {
	tracker := budget.NewTracker()
	capUSD := decimal.NewFromFloat(1.00) // deliberately tight — this test proves the REJECTION boundary
	_, reserved, primingReservation, primingEpoch := tracker.Reserve("topup-key", capUSD, 0)
	if !reserved {
		t.Fatal("setup: priming Reserve did not reserve anything")
	}
	primingRealCost := decimal.NewFromFloat(0.05)
	tracker.Reconcile("topup-key", primingReservation, primingEpoch, &primingRealCost, 0)

	const chunkChars = 40 // 10 estimated tokens/frame -> $0.10 estimated cost/frame
	// An upstream willing to stream far more than the budget allows —
	// mirroring runawaySSEReader's own "safety net, not a tight bound"
	// design: a genuinely broken guard would exhaust maxFrames instead of
	// being cut off by the top-up rejection this test expects around
	// frame 10 (hand-traced: reservation grows by $0.10/frame from a
	// $0.05 base under a $1.00 cap, rejected the first time spent+delta
	// would exceed $1.00).
	const maxFrames = 1000
	reader := newRunawaySSEReader(context.Background(), chunkChars, maxFrames)
	var logBuf bytes.Buffer
	p := newTopupTestPipeline(t, tracker, capUSD, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		return reader, nil
	}, slog.New(slog.NewJSONHandler(&logBuf, nil)))

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer topup-secret", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
		Messages: []adapter.Message{{Role: "user", Content: "generate as much as you possibly can"}},
	}, rec)
	if err != nil {
		t.Fatalf("HandleChatCompletionStream: %v, want nil — a top-up exhaustion must finish as a normal, truncated-but-valid stream, not an error", err)
	}

	framesServed := reader.FramesServed()
	if framesServed >= maxFrames {
		t.Fatalf("framesServed = %d, want far fewer than maxFrames(%d) — the top-up guard did not cut the stream off at all", framesServed, maxFrames)
	}
	if framesServed > 15 {
		t.Fatalf("framesServed = %d, want close to the hand-traced rejection point (frame 10)", framesServed)
	}

	// spent must be left EXACTLY at whatever the last SUCCESSFUL top-up
	// (or the initial reservation, if none succeeded) set it to — never
	// increased by the rejected attempt itself, per IncreaseReservation's
	// own "leave completely unchanged on rejection" contract.
	finalSpent := tracker.SpentUSD("topup-key", 0)
	if finalSpent.GreaterThan(decimal.NewFromFloat(1.00)) {
		t.Fatalf("SpentUSD after the guard tripped = %s, want <= the $1.00 cap — a rejected top-up must never push spend past the cap it was rejected FOR exceeding", finalSpent)
	}

	if got := logBuf.String(); !strings.Contains(got, "streaming_midstream_reservation_topup_exhausted") {
		t.Fatalf("expected a streaming_midstream_reservation_topup_exhausted warning log line, got: %s", got)
	}
}
