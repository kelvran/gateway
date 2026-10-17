package dataplane

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// gateBackend is a ratelimit.RedisBackend that answers the phase-2b gate and
// paired settle and counts every legacy single-dimension call, which the chat
// path must no longer make.
type gateBackend struct {
	mu          sync.Mutex
	result      admission.Result
	err         error
	admits      []admission.Request
	settles     [][]admission.Settlement
	legacyCalls int
}

func (b *gateBackend) Admit(_ context.Context, req admission.Request) (admission.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.admits = append(b.admits, req)
	return b.result, b.err
}
func (b *gateBackend) SettleTokens(_ context.Context, s []admission.Settlement) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.settles = append(b.settles, s)
	return nil
}
func (b *gateBackend) legacy() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.legacyCalls++
}
func (b *gateBackend) Allow(context.Context, string, float64, float64) (bool, error) {
	b.legacy()
	return true, nil
}
func (b *gateBackend) AllowTPM(context.Context, string, float64, float64, float64, float64) (bool, float64, error) {
	b.legacy()
	return true, 0, nil
}
func (b *gateBackend) AdjustTPM(context.Context, string, float64) error { b.legacy(); return nil }
func (b *gateBackend) ReserveTPM(_ context.Context, _ string, _, _, _, cold float64) (bool, float64, float64, error) {
	b.legacy()
	return true, 0, cold, nil
}
func (b *gateBackend) SettleTPM(context.Context, string, float64, *float64) error {
	b.legacy()
	return nil
}
func (b *gateBackend) Close() error { return nil }

const gateTestSSE = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n" +
	"data: [DONE]\n\n"

// streamThroughGate runs one streamed request for test-key through a Redis
// limiter backed by the gate fake.
func streamThroughGate(t *testing.T, backend *gateBackend) (*httptest.ResponseRecorder, error) {
	t.Helper()
	p := newStreamingTestPipelineWithLimiter(t, func(context.Context, Deployment, any) (io.ReadCloser, error) {
		return nopCloserReader{strings.NewReader(gateTestSSE)}, nil
	}, nil, adapter.Registry{"openai": openai.New()}, defaultTestVirtualKeys(), budget.NewTracker(), gateLimiter(backend), discardLogger())
	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}}}, rec, "")
	return rec, err
}

func gateLimiter(backend *gateBackend) *ratelimit.KeyLimiter {
	return ratelimit.NewRedisKeyLimiter([]ratelimit.KeyConfig{
		{ID: "test-key", Capacity: 100, RefillPerSecond: 100, TPMCapacity: 1000, TPMRefillPerSecond: 50, TokensPerDay: 86400},
	}, backend)
}

