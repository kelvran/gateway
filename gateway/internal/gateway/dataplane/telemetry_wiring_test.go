package dataplane

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// spanRecorder is installed exactly once for this whole test binary, in
// TestMain — never per-test. Verified empirically (not assumed): once
// telemetry.Tracer (obtained via otel.Tracer at package-init time, before
// any test runs) starts its first real span against a delegate,
// go.opentelemetry.io/otel's global TracerProvider silently stops
// re-delegating to any LATER otel.SetTracerProvider call for that already-
// obtained Tracer handle — a second, third, etc. call is accepted (no
// error) but has no observable effect. Installing the real provider once,
// before any test's first span, and tracking each test's own spans by
// index delta (spansSince) against one shared recorder is the pattern
// that actually works, matching production's own "Init once at startup,
// before any request" usage exactly.
var spanRecorder *tracetest.SpanRecorder

func TestMain(m *testing.M) {
	spanRecorder = tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	otel.SetTracerProvider(tp)
	os.Exit(m.Run())
}

// spansSince returns every span recorded after index before — i.e. the
// spans produced by whatever ran between capturing before and calling
// spansSince.
func spansSince(before int) []sdktrace.ReadOnlySpan {
	return spanRecorder.Ended()[before:]
}

func spanAttr(t *testing.T, attrs []attribute.KeyValue, key string) (attribute.Value, bool) {
	t.Helper()
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestHandleChatCompletionEmitsSpanOnSuccess(t *testing.T) {
	before := len(spanRecorder.Ended())

	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{
		Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	spans := spansSince(before)
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "chat gpt-4o" {
		t.Errorf("span name = %q, want %q", span.Name(), "chat gpt-4o")
	}
	attrs := span.Attributes()

	if v, ok := spanAttr(t, attrs, telemetry.AttrKelvranVirtualKeyID); !ok || v.AsString() != "test-key" {
		t.Errorf("%s = %v, ok=%v, want %q", telemetry.AttrKelvranVirtualKeyID, v, ok, "test-key")
	}
	if v, ok := spanAttr(t, attrs, telemetry.AttrGenAIProviderName); !ok || v.AsString() != "openai" {
		t.Errorf("%s = %v, ok=%v, want %q", telemetry.AttrGenAIProviderName, v, ok, "openai")
	}
	if v, ok := spanAttr(t, attrs, telemetry.AttrKelvranDeploymentName); !ok || v.AsString() != "d1" {
		t.Errorf("%s = %v, ok=%v, want %q", telemetry.AttrKelvranDeploymentName, v, ok, "d1")
	}
	if v, ok := spanAttr(t, attrs, telemetry.AttrGenAIResponseModel); !ok || v.AsString() != "gpt-4o" {
		t.Errorf("%s = %v, ok=%v, want %q", telemetry.AttrGenAIResponseModel, v, ok, "gpt-4o")
	}
	if v, ok := spanAttr(t, attrs, telemetry.AttrKelvranCacheHit); !ok || v.AsBool() != false {
		t.Errorf("%s = %v, ok=%v, want false", telemetry.AttrKelvranCacheHit, v, ok)
	}
	if _, ok := spanAttr(t, attrs, telemetry.AttrGenAIResponseFinishReasons); !ok {
		t.Errorf("%s not set", telemetry.AttrGenAIResponseFinishReasons)
	}
	if span.Status().Code == codes.Error {
		t.Errorf("status = Error on a successful request")
	}
}

func TestHandleChatCompletionEmitsSpanOnAuthFailureWithoutVirtualKeyID(t *testing.T) {
	before := len(spanRecorder.Ended())

	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		t.Fatal("upstream should never be called when auth fails")
		return nil, nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	_, err := p.HandleChatCompletion(context.Background(), "", adapter.ChatRequest{Model: "gpt-4o"})
	if err == nil {
		t.Fatal("expected an error for missing Authorization header")
	}

	spans := spansSince(before)
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1 (a span must still exist even on auth failure)", len(spans))
	}
	span := spans[0]
	if _, ok := spanAttr(t, span.Attributes(), telemetry.AttrKelvranVirtualKeyID); ok {
		t.Error("kelvran.virtual_key.id is set on an auth-failure span — auth never resolved an identity")
	}
	if span.Status().Code != codes.Error {
		t.Errorf("status code = %v, want codes.Error", span.Status().Code)
	}
}

func TestHandleChatCompletionEmitsSpanWithCacheHitTrue(t *testing.T) {
	before := len(spanRecorder.Ended())

	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("second call: %v", err)
	}

	spans := spansSince(before)
	if len(spans) != 2 {
		t.Fatalf("len(spans) = %d, want 2", len(spans))
	}
	if v, ok := spanAttr(t, spans[0].Attributes(), telemetry.AttrKelvranCacheHit); !ok || v.AsBool() != false {
		t.Errorf("first call's %s = %v, ok=%v, want false", telemetry.AttrKelvranCacheHit, v, ok)
	}
	if v, ok := spanAttr(t, spans[1].Attributes(), telemetry.AttrKelvranCacheHit); !ok || v.AsBool() != true {
		t.Errorf("second call's %s = %v, ok=%v, want true", telemetry.AttrKelvranCacheHit, v, ok)
	}
}

