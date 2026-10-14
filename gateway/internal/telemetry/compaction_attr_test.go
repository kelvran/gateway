package telemetry

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// kelvran.usage.compaction_iterations (round-5 Tier A item 3) follows the
// pricing members' convention: emitted only when the response ran at least
// one compaction, never as a fabricated zero.
func TestRecordChatCompletionResultEmitsCompactionIterationsOnlyWhenSet(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	tracer := tp.Tracer("result_test")

	_, span := tracer.Start(t.Context(), "set")
	RecordChatCompletionResult(span, ChatCompletionResult{InputTokens: 172, OutputTokens: 241, CompactionIterations: 2})
	span.End()
	_, span = tracer.Start(t.Context(), "unset")
	RecordChatCompletionResult(span, ChatCompletionResult{InputTokens: 1000, OutputTokens: 10})
	span.End()

	set, unset := sr.Ended()[0].Attributes(), sr.Ended()[1].Attributes()
	if v, ok := attrValue(t, set, attribute.Key(AttrKelvranUsageCompactionIterations)); !ok || v.AsInt64() != 2 {
		t.Errorf("%s = %v, ok=%v, want 2", AttrKelvranUsageCompactionIterations, v, ok)
	}
	if _, ok := attrValue(t, unset, attribute.Key(AttrKelvranUsageCompactionIterations)); ok {
		t.Errorf("%s emitted on a result without a compaction", AttrKelvranUsageCompactionIterations)
	}
}