func gatePipeline(t *testing.T, backend *gateBackend, logBuf *bytes.Buffer) *Pipeline {
	t.Helper()
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatal(err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	logger := discardLogger()
	if logBuf != nil {
		logger = slog.New(slog.NewJSONHandler(logBuf, nil))
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        gateLimiter(backend),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(_ context.Context, dep Deployment, _ any) (any, error) {
			return fakeOpenAIResponse(dep.UpstreamModel), nil
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckRateLimitMakesOneGateCallInRedisMode(t *testing.T) {
	backend := &gateBackend{result: admission.Result{Allowed: true, TPMCost: 1000, TPDCost: 1000, TPDColdStart: 1000}}
	p := &Pipeline{limiter: gateLimiter(backend), logger: discardLogger()}
	d := p.checkRateLimit(context.Background(), &identity.VirtualKey{ID: "test-key"}, "gpt-4o")
	if !d.ok || d.failedOpen || d.tpm != (tokenReservation{reserved: true, tokens: 1000}) || d.tpd != (tokenReservation{reserved: true, tokens: 1000}) || d.retryAfter != 0 {
		t.Fatalf("decision = %+v, want ok with 1000 reserved on both token dimensions", d)
	}
	if len(backend.admits) != 1 || backend.legacyCalls != 0 || len(backend.settles) != 0 {
		t.Fatalf("admits %d, legacy calls %d, settles %d; want one gate call and nothing else before finalize", len(backend.admits), backend.legacyCalls, len(backend.settles))
	}
	req := backend.admits[0]
	if req.RPMKey != "test-key" || req.TPM.Burst != 1000 || req.TPM.ColdStart != 1000 || req.TPD.Key != "test-key\x00dim=day" || req.TPD.Burst != 86400 || req.TPD.ColdStart != 3600 {
		t.Errorf("gate request = %+v, want all three dimensions with the day cold cap 3600 (the script lowers it to the minute reservation)", req)
	}
}

func TestCheckRateLimitDayRefusalReservesNothingAndCarriesTheWait(t *testing.T) {
	backend := &gateBackend{result: admission.Result{RefusedBy: "tpd", RetryAfterSec: 3600, TPMCost: 1000, TPDCost: 100, TPDColdStart: 3600}}
	p := &Pipeline{limiter: gateLimiter(backend), logger: discardLogger()}
	d := p.checkRateLimit(context.Background(), &identity.VirtualKey{ID: "test-key"}, "gpt-4o")
	if d.ok || d.failedOpen || !d.rejectedByDay || d.retryAfter != time.Hour || d.tpm.reserved || d.tpd.reserved {
		t.Fatalf("decision = %+v, want a day rejection with a 1 h wait and nothing reserved", d)
	}
	if len(backend.settles) != 0 || backend.legacyCalls != 0 {
		t.Fatalf("settles %d, legacy calls %d; want none: the gate reserved nothing, so there is nothing to release", len(backend.settles), backend.legacyCalls)
	}
	if err := d.rejection(); !errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "tokens_per_day") {
		t.Errorf("rejection = %v, want a tokens_per_day ErrRateLimited", err)
	}
	// RPM and TPM refusals are the plain ErrRateLimited with the backoff alone.
	for _, dim := range []string{"rpm", "tpm"} {
		backend.result = admission.Result{RefusedBy: dim, RetryAfterSec: 5}
		d := p.checkRateLimit(context.Background(), &identity.VirtualKey{ID: "test-key"}, "gpt-4o")
		if d.ok || d.rejectedByDay || d.retryAfter != 0 {
			t.Errorf("%s refusal decision = %+v, want a plain rejection with no wait", dim, d)
		}
	}
}

func TestCheckRateLimitBackendErrorFailsOpenOnceForAllDimensions(t *testing.T) {
	reader := dataplaneTelemetryMetricsReaderForTest()
	var before metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &before); err != nil {
		t.Fatal(err)
	}
	beforeSnap := snapshotDataplaneTelemetry(t, before)

	var logBuf bytes.Buffer
	backend := &gateBackend{err: errors.New("simulated redis outage")}
	p := &Pipeline{limiter: gateLimiter(backend), logger: slog.New(slog.NewJSONHandler(&logBuf, nil))}
	d := p.checkRateLimit(context.Background(), &identity.VirtualKey{ID: "gate-failopen"}, "gpt-4o")
	if !d.ok || !d.failedOpen || d.tpm.reserved || d.tpd.reserved {
		t.Fatalf("decision = %+v, want admitted, failed open, nothing reserved", d)
	}
	var after metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &after); err != nil {
		t.Fatal(err)
	}
	afterSnap := snapshotDataplaneTelemetry(t, after)
	if got := afterSnap.failOpenByKeyID["gate-failopen"] - beforeSnap.failOpenByKeyID["gate-failopen"]; got != 1 {
		t.Errorf("kelvran.ratelimit.fail_open delta = %d, want exactly 1 for the one gate call", got)
	}
	if n := strings.Count(logBuf.String(), `"msg":"ratelimit_backend_unavailable"`); n != 1 {
		t.Errorf("ratelimit_backend_unavailable lines = %d, want 1 (one gate, one line): %s", n, logBuf.String())
	}
	if strings.Contains(logBuf.String(), "ratelimit_tpm_backend_unavailable") || strings.Contains(logBuf.String(), "ratelimit_tpd_backend_unavailable") {
		t.Errorf("the per-dimension pre-call lines are gone with the gate: %s", logBuf.String())
	}
}

func TestFinalizeSettlesBothTokenDimensionsInOneCall(t *testing.T) {
	backend := &gateBackend{result: admission.Result{Allowed: true, TPMCost: 1000, TPDCost: 1000, TPDColdStart: 1000}}
	p := gatePipeline(t, backend, nil)
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}, ""); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.admits) != 1 || len(backend.settles) != 1 || backend.legacyCalls != 0 {
		t.Fatalf("admits %d, settles %d, legacy calls %d; want one gate call and one paired settle", len(backend.admits), len(backend.settles), backend.legacyCalls)
	}
	s := backend.settles[0]
	if len(s) != 2 || s[0].Key != "test-key" || s[1].Key != "test-key\x00dim=day" || s[0].RealTokens == nil || s[1].RealTokens == nil || *s[0].RealTokens != *s[1].RealTokens {
		t.Fatalf("settlements = %+v, want the minute and the day key, both with the response's real token count", s)
	}
	if s[0].DeltaIncrement != (1000.0/50)*(*s[0].RealTokens-1000) || s[1].DeltaIncrement != (1000.0/1)*(*s[1].RealTokens-1000) {
		t.Errorf("deltas = %v / %v, want the minute and day emission intervals times (real - 1000)", s[0].DeltaIncrement, s[1].DeltaIncrement)
	}
}

func TestStreamingDayRefusalIsATokensPerDay429InRedisMode(t *testing.T) {
	backend := &gateBackend{result: admission.Result{RefusedBy: "tpd", RetryAfterSec: 3600, TPDCost: 100, TPDColdStart: 3600}}
	rec, err := streamThroughGate(t, backend)
	if !errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "tokens_per_day") || retryAfterOf(err) != time.Hour {
		t.Fatalf("streaming err = %v (Retry-After %v), want a tokens_per_day ErrRateLimited with the 1 h wait", err, retryAfterOf(err))
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("a refused stream must write no chunk, got %q", rec.Body.String())
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.admits) != 1 || len(backend.settles) != 0 || backend.legacyCalls != 0 {
		t.Fatalf("admits %d, settles %d, legacy %d; want one gate call and nothing to settle", len(backend.admits), len(backend.settles), backend.legacyCalls)
	}
}

func TestStreamingGateErrorFailsOpenOnceInRedisMode(t *testing.T) {
	reader := dataplaneTelemetryMetricsReaderForTest()
	var before metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &before); err != nil {
		t.Fatal(err)
	}
	beforeSnap := snapshotDataplaneTelemetry(t, before)
	backend := &gateBackend{err: errors.New("simulated redis outage")}
	rec, err := streamThroughGate(t, backend)
	if err != nil || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("a gate error must fail open and the stream complete: err %v, body %q", err, rec.Body.String())
	}
	var after metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &after); err != nil {
		t.Fatal(err)
	}
	afterSnap := snapshotDataplaneTelemetry(t, after)
	if got := afterSnap.failOpenByKeyID["test-key"] - beforeSnap.failOpenByKeyID["test-key"]; got != 1 {
		t.Errorf("kelvran.ratelimit.fail_open delta = %d, want exactly 1 across the gate and the stream", got)
	}
}
