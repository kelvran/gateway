package dataplane

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

func toolResultPartsRequest(part adapter.ContentPart) adapter.ChatRequest {
	return adapter.ChatRequest{
		Model: "m",
		Messages: []adapter.Message{
			{Role: "user", Content: "call the tool"},
			{Role: "assistant", ToolCalls: []adapter.ToolCall{{ID: "call_1", Name: "take_screenshot", ArgumentsJSON: "{}"}}},
			{Role: "tool", ToolCallID: "call_1", Content: "screenshot attached", Parts: []adapter.ContentPart{part}},
		},
	}
}

// TestCapabilityOKForRequestToolResultParts (item 11 slice S6): a request
// whose role:"tool" message carries media parts is ineligible for a
// deployment whose provider cannot carry them (openai, openaicompat: text
// parts only; gemini: none), eligible for anthropic and bedrock -- the same
// routing property response_format already is.
func TestCapabilityOKForRequestToolResultParts(t *testing.T) {
	image := toolResultPartsRequest(adapter.ContentPart{Type: "image", MediaType: "image/png", Data: "QUJD"})
	text := toolResultPartsRequest(adapter.ContentPart{Type: "text", Text: "ok"})
	for _, tt := range []struct {
		provider string
		req      adapter.ChatRequest
		ok       bool
	}{
		{"anthropic", image, true}, {"bedrock", image, true}, {"openai", image, false}, {"openaicompat", image, false}, {"gemini", image, false},
		{"openai", text, true}, {"gemini", text, false},
	} {
		dep := Deployment{Name: "d", Model: "m", Provider: tt.provider, UpstreamModel: "global.anthropic.claude-sonnet-4-6"}
		if got := capabilityOKForRequest(dep, tt.req); got != tt.ok {
			t.Errorf("capabilityOKForRequest(%s, %s part) = %v, want %v", tt.provider, tt.req.Messages[2].Parts[0].Type, got, tt.ok)
		}
	}
	plain := adapter.ChatRequest{Model: "m", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if !capabilityOKForRequest(Deployment{Provider: "gemini"}, plain) {
		t.Error("a request without tool-result parts must stay eligible everywhere")
	}
}

// TestHandleChatCompletionReroutesToolResultPartsToCapableDeployment: with a
// pool of {openai, anthropic} and an image tool-result part, the first-pick
// reroute lands on the anthropic deployment before any upstream call.
func TestHandleChatCompletionReroutesToolResultPartsToCapableDeployment(t *testing.T) {
	var providers []string
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		providers = append(providers, dep.Provider)
		if dep.Provider == "anthropic" {
			return &anthropic.Response{ID: "msg_1", Model: "claude-sonnet-4-6", Role: "assistant", StopReason: "end_turn", Content: []anthropic.ContentBlock{{Type: "text", Text: "red"}}}, nil
		}
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{
		{Name: "openai-1", Model: "m", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "anthropic-1", Model: "m", Provider: "anthropic", UpstreamModel: "claude-sonnet-4-6", BaseURL: "http://unused"},
	})
	p.adapters["anthropic"] = anthropic.New()
	for i := 0; i < 3; i++ {
		req := toolResultPartsRequest(adapter.ContentPart{Type: "image", MediaType: "image/png", Data: "QUJD"})
		req.Messages[0].Content = "call the tool, attempt " + string(rune('a'+i))
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
			t.Fatalf("HandleChatCompletion #%d: %v", i+1, err)
		}
	}
	for _, prov := range providers {
		if prov != "anthropic" {
			t.Fatalf("upstream providers called = %v, want anthropic only (the openai deployment cannot carry an image tool result)", providers)
		}
	}
	if len(providers) != 3 {
		t.Errorf("upstream calls = %d, want 3", len(providers))
	}
}

// TestHandleChatCompletionToolResultPartsWithNoCapableDeploymentIsTheSentinel:
// a pool of only openai cannot carry an image tool result; the request is
// adapter.ErrToolResultPartsUnsupported before any upstream call.
func TestHandleChatCompletionToolResultPartsWithNoCapableDeploymentIsTheSentinel(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "openai-1", Model: "m", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", toolResultPartsRequest(adapter.ContentPart{Type: "image", MediaType: "image/png", Data: "QUJD"}), "")
	if !errors.Is(err, adapter.ErrToolResultPartsUnsupported) {
		t.Fatalf("err = %v, want errors.Is(adapter.ErrToolResultPartsUnsupported)", err)
	}
	if upstreamCalls != 0 {
		t.Errorf("upstreamCalls = %d, want 0 (decided from the request alone)", upstreamCalls)
	}
}

// TestHandleChatCompletionUnrepresentableResponseNeverCached (item 11 slice
// S6): an anthropic response carrying a block the canonical schema cannot
// represent is delivered but never written to any cache layer -- an
// identical second request reaches upstream again -- while a response of
// known blocks is cached as before.
func TestHandleChatCompletionUnrepresentableResponseNeverCached(t *testing.T) {
	run := func(t *testing.T, content []anthropic.ContentBlock) int {
		t.Helper()
		var upstreamCalls int
		p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
			upstreamCalls++
			return &anthropic.Response{ID: "msg_1", Model: "claude-sonnet-4-6", Role: "assistant", StopReason: "end_turn", Content: content}, nil
		}, []Deployment{{Name: "anthropic-1", Model: "m", Provider: "anthropic", UpstreamModel: "claude-sonnet-4-6", BaseURL: "http://unused"}})
		p.adapters["anthropic"] = anthropic.New()
		req := adapter.ChatRequest{Model: "m", Messages: []adapter.Message{{Role: "user", Content: "search for kelvran"}}}
		for i := 0; i < 2; i++ {
			if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
				t.Fatalf("HandleChatCompletion #%d: %v", i+1, err)
			}
		}
		return upstreamCalls
	}
	if calls := run(t, []anthropic.ContentBlock{{Type: "server_tool_use", ID: "srvtoolu_1", Name: "web_search"}, {Type: "text", Text: "found it"}}); calls != 2 {
		t.Errorf("unrepresentable response: upstreamCalls = %d, want 2 (never cached)", calls)
	}
	if calls := run(t, []anthropic.ContentBlock{{Type: "text", Text: "found it"}}); calls != 1 {
		t.Errorf("representable response: upstreamCalls = %d, want 1 (cached as before)", calls)
	}
}

