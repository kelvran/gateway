package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/idempotency"
	idempotencyinprocess "github.com/kelvran/gateway/gateway/internal/idempotency/inprocess"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// passthroughWith builds the Passthrough an Anthropic Messages request would
// carry: the raw body and the given unknown members.
func passthroughWith(raw string, unknown map[string]string) *adapter.Passthrough {
	pt := &adapter.Passthrough{Format: "anthropic-messages", RawBody: json.RawMessage(raw)}
	if len(unknown) > 0 {
		pt.UnknownFields = map[string]json.RawMessage{}
		for k, v := range unknown {
			pt.UnknownFields[k] = json.RawMessage(v)
		}
	}
	return pt
}

// TestHandleChatCompletionNeverServesAcrossDifferentPassthroughFields: two
// requests whose canonical shadows match but whose unknown members differ
// are different requests to the model (the passthrough hop forwards the raw
// body), so neither L1/L2 nor singleflight may serve one the other's answer;
// the repeated request is still a hit.
func TestHandleChatCompletionNeverServesAcrossDifferentPassthroughFields(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", AcceptLossyAnthropicIngress: true}})
	const content = "Explain how binary search works in a sorted array"
	base := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: content}}}
	plain := base
	plain.Passthrough = passthroughWith(`{"x":1}`, nil)
	withUnknown := base
	withUnknown.Passthrough = passthroughWith(`{"x":1,"service_tier":"auto"}`, map[string]string{"/service_tier": `"auto"`})
	for i, req := range []adapter.ChatRequest{plain, withUnknown, withUnknown} {
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
			t.Fatalf("HandleChatCompletion #%d: %v", i+1, err)
		}
	}
	if upstreamCalls != 2 {
		t.Errorf("upstreamCalls = %d, want 2 (an unknown member makes a different request; the repeated one is a hit)", upstreamCalls)
	}
}

// TestCheckLexicalCacheNeverServesAcrossDifferentPassthroughFingerprint: the
// L3 near-duplicate gate is exact on the passthrough fingerprint too.
func TestCheckLexicalCacheNeverServesAcrossDifferentPassthroughFingerprint(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})
	ctx := context.Background()
	vk := &identity.VirtualKey{ID: "test-key"}
	fixedSignature := []uint64{1, 2, 3, 4}
	messages := []adapter.Message{{Role: "user", Content: "hi"}}
	written := adapter.ChatRequest{Model: "gpt-4o", Messages: messages, Passthrough: passthroughWith(`{}`, map[string]string{"/service_tier": `"auto"`})}
	writtenResp := []byte(`{"id":"cached-resp"}`)
	if err := p.cacheL3.Put(ctx, vk.ID, fixedSignature, writtenResp, nil, "gpt-4o", p.guardrails.Version(), "", "", nil, reasoningBlocksFingerprint(messages), "", "", "", "", passthroughFingerprint(written), time.Hour); err != nil {
		t.Fatalf("cacheL3.Put: %v", err)
	}
	if _, _, _, hit := p.checkLexicalCache(ctx, vk, vk.ID, adapter.ChatRequest{Model: "gpt-4o", Messages: messages}, "irrelevant-l1-key", fixedSignature, ""); hit {
		t.Error("checkLexicalCache returned a hit for a query without unknown members against an entry written with one -- the gate must reject this")
	}
	cached, _, _, hit := p.checkLexicalCache(ctx, vk, vk.ID, written, "irrelevant-l1-key", fixedSignature, "")
	if !hit || string(cached) != string(writtenResp) {
		t.Fatalf("checkLexicalCache miss (or wrong body %q) for a query whose passthrough fingerprint matches the written entry", cached)
	}
}

