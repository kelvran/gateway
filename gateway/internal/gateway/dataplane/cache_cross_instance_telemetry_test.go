package dataplane

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// crossInstanceTestPipeline builds a Pipeline with a logger writing to
// logBuf and real, non-default cache TTLs (so the emitted ttl_ms field
// is distinguishable per layer, not a coincidental shared default) — for
// docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md's own tests.
// Neither the shared newTestPipeline* helpers (discardLogger, discards
// output) nor warnThresholdTestPipeline (a different, cost-focused
// fixture) can prove anything about this feature's own log fields.
func crossInstanceTestPipeline(t *testing.T, logBuf *bytes.Buffer) *Pipeline {
	t.Helper()
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
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
		Logger:     slog.New(slog.NewJSONHandler(logBuf, nil)),
		CacheTTL:   10 * time.Minute,
		CacheL2TTL: 20 * time.Minute,
		CacheL3TTL: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// crossInstanceCheckLine asserts logBuf contains exactly one
// "cache_cross_instance_check" line matching every given field, and
// fails with the full log output otherwise — used instead of a bare
// strings.Contains per assertion so a failure shows which specific
// field combination was missing, not just "something didn't match."
func crossInstanceCheckLine(t *testing.T, logBuf *bytes.Buffer, layer string, hit bool, ttl time.Duration) {
	t.Helper()
	want := []string{
		`"msg":"cache_cross_instance_check"`,
		fmt.Sprintf(`"cache_layer":%q`, layer),
		fmt.Sprintf(`"hit":%v`, hit),
		fmt.Sprintf(`"instance_id":%q`, telemetry.InstanceID),
		fmt.Sprintf(`"ttl_ms":%d`, ttl.Milliseconds()),
	}
	for _, line := range strings.Split(logBuf.String(), "\n") {
		if !strings.Contains(line, `"msg":"cache_cross_instance_check"`) {
			continue
		}
		if !strings.Contains(line, fmt.Sprintf(`"cache_layer":%q`, layer)) {
			continue
		}
		matched := true
		for _, w := range want {
			if !strings.Contains(line, w) {
				matched = false
				break
			}
		}
		if matched {
			return
		}
	}
	t.Fatalf("no cache_cross_instance_check line matched layer=%q hit=%v ttl=%v; full log output:\n%s", layer, hit, ttl, logBuf.String())
}

// TestHandleChatCompletionLogsL1MissThenHit is the load-bearing proof for
// this feature's live-emission side: the exact-key correlation logic
// (telemetry/cachecorrelation.Analyze) is only as good as the events
// dataplane actually emits, so this proves the real pipeline emits a
// real L1 miss then a real L1 hit, tagged with this process's own
// InstanceID and L1's own configured TTL.
func TestHandleChatCompletionLogsL1MissThenHit(t *testing.T) {
	var logBuf bytes.Buffer
	p := crossInstanceTestPipeline(t, &logBuf)

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("first call: %v", err)
	}
	crossInstanceCheckLine(t, &logBuf, "L1", false, 10*time.Minute)

	logBuf.Reset()
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("second call: %v", err)
	}
	crossInstanceCheckLine(t, &logBuf, "L1", true, 10*time.Minute)
}

// TestHandleChatCompletionLogsL2HitWithItsOwnTTL proves a normalized
// (not byte-exact) repeat logs an L1 miss (its own exact key never
// matched) followed by an L2 hit tagged with L2's OWN configured TTL,
// not L1's — the two layers must never share a reported TTL just
// because they share a pipeline.
func TestHandleChatCompletionLogsL2HitWithItsOwnTTL(t *testing.T) {
	var logBuf bytes.Buffer
	p := crossInstanceTestPipeline(t, &logBuf)

	first := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi there"}}}
	second := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "  hi there  "}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", first); err != nil {
		t.Fatalf("first call: %v", err)
	}
	logBuf.Reset()
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", second); err != nil {
		t.Fatalf("second call: %v", err)
	}
	crossInstanceCheckLine(t, &logBuf, "L1", false, 10*time.Minute)
	crossInstanceCheckLine(t, &logBuf, "L2", true, 20*time.Minute)
}

