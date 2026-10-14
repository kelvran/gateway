package dataplane

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
)

func claudeGatesRun(t *testing.T, provider, upstreamModel string, upstream UpstreamCaller, req adapter.ChatRequest) (string, error) {
	t.Helper()
	var logBuf bytes.Buffer
	p := newTestPipeline(t, upstream, []Deployment{{Name: "d-1", Model: "m", Provider: provider, UpstreamModel: upstreamModel, BaseURL: "http://unused"}})
	p.logger = slog.New(slog.NewJSONHandler(&logBuf, nil))
	p.adapters["bedrock"] = bedrock.New()
	p.adapters["anthropic"] = anthropic.New()
	req.Model = "m"
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, "")
	return logBuf.String(), err
}

// A rejected sampling value is logged on the same request_field_dropped line
// top_k and thinking already use, on both Claude-serving providers; a family
// that accepts the value, and openai (which forwards every sampling field),
// stay silent.
func TestCallDeploymentLogsRequestFieldDroppedForSamplingOnClaude55(t *testing.T) {
	half, p99, one := 0.5, 0.99, 1.0
	anthropicUp := func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeAnthropicResponse(dep.UpstreamModel), nil
	}
	bedrockUp := func(ctx context.Context, dep Deployment, req any) (any, error) { return fakeBedrockResponse("ok"), nil }
	openaiUp := func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}
	msg := []adapter.Message{{Role: "user", Content: "hi"}}

	logs, err := claudeGatesRun(t, "anthropic", "claude-sonnet-5-5", anthropicUp, adapter.ChatRequest{Messages: msg, Temperature: &half})
	if err != nil {
		t.Fatalf("anthropic: %v", err)
	}
	if !strings.Contains(logs, `"msg":"request_field_dropped"`) || !strings.Contains(logs, `"field":"temperature"`) || !strings.Contains(logs, `"value":"0.5"`) {
		t.Errorf("want a request_field_dropped line for temperature 0.5 on Sonnet 5.5; logs:\n%s", logs)
	}
	logs, err = claudeGatesRun(t, "bedrock", "global.anthropic.claude-haiku-5-5", bedrockUp, adapter.ChatRequest{Messages: msg, Temperature: &one, TopP: &p99})
	if err != nil {
		t.Fatalf("bedrock: %v", err)
	}
	if !strings.Contains(logs, `"field":"top_p"`) || strings.Contains(logs, `"field":"temperature"`) {
		t.Errorf("temperature 1 + top_p 0.99 on Bedrock Haiku 5.5: want top_p dropped and temperature kept; logs:\n%s", logs)
	}
	logs, err = claudeGatesRun(t, "anthropic", "claude-sonnet-4-6", anthropicUp, adapter.ChatRequest{Messages: msg, Temperature: &half})
	if err != nil {
		t.Fatalf("anthropic 4.6: %v", err)
	}
	if strings.Contains(logs, `"field":"temperature"`) {
		t.Errorf("Sonnet 4.6 accepts temperature; a drop was logged:\n%s", logs)
	}
	logs, err = claudeGatesRun(t, "openai", "gpt-4o", openaiUp, adapter.ChatRequest{Messages: msg, Temperature: &half})
	if err != nil {
		t.Fatalf("openai: %v", err)
	}
	if strings.Contains(logs, `"field":"temperature"`) {
		t.Errorf("openai forwards temperature; a drop was logged:\n%s", logs)
	}
}

// thinking disabled is logged with the treatment the model needs: rewritten to
// between_tools on Sonnet 5.5, dropped on Fable 5.1.
func TestCallDeploymentLogsThinkingDisabledTreatment(t *testing.T) {
	anthropicUp := func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeAnthropicResponse(dep.UpstreamModel), nil
	}
	msg := []adapter.Message{{Role: "user", Content: "hi"}}
	logs, err := claudeGatesRun(t, "anthropic", "claude-sonnet-5-5", anthropicUp, adapter.ChatRequest{Messages: msg, Thinking: &adapter.ThinkingConfig{Type: "disabled"}})
	if err != nil {
		t.Fatalf("sonnet 5.5: %v", err)
	}
	if !strings.Contains(logs, `"field":"thinking"`) || !strings.Contains(logs, "between_tools") {
		t.Errorf("want a thinking line naming the between_tools rewrite on Sonnet 5.5; logs:\n%s", logs)
	}
	logs, err = claudeGatesRun(t, "anthropic", "claude-fable-5-1", anthropicUp, adapter.ChatRequest{Messages: msg, Thinking: &adapter.ThinkingConfig{Type: "disabled"}})
	if err != nil {
		t.Fatalf("fable 5.1: %v", err)
	}
	if !strings.Contains(logs, `"field":"thinking"`) || !strings.Contains(logs, "always on") {
		t.Errorf("want a thinking drop line saying thinking is always on for Fable 5.1; logs:\n%s", logs)
	}
}

