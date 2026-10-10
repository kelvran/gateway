package dataplane

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

// Plan item 18 (docs/rfcs/2026-10-08-gateway-cache-key-tools-fingerprint.md):
// tools and tool_choice are part of a request's identity at every cache
// layer. These tests were written first; before the fold, the two
// "never serves across" tests below saw ONE upstream call where they
// require two.

func weatherTool() adapter.ToolDef {
	return adapter.ToolDef{Name: "get_weather", Description: "Current weather", ParametersJSON: `{"type":"object","properties":{"city":{"type":"string"}}}`}
}

func TestToolsFingerprintEmptyWithoutToolsOrToolChoice(t *testing.T) {
	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if got := toolsFingerprint(req); got != "" {
		t.Errorf("toolsFingerprint(no tools, no tool_choice) = %q, want \"\" so tool-free requests keep their keys", got)
	}
}

func TestToolsFingerprintDiffersOnToolChoiceAndTools(t *testing.T) {
	base := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, Tools: []adapter.ToolDef{weatherTool()}}
	required := base
	required.ToolChoice = &adapter.ToolChoice{Mode: "required"}
	none := base
	none.ToolChoice = &adapter.ToolChoice{Mode: "none"}
	otherTool := base
	otherTool.Tools = []adapter.ToolDef{{Name: "get_time", ParametersJSON: `{"type":"object"}`}}
	sameAgain := base
	sameAgain.Tools = []adapter.ToolDef{weatherTool()}

	fps := map[string]string{
		"tools only":            toolsFingerprint(base),
		"tools + required":      toolsFingerprint(required),
		"tools + none":          toolsFingerprint(none),
		"other tool":            toolsFingerprint(otherTool),
		"tool_choice, no tools": toolsFingerprint(adapter.ChatRequest{ToolChoice: &adapter.ToolChoice{Mode: "auto"}}),
	}
	seen := map[string]string{}
	for name, fp := range fps {
		if fp == "" {
			t.Errorf("%s: fingerprint is empty", name)
		}
		if other, dup := seen[fp]; dup {
			t.Errorf("%s and %s share fingerprint %q", name, other, fp)
		}
		seen[fp] = name
	}
	if toolsFingerprint(base) != toolsFingerprint(sameAgain) {
		t.Error("equal tool definitions produced different fingerprints")
	}
}

// chatWithTools issues one buffered request with the given tools and
// tool_choice over the shared pipeline.
func chatWithTools(t *testing.T, p *Pipeline, content string, tools []adapter.ToolDef, choice *adapter.ToolChoice) adapter.ChatResponse {
	t.Helper()
	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: content}}, Tools: tools, ToolChoice: choice,
	}, "")
	if err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	return resp
}

// TestHandleChatCompletionNeverServesAcrossDifferentToolChoice: identical
// messages and tools, tool_choice "required" then "none" -- two upstream
// calls; an exact repeat of the first is then a genuine hit.
func TestHandleChatCompletionNeverServesAcrossDifferentToolChoice(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(_ context.Context, dep Deployment, _ any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})
	tools := []adapter.ToolDef{weatherTool()}

	chatWithTools(t, p, "what is the weather", tools, &adapter.ToolChoice{Mode: "required"})
	chatWithTools(t, p, "what is the weather", tools, &adapter.ToolChoice{Mode: "none"})
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2: tool_choice required vs none must never share a cached response", upstreamCalls)
	}
	chatWithTools(t, p, "what is the weather", tools, &adapter.ToolChoice{Mode: "required"})
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want still 2: an exact repeat (same tools, same tool_choice) is a cache hit", upstreamCalls)
	}
}

// TestHandleChatCompletionNeverServesAcrossToolsPresence: the same message
// with and without tool definitions are different requests.
func TestHandleChatCompletionNeverServesAcrossToolsPresence(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(_ context.Context, dep Deployment, _ any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	chatWithTools(t, p, "what is the weather", nil, nil)
	chatWithTools(t, p, "what is the weather", []adapter.ToolDef{weatherTool()}, nil)
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2: a request offering tools must not be served the tool-free answer", upstreamCalls)
	}
	chatWithTools(t, p, "what is the weather", nil, nil)
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want still 2: the tool-free repeat is a cache hit", upstreamCalls)
	}
}

