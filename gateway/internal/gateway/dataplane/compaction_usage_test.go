package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// The bodies below are the real Bedrock InvokeModel responses captured on
// 2026-10-14 (Anthropic's native shape; round-5 Tier A item 3), with the
// compaction block's text and signature shortened. On-demand compaction
// (compact-2026-09-04) reports top-level zeros and the summarization call only
// in usage.iterations; before this change the gateway billed it $0.
const onDemandCompactionBody = `{"id":"msg_bdrk_01Compact","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[{"type":"compaction","content":"The user is planning a small garden.","signature":"sig"}],"stop_reason":"compaction","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"iterations":[{"type":"compaction","input_tokens":172,"output_tokens":241,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}}],"service_tier":"standard"}}`

// A failed summarization per Anthropic's compaction-on-demand page: still a
// 200, empty content, an ordinary stop_reason, and the compaction iteration
// still billed. Representable, so without a gate it would be cached and a
// retry would replay the failure for the life of the TTL.
const failedSummarizationBody = `{"id":"msg_fail","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"iterations":[{"type":"compaction","input_tokens":9000,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}]}}`

// The beta on, no compaction: iterations holds the message entry alone.
const messageOnlyIterationsBody = `{"id":"msg_plain","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":114,"output_tokens":16,"iterations":[{"type":"message","input_tokens":114,"output_tokens":16,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}]}}`

const advisorIterationsBody = `{"id":"msg_adv","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":100,"output_tokens":10,"iterations":[{"type":"advisor_message","model":"claude-opus-5-5","input_tokens":300,"output_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},{"type":"message","model":"claude-haiku-5-5","input_tokens":100,"output_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}]}}`

func anthropicResponseFromJSON(t *testing.T, body string) *anthropic.Response {
	t.Helper()
	var r anthropic.Response
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &r
}

// Through finalize: the compaction iteration's 172/241 is priced at the
// serving model's rates (memberPriceTable: 0.00001 / 0.00005), 0.01377 --
// today the key is charged $0 for a call Anthropic billed.
func TestHandleChatCompletionBillsCompactionIterations(t *testing.T) {
	deployments := []Deployment{{Name: "d1", Model: "claude-sonnet-5-5", Provider: "anthropic", UpstreamModel: "claude-sonnet-5-5", BaseURL: "http://unused"}}
	p, tracker := memberPipeline(t, deployments, adapter.Registry{"anthropic": anthropic.New()}, func(context.Context, Deployment, any) (any, error) {
		return anthropicResponseFromJSON(t, onDemandCompactionBody), nil
	})
	req := adapter.ChatRequest{Model: "claude-sonnet-5-5", Messages: []adapter.Message{{Role: "user", Content: "summarize"}}}
	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, "")
	if err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if resp.Usage.PromptTokens != 172 || resp.Usage.CompletionTokens != 241 || resp.Usage.CompactionIterations != 1 {
		t.Errorf("Usage = %+v, want 172/241 with one compaction iteration", resp.Usage)
	}
	want := decimal.RequireFromString("0.01377")
	if got := tracker.SpentUSD(context.Background(), "test-key", 0); !got.Equal(want) {
		t.Errorf("SpentUSD = %v, want %v (the compaction iteration priced)", got, want)
	}
}

// A response that ran a compaction is not a reusable answer: it is never
// written to the cache, even when it is representable (a failed
// summarization: empty content, stop_reason end_turn). A message-only
// iterations array keeps the normal cache behaviour.
func TestHandleChatCompletionNeverCachesAResponseThatRanCompaction(t *testing.T) {
	run := func(t *testing.T, body string) int {
		t.Helper()
		calls := 0
		p := newTestPipeline(t, func(context.Context, Deployment, any) (any, error) {
			calls++
			return anthropicResponseFromJSON(t, body), nil
		}, []Deployment{{Name: "d-1", Model: "m", Provider: "anthropic", UpstreamModel: "claude-haiku-5-5", BaseURL: "http://unused"}})
		p.adapters["anthropic"] = anthropic.New()
		req := adapter.ChatRequest{Model: "m", Messages: []adapter.Message{{Role: "user", Content: "please summarize the plan"}}}
		for i := 0; i < 2; i++ {
			if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
				t.Fatalf("call %d: %v", i+1, err)
			}
		}
		return calls
	}
	if calls := run(t, failedSummarizationBody); calls != 2 {
		t.Errorf("failed summarization: upstream calls = %d, want 2 (a compaction result is never cached)", calls)
	}
	if calls := run(t, messageOnlyIterationsBody); calls != 1 {
		t.Errorf("message-only iterations: upstream calls = %d, want 1 (cached as before)", calls)
	}
}