// TestStreamAccumulatorFoldsUnrepresentable: any chunk flagged by a decoder
// makes the assembled response unrepresentable; none flagged leaves it
// representable.
func TestStreamAccumulatorFoldsUnrepresentable(t *testing.T) {
	acc := newStreamAccumulator()
	acc.add(streaming.ChatCompletionChunk{ID: "s1", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Role: "assistant", Content: "a"}}}})
	acc.add(streaming.ChatCompletionChunk{ID: "s1", Model: "m", Unrepresentable: true, Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Content: "b"}}}})
	if resp := acc.build(adapter.Usage{}); !resp.Unrepresentable {
		t.Error("build().Unrepresentable = false after a flagged chunk, want true")
	}
	plain := newStreamAccumulator()
	plain.add(streaming.ChatCompletionChunk{ID: "s2", Model: "m", Choices: []streaming.ChunkChoice{{Index: 0, Delta: streaming.MessageDelta{Role: "assistant", Content: "a"}}}})
	if resp := plain.build(adapter.Usage{}); resp.Unrepresentable {
		t.Error("build().Unrepresentable = true with no flagged chunk, want false")
	}
}

// anthropicSSE joins Anthropic stream events into the wire the anthropic
// StreamDecoder reads.
func anthropicSSE(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString(e)
		b.WriteString("\n\n")
	}
	return b.String()
}

// TestHandleChatCompletionStreamUnrepresentableResponseNeverCached (item 11
// slice S6, the streaming path): a stream whose content_block_start names a
// block type the decoder does not know is delivered to the client but the
// assembled response is never written to any cache layer -- the identical
// second stream reaches upstream again -- while a known-blocks stream is
// cached and served from L1 the second time.
func TestHandleChatCompletionStreamUnrepresentableResponseNeverCached(t *testing.T) {
	run := func(t *testing.T, firstBlockStart string) int {
		t.Helper()
		wire := anthropicSSE(
			`event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_s6","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":1}}}`,
			firstBlockStart,
			`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
			`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Found it."}}`,
			`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":1}`,
			`event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":9}}`,
			`event: message_stop`+"\n"+`data: {"type":"message_stop"}`,
		)
		var upstreamCalls int
		p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
			upstreamCalls++
			return io.NopCloser(strings.NewReader(wire)), nil
		}, []Deployment{{Name: "anthropic-1", Model: "m", Provider: "anthropic", UpstreamModel: "claude-sonnet-4-6", BaseURL: "http://unused"}},
			adapter.Registry{"anthropic": anthropic.New()})
		req := adapter.ChatRequest{Model: "m", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "search for kelvran"}}}
		for i := 0; i < 2; i++ {
			rec := httptest.NewRecorder()
			if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, rec, ""); err != nil {
				t.Fatalf("HandleChatCompletionStream #%d: %v", i+1, err)
			}
		}
		return upstreamCalls
	}
	unknown := `event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_01","name":"web_search","input":{}}}`
	if calls := run(t, unknown); calls != 2 {
		t.Errorf("unrepresentable stream: upstreamCalls = %d, want 2 (never cached)", calls)
	}
	known := `event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Searching. "}}`
	if calls := run(t, known); calls != 1 {
		t.Errorf("representable stream: upstreamCalls = %d, want 1 (cached as before)", calls)
	}
}

// TestCheckToolResultPartsCarriable pins the first-pick precheck directly:
// the pipeline-level sentinel test above cannot tell the precheck from the
// adapters' own backstop (both are errors.Is the same sentinel before any
// upstream call), and the precheck's distinct value -- deciding before a
// deployment is reserved or called, beside checkResponseFormatEnforceable
// -- is only observable here.
func TestCheckToolResultPartsCarriable(t *testing.T) {
	image := toolResultPartsRequest(adapter.ContentPart{Type: "image", MediaType: "image/png", Data: "QUJD"})
	text := toolResultPartsRequest(adapter.ContentPart{Type: "text", Text: "ok"})
	plain := adapter.ChatRequest{Model: "m", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	for _, tt := range []struct {
		provider string
		req      adapter.ChatRequest
		wantErr  bool
	}{
		{"anthropic", image, false}, {"bedrock", image, false}, {"openai", image, true}, {"openaicompat", image, true}, {"gemini", image, true},
		{"openai", text, false}, {"gemini", text, true}, {"gemini", plain, false},
	} {
		err := checkToolResultPartsCarriable(Deployment{Name: "d", Model: "m", Provider: tt.provider}, tt.req)
		if tt.wantErr && !errors.Is(err, adapter.ErrToolResultPartsUnsupported) {
			t.Errorf("checkToolResultPartsCarriable(%s) = %v, want the sentinel", tt.provider, err)
		}
		if !tt.wantErr && err != nil {
			t.Errorf("checkToolResultPartsCarriable(%s) = %v, want nil", tt.provider, err)
		}
	}
}
