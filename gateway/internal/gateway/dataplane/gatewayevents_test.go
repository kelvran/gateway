package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	gatewayeventsv1 "github.com/kelvran/gateway/api/gatewayevents/v1"
	"github.com/kelvran/gateway/internal/adapter"
	"github.com/kelvran/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/internal/budget"
	"github.com/kelvran/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/internal/identity"
	"github.com/kelvran/gateway/internal/ratelimit"
)

// TestOutcomeForClassifiesEverySentinelError proves outcomeFor's
// errors.Is chain covers every rejection HandleChatCompletion/
// HandleChatCompletionStream can actually produce today, per
// docs/rfcs/2026-09-03-api-gatewayevents-contract.md's "Outcome must be
// derivable from finalize's existing err parameter alone" constraint.
func TestOutcomeForClassifiesEverySentinelError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want gatewayeventsv1.GatewayDecisionEvent_Outcome
	}{
		{"nil", nil, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_OK},
		{"missing header", identity.ErrMissingHeader, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_AUTH_FAILED},
		{"invalid key", identity.ErrInvalidKey, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_AUTH_FAILED},
		{"model not allowed", ErrModelNotAllowed, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_MODEL_NOT_ALLOWED},
		{"rate limited", ErrRateLimited, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_RATE_LIMITED},
		{"budget exceeded", ErrBudgetExceeded, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_BUDGET_EXCEEDED},
		{"no deployment", ErrNoDeployment, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_NO_DEPLOYMENT},
		{"generic upstream error", context.DeadlineExceeded, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_UPSTREAM_ERROR},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := outcomeFor(tt.err); got != tt.want {
				t.Errorf("outcomeFor(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// decodeLoggedGatewayEvent parses the last "gatewayevents_v1" field out of
// logBuf's JSON-per-line slog output and decodes it via the real
// generated Go bindings — proving the producer side of
// docs/rfcs/2026-09-03-api-gatewayevents-contract.md end-to-end, not just
// that outcomeFor's switch statement is correct in isolation.
func decodeLoggedGatewayEvent(t *testing.T, logBuf *bytes.Buffer) *gatewayeventsv1.GatewayDecisionEvent {
	t.Helper()
	var lastLine map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(logBuf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		lastLine = nil
		if err := json.Unmarshal(line, &lastLine); err != nil {
			t.Fatalf("unmarshaling log line %q: %v", line, err)
		}
	}
	if lastLine == nil {
		t.Fatal("no log lines captured")
	}
	raw, ok := lastLine["gatewayevents_v1"].(string)
	if !ok {
		t.Fatalf("log line %+v has no string gatewayevents_v1 field", lastLine)
	}
	event := &gatewayeventsv1.GatewayDecisionEvent{}
	if err := protojson.Unmarshal([]byte(raw), event); err != nil {
		t.Fatalf("protojson.Unmarshal(%q): %v", raw, err)
	}
	return event
}

func TestGatewayEventLoggedOnSuccessHasOutcomeOK(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	keys := []identity.VirtualKey{
		{ID: "team-events", KeyHash: testHashOf("team-events"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	p := newTestPipelineWithKeysAndLogger(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}, nil, keys, logger)

	_, err := p.HandleChatCompletion(context.Background(), "Bearer team-events", adapter.ChatRequest{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	event := decodeLoggedGatewayEvent(t, &logBuf)
	if event.GetOutcome() != gatewayeventsv1.GatewayDecisionEvent_OUTCOME_OK {
		t.Errorf("Outcome = %v, want OUTCOME_OK", event.GetOutcome())
	}
	if event.GetVirtualKeyId() != "team-events" {
		t.Errorf("VirtualKeyId = %q, want %q", event.GetVirtualKeyId(), "team-events")
	}
	if event.GetRequestedModel() != "gpt-4o" {
		t.Errorf("RequestedModel = %q, want %q", event.GetRequestedModel(), "gpt-4o")
	}
	if event.GetTraceId() == "" {
		t.Error("TraceId is empty, want a real trace ID from the OTel span")
	}
}

func TestGatewayEventLoggedOnRejectionHasCorrectOutcome(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	keys := []identity.VirtualKey{
		{
			ID: "team-restricted", KeyHash: testHashOf("team-restricted"),
			RateLimitBurst: 100, RateLimitRefill: 100,
			AllowedModels: map[string]struct{}{"claude-opus-4": {}},
		},
	}
	p := newTestPipelineWithKeysAndLogger(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		t.Fatal("Upstream should never be called for a model-not-allowed rejection")
		return nil, nil
	}, nil, keys, logger)

	_, err := p.HandleChatCompletion(context.Background(), "Bearer team-restricted", adapter.ChatRequest{Model: "gpt-4o"})
	if err == nil {
		t.Fatal("HandleChatCompletion succeeded, want ErrModelNotAllowed")
	}

	event := decodeLoggedGatewayEvent(t, &logBuf)
	if event.GetOutcome() != gatewayeventsv1.GatewayDecisionEvent_OUTCOME_MODEL_NOT_ALLOWED {
		t.Errorf("Outcome = %v, want OUTCOME_MODEL_NOT_ALLOWED", event.GetOutcome())
	}
	if event.GetVirtualKeyId() != "team-restricted" {
		t.Errorf("VirtualKeyId = %q, want %q", event.GetVirtualKeyId(), "team-restricted")
	}
}

// newTestPipelineWithKeysAndLogger mirrors newTestPipelineWithKeysAndBudget
// (dataplane_test.go) exactly, except it lets a test supply its own
// logger (to capture and decode gatewayevents_v1 log output) instead of
// the always-discarding discardLogger().
func newTestPipelineWithKeysAndLogger(t *testing.T, upstream UpstreamCaller, deployments []Deployment, keys []identity.VirtualKey, logger *slog.Logger) *Pipeline {
	t.Helper()

	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if deployments == nil {
		deployments = []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	}
	p, err := NewPipeline(Config{
		Verifier: verifier,
		Limiter:  ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:   budget.NewTracker(),
		Cache:    inprocess.New(),
		Adapters: adapter.Registry{
			"openai": openai.New(),
		},
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream:       upstream,
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}