// The streamed twin of the gate above.
func TestHandleChatCompletionStreamNeverCachesAResponseThatRanCompaction(t *testing.T) {
	failed := anthropicSSE(
		`event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_fs","type":"message","role":"assistant","content":[],"model":"claude-haiku-5-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`,
		`event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0,"iterations":[{"type":"compaction","input_tokens":9000,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}]}}`,
		`event: message_stop`+"\n"+`data: {"type":"message_stop"}`,
	)
	normal := anthropicSSE(
		`event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_ok","type":"message","role":"assistant","content":[],"model":"claude-haiku-5-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`,
		`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
		`event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2,"iterations":[{"type":"message","input_tokens":5,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}]}}`,
		`event: message_stop`+"\n"+`data: {"type":"message_stop"}`,
	)
	run := func(t *testing.T, wire string) int {
		t.Helper()
		calls := 0
		p := newStreamingTestPipeline(t, func(context.Context, Deployment, any) (io.ReadCloser, error) {
			calls++
			return io.NopCloser(strings.NewReader(wire)), nil
		}, []Deployment{{Name: "d-1", Model: "m", Provider: "anthropic", UpstreamModel: "claude-haiku-5-5", BaseURL: "http://unused"}},
			adapter.Registry{"anthropic": anthropic.New()})
		req := adapter.ChatRequest{Model: "m", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "please summarize the plan"}}}
		for i := 0; i < 2; i++ {
			if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, httptest.NewRecorder(), ""); err != nil {
				t.Fatalf("stream %d: %v", i+1, err)
			}
		}
		return calls
	}
	if calls := run(t, failed); calls != 2 {
		t.Errorf("streamed failed summarization: upstream calls = %d, want 2 (never cached)", calls)
	}
	if calls := run(t, normal); calls != 1 {
		t.Errorf("streamed message-only iterations: upstream calls = %d, want 1 (cached as before)", calls)
	}
}

// The real on-demand compaction stream (Bedrock InvokeModelWithResponseStream,
// 2026-10-14; signature shortened), as the Anthropic API frames it.
var onDemandCompactionFrames = []string{
	"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_bdrk_01Stream\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-haiku-5-5\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0,\"cache_creation\":{\"ephemeral_5m_input_tokens\":0,\"ephemeral_1h_input_tokens\":0},\"service_tier\":\"standard\"}}}\n\n",
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"compaction\",\"content\":\"The user is planning a small garden.\",\"signature\":\"sig\"}}\n\n",
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"compaction\",\"stop_sequence\":null},\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0,\"iterations\":[{\"type\":\"compaction\",\"input_tokens\":172,\"output_tokens\":224,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0,\"cache_creation\":{\"ephemeral_5m_input_tokens\":0,\"ephemeral_1h_input_tokens\":0}}]}}\n\n",
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
}

// On the /v1/messages passthrough relay the client receives Anthropic's bytes
// unchanged -- top-level zeros and the iterations array -- while the shadow
// the gateway meters carries the summed 172/224.
func TestHandleChatCompletionStreamSinkRelaysCompactionFramesAndMetersTheIterations(t *testing.T) {
	calls := map[string]int{}
	p := newRelayTestPipeline(t, func(Deployment) string { return strings.Join(onDemandCompactionFrames, "") }, calls)
	ctx, _ := WithUpstreamResponseMeta(context.Background())
	sink := &recordingRelaySink{}
	if err := p.HandleChatCompletionStreamSink(ctx, "Bearer relay-secret", "", "", relayPassthroughRequest("claude-sys"), sink, ""); err != nil {
		t.Fatalf("HandleChatCompletionStreamSink: %v", err)
	}
	if strings.Join(sink.raw, "") != strings.Join(onDemandCompactionFrames, "") {
		t.Fatalf("relayed frames differ from the upstream's: %q", sink.raw)
	}
	if len(sink.ends) != 1 {
		t.Fatalf("ends = %+v, want one WriteFinish", sink.ends)
	}
	u := sink.ends[0].Usage
	if u.PromptTokens != 172 || u.CompletionTokens != 224 || u.TotalTokens != 396 || u.CompactionIterations != 1 {
		t.Errorf("metered usage = %+v, want 172/224/396 with one compaction iteration", u)
	}
	if calls["gpt-fallback"] != 0 {
		t.Errorf("fallback deployment called %d times on a clean relay", calls["gpt-fallback"])
	}
}

// Iterations that name another model (advisor_message, fallback_message) are
// priced at the serving model's rates; finalize says so once per response.
func TestFinalizeWarnsWhenUsageIterationsNameAnotherModel(t *testing.T) {
	var logBuf bytes.Buffer
	p := newTestPipeline(t, func(context.Context, Deployment, any) (any, error) {
		return anthropicResponseFromJSON(t, advisorIterationsBody), nil
	}, []Deployment{{Name: "d-1", Model: "m", Provider: "anthropic", UpstreamModel: "claude-haiku-5-5", BaseURL: "http://unused"}})
	p.logger = slog.New(slog.NewJSONHandler(&logBuf, nil))
	p.adapters["anthropic"] = anthropic.New()
	req := adapter.ChatRequest{Model: "m", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, "")
	if err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if resp.Usage.PromptTokens != 400 || resp.Usage.CompletionTokens != 30 || resp.Usage.ForeignModelIterations != 1 {
		t.Errorf("Usage = %+v, want 400/30 with one foreign-model iteration", resp.Usage)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, `"msg":"usage_iterations_foreign_model"`) || !strings.Contains(logs, `"foreign_models":"claude-opus-5-5"`) || !strings.Contains(logs, `"virtual_key_id":"`) {
		t.Errorf("want a usage_iterations_foreign_model line naming the foreign model and the key; logs:\n%s", logs)
	}
}