// TestHandleChatCompletionStreamNeverServesAcrossDifferentToolChoice: the
// streaming path computes the same keys and must behave the same way.
func TestHandleChatCompletionStreamNeverServesAcrossDifferentToolChoice(t *testing.T) {
	var upstreamCalls int
	p := newStreamingTestPipeline(t, func(_ context.Context, _ Deployment, _ any) (io.ReadCloser, error) {
		upstreamCalls++
		return io.NopCloser(strings.NewReader(realOpenAISSEStream)), nil
	}, nil, adapter.Registry{"openai": openai.New()})
	tools := []adapter.ToolDef{weatherTool()}
	stream := func(choice *adapter.ToolChoice) {
		t.Helper()
		rec := httptest.NewRecorder()
		err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{
			Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "weather please"}}, Tools: tools, ToolChoice: choice,
		}, rec, "")
		if err != nil {
			t.Fatalf("HandleChatCompletionStream: %v", err)
		}
	}
	stream(&adapter.ToolChoice{Mode: "required"})
	stream(&adapter.ToolChoice{Mode: "none"})
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2 on the streaming path", upstreamCalls)
	}
	stream(&adapter.ToolChoice{Mode: "none"})
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want still 2: the exact streamed repeat is a fake-streamed cache hit", upstreamCalls)
	}
}

// TestCheckLexicalCacheNeverServesAcrossDifferentTools mirrors the
// ThinkingBindingMode gate test: an L3 entry written for one tool surface
// is not served to a near-duplicate carrying another, and is served to an
// exact match.
func TestCheckLexicalCacheNeverServesAcrossDifferentTools(t *testing.T) {
	p := newTestPipeline(t, func(_ context.Context, _ Deployment, _ any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	ctx := context.Background()
	vk := &identity.VirtualKey{ID: "test-key"}
	fixedSignature := []uint64{1, 2, 3, 4}
	messages := []adapter.Message{{Role: "user", Content: "hi"}}
	written := adapter.ChatRequest{Model: "gpt-4o", Messages: messages, Tools: []adapter.ToolDef{weatherTool()}, ToolChoice: &adapter.ToolChoice{Mode: "required"}}
	writtenResp := []byte(`{"id":"cached-resp"}`)
	if err := p.cacheL3.Put(ctx, vk.ID, fixedSignature, writtenResp, nil, "gpt-4o", p.guardrails.Version(), "", "", nil, reasoningBlocksFingerprint(messages), "", toolsFingerprint(written), "", time.Hour); err != nil {
		t.Fatalf("cacheL3.Put: %v", err)
	}

	for name, q := range map[string]adapter.ChatRequest{
		"no tools":         {Model: "gpt-4o", Messages: messages},
		"tool_choice none": {Model: "gpt-4o", Messages: messages, Tools: []adapter.ToolDef{weatherTool()}, ToolChoice: &adapter.ToolChoice{Mode: "none"}},
		"different tool":   {Model: "gpt-4o", Messages: messages, Tools: []adapter.ToolDef{{Name: "get_time", ParametersJSON: `{"type":"object"}`}}, ToolChoice: &adapter.ToolChoice{Mode: "required"}},
	} {
		if _, _, _, hit := p.checkLexicalCache(ctx, vk, vk.ID, q, "irrelevant-l1-key", fixedSignature, ""); hit {
			t.Errorf("%s: checkLexicalCache returned a hit for a query whose tools/tool_choice differ from the written entry's -- the hard gate must reject this", name)
		}
	}

	cached, _, _, hit := p.checkLexicalCache(ctx, vk, vk.ID, written, "irrelevant-l1-key", fixedSignature, "")
	if !hit {
		t.Fatal("checkLexicalCache returned a miss for a query whose tools and tool_choice exactly match the written entry -- the gate must not reject a genuine match")
	}
	if string(cached) != string(writtenResp) {
		t.Errorf("cached = %q, want %q", cached, writtenResp)
	}
}
