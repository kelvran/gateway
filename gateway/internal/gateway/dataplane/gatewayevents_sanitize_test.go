package dataplane

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

// TestGatewayEventRequestedModelIsSanitised: the decision event's
// requested_model is the client's model string before any resolution and
// rides every request's log line as protojson. It must go through the same
// sanitiser as the span name: a C1 control would otherwise pass protojson
// raw (it escapes only below U+0020 and the two line separators), and
// invalid UTF-8 makes protojson.Marshal fail, dropping the whole event from
// the line (found by the live proof on 2026-10-12).
func TestGatewayEventRequestedModelIsSanitised(t *testing.T) {
	cases := []struct{ name, model, want string }{
		{"C1 control and ESC", "gpt\u0085X\x1b[0m", "gptX[0m"},
		{"invalid UTF-8", "gpt\xffX", "gptX"},
		{"rune straddling the bound", strings.Repeat("a", maxModelForTelemetry-1) + "é", strings.Repeat("a", maxModelForTelemetry-1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
			keys := []identity.VirtualKey{{ID: "team-events", KeyHash: testHashOf("team-events"), RateLimitBurst: 100, RateLimitRefill: 100}}
			p := newTestPipelineWithKeysAndLogger(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
				return fakeOpenAIResponse("gpt-4o"), nil
			}, nil, keys, logger)
			_, err := p.HandleChatCompletion(context.Background(), "Bearer team-events", "", "", adapter.ChatRequest{
				Model: c.model, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
			}, "")
			if err == nil {
				t.Fatal("an unknown model must fail; the logged event is what is under test")
			}
			if strings.Contains(logBuf.String(), "gatewayevents_marshal_failed") {
				t.Fatalf("the decision event failed to marshal, so the line lost it: %s", logBuf.String())
			}
			event := decodeLoggedGatewayEvent(t, &logBuf)
			if got := event.GetRequestedModel(); got != c.want || !utf8.ValidString(got) {
				t.Errorf("RequestedModel = %q (valid UTF-8 %v), want %q", got, utf8.ValidString(got), c.want)
			}
		})
	}
}
