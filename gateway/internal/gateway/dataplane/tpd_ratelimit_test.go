package dataplane

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/admission"
)

// Round-5 Tier S item 24 (2026-10-15): tokens_per_day on the virtual key,
// a second token dimension on the TPM reservation path. These tests pin
// the request-level behaviour: the third request of an exhausted day is a
// 429 whose Retry-After is floored on the day bucket's own wait (hours in
// these fixtures, capped at a day) over the per-key backoff; a day rejection releases the
// minute reservation taken just before it; a day backend error fails open
// without dropping that reservation; the mid-stream top-up covers both
// dimensions; and the propagation payload carries the field.

// dayLimiter is tpmTestPipeline's limiter with a tokens_per_day beside a
// frozen (refill 0) TPM bucket, both on "test-key".
func dayLimiter(tokensPerDay, tpmCapacity float64) *ratelimit.KeyLimiter {
	return ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{
		{ID: "test-key", Capacity: 100, RefillPerSecond: 100, TPMCapacity: tpmCapacity, TPMRefillPerSecond: 0, TokensPerDay: tokensPerDay},
	})
}

// tpdTestPipeline mirrors tpmTestPipeline with an explicit limiter.
func tpdTestPipeline(t *testing.T, upstream UpstreamCaller, limiter *ratelimit.KeyLimiter) *Pipeline {
	t.Helper()
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        limiter,
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream:       upstream,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

func dayChatRequest(i int) adapter.ChatRequest {
	return adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "request number " + strings.Repeat("x", i)}}}
}

// retryAfterOf returns the Retry-After attachRetryAfter wrapped around err,
// zero when none was attached.
func retryAfterOf(err error) time.Duration {
	var ra *RetryAfterError
	if errors.As(err, &ra) {
		return ra.RetryAfter
	}
	return 0
}

// A 10-token day with no TPM: two 8-token requests pass (the second into
// overdraft), the third is a rate-limit rejection that never reaches the
// upstream and carries the day bucket's own wait -- about 14.4 h for 6
// tokens of debt at 10/86400 per second -- not the 1-30 s backoff.
func TestHandleChatCompletionRejectsOnceTheDayBucketIsExhaustedWithTheDayWait(t *testing.T) {
	var upstreamCalls int
	p := tpdTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, dayLimiter(10, 0))

	for i := 1; i <= 2; i++ {
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", dayChatRequest(i), ""); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", dayChatRequest(3), "")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("call 3 err = %v, want ErrRateLimited", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2 (call 3 must never reach the upstream)", upstreamCalls)
	}
	if ra := retryAfterOf(err); ra < 14*time.Hour || ra > 24*time.Hour {
		t.Errorf("call 3 Retry-After = %v, want the day bucket's wait (about 14.4 h), capped at 24 h", ra)
	}
	if !strings.Contains(err.Error(), "tokens_per_day") {
		t.Errorf("call 3 error %q should name the dimension that refused it", err.Error())
	}
}

// A day rejection must release the minute reservation taken just before
// it. TPM capacity 20 (frozen) with a 10-token day: after two 8-token
// calls the minute bucket holds 4 and the day bucket -6. Call 3 reserves 8
// minute tokens (4 -> -4), is refused by the day and must put them back
// (-> 4); call 4 then passes the minute check again and is refused by the
// day again -- visible as a Retry-After in hours. Had the minute
// reservation leaked, call 4 would be a MINUTE rejection carrying only the
// seconds-scale backoff.
func TestHandleChatCompletionDayRejectionReleasesTheMinuteReservation(t *testing.T) {
	var upstreamCalls int
	p := tpdTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, dayLimiter(10, 20))

	for i := 1; i <= 2; i++ {
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", dayChatRequest(i), ""); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	for i := 3; i <= 4; i++ {
		_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", dayChatRequest(i), "")
		if !errors.Is(err, ErrRateLimited) {
			t.Fatalf("call %d err = %v, want ErrRateLimited", i, err)
		}
		if ra := retryAfterOf(err); ra < time.Hour {
			t.Fatalf("call %d Retry-After = %v, want hours (a day rejection); a seconds-scale value means the minute bucket refused it, i.e. call 3 leaked its minute reservation", i, ra)
		}
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2", upstreamCalls)
	}
}