// A conversation ending on an assistant turn is a 400 on the models that
// reject prefill (proven on Bedrock Sonnet 5.5, Haiku 5.5, Fable 5.1,
// 2026-10-14): the gateway refuses it with its own typed error before any
// upstream call, exactly as response_format_unsupported does, and leaves the
// families that accept prefill alone.
func TestHandleChatCompletionRejectsAssistantPrefillOnRejectingClaudeFamilies(t *testing.T) {
	calls := 0
	anthropicUp := func(ctx context.Context, dep Deployment, req any) (any, error) {
		calls++
		return fakeAnthropicResponse(dep.UpstreamModel), nil
	}
	prefill := []adapter.Message{{Role: "user", Content: "Say hi."}, {Role: "assistant", Content: "Hi"}}
	_, err := claudeGatesRun(t, "anthropic", "claude-haiku-5-5", anthropicUp, adapter.ChatRequest{Messages: prefill})
	if !errors.Is(err, adapter.ErrAssistantPrefillUnsupported) {
		t.Fatalf("Haiku 5.5 prefill: err = %v, want ErrAssistantPrefillUnsupported", err)
	}
	if strings.Contains(err.Error(), "upstream call failed") {
		t.Errorf("prefill is decided at the first-pick site, before any call; its text must not claim one failed: %v", err)
	}
	if calls != 0 {
		t.Errorf("upstream was called %d times for a request the gateway should refuse", calls)
	}
	if _, err := claudeGatesRun(t, "anthropic", "claude-haiku-4-5", anthropicUp, adapter.ChatRequest{Messages: prefill}); err != nil {
		t.Fatalf("Haiku 4.5 accepts prefill: %v", err)
	}
}

// A pool with an accepting deployment serves the prefill there: prefill is
// a routing property (capabilityOKForRequest), so the reroute that already
// moves response_format and tool-result parts to a capable deployment moves
// a prefill off a rejecting family too, whichever deployment was picked
// first.
func TestHandleChatCompletionReroutesPrefillToAnAcceptingDeployment(t *testing.T) {
	served := map[string]int{}
	up := func(ctx context.Context, dep Deployment, req any) (any, error) {
		served[dep.UpstreamModel]++
		return fakeAnthropicResponse(dep.UpstreamModel), nil
	}
	p := newTestPipeline(t, up, []Deployment{
		{Name: "d-55", Model: "m", Provider: "anthropic", UpstreamModel: "claude-sonnet-5-5", BaseURL: "http://unused"},
		{Name: "d-45", Model: "m", Provider: "anthropic", UpstreamModel: "claude-haiku-4-5", BaseURL: "http://unused"},
	})
	p.adapters["anthropic"] = anthropic.New()
	prefill := []adapter.Message{{Role: "user", Content: "Say hi."}, {Role: "assistant", Content: "Hi"}}
	for i := 0; i < 3; i++ {
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{Model: "m", Messages: prefill}, ""); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if served["claude-sonnet-5-5"] != 0 || served["claude-haiku-4-5"] == 0 {
		t.Errorf("served = %v, want every prefill on claude-haiku-4-5 and none on claude-sonnet-5-5", served)
	}
}

// The streaming route refuses a prefill the same way, before any upstream
// call, and serves the accepting family.
func TestHandleChatCompletionStreamRejectsAssistantPrefillOnRejectingClaudeFamilies(t *testing.T) {
	wire := anthropicSSE(
		`event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_pf","type":"message","role":"assistant","content":[],"model":"claude-haiku-4-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`,
		`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" there"}}`,
		`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
		`event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
		`event: message_stop`+"\n"+`data: {"type":"message_stop"}`,
	)
	run := func(t *testing.T, upstreamModel string) (int, error) {
		t.Helper()
		calls := 0
		p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
			calls++
			return io.NopCloser(strings.NewReader(wire)), nil
		}, []Deployment{{Name: "d-1", Model: "m", Provider: "anthropic", UpstreamModel: upstreamModel, BaseURL: "http://unused"}},
			adapter.Registry{"anthropic": anthropic.New()})
		req := adapter.ChatRequest{Model: "m", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "Say hi."}, {Role: "assistant", Content: "Hi"}}}
		err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, httptest.NewRecorder(), "")
		return calls, err
	}
	calls, err := run(t, "claude-sonnet-5-5")
	if !errors.Is(err, adapter.ErrAssistantPrefillUnsupported) || calls != 0 {
		t.Errorf("Sonnet 5.5 streamed prefill: err = %v, upstream calls = %d; want the sentinel and no call", err, calls)
	}
	if err != nil && strings.Contains(err.Error(), "upstream call failed") {
		t.Errorf("streamed prefill is decided at the first-pick site; its text must not claim a call failed: %v", err)
	}
	if calls, err := run(t, "claude-haiku-4-5"); err != nil || calls != 1 {
		t.Errorf("Haiku 4.5 streamed prefill: err = %v, upstream calls = %d; want served once", err, calls)
	}
}

