package telemetry

import (
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func attrValue(t *testing.T, attrs []attribute.KeyValue, key attribute.Key) (attribute.Value, bool) {
	t.Helper()
	for _, kv := range attrs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestRecordChatCompletionResultSuccessSetsAllAttributes(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	tracer := tp.Tracer("result_test")

	_, span := tracer.Start(t.Context(), "test-span")
	RecordChatCompletionResult(span, ChatCompletionResult{
		VirtualKeyID:   "team-alpha",
		Provider:       "openai",
		DeploymentName: "gpt4o-primary",
		ResponseModel:  "gpt-4o",
		ResponseID:     "chatcmpl-1",
		FinishReasons:  []string{"stop"},
		InputTokens:    10,
		OutputTokens:   5,
		CacheHit:       false,
		CostUSD:        0.0001,
		AgentRunID:     "run-abc123",
	})
	span.End()

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("len(sr.Ended()) = %d, want 1", len(ended))
	}
	attrs := ended[0].Attributes()

	wantString := map[string]string{
		AttrKelvranVirtualKeyID:   "team-alpha",
		AttrGenAIProviderName:     "openai",
		AttrKelvranDeploymentName: "gpt4o-primary",
		AttrGenAIResponseModel:    "gpt-4o",
		AttrGenAIResponseID:       "chatcmpl-1",
		AttrKelvranAgentRunID:     "run-abc123",
	}
	for key, want := range wantString {
		v, ok := attrValue(t, attrs, attribute.Key(key))
		if !ok {
			t.Errorf("attribute %q not set", key)
			continue
		}
		if v.AsString() != want {
			t.Errorf("attribute %q = %q, want %q", key, v.AsString(), want)
		}
	}

	if v, ok := attrValue(t, attrs, attribute.Key(AttrGenAIUsageInputTokens)); !ok || v.AsInt64() != 10 {
		t.Errorf("%s = %v, ok=%v, want 10", AttrGenAIUsageInputTokens, v, ok)
	}
	if v, ok := attrValue(t, attrs, attribute.Key(AttrGenAIUsageOutputTokens)); !ok || v.AsInt64() != 5 {
		t.Errorf("%s = %v, ok=%v, want 5", AttrGenAIUsageOutputTokens, v, ok)
	}
	if v, ok := attrValue(t, attrs, attribute.Key(AttrKelvranCacheHit)); !ok || v.AsBool() != false {
		t.Errorf("%s = %v, ok=%v, want false", AttrKelvranCacheHit, v, ok)
	}
	if v, ok := attrValue(t, attrs, attribute.Key(AttrKelvranCostUSD)); !ok || v.AsFloat64() != 0.0001 {
		t.Errorf("%s = %v, ok=%v, want 0.0001", AttrKelvranCostUSD, v, ok)
	}
	if v, ok := attrValue(t, attrs, attribute.Key(AttrGenAIResponseFinishReasons)); !ok || len(v.AsStringSlice()) != 1 || v.AsStringSlice()[0] != "stop" {
		t.Errorf("%s = %v, ok=%v, want [stop]", AttrGenAIResponseFinishReasons, v, ok)
	}

	if ended[0].Status().Code == codes.Error {
		t.Errorf("status = Error on a successful result, want Unset/Ok")
	}
}

func TestRecordChatCompletionResultSkipsEmptyOptionalFields(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	tracer := tp.Tracer("result_test")

	// VirtualKeyID empty (as on an auth failure), AgentRunID empty (as
	// when the caller never sent one), FinishReasons nil.
	_, span := tracer.Start(t.Context(), "test-span")
	RecordChatCompletionResult(span, ChatCompletionResult{
		CacheHit: true,
		CostUSD:  0,
	})
	span.End()

	ended := sr.Ended()
	attrs := ended[0].Attributes()

	for _, key := range []string{
		AttrKelvranVirtualKeyID,
		AttrKelvranAgentRunID,
		AttrGenAIProviderName,
		AttrKelvranDeploymentName,
		AttrGenAIResponseModel,
		AttrGenAIResponseID,
		AttrGenAIResponseFinishReasons,
		AttrGenAIUsageInputTokens,
		AttrGenAIUsageOutputTokens,
	} {
		if _, ok := attrValue(t, attrs, attribute.Key(key)); ok {
			t.Errorf("attribute %q is set on a result with no value for it — must be absent, not an empty placeholder", key)
		}
	}

	// cache.hit and cost.usd are always set, even when cost is 0 — zero is
	// a real, meaningful value here, not "unknown."
	if v, ok := attrValue(t, attrs, attribute.Key(AttrKelvranCacheHit)); !ok || v.AsBool() != true {
		t.Errorf("%s = %v, ok=%v, want true", AttrKelvranCacheHit, v, ok)
	}
	if _, ok := attrValue(t, attrs, attribute.Key(AttrKelvranCostUSD)); !ok {
		t.Errorf("%s not set even though it's always meaningful", AttrKelvranCostUSD)
	}
}

func TestRecordChatCompletionResultErrorRecordsErrorAndSetsStatus(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	tracer := tp.Tracer("result_test")

	wantErr := errors.New("simulated upstream failure")
	_, span := tracer.Start(t.Context(), "test-span")
	RecordChatCompletionResult(span, ChatCompletionResult{Err: wantErr})
	span.End()

	ended := sr.Ended()
	if got := ended[0].Status().Code; got != codes.Error {
		t.Errorf("status code = %v, want codes.Error", got)
	}
	if got := ended[0].Status().Description; got != wantErr.Error() {
		t.Errorf("status description = %q, want %q", got, wantErr.Error())
	}

	events := ended[0].Events()
	var foundErrorEvent bool
	for _, e := range events {
		if e.Name == "exception" {
			foundErrorEvent = true
		}
	}
	if !foundErrorEvent {
		t.Error("no \"exception\" event recorded — RecordError did not fire")
	}
}
