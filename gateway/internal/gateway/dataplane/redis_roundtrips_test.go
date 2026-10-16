package dataplane

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/spendledger"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

func chatSpanAttrs(t *testing.T, before int) map[string]int64 {
	t.Helper()
	got := map[string]int64{}
	for _, span := range spansSince(before) {
		if len(span.Name()) < 5 || span.Name()[:5] != "chat " {
			continue
		}
		for _, kv := range span.Attributes() {
			if string(kv.Key) == telemetry.AttrKelvranRedisRoundTrips || string(kv.Key) == telemetry.AttrKelvranRedisCommands {
				got[string(kv.Key)] = kv.Value.AsInt64()
			}
		}
		return got
	}
	t.Fatal("no chat span recorded")
	return nil
}

func TestHandleChatCompletionStampsRedisRoundTripsOnTheSpan(t *testing.T) {
	before := len(spanRecorder.Ended())
	// The in-memory test pipeline makes no Redis call; the upstream stub adds
	// to the request's counter itself, the way redishook does from inside a
	// Redis-backed store, to prove the plumbing from the request context to
	// the span.
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		telemetry.AddRedisRoundTrips(ctx, 7, 9)
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}, ""); err != nil {
		t.Fatal(err)
	}
	got := chatSpanAttrs(t, before)
	if got[telemetry.AttrKelvranRedisRoundTrips] != 7 || got[telemetry.AttrKelvranRedisCommands] != 9 {
		t.Errorf("span attributes = %v; want 7 round trips and 9 commands", got)
	}
}

func TestHandleChatCompletionStampsZeroRedisRoundTripsInMemoryMode(t *testing.T) {
	before := len(spanRecorder.Ended())
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}, ""); err != nil {
		t.Fatal(err)
	}
	got := chatSpanAttrs(t, before)
	if v, ok := got[telemetry.AttrKelvranRedisRoundTrips]; !ok || v != 0 {
		t.Errorf("every request carries a counter, so in-memory mode reports 0, not nothing: %v", got)
	}
}

// redisCountingLedger stands in for a Redis-backed ledger: its Record is the
// request's last Redis call, so the span must include it.
type redisCountingLedger struct{ spendledger.Ledger }

func (l *redisCountingLedger) Record(ctx context.Context, e spendledger.Entry) error {
	telemetry.AddRedisRoundTrips(ctx, 1, 1)
	return l.Ledger.Record(ctx, e)
}

func TestSpanStampIncludesTheLedgerRecordAfterSettlement(t *testing.T) {
	before := len(spanRecorder.Ended())
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		telemetry.AddRedisRoundTrips(ctx, 7, 9)
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})
	p.ledger = &redisCountingLedger{Ledger: spendledger.NewMemory(spendledger.Options{})}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}, ""); err != nil {
		t.Fatal(err)
	}
	got := chatSpanAttrs(t, before)
	if got[telemetry.AttrKelvranRedisRoundTrips] != 8 || got[telemetry.AttrKelvranRedisCommands] != 10 {
		t.Errorf("span attributes = %v; want the upstream's 7/9 plus the ledger record's 1/1: the stamp must run after the last Redis call of the request", got)
	}
}

func TestHandleChatCompletionStreamStampsRedisRoundTripsOnTheSpan(t *testing.T) {
	before := len(spanRecorder.Ended())
	p := newStreamingTestPipeline(t, func(ctx context.Context, _ Deployment, _ any) (io.ReadCloser, error) {
		telemetry.AddRedisRoundTrips(ctx, 5, 5)
		return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
	}, nil, adapter.Registry{"openai": openai.New()})
	rec := httptest.NewRecorder()
	req := adapter.ChatRequest{Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	_ = p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, rec, "")
	got := chatSpanAttrs(t, before)
	if got[telemetry.AttrKelvranRedisRoundTrips] != 5 {
		t.Errorf("streaming span attributes = %v; want the 5 round trips counted on the request context", got)
	}
}