// A key without tokens_per_day behaves exactly as before.
func TestHandleChatCompletionUnaffectedByTheDayDimensionWhenNotConfigured(t *testing.T) {
	var upstreamCalls int
	p := tpdTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, dayLimiter(0, 0))
	for i := 1; i <= 5; i++ {
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", dayChatRequest(i), ""); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if upstreamCalls != 5 {
		t.Fatalf("upstreamCalls = %d, want 5", upstreamCalls)
	}
}

// recordingDayBackend is a ratelimit.RedisBackend that admits everything
// except, when asked, the day dimension (keys carrying the day tag), and
// records every reservation and settlement so the release choreography
// inside checkRateLimit can be read back.
type recordingDayBackend struct {
	mu               sync.Mutex
	refuseDay        bool
	dayErr           error
	minuteErr        error
	admits           []admission.Request
	dayRetryAfterSec float64
	reserves         []backendReserve
	settles          []backendSettle
}

type backendReserve struct {
	key       string
	coldStart float64
}

type backendSettle struct {
	key   string
	delta float64
	real  *float64
}

func isDayKey(key string) bool { return strings.Contains(key, "\x00dim=day") }

func (*recordingDayBackend) Allow(context.Context, string, float64, float64) (bool, error) {
	return true, nil
}

func (*recordingDayBackend) AllowTPM(context.Context, string, float64, float64, float64, float64) (bool, float64, error) {
	return true, 0, nil
}

func (*recordingDayBackend) AdjustTPM(context.Context, string, float64) error { return nil }

func (b *recordingDayBackend) ReserveTPM(_ context.Context, key string, _, _, _, coldStartCost float64) (bool, float64, float64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reserves = append(b.reserves, backendReserve{key: key, coldStart: coldStartCost})
	if !isDayKey(key) && b.minuteErr != nil {
		return false, 0, 0, b.minuteErr
	}
	if isDayKey(key) {
		if b.dayErr != nil {
			return false, 0, 0, b.dayErr
		}
		if b.refuseDay {
			return false, b.dayRetryAfterSec, 0, nil
		}
	}
	return true, 0, coldStartCost, nil
}

func (b *recordingDayBackend) SettleTPM(_ context.Context, key string, deltaIncrement float64, realTokens *float64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.settles = append(b.settles, backendSettle{key: key, delta: deltaIncrement, real: realTokens})
	return nil
}

// Admit is the phase-2b gate: one call for all three dimensions. Either
// configured error fails the whole gate (one error, one fail-open); refuseDay
// is a day refusal with the configured wait; otherwise both token dimensions
// reserve their cold start, the day one lowered to the minute reservation as
// the script does.
func (b *recordingDayBackend) Admit(_ context.Context, req admission.Request) (admission.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.admits = append(b.admits, req)
	if b.minuteErr != nil {
		return admission.Result{}, b.minuteErr
	}
	if b.dayErr != nil {
		return admission.Result{}, b.dayErr
	}
	tpmCost := req.TPM.ColdStart
	tpdCold := req.TPD.ColdStart
	if tpmCost > 0 && tpmCost < tpdCold {
		tpdCold = tpmCost
	}
	if b.refuseDay {
		return admission.Result{RefusedBy: ratelimit.RefusedByTPD, RetryAfterSec: b.dayRetryAfterSec, TPMCost: tpmCost, TPDCost: 1, TPDColdStart: tpdCold}, nil
	}
	return admission.Result{Allowed: true, TPMCost: tpmCost, TPDCost: tpdCold, TPDColdStart: tpdCold}, nil
}

func (b *recordingDayBackend) SettleTokens(_ context.Context, settlements []admission.Settlement) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range settlements {
		b.settles = append(b.settles, backendSettle{key: s.Key, delta: s.DeltaIncrement, real: s.RealTokens})
	}
	return nil
}

func (*recordingDayBackend) Close() error { return nil }

func redisDayPipeline(backend *recordingDayBackend) (*Pipeline, *identity.VirtualKey) {
	return redisDayPipelineFor(backend, "test-key")
}

func redisDayPipelineFor(backend *recordingDayBackend, keyID string) (*Pipeline, *identity.VirtualKey) {
	limiter := ratelimit.NewRedisKeyLimiter([]ratelimit.KeyConfig{
		{ID: keyID, Capacity: 100, RefillPerSecond: 100, TPMCapacity: 1000, TPMRefillPerSecond: 50, TokensPerDay: 86400},
	}, backend)
	return &Pipeline{limiter: limiter, logger: discardLogger()}, &identity.VirtualKey{ID: keyID}
}