func TestHandleChatCompletionEmitsSpanWithAgentRunIDFromBaggage(t *testing.T) {
	before := len(spanRecorder.Ended())

	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	member, err := baggage.NewMember("agent_run_id", "run-xyz789")
	if err != nil {
		t.Fatalf("baggage.NewMember: %v", err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatalf("baggage.New: %v", err)
	}
	ctx := baggage.ContextWithBaggage(context.Background(), bag)

	if _, err := p.HandleChatCompletion(ctx, "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o"}); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	spans := spansSince(before)
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}
	if v, ok := spanAttr(t, spans[0].Attributes(), telemetry.AttrKelvranAgentRunID); !ok || v.AsString() != "run-xyz789" {
		t.Errorf("%s = %v, ok=%v, want %q", telemetry.AttrKelvranAgentRunID, v, ok, "run-xyz789")
	}
}

func TestHandleChatCompletionStreamEmitsSpanOnSuccessAndCacheHit(t *testing.T) {
	before := len(spanRecorder.Ended())

	var upstreamCalls int
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		upstreamCalls++
		return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
	}, nil, adapter.Registry{"openai": openai.New()})

	req := adapter.ChatRequest{Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}}}

	rec1 := httptest.NewRecorder()
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", req, rec1); err != nil {
		t.Fatalf("first HandleChatCompletionStream: %v", err)
	}
	rec2 := httptest.NewRecorder()
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", req, rec2); err != nil {
		t.Fatalf("second HandleChatCompletionStream: %v", err)
	}

	spans := spansSince(before)
	if len(spans) != 2 {
		t.Fatalf("len(spans) = %d, want 2", len(spans))
	}
	if v, ok := spanAttr(t, spans[0].Attributes(), telemetry.AttrKelvranCacheHit); !ok || v.AsBool() != false {
		t.Errorf("first span's %s = %v, ok=%v, want false", telemetry.AttrKelvranCacheHit, v, ok)
	}
	if v, ok := spanAttr(t, spans[1].Attributes(), telemetry.AttrKelvranCacheHit); !ok || v.AsBool() != true {
		t.Errorf("second span's %s = %v, ok=%v, want true", telemetry.AttrKelvranCacheHit, v, ok)
	}
	if v, ok := spanAttr(t, spans[0].Attributes(), telemetry.AttrGenAIProviderName); !ok || v.AsString() != "openai" {
		t.Errorf("first span's %s = %v, ok=%v, want %q", telemetry.AttrGenAIProviderName, v, ok, "openai")
	}
}
