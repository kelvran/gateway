package dataplane

import (
	"bytes"
	"context"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// TestHandleChatCompletionAnthropicHopSpanReportsPassthroughAndNothingDropped
// is TestHandleChatCompletionLossyIngressSpanCarriesIngressAttributes' twin
// for an anthropic deployment (item 11 slice S11a): the upstream receives a
// *anthropic.PassthroughRequest carrying the raw body, and the span says the
// hop relayed as received -- kelvran.ingress.passthrough true, no
// kelvran.ingress.dropped_fields even though the shadow had an unknown
// member.
func TestHandleChatCompletionAnthropicHopSpanReportsPassthroughAndNothingDropped(t *testing.T) {
	before := len(spanRecorder.Ended())
	var sawRaw bool
	p := newMixedProviderTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if pr, ok := req.(*anthropic.PassthroughRequest); ok && bytes.Contains(pr.Raw, []byte(`"zeta"`)) && bytes.Contains(pr.Raw, []byte(`"claude-up"`)) {
			sawRaw = true
		}
		return &anthropic.Response{
			ID: "msg_1", Model: dep.UpstreamModel, Role: "assistant",
			Content:    []anthropic.ContentBlock{{Type: "text", Text: "ok"}},
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 3, OutputTokens: 1},
		}, nil
	}, []Deployment{{Name: "claude-primary", Model: "claude-sys", Provider: "anthropic", UpstreamModel: "claude-up", BaseURL: "http://unused"}})

	req := adapter.ChatRequest{
		Model:       "claude-sys",
		Messages:    []adapter.Message{{Role: "user", Content: "hi"}},
		Passthrough: passthroughWith(`{"model":"claude-sys","max_tokens":5,"messages":[{"role":"user","content":"hi"}],"zeta":1}`, map[string]string{"/zeta": `1`}),
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if !sawRaw {
		t.Fatal("the upstream did not receive a PassthroughRequest carrying the raw body with the upstream model id")
	}
	spans := spansSince(before)
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}
	attrs := spans[0].Attributes()
	if v, ok := spanAttr(t, attrs, telemetry.AttrKelvranIngressFormat); !ok || v.AsString() != "anthropic-messages" {
		t.Errorf("%s = %v, ok=%v", telemetry.AttrKelvranIngressFormat, v, ok)
	}
	if v, ok := spanAttr(t, attrs, telemetry.AttrKelvranIngressPassthrough); !ok || !v.AsBool() {
		t.Errorf("%s = %v, ok=%v, want true on an anthropic hop", telemetry.AttrKelvranIngressPassthrough, v, ok)
	}
	if v, ok := spanAttr(t, attrs, telemetry.AttrKelvranIngressDroppedFields); ok {
		t.Errorf("%s = %v, want absent: nothing is dropped on a passthrough hop", telemetry.AttrKelvranIngressDroppedFields, v)
	}
}