// TestPassthroughFingerprintIsEmptyWithoutUnknownFields: a request from the
// OpenAI route (no Passthrough) and an Anthropic request with nothing
// unknown share the empty fingerprint, so cross-format hits still happen.
func TestPassthroughFingerprintIsEmptyWithoutUnknownFields(t *testing.T) {
	if fp := passthroughFingerprint(adapter.ChatRequest{}); fp != "" {
		t.Errorf("no Passthrough: fingerprint %q, want empty", fp)
	}
	if fp := passthroughFingerprint(adapter.ChatRequest{Passthrough: passthroughWith(`{}`, nil)}); fp != "" {
		t.Errorf("Passthrough without unknowns: fingerprint %q, want empty", fp)
	}
	a := passthroughFingerprint(adapter.ChatRequest{Passthrough: passthroughWith(`{}`, map[string]string{"/b": `1`, "/a": `2`})})
	b := passthroughFingerprint(adapter.ChatRequest{Passthrough: passthroughWith(`{}`, map[string]string{"/a": `2`, "/b": `1`})})
	if a == "" || a != b {
		t.Errorf("fingerprint must be non-empty and independent of map iteration order: %q vs %q", a, b)
	}
}

// TestHandleChatCompletionLossyIngressReroutesToAnthropicInAMixedPool: with
// unknown members and no lossy flag, a non-anthropic first pick is
// ineligible and the request moves to the anthropic deployment of the same
// model before any upstream call (RFC-1 §6: lossiness is a routing
// property); no sentinel, no 400.
func TestHandleChatCompletionLossyIngressReroutesToAnthropicInAMixedPool(t *testing.T) {
	var served []string
	p := newMixedProviderTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		served = append(served, dep.Provider)
		if dep.Provider == "anthropic" {
			return fakeAnthropicResponse("claude"), nil
		}
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{
		{Name: "oa", Model: "m", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "an", Model: "m", Provider: "anthropic", UpstreamModel: "claude", BaseURL: "http://unused"},
	})
	req := adapter.ChatRequest{Model: "m", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, Passthrough: passthroughWith(`{"service_tier":"auto"}`, map[string]string{"/service_tier": `"auto"`})}
	for i := 0; i < 4; i++ {
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
			t.Fatalf("HandleChatCompletion #%d: %v", i+1, err)
		}
	}
	if len(served) == 0 {
		t.Fatal("no upstream call")
	}
	for _, provider := range served {
		if provider != "anthropic" {
			t.Errorf("a lossy request reached provider %q; want every call routed to anthropic", provider)
		}
	}
}

// TestHandleChatCompletionLossyIngressRejectedWithoutAnthropic: a pool with
// no anthropic deployment and no lossy flag refuses the request before any
// upstream call with ErrLossyIngressRejected, whose message names the config
// key and the client remedy, names a top-level unknown member, counts nested
// ones without naming them, and never contains the words Claude Code matches
// for recovery; Param carries every pointer.
func TestHandleChatCompletionLossyIngressRejectedWithoutAnthropic(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})
	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, Passthrough: passthroughWith(`{}`, map[string]string{
		"/foo": `1`, "/system/0/foo": `2`, "/thinking/display": `"summarized"`,
	})}
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, "")
	var lossy *ErrLossyIngressRejected
	if !errors.As(err, &lossy) {
		t.Fatalf("err = %v, want *ErrLossyIngressRejected", err)
	}
	if upstreamCalls != 0 {
		t.Errorf("upstreamCalls = %d, want 0: the refusal is decided from the request alone", upstreamCalls)
	}
	msg := lossy.Error()
	for _, want := range []string{"accept_lossy_anthropic_ingress", "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1", "foo", "2 nested"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q: %s", want, msg)
		}
	}
	for _, forbidden := range []string{"system", "thinking", "cache_control", "output_config.effort", "Extra inputs are not permitted", "Input tag", "bound to a different conversation", "capability_rejected:", "d1"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("message must not contain %q (a nested parent, a deployment name or a Claude Code recovery string): %s", forbidden, msg)
		}
	}
	if got := strings.Join(lossy.Pointers, ","); got != "/foo,/system/0/foo,/thinking/display" {
		t.Errorf("Pointers = %q, want all three, sorted", got)
	}
	if lossy.TopLevelFields != 1 || lossy.NestedCount != 2 {
		t.Errorf("TopLevelFields/NestedCount = %d/%d, want 1/2", lossy.TopLevelFields, lossy.NestedCount)
	}
}