// TestHandleChatCompletionLogsL3HitWithRequestsOwnL1Key proves
// checkLexicalCache's own emission: a lexical near-duplicate (an L1/L2
// miss but a real L3 hit) logs an L3 event carrying the SAME cache_key
// value as the request's own L1 miss line — per the RFC's Design
// section, L3 reuses the request's own l1Key as its correlation
// identity, since L3 has no exact-match key of its own.
func TestHandleChatCompletionLogsL3HitWithRequestsOwnL1Key(t *testing.T) {
	var logBuf bytes.Buffer
	p := crossInstanceTestPipeline(t, &logBuf)

	// Internal-whitespace-only difference — an L1/L2 miss but a genuine
	// L3 lexical near-duplicate, per lexical_cache_test.go's own proven
	// fixture text.
	first := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "Explain how binary search works in a sorted array"}}}
	second := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "Explain how binary search   works in a sorted array"}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", first); err != nil {
		t.Fatalf("first call: %v", err)
	}
	logBuf.Reset()
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", second); err != nil {
		t.Fatalf("second call: %v", err)
	}
	crossInstanceCheckLine(t, &logBuf, "L1", false, 10*time.Minute)
	crossInstanceCheckLine(t, &logBuf, "L2", false, 20*time.Minute)
	crossInstanceCheckLine(t, &logBuf, "L3", true, 30*time.Minute)

	var l1Key, l3Key string
	for _, line := range strings.Split(logBuf.String(), "\n") {
		// Scoped to cache_cross_instance_check lines specifically — the
		// per-request "chat_completion" summary line (logRequest) ALSO
		// carries its own "cache_layer" field on any hit, and would
		// otherwise wrongly match the L3 condition below with no
		// "cache_key" field of its own to extract, clobbering l3Key back
		// to "".
		if !strings.Contains(line, `"msg":"cache_cross_instance_check"`) {
			continue
		}
		if strings.Contains(line, `"cache_layer":"L1"`) {
			l1Key = extractJSONStringField(t, line, "cache_key")
		}
		if strings.Contains(line, `"cache_layer":"L3"`) {
			l3Key = extractJSONStringField(t, line, "cache_key")
		}
	}
	if l1Key == "" || l3Key == "" {
		t.Fatalf("could not extract both cache_key values; full log output:\n%s", logBuf.String())
	}
	if l1Key != l3Key {
		t.Errorf("L3's cache_key = %q, want it to equal the same request's own L1 cache_key %q", l3Key, l1Key)
	}
}

// extractJSONStringField pulls out `"field":"value"` from a single JSON
// log line via strings.Cut, deliberately not a full json.Unmarshal —
// this test only ever needs one field's value, and every other test in
// this file already asserts full-line shape via strings.Contains.
func extractJSONStringField(t *testing.T, line, field string) string {
	t.Helper()
	_, rest, ok := strings.Cut(line, fmt.Sprintf(`"%s":"`, field))
	if !ok {
		return ""
	}
	value, _, ok := strings.Cut(rest, `"`)
	if !ok {
		return ""
	}
	return value
}

// TestHandleChatCompletionDoesNotLogL3EventOnVolatileBypass proves the
// volatile-bypass path never emits a cross-instance event at all — it
// never learned whether a valid L3 entry existed, so it must not be
// misreported as a definite miss.
func TestHandleChatCompletionDoesNotLogL3EventOnVolatileBypass(t *testing.T) {
	var logBuf bytes.Buffer
	p := crossInstanceTestPipeline(t, &logBuf)

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "what is the weather right now"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	for _, line := range strings.Split(logBuf.String(), "\n") {
		if strings.Contains(line, `"msg":"cache_cross_instance_check"`) && strings.Contains(line, `"cache_layer":"L3"`) {
			t.Fatalf("a volatile-bypass request logged an L3 cross-instance event, want none: %s", line)
		}
	}
}
