package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	idempotencyinprocess "github.com/kelvran/gateway/gateway/internal/idempotency/inprocess"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/streaming"
)

// completionIDPattern is the gateway-issued id shape ("chatcmpl-" + 32
// lowercase hex). It identifies a gateway-minted id in these tests only;
// nothing in production depends on telling it apart from a provider's id.
var completionIDPattern = regexp.MustCompile(`^chatcmpl-[0-9a-f]{32}$`)

var pinnedCreated = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// pinClock replaces the pipeline clock with one the test controls and
// returns a function that advances it. Every envelope stamp must read
// p.now, never time.Now -- the "created" assertions below are what prove
// that.
func pinClock(p *Pipeline, at time.Time) func(d time.Duration) {
	now := at
	p.now = func() time.Time { return now }
	return func(d time.Duration) { now = now.Add(d) }
}

func fakeBedrockResponse(text string) *bedrock.Response {
	return &bedrock.Response{
		Output:     bedrock.Output{Message: bedrock.Message{Role: "assistant", Content: []bedrock.ContentBlock{{Text: text}}}},
		StopReason: "end_turn",
		Usage:      bedrock.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	}
}

func bedrockEnvelopeDeployments() []Deployment {
	return []Deployment{{Name: "br", Model: "planner", Provider: "bedrock", UpstreamModel: "anthropic.claude-3-5-sonnet-20241022-v2:0", BaseURL: "http://unused", Region: "us-east-1"}}
}

func chatOnce(t *testing.T, p *Pipeline, model, content, idempotencyKey string) adapter.ChatResponse {
	t.Helper()
	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{
		Model: model, Messages: []adapter.Message{{Role: "user", Content: content}},
	}, idempotencyKey)
	if err != nil {
		t.Fatalf("HandleChatCompletion(%q): %v", content, err)
	}
	return resp
}

// envelopeFrame is the envelope slice of one SSE data frame.
type envelopeFrame struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
}

// sseEnvelopeFrames decodes every "data:" frame of an SSE body except the
// [DONE] sentinel.
func sseEnvelopeFrames(t *testing.T, body string) []envelopeFrame {
	t.Helper()
	var frames []envelopeFrame
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var f envelopeFrame
		if err := json.Unmarshal([]byte(payload), &f); err != nil {
			t.Fatalf("frame is not JSON: %v\n%s", err, payload)
		}
		frames = append(frames, f)
	}
	if len(frames) == 0 {
		t.Fatalf("no data frames in body:\n%s", body)
	}
	return frames
}

// assertFramesShareEnvelope checks every frame carries the SAME
// gateway-issued id, the chunk object, the canonical model and one
// created, and returns that id.
func assertFramesShareEnvelope(t *testing.T, frames []envelopeFrame, wantModel string, wantCreated int64) string {
	t.Helper()
	id := frames[0].ID
	if !completionIDPattern.MatchString(id) {
		t.Fatalf("first frame id = %q, want chatcmpl- + 32 hex", id)
	}
	for i, f := range frames {
		if f.ID != id {
			t.Errorf("frame %d id = %q, want the stream's single id %q", i, f.ID, id)
		}
		if f.Object != "chat.completion.chunk" {
			t.Errorf("frame %d object = %q, want chat.completion.chunk", i, f.Object)
		}
		if f.Model != wantModel {
			t.Errorf("frame %d model = %q, want the canonical %q", i, f.Model, wantModel)
		}
		if f.Created != wantCreated {
			t.Errorf("frame %d created = %d, want %d (minted once per stream from p.now)", i, f.Created, wantCreated)
		}
	}
	return id
}

func TestNewCompletionIDShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 256; i++ {
		id := newCompletionID()
		if !completionIDPattern.MatchString(id) {
			t.Fatalf("newCompletionID() = %q, want chatcmpl- + 32 lowercase hex", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q after %d mints", id, i)
		}
		seen[id] = true
	}
}

func TestStampCompletionEnvelopeFillsOnlyEmptyFields(t *testing.T) {
	resp := adapter.ChatResponse{ID: "chatcmpl-upstream", Object: "custom", Created: 5}
	stampCompletionEnvelope(&resp, pinnedCreated)
	if resp.ID != "chatcmpl-upstream" || resp.Object != "custom" || resp.Created != 5 {
		t.Errorf("stamp overwrote populated fields: %+v", resp)
	}
	empty := adapter.ChatResponse{}
	stampCompletionEnvelope(&empty, pinnedCreated)
	if !completionIDPattern.MatchString(empty.ID) || empty.Object != "chat.completion" || empty.Created != pinnedCreated.Unix() {
		t.Errorf("stamp on an empty response = %+v", empty)
	}
}