// TestHandleChatCompletionLossyIngressAcceptedTranslatesAndRecordsDroppedFields:
// with the flag on, the request is translated with its unknown members
// dropped, and the request log line carries ingress_format, passthrough and
// the sorted dropped pointers.
func TestHandleChatCompletionLossyIngressAcceptedTranslatesAndRecordsDroppedFields(t *testing.T) {
	var logBuf bytes.Buffer
	var upstreamCalls int
	keys := defaultTestVirtualKeys()
	p := newTestPipelineWithKeysAndLogger(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", AcceptLossyAnthropicIngress: true}}, keys, slog.New(slog.NewJSONHandler(&logBuf, nil)))
	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, Passthrough: passthroughWith(`{}`, map[string]string{"/zeta": `1`, "/alpha/0/x": `2`})}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1 (the flag accepts the lossy translate)", upstreamCalls)
	}
	out := logBuf.String()
	for _, want := range []string{`"ingress_format":"anthropic-messages"`, `"passthrough":false`, `"dropped_fields":"/alpha/0/x,/zeta"`} {
		if !strings.Contains(out, want) {
			t.Errorf("request log lacks %s:\n%s", want, out)
		}
	}
}

// TestIdempotencyFingerprintUsesTheRawBodyOnTheIngress: on the Anthropic
// ingress the Idempotency-Key fingerprint is sha256 of the raw body, so two
// bodies that differ only in an unknown member at depth are different
// requests (ErrFingerprintMismatch) even though their shadows match.
func TestIdempotencyFingerprintUsesTheRawBodyOnTheIngress(t *testing.T) {
	keys := defaultTestVirtualKeys()
	p := newTestPipelineWithIdempotency(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", AcceptLossyAnthropicIngress: true}}, keys, idempotencyinprocess.New())
	base := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	first := base
	first.Passthrough = passthroughWith(`{"model":"gpt-4o","messages":[],"metadata":{"session_id":"a"}}`, map[string]string{"/metadata/session_id": `"a"`})
	second := base
	second.Passthrough = passthroughWith(`{"model":"gpt-4o","messages":[],"metadata":{"session_id":"b"}}`, map[string]string{"/metadata/session_id": `"b"`})
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", first, "reused-key"); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", second, "reused-key")
	if !errors.Is(err, idempotency.ErrFingerprintMismatch) {
		t.Fatalf("err = %v, want ErrFingerprintMismatch (the raw bodies differ)", err)
	}
}

// TestDroppedFieldsSummaryIsBoundedOnAPointerBoundary: the telemetry
// summary of dropped pointers is sorted, comma-joined and never longer than
// 512 bytes; a cut never splits a pointer and says how many were omitted.
func TestDroppedFieldsSummaryIsBoundedOnAPointerBoundary(t *testing.T) {
	if got := droppedFieldsSummary([]string{"/zeta", "/alpha"}); got != "/alpha,/zeta" {
		t.Errorf("summary = %q, want sorted, comma-joined", got)
	}
	var many []string
	for i := 0; i < 100; i++ {
		many = append(many, "/messages/"+strings.Repeat("x", 20)+"/"+string(rune('a'+i%26))+"/"+strings.Repeat("y", i%7))
	}
	got := droppedFieldsSummary(many)
	if len(got) > maxDroppedFieldsBytes {
		t.Fatalf("summary is %d bytes, cap %d", len(got), maxDroppedFieldsBytes)
	}
	tokens := strings.Split(got, ",")
	last := tokens[len(tokens)-1]
	if !strings.HasPrefix(last, "…+") {
		t.Fatalf("a cut summary must end with the omitted count, got %q", last)
	}
	set := map[string]bool{}
	for _, m := range many {
		set[m] = true
	}
	for _, tok := range tokens[:len(tokens)-1] {
		if !set[tok] {
			t.Errorf("token %q is not a whole input pointer: the cut split a pointer", tok)
		}
	}
}

