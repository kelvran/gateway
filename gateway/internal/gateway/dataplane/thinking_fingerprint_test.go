package dataplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/gateway/internal/idempotency"
	idempotencyinprocess "github.com/kelvran/gateway/gateway/internal/idempotency/inprocess"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

// TestThinkingFingerprint pins the cache-key / L3-gate input item 11
// slice S4 derives from ChatRequest.Thinking: "" when the field is nil
// (so every request built before the field existed, and every
// /v1/chat/completions request, keeps folding the empty string exactly
// as every other empty-string input does), otherwise the canonical JSON
// of the configuration -- distinct for every distinct type/budget.
func TestThinkingFingerprint(t *testing.T) {
	if got := thinkingFingerprint(adapter.ChatRequest{}); got != "" {
		t.Errorf("thinkingFingerprint(nil Thinking) = %q, want \"\"", got)
	}
	adaptive := thinkingFingerprint(adapter.ChatRequest{Thinking: &adapter.ThinkingConfig{Type: "adaptive"}})
	if adaptive != `{"type":"adaptive"}` {
		t.Errorf("thinkingFingerprint(adaptive) = %q, want {\"type\":\"adaptive\"}", adaptive)
	}
	enabled := thinkingFingerprint(adapter.ChatRequest{Thinking: &adapter.ThinkingConfig{Type: "enabled", BudgetTokens: 1024}})
	if enabled != `{"type":"enabled","budget_tokens":1024}` {
		t.Errorf("thinkingFingerprint(enabled 1024) = %q", enabled)
	}
	if other := thinkingFingerprint(adapter.ChatRequest{Thinking: &adapter.ThinkingConfig{Type: "enabled", BudgetTokens: 4096}}); other == enabled {
		t.Errorf("thinkingFingerprint did not change with the budget: %q", other)
	}
}

// TestHandleChatCompletionNeverServesAcrossDifferentThinking is the
// full-pipeline proof (L1 exact match included) for slice S4's fold:
// two byte-identical requests differing ONLY in Thinking must never
// collide on a cache entry at any layer -- a reply produced without
// extended thinking must not be replayed to a caller who asked for it,
// or the reverse. Mirrors
// TestHandleChatCompletionNeverServesAcrossDifferentThinkingBindingMode.
func TestHandleChatCompletionNeverServesAcrossDifferentThinking(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	const content = "Explain how binary search works in a sorted array"
	plain := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: content}}}
	thinking := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: content}}, Thinking: &adapter.ThinkingConfig{Type: "adaptive"}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", plain, ""); err != nil {
		t.Fatalf("first HandleChatCompletion (no thinking): %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("after first request: upstreamCalls = %d, want 1", upstreamCalls)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", thinking, ""); err != nil {
		t.Fatalf("second HandleChatCompletion (adaptive thinking): %v", err)
	}
	if upstreamCalls != 2 {
		t.Errorf("after a byte-identical-messages request that ONLY differs in Thinking: upstreamCalls = %d, want 2 (no cache layer may serve across thinking configurations)", upstreamCalls)
	}
	// Sanity: the same thinking request again IS a hit -- the fold
	// separates configurations, it does not disable the cache.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", thinking, ""); err != nil {
		t.Fatalf("third HandleChatCompletion (adaptive thinking again): %v", err)
	}
	if upstreamCalls != 2 {
		t.Errorf("after repeating the thinking request: upstreamCalls = %d, want 2 (a genuine same-configuration hit)", upstreamCalls)
	}
}