// TestHandleChatCompletionStampsEnvelopeWhenProviderHasNoID is the F7
// reproduction: Bedrock's Converse response has no id, so until the stamp
// existed clients received "id":"" and no object/created at all.
func TestHandleChatCompletionStampsEnvelopeWhenProviderHasNoID(t *testing.T) {
	p := newProbeTestPipeline(t, func(_ context.Context, _ Deployment, _ any) (any, error) {
		return fakeBedrockResponse("hello"), nil
	}, nil, bedrockEnvelopeDeployments())
	pinClock(p, pinnedCreated)

	resp := chatOnce(t, p, "planner", "hi", "")
	if !completionIDPattern.MatchString(resp.ID) {
		t.Errorf("resp.ID = %q, want a gateway-issued chatcmpl- id (Bedrock has none)", resp.ID)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("resp.Object = %q, want chat.completion", resp.Object)
	}
	if resp.Created != pinnedCreated.Unix() {
		t.Errorf("resp.Created = %d, want %d from the pipeline clock", resp.Created, pinnedCreated.Unix())
	}
	if resp.Model != "planner" {
		t.Errorf("resp.Model = %q, want the canonical planner", resp.Model)
	}
	encoded, _ := json.Marshal(resp)
	for _, want := range []string{`"object":"chat.completion"`, `"created":` + jsonInt(pinnedCreated.Unix()), `"id":"chatcmpl-`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("client JSON missing %s:\n%s", want, encoded)
		}
	}
}

func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestHandleChatCompletionPreservesProviderID: an OpenAI id is kept
// verbatim so clients can still correlate with the provider's own logs;
// object/created are still stamped (the OpenAI adapter does not carry
// them through).
func TestHandleChatCompletionPreservesProviderID(t *testing.T) {
	p := newTestPipeline(t, func(_ context.Context, dep Deployment, _ any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})
	pinClock(p, pinnedCreated)

	resp := chatOnce(t, p, "gpt-4o", "hi", "")
	if resp.ID != "chatcmpl-fake" {
		t.Errorf("resp.ID = %q, want the upstream id chatcmpl-fake preserved", resp.ID)
	}
	if resp.Object != "chat.completion" || resp.Created != pinnedCreated.Unix() {
		t.Errorf("object/created = %q/%d, want chat.completion/%d", resp.Object, resp.Created, pinnedCreated.Unix())
	}
}

func TestCompletionIDsDifferAcrossCompletions(t *testing.T) {
	p := newProbeTestPipeline(t, func(_ context.Context, _ Deployment, _ any) (any, error) {
		return fakeBedrockResponse("hello"), nil
	}, nil, bedrockEnvelopeDeployments())
	pinClock(p, pinnedCreated)

	a := chatOnce(t, p, "planner", "first question", "")
	b := chatOnce(t, p, "planner", "second question", "")
	if a.ID == b.ID {
		t.Fatalf("two distinct completions share id %q", a.ID)
	}
}

// TestCacheReplayKeepsCompletionIDAndCreated: the id names the completion,
// not the HTTP exchange -- a cache hit returns the stored id and the
// ORIGINAL created, even when the clock has moved on.
func TestCacheReplayKeepsCompletionIDAndCreated(t *testing.T) {
	var upstreamCalls int
	p := newProbeTestPipeline(t, func(_ context.Context, _ Deployment, _ any) (any, error) {
		upstreamCalls++
		return fakeBedrockResponse("hello"), nil
	}, nil, bedrockEnvelopeDeployments())
	advance := pinClock(p, pinnedCreated)

	first := chatOnce(t, p, "planner", "same question", "")
	advance(90 * time.Second)
	second := chatOnce(t, p, "planner", "same question", "")
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1 (second call must be a cache hit)", upstreamCalls)
	}
	if second.ID != first.ID {
		t.Errorf("cache replay id = %q, want the stored %q", second.ID, first.ID)
	}
	if second.Created != pinnedCreated.Unix() {
		t.Errorf("cache replay created = %d, want the original %d, not the replay time", second.Created, pinnedCreated.Unix())
	}
	if second.Object != "chat.completion" {
		t.Errorf("cache replay object = %q", second.Object)
	}
}

// TestIdempotencyReplayKeepsCompletionID: an Idempotency-Key replay must
// return the same completion, id included, by contract.
func TestIdempotencyReplayKeepsCompletionID(t *testing.T) {
	keys := []identity.VirtualKey{{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100}}
	var upstreamCalls int
	p := newTestPipelineWithIdempotency(t, func(_ context.Context, dep Deployment, _ any) (any, error) {
		upstreamCalls++
		r := fakeOpenAIResponse(dep.UpstreamModel)
		r.ID = "" // a provider that sends no id, so the gateway mints one
		return r, nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, keys, idempotencyinprocess.New())
	advance := pinClock(p, pinnedCreated)

	first := chatOnce(t, p, "gpt-4o", "idempotent question", "idem-8c-1")
	advance(time.Minute)
	second := chatOnce(t, p, "gpt-4o", "idempotent question", "idem-8c-1")
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1", upstreamCalls)
	}
	if !completionIDPattern.MatchString(first.ID) {
		t.Fatalf("first id = %q, want gateway-issued", first.ID)
	}
	if second.ID != first.ID || second.Created != first.Created {
		t.Errorf("idempotent replay = (%q, %d), want the first response's (%q, %d)", second.ID, second.Created, first.ID, first.Created)
	}
}