// TestPassthroughRequestNeverGetsCacheControlAutoPopulated: on the ingress
// the client owns cache_control (RFC-1 §8), so a passthrough request is not
// auto-populated even when the deployment leaves auto-populate on.
func TestPassthroughRequestNeverGetsCacheControlAutoPopulated(t *testing.T) {
	var seen []byte
	p := newAnthropicTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		seen, _ = json.Marshal(req)
		return fakeAnthropicResponse("claude"), nil
	}, []Deployment{{Name: "an", Model: "claude", Provider: "anthropic", UpstreamModel: "claude", BaseURL: "http://unused"}})
	req := adapter.ChatRequest{Model: "claude", Messages: []adapter.Message{{Role: "system", Content: "You are terse."}, {Role: "user", Content: "hi"}}, Passthrough: passthroughWith(`{}`, nil)}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if bytes.Contains(seen, []byte("cache_control")) {
		t.Errorf("the provider request carries an injected cache_control on a passthrough request:\n%s", seen)
	}
	// Same path, no Passthrough (and a different prompt, so the first call's
	// cached response is not served instead): the deployment's own
	// auto-populate still marks the system block, so the assertion above is
	// not vacuous.
	req.Passthrough = nil
	req.Messages[1].Content = "hi again"
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion (control): %v", err)
	}
	if !bytes.Contains(seen, []byte("cache_control")) {
		t.Fatalf("control: the same request without Passthrough did not get the auto-populated cache_control; the test cannot tell the OR apart from an unmarked model:\n%s", seen)
	}
}

// TestPassthroughStreamRequestNeverGetsCacheControlAutoPopulated is the
// streaming twin (streamDeploymentBedrock's OR): a Bedrock stream built
// from a passthrough request carries no cachePoint, while the same request
// without Passthrough does.
func TestPassthroughStreamRequestNeverGetsCacheControlAutoPopulated(t *testing.T) {
	var seen []byte
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		seen, _ = json.Marshal(req)
		return nil, errors.New("stop after building the provider request")
	}, []Deployment{{Name: "d1", Model: "claude-bedrock", Provider: "bedrock", UpstreamModel: "anthropic.claude-3-5-sonnet-20241022-v2:0", BaseURL: "http://unused"}},
		adapter.Registry{"bedrock": bedrock.New()})
	req := adapter.ChatRequest{Model: "claude-bedrock", Stream: true, Messages: []adapter.Message{{Role: "system", Content: "You are terse."}, {Role: "user", Content: "hi"}}, Passthrough: passthroughWith(`{}`, nil)}
	_ = p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, httptest.NewRecorder(), "")
	if len(seen) == 0 {
		t.Fatal("the upstream fake never saw a provider request")
	}
	if bytes.Contains(seen, []byte("cachePoint")) {
		t.Errorf("the Bedrock stream request carries an injected cachePoint on a passthrough request:\n%s", seen)
	}
	req.Passthrough = nil
	_ = p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, httptest.NewRecorder(), "")
	if !bytes.Contains(seen, []byte("cachePoint")) {
		t.Fatalf("control: the same stream request without Passthrough did not get the auto-populated cachePoint:\n%s", seen)
	}
}

// TestHandleChatCompletionStreamLossyIngressRejectedWithoutAnthropic: the
// streaming first pick refuses a lossy request the same way the buffered one
// does, before any upstream call.
func TestHandleChatCompletionStreamLossyIngressRejectedWithoutAnthropic(t *testing.T) {
	var upstreamCalls int
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		upstreamCalls++
		return nil, errors.New("must not be reached")
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}},
		adapter.Registry{"openai": openai.New()})
	req := adapter.ChatRequest{Model: "gpt-4o", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}}, Passthrough: passthroughWith(`{}`, map[string]string{"/foo": `1`})}
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, httptest.NewRecorder(), "")
	var lossy *ErrLossyIngressRejected
	if !errors.As(err, &lossy) {
		t.Fatalf("err = %v, want *ErrLossyIngressRejected", err)
	}
	if upstreamCalls != 0 {
		t.Errorf("upstreamCalls = %d, want 0", upstreamCalls)
	}
}