// TestCheckLexicalCacheNeverServesAcrossDifferentThinkingFingerprint is
// the L3 gate half, with a hand-fixed signature so the thinking gate is
// the ONLY variable (the pattern of this package's other L3 gate tests):
// an entry written under an adaptive-thinking request must not be served
// to a query without thinking, and must still be served to a query with
// the same configuration.
func TestCheckLexicalCacheNeverServesAcrossDifferentThinkingFingerprint(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	ctx := context.Background()
	vk := &identity.VirtualKey{ID: "test-key"}
	fixedSignature := []uint64{1, 2, 3, 4}

	writtenMessages := []adapter.Message{{Role: "user", Content: "hi"}}
	written := adapter.ChatRequest{Model: "gpt-4o", Messages: writtenMessages, Thinking: &adapter.ThinkingConfig{Type: "adaptive"}}
	writtenResp := []byte(`{"id":"cached-resp"}`)
	if err := p.cacheL3.Put(ctx, vk.ID, fixedSignature, writtenResp, nil, "gpt-4o", p.guardrails.Version(), "", "", nil, reasoningBlocksFingerprint(writtenMessages), "", "", thinkingFingerprint(written), time.Hour); err != nil {
		t.Fatalf("cacheL3.Put: %v", err)
	}

	noThinking := adapter.ChatRequest{Model: "gpt-4o", Messages: writtenMessages}
	if _, _, _, hit := p.checkLexicalCache(ctx, vk, vk.ID, noThinking, "irrelevant-l1-key", fixedSignature, ""); hit {
		t.Error("checkLexicalCache returned a hit for a query without thinking against an entry written with adaptive thinking -- the gate must reject this")
	}
	enabled := adapter.ChatRequest{Model: "gpt-4o", Messages: writtenMessages, Thinking: &adapter.ThinkingConfig{Type: "enabled", BudgetTokens: 1024}}
	if _, _, _, hit := p.checkLexicalCache(ctx, vk, vk.ID, enabled, "irrelevant-l1-key", fixedSignature, ""); hit {
		t.Error("checkLexicalCache returned a hit for an enabled-thinking query against an adaptive-thinking entry -- the gate must reject this")
	}

	cached, _, _, hit := p.checkLexicalCache(ctx, vk, vk.ID, written, "irrelevant-l1-key", fixedSignature, "")
	if !hit {
		t.Fatal("checkLexicalCache returned a miss for a query whose Thinking matches the written entry -- the gate must not reject a genuine match")
	}
	if string(cached) != string(writtenResp) {
		t.Errorf("cached = %q, want %q", cached, writtenResp)
	}
}

// TestCallDeploymentLogsThinkingDroppedOnRejectingBedrockFamily: when
// the Bedrock adapter drops a thinking type the served model's family
// rejects (slice S4's capability table), the dataplane records the drop
// on a thinking_dropped log line naming the deployment, the model, the
// type and the reason -- the request itself still succeeds. Until slice
// S9b adds the telemetry carrier this line is the only record, so its
// fields are pinned. The accepting-family control proves the line is
// not emitted when nothing was dropped.
func TestCallDeploymentLogsThinkingDroppedOnRejectingBedrockFamily(t *testing.T) {
	run := func(t *testing.T, upstreamModel string) string {
		t.Helper()
		var logBuf bytes.Buffer
		p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
			return fakeBedrockResponse("ok"), nil
		}, []Deployment{{Name: "bedrock-1", Model: "claude", Provider: "bedrock", UpstreamModel: upstreamModel, BaseURL: "http://unused"}})
		p.logger = slog.New(slog.NewJSONHandler(&logBuf, nil))
		p.adapters["bedrock"] = bedrock.New()

		req := adapter.ChatRequest{
			Model:    "claude",
			Messages: []adapter.Message{{Role: "user", Content: "hi"}},
			Thinking: &adapter.ThinkingConfig{Type: "enabled", BudgetTokens: 1024},
		}
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
			t.Fatalf("HandleChatCompletion(%s): %v (a dropped thinking type must not fail the request)", upstreamModel, err)
		}
		return logBuf.String()
	}

	dropped := run(t, "global.anthropic.claude-fable-5-1")
	line := ""
	for _, l := range strings.Split(dropped, "\n") {
		if strings.Contains(l, `"msg":"thinking_dropped"`) {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no thinking_dropped log line for enabled thinking on a Claude 5.x Bedrock model; logs:\n%s", dropped)
	}
	for _, want := range []string{`"deployment":"bedrock-1"`, `"model":"global.anthropic.claude-fable-5-1"`, `"thinking_type":"enabled"`, `"reason":"`} {
		if !strings.Contains(line, want) {
			t.Errorf("thinking_dropped line lacks %s: %s", want, line)
		}
	}

	if kept := run(t, "global.anthropic.claude-sonnet-4-6"); strings.Contains(kept, "thinking_dropped") {
		t.Errorf("thinking_dropped logged for a family that accepts enabled thinking:\n%s", kept)
	}
}

// TestCallDeploymentLogsThinkingDroppedOnProviderWithoutThinking: a
// provider whose adapter has no thinking configuration at all (openai,
// openaicompat, gemini) ignores the canonical object as a no-op, the
// established "provider ignores, never errors" convention -- but the
// loss is recorded on the same thinking_dropped line, with a reason
// naming the provider, so it is never silent.
func TestCallDeploymentLogsThinkingDroppedOnProviderWithoutThinking(t *testing.T) {
	var logBuf bytes.Buffer
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "openai-1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})
	p.logger = slog.New(slog.NewJSONHandler(&logBuf, nil))

	req := adapter.ChatRequest{
		Model:    "gpt-4o",
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
		Thinking: &adapter.ThinkingConfig{Type: "adaptive"},
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, `"msg":"thinking_dropped"`) || !strings.Contains(logs, `"deployment":"openai-1"`) || !strings.Contains(logs, `"thinking_type":"adaptive"`) || !strings.Contains(logs, "openai") {
		t.Errorf("want a thinking_dropped line naming openai-1 / adaptive / the provider; logs:\n%s", logs)
	}
}

