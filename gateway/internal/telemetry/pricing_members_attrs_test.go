package telemetry

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestRecordChatCompletionResultEmitsPricingMemberAttributesOnlyWhenSet(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	tracer := tp.Tracer("result_test")

	_, span := tracer.Start(t.Context(), "set")
	RecordChatCompletionResult(span, ChatCompletionResult{
		InputTokens: 1000, OutputTokens: 10, CacheCreationTokens: 500,
		CacheCreation1hTokens: 300, WebSearchRequests: 3, InferenceGeo: "us", ServiceTier: "standard",
		LongContextTier: true,
	})
	span.End()
	_, span = tracer.Start(t.Context(), "unset")
	RecordChatCompletionResult(span, ChatCompletionResult{InputTokens: 1000, OutputTokens: 10})
	span.End()

	set, unset := sr.Ended()[0].Attributes(), sr.Ended()[1].Attributes()
	if v, ok := attrValue(t, set, attribute.Key(AttrKelvranUsageCacheCreation1hInputTokens)); !ok || v.AsInt64() != 300 {
		t.Errorf("%s = %v, ok=%v, want 300", AttrKelvranUsageCacheCreation1hInputTokens, v, ok)
	}
	if v, ok := attrValue(t, set, attribute.Key(AttrKelvranUsageWebSearchRequests)); !ok || v.AsInt64() != 3 {
		t.Errorf("%s = %v, ok=%v, want 3", AttrKelvranUsageWebSearchRequests, v, ok)
	}
	if v, ok := attrValue(t, set, attribute.Key(AttrKelvranUsageInferenceGeo)); !ok || v.AsString() != "us" {
		t.Errorf("%s = %v, ok=%v, want us", AttrKelvranUsageInferenceGeo, v, ok)
	}
	if v, ok := attrValue(t, set, attribute.Key(AttrKelvranUsageServiceTier)); !ok || v.AsString() != "standard" {
		t.Errorf("%s = %v, ok=%v, want standard", AttrKelvranUsageServiceTier, v, ok)
	}
	if v, ok := attrValue(t, set, attribute.Key(AttrKelvranPricingLongContextTier)); !ok || !v.AsBool() {
		t.Errorf("%s = %v, ok=%v, want true", AttrKelvranPricingLongContextTier, v, ok)
	}
	for _, key := range []string{AttrKelvranUsageCacheCreation1hInputTokens, AttrKelvranUsageWebSearchRequests, AttrKelvranUsageInferenceGeo, AttrKelvranUsageServiceTier, AttrKelvranPricingLongContextTier} {
		if _, ok := attrValue(t, unset, attribute.Key(key)); ok {
			t.Errorf("%s emitted on a result without the member", key)
		}
	}
}