// TestHandleChatCompletionLossyIngressSpanCarriesIngressAttributes: the span
// built by finalize carries the ingress carrier -- format, passthrough=false
// (every hop translates until S11), dropped_fields on an accepted translate
// -- and a rejected request's span carries the format but no dropped_fields
// (nothing was dropped; the error names the pointers).
func TestHandleChatCompletionLossyIngressSpanCarriesIngressAttributes(t *testing.T) {
	before := len(spanRecorder.Ended())
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", AcceptLossyAnthropicIngress: true}})
	accepted := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, Passthrough: passthroughWith(`{}`, map[string]string{"/zeta": `1`, "/alpha/0/x": `2`})}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", accepted, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	strict := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})
	if _, err := strict.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", accepted, ""); err == nil {
		t.Fatal("the strict pipeline accepted a lossy request")
	}
	spans := spansSince(before)
	if len(spans) != 2 {
		t.Fatalf("len(spans) = %d, want 2", len(spans))
	}
	acceptedAttrs, rejectedAttrs := spans[0].Attributes(), spans[1].Attributes()
	if v, ok := spanAttr(t, acceptedAttrs, telemetry.AttrKelvranIngressFormat); !ok || v.AsString() != "anthropic-messages" {
		t.Errorf("accepted %s = %v, ok=%v", telemetry.AttrKelvranIngressFormat, v, ok)
	}
	if v, ok := spanAttr(t, acceptedAttrs, telemetry.AttrKelvranIngressPassthrough); !ok || v.AsBool() {
		t.Errorf("accepted %s = %v, ok=%v, want false", telemetry.AttrKelvranIngressPassthrough, v, ok)
	}
	if v, ok := spanAttr(t, acceptedAttrs, telemetry.AttrKelvranIngressDroppedFields); !ok || v.AsString() != "/alpha/0/x,/zeta" {
		t.Errorf("accepted %s = %v, ok=%v", telemetry.AttrKelvranIngressDroppedFields, v, ok)
	}
	if v, ok := spanAttr(t, rejectedAttrs, telemetry.AttrKelvranIngressFormat); !ok || v.AsString() != "anthropic-messages" {
		t.Errorf("rejected %s = %v, ok=%v", telemetry.AttrKelvranIngressFormat, v, ok)
	}
	if _, ok := spanAttr(t, rejectedAttrs, telemetry.AttrKelvranIngressDroppedFields); ok {
		t.Errorf("rejected request's span carries %s; nothing was dropped", telemetry.AttrKelvranIngressDroppedFields)
	}
}

// TestErrLossyIngressRejectedNeverNamesARecoveryString: a body can call a
// top-level member anything, so the message names a member only when its
// name is a plain identifier free of the strings Claude Code matches on for
// recovery (gate-decisions Q5); every other top-level member is counted and
// left to param. The security review of this slice found the first version
// quoted names verbatim, so the earlier never-contains test, which used
// only the name foo, was vacuous for the client-chosen case.
func TestErrLossyIngressRejectedNeverNamesARecoveryString(t *testing.T) {
	unknown := map[string]json.RawMessage{}
	for _, name := range []string{"cache_control", "system_prompt", "thinking_budget", "Extra inputs are not permitted", "output_config.effort", "Input tag", "CACHE_CONTROL_v2", "Thinking.Mode", "service_tier", "x" + strings.Repeat("y", 70)} {
		unknown["/"+name] = json.RawMessage(`1`)
	}
	err := checkLossyIngressEligible(Deployment{Provider: "openai"}, adapter.ChatRequest{Passthrough: &adapter.Passthrough{UnknownFields: unknown}})
	var lossy *ErrLossyIngressRejected
	if !errors.As(err, &lossy) {
		t.Fatalf("err = %v, want *ErrLossyIngressRejected", err)
	}
	msg := lossy.Error()
	lower := strings.ToLower(msg)
	for _, forbidden := range []string{"thinking", "cache_control", "system", "output_config.effort", "extra inputs are not permitted", "input tag", "bound to a different conversation", "capability_rejected:"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("message contains %q through a client-chosen member name: %s", forbidden, msg)
		}
	}
	for _, want := range []string{"1 member this gateway cannot translate", "(service_tier)", "9 members named only in param"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q: %s", want, msg)
		}
	}
	if lossy.TopLevelFields != 10 || lossy.NestedCount != 0 {
		t.Errorf("TopLevelFields/NestedCount = %d/%d, want 10/0 (the filter changes the sentence, not the counts)", lossy.TopLevelFields, lossy.NestedCount)
	}
	if got := lossy.Param(); !strings.Contains(got, "/cache_control") || !strings.Contains(got, "/service_tier") {
		t.Errorf("param must still carry every pointer: %q", got)
	}
}