// TestIdempotencyFingerprintFoldsThinkingWithoutChangingTheNilCase pins the
// S4 security-review repair: the Idempotency-Key fingerprint was
// sha256(json.Marshal(req)), and json:"-" silently left Thinking out of it
// -- the first caller-controlled field ever excluded -- so once the ingress
// sets the field, two bodies differing only in thinking would replay each
// other's response. The fold must separate them, and a request WITHOUT a
// thinking configuration must fingerprint byte-for-byte as before, so a
// key claimed by the previous build and reused across the upgrade inside
// its 10-minute window does not become a spurious 422.
func TestIdempotencyFingerprintFoldsThinkingWithoutChangingTheNilCase(t *testing.T) {
	plain := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	body, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := idempotencyFingerprint(plain)
	if err != nil {
		t.Fatalf("idempotencyFingerprint: %v", err)
	}
	if want := sha256.Sum256(body); got != want {
		t.Errorf("fingerprint of a request without thinking changed: got %x, want sha256(json.Marshal(req)) %x", got, want)
	}

	adaptive := plain
	adaptive.Thinking = &adapter.ThinkingConfig{Type: "adaptive"}
	withThinking, err := idempotencyFingerprint(adaptive)
	if err != nil {
		t.Fatalf("idempotencyFingerprint(adaptive): %v", err)
	}
	if withThinking == got {
		t.Error("fingerprint did not change when a thinking configuration was added")
	}
	enabled := plain
	enabled.Thinking = &adapter.ThinkingConfig{Type: "enabled", BudgetTokens: 1024}
	withEnabled, err := idempotencyFingerprint(enabled)
	if err != nil {
		t.Fatalf("idempotencyFingerprint(enabled): %v", err)
	}
	if withEnabled == withThinking {
		t.Error("fingerprint did not change between two different thinking configurations")
	}
}

// TestHandleChatCompletionIdempotencyKeyReusedWithDifferentThinkingIsAMismatch
// is the pipeline-level proof of the same repair, mirroring
// TestHandleChatCompletionIdempotencyKeyMismatchedBodyReturnsFingerprintMismatchError
// with the ONLY difference between the two bodies being Thinking.
func TestHandleChatCompletionIdempotencyKeyReusedWithDifferentThinkingIsAMismatch(t *testing.T) {
	p := newTestPipelineWithIdempotency(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, defaultTestVirtualKeys(), idempotencyinprocess.New())

	plain := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	thinking := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, Thinking: &adapter.ThinkingConfig{Type: "adaptive"}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", plain, "reused-key"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", thinking, "reused-key")
	if !errors.Is(err, idempotency.ErrFingerprintMismatch) {
		t.Fatalf("err = %v, want ErrFingerprintMismatch for a reused key whose body differs only in thinking", err)
	}
}

// TestNoteThinkingDroppedBoundsThinkingType: the type is a caller-controlled
// string once the ingress exists, so the log field is bounded the same way
// the model name is (256 bytes) -- a megabyte type must not land in logs.
func TestNoteThinkingDroppedBoundsThinkingType(t *testing.T) {
	var logBuf bytes.Buffer
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "openai-1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})
	p.logger = slog.New(slog.NewJSONHandler(&logBuf, nil))

	huge := strings.Repeat("x", 4096)
	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, Thinking: &adapter.ThinkingConfig{Type: huge}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	var line map[string]any
	for _, l := range strings.Split(logBuf.String(), "\n") {
		if strings.Contains(l, `"msg":"thinking_dropped"`) {
			if err := json.Unmarshal([]byte(l), &line); err != nil {
				t.Fatalf("thinking_dropped line is not JSON: %v", err)
			}
		}
	}
	if line == nil {
		t.Fatalf("no thinking_dropped line; logs:\n%s", logBuf.String())
	}
	got, _ := line["thinking_type"].(string)
	if len(got) != maxModelForTelemetry || !strings.HasPrefix(huge, got) {
		t.Errorf("thinking_type logged with %d bytes, want the first %d bytes of the type", len(got), maxModelForTelemetry)
	}
}