// sseStreamWithoutEnvelope is an OpenAI-shaped stream whose chunks carry
// no id and no model -- the shape the gateway must complete itself.
const sseStreamWithoutEnvelope = "" +
	`data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n" +
	`data: {"choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}` + "\n\n" +
	`data: {"choices":[{"index":0,"delta":{"content":"lo!"},"finish_reason":null}]}` + "\n\n" +
	`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}` + "\n\n" +
	"data: [DONE]\n\n"

// TestHandleChatCompletionStreamStampsEveryChunkFromOneEnvelope: one id,
// one created, the chunk object and the canonical model on every frame;
// and the cached response replays the SAME id and created later.
func TestHandleChatCompletionStreamStampsEveryChunkFromOneEnvelope(t *testing.T) {
	var upstreamCalls int
	p := newStreamingTestPipeline(t, func(_ context.Context, _ Deployment, _ any) (io.ReadCloser, error) {
		upstreamCalls++
		return io.NopCloser(strings.NewReader(sseStreamWithoutEnvelope)), nil
	}, nil, adapter.Registry{"openai": openai.New()})
	advance := pinClock(p, pinnedCreated)

	req := adapter.ChatRequest{Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	rec := httptest.NewRecorder()
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, rec, ""); err != nil {
		t.Fatalf("HandleChatCompletionStream: %v", err)
	}
	frames := sseEnvelopeFrames(t, rec.Body.String())
	id := assertFramesShareEnvelope(t, frames, "gpt-4o", pinnedCreated.Unix())

	advance(2 * time.Minute)
	rec2 := httptest.NewRecorder()
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, rec2, ""); err != nil {
		t.Fatalf("second HandleChatCompletionStream: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1 (second stream must be a cache hit)", upstreamCalls)
	}
	replay := sseEnvelopeFrames(t, rec2.Body.String())
	replayID := assertFramesShareEnvelope(t, replay, "gpt-4o", pinnedCreated.Unix())
	if replayID != id {
		t.Errorf("cache-hit fake stream id = %q, want the original stream's %q", replayID, id)
	}
}

// TestHandleChatCompletionStreamBedrockChunksCarryEnvelope drives the real
// binary event-stream path: Bedrock chunks have neither id nor model.
func TestHandleChatCompletionStreamBedrockChunksCarryEnvelope(t *testing.T) {
	wire := encodeBedrockWireFixture(t, []eventstream.Message{
		bedrockWireEvent("messageStart", `{"role":"assistant"}`),
		bedrockWireEvent("contentBlockStart", `{"contentBlockIndex":0,"start":{}}`),
		bedrockWireEvent("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hel"}}`),
		bedrockWireEvent("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"lo!"}}`),
		bedrockWireEvent("contentBlockStop", `{"contentBlockIndex":0}`),
		bedrockWireEvent("messageStop", `{"stopReason":"end_turn"}`),
		bedrockWireEvent("metadata", `{"usage":{"inputTokens":9,"outputTokens":4,"totalTokens":13}}`),
	})
	p := newStreamingTestPipeline(t, func(_ context.Context, _ Deployment, _ any) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(wire)), nil
	}, []Deployment{{Name: "d1", Model: "claude-bedrock", Provider: "bedrock", UpstreamModel: "anthropic.claude-3-5-sonnet-20241022-v2:0", BaseURL: "http://unused"}},
		adapter.Registry{"bedrock": bedrock.New()})
	pinClock(p, pinnedCreated)

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{
		Model: "claude-bedrock", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, rec, "")
	if err != nil {
		t.Fatalf("HandleChatCompletionStream: %v", err)
	}
	assertFramesShareEnvelope(t, sseEnvelopeFrames(t, rec.Body.String()), "claude-bedrock", pinnedCreated.Unix())
}

// TestWriteFakeStreamMintsOnceForPreUpgradeCacheEntry: a cache entry
// written before this change has no id/created; its replay mints ONE id
// for the whole fake stream rather than leaving "id":"" on every frame.
func TestWriteFakeStreamMintsOnceForPreUpgradeCacheEntry(t *testing.T) {
	resp := adapter.ChatResponse{
		Model:   "gpt-4o",
		Choices: []adapter.Choice{{Index: 0, Message: adapter.Message{Role: "assistant", Content: "Hello!"}, FinishReason: "stop"}},
		Usage:   adapter.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
	}
	rec := httptest.NewRecorder()
	sw, err := streaming.NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := writeFakeStream(sw, resp, pinnedCreated); err != nil {
		t.Fatalf("writeFakeStream: %v", err)
	}
	frames := sseEnvelopeFrames(t, rec.Body.String())
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2 (content + usage)", len(frames))
	}
	assertFramesShareEnvelope(t, frames, "gpt-4o", pinnedCreated.Unix())
}