// kelvran.ratelimit.fail_open counts requests ALLOWED THROUGH after a
// backend error, so the minute check's error is counted only once the day
// check has admitted the request: a request the day check then refuses is
// a 429, not a fail-open; a request whose day check errors too counts once.
func TestCheckRateLimitCountsAFailOpenOnlyWhenTheRequestIsAdmitted(t *testing.T) {
	reader := dataplaneTelemetryMetricsReaderForTest()
	var before metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &before); err != nil {
		t.Fatalf("reader.Collect (before): %v", err)
	}
	beforeSnap := snapshotDataplaneTelemetry(t, before)

	// Since round-5 item 10 phase 2b the three dimensions are one gate call:
	// an error anywhere fails the whole gate open (counted once), a day
	// refusal is a refusal with nothing counted, an admit counts nothing.
	cases := []struct {
		key     string
		backend *recordingDayBackend
		wantOK  bool
		want    int64
	}{
		{"team-day-gate-err", &recordingDayBackend{minuteErr: errors.New("simulated redis outage")}, true, 1},
		{"team-day-gate-refuses", &recordingDayBackend{refuseDay: true, dayRetryAfterSec: 3600}, false, 0},
		{"team-day-gate-admits", &recordingDayBackend{}, true, 0},
	}
	for _, c := range cases {
		p, vk := redisDayPipelineFor(c.backend, c.key)
		if d := p.checkRateLimit(context.Background(), vk, "gpt-4o"); d.ok != c.wantOK {
			t.Fatalf("%s: ok = %v, want %v (%+v)", c.key, d.ok, c.wantOK, d)
		}
	}

	var after metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &after); err != nil {
		t.Fatalf("reader.Collect (after): %v", err)
	}
	afterSnap := snapshotDataplaneTelemetry(t, after)
	for _, c := range cases {
		if got := afterSnap.failOpenByKeyID[c.key] - beforeSnap.failOpenByKeyID[c.key]; got != c.want {
			t.Errorf("%s: kelvran.ratelimit.fail_open delta = %d, want %d", c.key, got, c.want)
		}
	}
}

// Redis mode, both dimensions admit: one gate request carries both -- the
// minute cold start 1000 and the day cold cap 3600, which the script lowers
// to the minute reservation it has just sized.
func TestCheckRateLimitReservesBothTokenDimensionsThroughOneGateCallInRedisMode(t *testing.T) {
	backend := &recordingDayBackend{}
	p, vk := redisDayPipeline(backend)
	d := p.checkRateLimit(context.Background(), vk, "gpt-4o")
	if !d.ok || d.failedOpen || !d.tpm.reserved || d.tpm.tokens != 1000 || !d.tpd.reserved || d.tpd.tokens != 1000 || d.retryAfter != 0 {
		t.Fatalf("decision = %+v, want ok with 1000 reserved on both dimensions (the day cold start lowered to the minute reservation)", d)
	}
	if len(backend.admits) != 1 || len(backend.reserves) != 0 {
		t.Fatalf("admits = %d, single-dimension reserves = %d; want one gate call and no single-dimension call (phase 2b)", len(backend.admits), len(backend.reserves))
	}
	req := backend.admits[0]
	if req.RPMKey != "test-key" || req.TPM.Key != "test-key" || req.TPM.ColdStart != 1000 || !isDayKey(req.TPD.Key) || req.TPD.ColdStart != 3600 {
		t.Errorf("gate request = %+v, want the minute key with cold start 1000 and the day key with the 86400/24 cold cap", req)
	}
	if len(backend.settles) != 0 {
		t.Errorf("settles = %+v, want none before finalize", backend.settles)
	}
}

