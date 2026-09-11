package dataplane

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// TestTraceLogFieldsReturnsNilWithoutASpan proves traceLogFields' own
// negative case: a context with no valid span (e.g. context.Background(),
// or a background health-probe pass) must never produce a fabricated
// pair of all-zero trace_id/span_id values.
func TestTraceLogFieldsReturnsNilWithoutASpan(t *testing.T) {
	if got := traceLogFields(context.Background()); got != nil {
		t.Errorf("traceLogFields(context.Background()) = %v, want nil", got)
	}
}

// TestTraceLogFieldsReturnsRealIDsFromARealSpan proves the positive case
// directly against telemetry.Tracer (the exact Tracer HandleChatCompletion/
// HandleChatCompletionStream start every request's own span with), rather
// than only through a full pipeline run.
func TestTraceLogFieldsReturnsRealIDsFromARealSpan(t *testing.T) {
	ctx, span := telemetry.Tracer.Start(context.Background(), "test-span")
	defer span.End()

	got := traceLogFields(ctx)
	if len(got) != 4 || got[0] != "trace_id" || got[2] != "span_id" {
		t.Fatalf("traceLogFields(ctx) = %v, want [\"trace_id\", <id>, \"span_id\", <id>]", got)
	}
	wantTraceID := span.SpanContext().TraceID().String()
	wantSpanID := span.SpanContext().SpanID().String()
	if got[1] != wantTraceID {
		t.Errorf("trace_id = %v, want %q (the real span's own TraceID)", got[1], wantTraceID)
	}
	if got[3] != wantSpanID {
		t.Errorf("span_id = %v, want %q (the real span's own SpanID)", got[3], wantSpanID)
	}
}

// TestChatCompletionLogLineIncludesTopLevelTraceAndSpanIDMatchingGatewayEvents
// is the load-bearing proof for the round-2 backlog audit's final
// gateway-observability finding: trace_id/span_id were only ever visible
// nested inside the gatewayevents_v1 JSON string, never as top-level
// fields an operator could filter/grep a log line on directly, the way
// every other structured field on the same "chat_completion" line
// already works. Proves both that the new top-level fields exist AND
// that they carry the SAME values as the pre-existing nested ones — the
// same span, not a second, different one.
func TestChatCompletionLogLineIncludesTopLevelTraceAndSpanIDMatchingGatewayEvents(t *testing.T) {
	var logBuf bytes.Buffer
	p := crossInstanceTestPipeline(t, &logBuf)

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	var chatCompletionLine string
	for _, line := range strings.Split(logBuf.String(), "\n") {
		if strings.Contains(line, `"msg":"chat_completion"`) {
			chatCompletionLine = line
			break
		}
	}
	if chatCompletionLine == "" {
		t.Fatalf("no chat_completion log line found; full log output:\n%s", logBuf.String())
	}

	topLevelTraceID := extractJSONStringField(t, chatCompletionLine, "trace_id")
	topLevelSpanID := extractJSONStringField(t, chatCompletionLine, "span_id")
	if topLevelTraceID == "" {
		t.Fatal("chat_completion line has no top-level trace_id field at all")
	}
	if topLevelSpanID == "" {
		t.Fatal("chat_completion line has no top-level span_id field at all")
	}

	// gatewayevents_v1 is a JSON-encoded STRING value on this same line
	// (protojson.Marshal's output, per logRequest's own doc comment) --
	// its own traceId/spanId keys are backslash-escaped inside that outer
	// string (`\"traceId\":\"...\"`), so extractJSONStringField's plain
	// `"traceId":"` pattern would find nothing; extract the escaped form
	// directly instead.
	_, nestedRest, ok := strings.Cut(chatCompletionLine, `"gatewayevents_v1":"`)
	if !ok {
		t.Fatal("chat_completion line has no gatewayevents_v1 field at all")
	}
	nestedTraceID := extractEscapedJSONStringField(t, nestedRest, "traceId")
	nestedSpanID := extractEscapedJSONStringField(t, nestedRest, "spanId")
	if nestedTraceID != topLevelTraceID {
		t.Errorf("top-level trace_id = %q, nested gatewayevents_v1.traceId = %q, want equal (same span)", topLevelTraceID, nestedTraceID)
	}
	if nestedSpanID != topLevelSpanID {
		t.Errorf("top-level span_id = %q, nested gatewayevents_v1.spanId = %q, want equal (same span)", topLevelSpanID, nestedSpanID)
	}
}

// extractEscapedJSONStringField is extractJSONStringField's counterpart
// for a field living inside an ALREADY-JSON-ENCODED string value (e.g.
// gatewayevents_v1's own protojson.Marshal output, itself embedded as one
// string field on the outer slog JSON line) -- its quotes are
// backslash-escaped one level deeper than a plain top-level field's.
func extractEscapedJSONStringField(t *testing.T, s, field string) string {
	t.Helper()
	_, rest, ok := strings.Cut(s, `\"`+field+`\":\"`)
	if !ok {
		return ""
	}
	value, _, ok := strings.Cut(rest, `\"`)
	if !ok {
		return ""
	}
	return value
}

// TestBudgetWarnThresholdLogIncludesTraceAndSpanID proves the fix reaches
// a SECOND, independent mid-pipeline Warn call site, not just logRequest's
// own final line -- checkBudgetWarnThreshold is one of the exact call
// sites the original finding named.
func TestBudgetWarnThresholdLogIncludesTraceAndSpanID(t *testing.T) {
	var logBuf bytes.Buffer
	vk := identity.VirtualKey{
		ID:                "warn-key-trace",
		KeyHash:           testHashOf("warn-key-trace"),
		BudgetUSD:         decimal.NewFromFloat(0.01),
		BudgetWarnPercent: 0.5, // warn at $0.005; the real call below costs $0.011
		RateLimitBurst:    100,
		RateLimitRefill:   100,
	}
	p := warnThresholdTestPipeline(t, vk, &logBuf)

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer warn-key-trace", req); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	var warnLine string
	for _, line := range strings.Split(logBuf.String(), "\n") {
		if strings.Contains(line, `"msg":"budget_warn_threshold_crossed"`) {
			warnLine = line
			break
		}
	}
	if warnLine == "" {
		t.Fatalf("no budget_warn_threshold_crossed log line found; full log output:\n%s", logBuf.String())
	}
	if extractJSONStringField(t, warnLine, "trace_id") == "" {
		t.Errorf("budget_warn_threshold_crossed line has no trace_id field; line: %s", warnLine)
	}
	if extractJSONStringField(t, warnLine, "span_id") == "" {
		t.Errorf("budget_warn_threshold_crossed line has no span_id field; line: %s", warnLine)
	}
}