// The chat span carries the compaction count beside the summed token counts.
func TestHandleChatCompletionEmitsCompactionIterationsSpanAttribute(t *testing.T) {
	before := len(spanRecorder.Ended())
	deployments := []Deployment{{Name: "d1", Model: "claude-sonnet-5-5", Provider: "anthropic", UpstreamModel: "claude-sonnet-5-5", BaseURL: "http://unused"}}
	p, _ := memberPipeline(t, deployments, adapter.Registry{"anthropic": anthropic.New()}, func(context.Context, Deployment, any) (any, error) {
		return anthropicResponseFromJSON(t, onDemandCompactionBody), nil
	})
	req := adapter.ChatRequest{Model: "claude-sonnet-5-5", Messages: []adapter.Message{{Role: "user", Content: "summarize"}}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	spans := spansSince(before)
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}
	if v, ok := spanAttr(t, spans[0].Attributes(), telemetry.AttrKelvranUsageCompactionIterations); !ok || v.AsInt64() != 1 {
		t.Errorf("%s = %v, ok=%v, want 1", telemetry.AttrKelvranUsageCompactionIterations, v, ok)
	}
	if v, ok := spanAttr(t, spans[0].Attributes(), telemetry.AttrGenAIUsageInputTokens); !ok || v.AsInt64() != 172 {
		t.Errorf("%s = %v, ok=%v, want 172 (summed from the compaction iteration)", telemetry.AttrGenAIUsageInputTokens, v, ok)
	}
}