// TestErrLossyIngressRejectedBoundsMessageAndParam: the pointers are
// client input echoed back, so the message's name list and Param are cut
// like dropped_fields -- a body with thousands of unknown members cannot
// turn the 400 into a reflection of itself.
func TestErrLossyIngressRejectedBoundsMessageAndParam(t *testing.T) {
	unknown := map[string]json.RawMessage{}
	for i := 0; i < 2000; i++ {
		unknown["/member_"+strings.Repeat("x", 10)+"_"+strconv.Itoa(i)] = json.RawMessage(`1`)
	}
	err := checkLossyIngressEligible(Deployment{Provider: "openai"}, adapter.ChatRequest{Passthrough: &adapter.Passthrough{UnknownFields: unknown}})
	var lossy *ErrLossyIngressRejected
	if !errors.As(err, &lossy) {
		t.Fatalf("err = %v, want *ErrLossyIngressRejected", err)
	}
	if param := lossy.Param(); len(param) > maxDroppedFieldsBytes || !strings.Contains(param, ",…+") {
		t.Errorf("Param is %d bytes (cap %d) or lacks the omitted count: %q", len(param), maxDroppedFieldsBytes, param)
	}
	if msg := lossy.Error(); len(msg) > maxDroppedFieldsBytes+256 || !strings.Contains(msg, "…+") {
		t.Errorf("message is %d bytes or lacks the omitted count: %.200s", len(msg), msg)
	}
	short := &ErrLossyIngressRejected{Pointers: []string{"/a", "/b"}, TopLevelFields: 2}
	if short.Param() != "/a,/b" {
		t.Errorf("a short list must be echoed whole, got %q", short.Param())
	}
}

// TestPassthroughFingerprintDiffersOnUnknownMembers: the fingerprint is a
// function of the unknown members' pointers AND raw values -- two requests
// unknown in a different member, or in the same member with a different
// value, are different requests to the model.
func TestPassthroughFingerprintDiffersOnUnknownMembers(t *testing.T) {
	auto := passthroughFingerprint(adapter.ChatRequest{Passthrough: passthroughWith(`{}`, map[string]string{"/service_tier": `"auto"`})})
	standard := passthroughFingerprint(adapter.ChatRequest{Passthrough: passthroughWith(`{}`, map[string]string{"/service_tier": `"standard"`})})
	other := passthroughFingerprint(adapter.ChatRequest{Passthrough: passthroughWith(`{}`, map[string]string{"/other": `"auto"`})})
	if auto == standard {
		t.Errorf("same member, different value: fingerprints equal (%q)", auto)
	}
	if auto == other {
		t.Errorf("different member, same value: fingerprints equal (%q)", auto)
	}
}