// The /v1/messages ingress sets Passthrough on every request. Only an
// anthropic deployment relays the body as received (isPassthroughHop); a
// bedrock deployment re-encodes the shadow, so its prefill is still refused
// and its sampling drops still logged. On the relaying hop nothing the
// gateway encodes is dropped, so no request_field_dropped line is written
// and the prefill is the upstream's own call.
func TestClaudeGatesOnTheMessagesIngressFollowTheHopNotTheField(t *testing.T) {
	half := 0.5
	prefill := []adapter.Message{{Role: "user", Content: "Say hi."}, {Role: "assistant", Content: "Hi"}}
	raw := []byte(`{"model":"claude-sonnet-5-5","max_tokens":8,"messages":[{"role":"user","content":"Say hi."},{"role":"assistant","content":"Hi"}]}`)
	translateHop := &adapter.Passthrough{Format: adapter.IngressFormatAnthropicMessages}
	relayHop := &adapter.Passthrough{Format: adapter.IngressFormatAnthropicMessages, RawBody: raw}

	bedrockCalls := 0
	bedrockUp := func(ctx context.Context, dep Deployment, req any) (any, error) {
		bedrockCalls++
		return fakeBedrockResponse("ok"), nil
	}
	_, err := claudeGatesRun(t, "bedrock", "global.anthropic.claude-sonnet-5-5", bedrockUp, adapter.ChatRequest{Messages: prefill, Passthrough: translateHop})
	if !errors.Is(err, adapter.ErrAssistantPrefillUnsupported) || bedrockCalls != 0 {
		t.Errorf("bedrock translate hop: err = %v, calls = %d; want the sentinel and no call", err, bedrockCalls)
	}

	anthropicCalls := 0
	anthropicUp := func(ctx context.Context, dep Deployment, req any) (any, error) {
		anthropicCalls++
		return fakeAnthropicResponse(dep.UpstreamModel), nil
	}
	if _, err := claudeGatesRun(t, "anthropic", "claude-sonnet-5-5", anthropicUp, adapter.ChatRequest{Messages: prefill, Passthrough: relayHop}); err != nil || anthropicCalls != 1 {
		t.Errorf("anthropic relay hop prefill: err = %v, calls = %d; want relayed once, the upstream's answer standing", err, anthropicCalls)
	}
	logs, err := claudeGatesRun(t, "anthropic", "claude-sonnet-5-5", anthropicUp, adapter.ChatRequest{Messages: prefill[:1], Temperature: &half, Passthrough: relayHop})
	if err != nil {
		t.Fatalf("anthropic relay hop sampling: %v", err)
	}
	if strings.Contains(logs, `"msg":"request_field_dropped"`) {
		t.Errorf("a relayed body drops nothing; a request_field_dropped line was written:\n%s", logs)
	}
}

// The model's rule is "the conversation must end with a user message"
// (Bedrock's own ValidationException text), so an assistant turn that ends
// in tool calls is refused like a text prefill.
func TestHandleChatCompletionRejectsAssistantTurnEndingInToolCalls(t *testing.T) {
	calls := 0
	up := func(ctx context.Context, dep Deployment, req any) (any, error) {
		calls++
		return fakeAnthropicResponse(dep.UpstreamModel), nil
	}
	msgs := []adapter.Message{
		{Role: "user", Content: "look it up"},
		{Role: "assistant", ToolCalls: []adapter.ToolCall{{ID: "t1", Name: "lookup", ArgumentsJSON: `{}`}}},
	}
	_, err := claudeGatesRun(t, "anthropic", "claude-sonnet-5-5", up, adapter.ChatRequest{Messages: msgs})
	if !errors.Is(err, adapter.ErrAssistantPrefillUnsupported) || calls != 0 {
		t.Errorf("tool-call-ending assistant turn on Sonnet 5.5: err = %v, calls = %d; want the sentinel and no call", err, calls)
	}
}

// Mythos 5.1 top_k on bedrock is dropped through the fixed-sampling verdict
// (the per-generation table never probed the family) and logged like the
// table's own drops.
func TestCallDeploymentLogsTopKDropForMythos51OnBedrock(t *testing.T) {
	topK := 5
	up := func(ctx context.Context, dep Deployment, req any) (any, error) { return fakeBedrockResponse("ok"), nil }
	logs, err := claudeGatesRun(t, "bedrock", "global.anthropic.claude-mythos-5-1", up, adapter.ChatRequest{Messages: []adapter.Message{{Role: "user", Content: "hi"}}, TopK: &topK})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, `"field":"top_k"`) || !strings.Contains(logs, `"value":"5"`) {
		t.Errorf("want a request_field_dropped line for top_k on Bedrock Mythos 5.1; logs:\n%s", logs)
	}
}