// Redis mode, the day refuses: one gate call, a rejection carrying the
// script's wait, nothing reserved and nothing to release (until phase 2b a
// second call released the minute reservation the first had taken).
func TestCheckRateLimitDayRejectionReservesNothingInRedisMode(t *testing.T) {
	backend := &recordingDayBackend{refuseDay: true, dayRetryAfterSec: 3600}
	p, vk := redisDayPipeline(backend)
	d := p.checkRateLimit(context.Background(), vk, "gpt-4o")
	if d.ok || d.failedOpen || !d.rejectedByDay || d.tpm.reserved || d.tpm.tokens != 0 || d.tpd.reserved || d.retryAfter != time.Hour {
		t.Fatalf("decision = %+v, want a day rejection with nothing reserved and a 1 h wait", d)
	}
	// Until phase 2b the day refusal had to release the minute reservation
	// the previous call had taken (one more round trip); the gate reserves
	// nothing unless all three admit, so there is nothing to release.
	if len(backend.admits) != 1 || len(backend.settles) != 0 {
		t.Fatalf("admits = %d, settles = %+v; want one gate call and no release", len(backend.admits), backend.settles)
	}
}

// Redis mode, the gate errors: fail open (the request proceeds, counted
// once) with nothing reserved -- one script, so no dimension can have
// reserved before the error.
func TestCheckRateLimitGateErrorFailsOpenWithNothingReserved(t *testing.T) {
	backend := &recordingDayBackend{dayErr: errors.New("simulated redis outage")}
	p, vk := redisDayPipeline(backend)
	d := p.checkRateLimit(context.Background(), vk, "gpt-4o")
	// One script, one error: every dimension fails open together and nothing
	// is reserved (until phase 2b a day error kept the minute reservation
	// the previous call had taken, for finalize to release).
	if !d.ok || !d.failedOpen || d.tpm.reserved || d.tpm.tokens != 0 || d.tpd.reserved || d.tpd.tokens != 0 {
		t.Fatalf("decision = %+v, want ok, failed open, nothing reserved", d)
	}
	if len(backend.settles) != 0 {
		t.Errorf("settles = %+v, want none", backend.settles)
	}
}

// attachRetryAfter floors the per-key backoff on the day wait, capped at
// 24 h; a bare rate-limit rejection keeps the backoff alone.
func TestAttachRetryAfterFloorsOnTheDayWaitCappedAtTwentyFourHours(t *testing.T) {
	p := &Pipeline{retryBackoff: ratelimit.NewRetryBackoff()}
	vk := &identity.VirtualKey{ID: "team-retry"}

	err := p.attachRetryAfter(vk, &tokensPerDayError{wait: 2 * time.Hour})
	if !errors.Is(err, ErrRateLimited) || retryAfterOf(err) != 2*time.Hour {
		t.Fatalf("2 h wait: err = %v, Retry-After %v; want ErrRateLimited with 2h", err, retryAfterOf(err))
	}
	err = p.attachRetryAfter(vk, &tokensPerDayError{wait: 90000 * time.Second})
	if retryAfterOf(err) != maxLimiterRetryAfter || maxLimiterRetryAfter != 24*time.Hour {
		t.Fatalf("90000 s wait: Retry-After %v, want the 24 h cap", retryAfterOf(err))
	}
	if ra := retryAfterOf(p.attachRetryAfter(vk, ErrRateLimited)); ra <= 0 || ra > 30*time.Second {
		t.Fatalf("bare ErrRateLimited: Retry-After %v, want the 0-30 s backoff alone", ra)
	}
}