// TestPassthroughFingerprintFoldsToolResultIsError: is_error is json:"-" on
// adapter.Message, so serializeMessages/normalizeMessages never see it; the
// fingerprint carries it, or a failed and a successful tool result would
// share every cache layer.
func TestPassthroughFingerprintFoldsToolResultIsError(t *testing.T) {
	succeeded := adapter.ChatRequest{Messages: []adapter.Message{{Role: "tool", ToolCallID: "t1", Content: "x"}}}
	failed := adapter.ChatRequest{Messages: []adapter.Message{{Role: "tool", ToolCallID: "t1", Content: "x", ToolResultIsError: true}}}
	if fp := passthroughFingerprint(succeeded); fp != "" {
		t.Errorf("a successful tool result without Passthrough: fingerprint %q, want empty", fp)
	}
	if fp := passthroughFingerprint(failed); fp == "" {
		t.Error("a failed tool result must change the fingerprint")
	}
}

// newMixedProviderTestPipeline is newTestPipeline with both the "openai" and
// the "anthropic" adapters registered, for pools that mix the two.
func newMixedProviderTestPipeline(t *testing.T, upstream UpstreamCaller, deployments []Deployment) *Pipeline {
	t.Helper()
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New(), "anthropic": anthropic.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream:       upstream,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestPassthroughFingerprintFoldsAnthropicBetaHeader: anthropic-beta changes
// what the model does (interleaved thinking, 1M context) once the upstream
// sees it, so two requests differing only there never share an entry;
// token order and whitespace are normalised, anthropic-version alone (sent
// on every request) folds nothing so cross-format hits survive.
func TestPassthroughFingerprintFoldsAnthropicBetaHeader(t *testing.T) {
	with := func(betas ...string) adapter.ChatRequest {
		h := http.Header{}
		h.Set("Anthropic-Version", "2023-06-01")
		for _, b := range betas {
			h.Add("Anthropic-Beta", b)
		}
		return adapter.ChatRequest{Passthrough: &adapter.Passthrough{Format: "anthropic-messages", ForwardHeaders: h}}
	}
	if fp := passthroughFingerprint(with()); fp != "" {
		t.Errorf("anthropic-version alone: fingerprint %q, want empty", fp)
	}
	a := passthroughFingerprint(with("interleaved-thinking-2025-05-14, context-1m-2025-08-07"))
	b := passthroughFingerprint(with("context-1m-2025-08-07", "interleaved-thinking-2025-05-14"))
	c := passthroughFingerprint(with("interleaved-thinking-2025-05-14"))
	if a == "" || a != b {
		t.Errorf("normalised beta sets must match: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("different beta sets must differ: %q", a)
	}
}

// TestIdempotencyFingerprintFoldsAnthropicBeta: a reused Idempotency-Key
// with the same body but different betas is a different request.
func TestIdempotencyFingerprintFoldsAnthropicBeta(t *testing.T) {
	mk := func(beta string) adapter.ChatRequest {
		h := http.Header{}
		if beta != "" {
			h.Set("Anthropic-Beta", beta)
		}
		return adapter.ChatRequest{Model: "m", Passthrough: &adapter.Passthrough{Format: "anthropic-messages", RawBody: json.RawMessage(`{"model":"m"}`), ForwardHeaders: h}}
	}
	plain, _ := idempotencyFingerprint(mk(""))
	withBeta, _ := idempotencyFingerprint(mk("interleaved-thinking-2025-05-14"))
	same, _ := idempotencyFingerprint(mk("interleaved-thinking-2025-05-14"))
	if plain == withBeta || withBeta != same {
		t.Errorf("fingerprints: plain=%x beta=%x same=%x", plain[:4], withBeta[:4], same[:4])
	}
}

// TestIsContextWindowExceeded: the exported predicate the Anthropic
// envelope uses for the capability_rejected: prompt_too_long token.
func TestIsContextWindowExceeded(t *testing.T) {
	if !IsContextWindowExceeded(&UpstreamHTTPError{StatusCode: 400, Body: "Input is too long for requested model."}) {
		t.Error("Bedrock's over-long ValidationException wording must classify as a context-window rejection")
	}
	if IsContextWindowExceeded(&UpstreamHTTPError{StatusCode: 500, Body: "boom"}) || IsContextWindowExceeded(errors.New("plain")) {
		t.Error("anything else must not")
	}
}