// The mid-stream top-up tops up the day reservation too: a stream whose
// estimate outgrows the day bucket's remaining balance is stopped, one
// that fits has its day reservation raised for finalize to settle.
func TestMidStreamTopupCoversTheDayDimension(t *testing.T) {
	limiter := dayLimiter(240, 0)
	p := &Pipeline{limiter: limiter, costCalc: costaccounting.NewCalculator(costaccounting.PriceTable{}), budget: budget.NewTracker(), logger: discardLogger()}
	vk := &identity.VirtualKey{ID: "test-key"}
	dep := Deployment{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o"}
	req := adapter.ChatRequest{Model: "gpt-4o"}

	_, _, tokens, epoch, _, err := limiter.ReserveTPD(context.Background(), "test-key", 4) // 240 -> 236: the minute reservation 4 is below the hour's share of 10
	if err != nil || tokens != 4 {
		t.Fatalf("ReserveTPD = %v tokens, %v", tokens, err)
	}
	zeroUSD := decimal.Zero
	var budgetEpoch, tpmEpoch int64
	var tpmTokens float64
	var budgetNoted, tpmNoted bool
	tpd := tokenReservation{reserved: true, tokens: tokens, epoch: epoch}
	msr := midStreamReservation{vk: vk, budgetReservedUSD: &zeroUSD, budgetReservationEpoch: &budgetEpoch, tpmReservedTokens: &tpmTokens, tpmReservationEpoch: &tpmEpoch, tpd: &tpd, budgetTopupFailOpenNoted: &budgetNoted, tpmTopupFailOpenNoted: &tpmNoted}

	// 4000 chars = 1000 estimated tokens: the delta of 996 exceeds the 236 left.
	if p.checkMidStreamReservationTopup(context.Background(), dep, req, 4000, msr) {
		t.Fatal("a top-up past the day bucket's remaining balance must stop the stream")
	}
	if tpd.tokens != 4 {
		t.Fatalf("a refused top-up left the day reservation at %v, want 4", tpd.tokens)
	}
	// 32 chars = 8 estimated tokens: the delta of 4 fits.
	if !p.checkMidStreamReservationTopup(context.Background(), dep, req, 32, msr) {
		t.Fatal("a top-up that fits the day bucket must let the stream continue")
	}
	if tpd.tokens != 8 {
		t.Fatalf("day reservation after the top-up = %v, want 8", tpd.tokens)
	}
}

// The propagation payload carries tokens_per_day both ways, or a replica
// re-registering the key from an upsert event would lose the cap.
func TestKeyConfigPayloadRoundTripCarriesTokensPerDay(t *testing.T) {
	cfg := ratelimit.KeyConfig{ID: "team-gamma", Capacity: 100, RefillPerSecond: 100, TokensPerDay: 5000}
	payload := keyConfigToPayload(cfg)
	if payload.TokensPerDay != 5000 {
		t.Fatalf("payload.TokensPerDay = %v, want 5000", payload.TokensPerDay)
	}
	back := payloadToKeyConfig("team-gamma", payload)
	if back == nil || back.TokensPerDay != 5000 {
		t.Fatalf("round trip = %+v, want TokensPerDay 5000", back)
	}
}

// minuteErrDayRefusesTopupBackend errors on every minute-key top-up and
// refuses every day-key top-up; nothing else is reached.
type minuteErrDayRefusesTopupBackend struct{ recordingDayBackend }

func (*minuteErrDayRefusesTopupBackend) AllowTPM(_ context.Context, key string, _, _, _, _ float64) (bool, float64, error) {
	if isDayKey(key) {
		return false, 0, nil
	}
	return false, 0, errors.New("simulated redis outage on the minute key")
}

// A minute-key error during the mid-stream top-up must not skip the same
// chunk's day top-up: until 2026-10-15 the TPM branch returned early on a
// backend error, so a day refusal on that chunk went unseen.
func TestMidStreamTopupTPMErrorStillRunsTheDayTopup(t *testing.T) {
	backend := &minuteErrDayRefusesTopupBackend{}
	limiter := ratelimit.NewRedisKeyLimiter([]ratelimit.KeyConfig{
		{ID: "test-key", Capacity: 100, RefillPerSecond: 100, TPMCapacity: 1000, TPMRefillPerSecond: 50, TokensPerDay: 86400},
	}, backend)
	p := &Pipeline{limiter: limiter, costCalc: costaccounting.NewCalculator(costaccounting.PriceTable{}), budget: budget.NewTracker(), logger: discardLogger()}
	vk := &identity.VirtualKey{ID: "test-key"}
	dep := Deployment{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o"}
	req := adapter.ChatRequest{Model: "gpt-4o"}

	zeroUSD := decimal.Zero
	var budgetEpoch, tpmEpoch int64
	tpmTokens := 1.0
	var budgetNoted, tpmNoted bool
	tpd := tokenReservation{reserved: true, tokens: 1}
	msr := midStreamReservation{vk: vk, budgetReservedUSD: &zeroUSD, budgetReservationEpoch: &budgetEpoch, tpmReservedTokens: &tpmTokens, tpmReservationEpoch: &tpmEpoch, tpd: &tpd, budgetTopupFailOpenNoted: &budgetNoted, tpmTopupFailOpenNoted: &tpmNoted}

	// 4000 chars = 1000 estimated tokens: the minute top-up errors (fail
	// open, noted once), the day top-up is refused and must stop the stream.
	if p.checkMidStreamReservationTopup(context.Background(), dep, req, 4000, msr) {
		t.Fatal("the day refusal must stop the stream even though the minute top-up errored on the same chunk")
	}
	if !tpmNoted {
		t.Fatal("the minute top-up error must still be noted once per stream")
	}
}
