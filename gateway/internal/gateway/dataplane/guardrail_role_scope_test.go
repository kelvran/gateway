package dataplane

// Role-scoped guardrail detectors (round-5 Tier S item 25): a category's
// detectors run only over the message roles its scope admits. These are
// the pipeline-level proofs: the pre-call scan honours the role of each
// message (buffered and streaming), the post-call scan runs as the
// assistant role, an embeddings input is scanned as the user role, the
// per-message segments join back to exactly the string the scan used
// before, and a scope change forces a real cache miss without a
// policy_version bump -- the same guarantee category_overrides already
// has.

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

var roleScopeTestDeployments = []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}

func roleScopedPolicy(scopes map[guardrail.Category][]string) guardrail.Policy {
	policy := guardrail.DefaultPolicy()
	policy.RoleScopes = scopes
	return policy
}

// newRoleScopedTestPipeline is newTestPipeline with a guardrail policy of
// the test's choosing; everything else matches the house test pipeline.
func newRoleScopedTestPipeline(t *testing.T, upstream UpstreamCaller, upstreamStream UpstreamStreamCaller, policy guardrail.Policy) *Pipeline {
	t.Helper()
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if upstream == nil {
		upstream = func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("non-streaming Upstream should never be called by this test")
			return nil, nil
		}
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), policy, "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(roleScopeTestDeployments),
		Deployments:    roleScopeTestDeployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream:       upstream,
		UpstreamStream: upstreamStream,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

func TestPreCallGuardrailRoleScopeLeavesUnscopedRolesUnscanned(t *testing.T) {
	var upstreamCalls int
	p := newRoleScopedTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, nil, roleScopedPolicy(map[guardrail.Category][]string{guardrail.CategoryFinancialID: {guardrail.RoleUser, guardrail.RoleTool}}))

	// The card number sits in the SYSTEM message (a harness prompt
	// quoting a test fixture); financial_id is scoped to user and tool,
	// so pre-call must let it through -- today's unscoped engine blocks it.
	inSystem := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{
		{Role: "system", Content: "Test fixtures you may quote: card " + fakeCreditCardNumber},
		{Role: "user", Content: "hello"},
	}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", inSystem, ""); err != nil {
		t.Fatalf("HandleChatCompletion with the card in a system message: %v, want nil -- the system role is outside financial_id's scope", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1", upstreamCalls)
	}

	// The same number typed by the user is inside the scope and blocks.
	inUser := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{
		{Role: "user", Content: "my card is " + fakeCreditCardNumber},
	}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", inUser, ""); !errors.Is(err, ErrGuardrailBlocked) {
		t.Errorf("HandleChatCompletion with the card in a user message: err = %v, want ErrGuardrailBlocked", err)
	}
	if upstreamCalls != 1 {
		t.Errorf("upstreamCalls = %d, want still 1: a blocked request never reaches the upstream", upstreamCalls)
	}
}

func TestPreCallGuardrailRoleScopeStillScansToolResults(t *testing.T) {
	var upstreamCalls int
	p := newRoleScopedTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, nil, roleScopedPolicy(map[guardrail.Category][]string{guardrail.CategoryFinancialID: {guardrail.RoleTool}}))

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{
		{Role: "user", Content: "What did the lookup tool find?"},
		{Role: "assistant", ToolCalls: []adapter.ToolCall{{ID: "call_1", Name: "lookup_customer", ArgumentsJSON: `{"customer_id":"c_123"}`}}},
		{Role: "tool", ToolCallID: "call_1", Content: "customer card on file: " + fakeCreditCardNumber},
	}}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); !errors.Is(err, ErrGuardrailBlocked) {
		t.Errorf("err = %v, want ErrGuardrailBlocked: the tool result is the one role financial_id is scoped to", err)
	}
	if upstreamCalls != 0 {
		t.Errorf("upstreamCalls = %d, want 0", upstreamCalls)
	}
}

func TestStreamPreCallGuardrailRoleScopeHonoursMessageRoles(t *testing.T) {
	var upstreamCalls int
	p := newRoleScopedTestPipeline(t, nil, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		upstreamCalls++
		return nopCloserReader{strings.NewReader(sseStreamWithContent("hello"))}, nil
	}, roleScopedPolicy(map[guardrail.Category][]string{guardrail.CategoryFinancialID: {guardrail.RoleUser}}))

	rec := httptest.NewRecorder()
	inSystem := adapter.ChatRequest{Model: "gpt-4o", Stream: true, Messages: []adapter.Message{
		{Role: "system", Content: "fixture card " + fakeCreditCardNumber},
		{Role: "user", Content: "hi"},
	}}
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", inSystem, rec, ""); err != nil {
		t.Fatalf("streaming request with the card in a system message: %v, want nil", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1", upstreamCalls)
	}

	inUser := adapter.ChatRequest{Model: "gpt-4o", Stream: true, Messages: []adapter.Message{
		{Role: "user", Content: "my card is " + fakeCreditCardNumber},
	}}
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", inUser, httptest.NewRecorder(), ""); !errors.Is(err, ErrGuardrailBlocked) {
		t.Errorf("streaming request with the card in a user message: err = %v, want ErrGuardrailBlocked", err)
	}
	if upstreamCalls != 1 {
		t.Errorf("upstreamCalls = %d, want still 1", upstreamCalls)
	}
}

func TestPostCallGuardrailScansTheResponseAsTheAssistantRole(t *testing.T) {
	upstream := func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponseWithContent("gpt-4o", "sure, the card is "+fakeCreditCardNumber), nil
	}
	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "read me the card on file"}}}

	// financial_id scoped to the user role only: the model's answer is
	// assistant content, outside the scope, so the response is delivered.
	userOnly := newRoleScopedTestPipeline(t, upstream, nil, roleScopedPolicy(map[guardrail.Category][]string{guardrail.CategoryFinancialID: {guardrail.RoleUser}}))
	resp, err := userOnly.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, "")
	if err != nil {
		t.Fatalf("HandleChatCompletion under financial_id: [user]: %v, want nil -- the response is assistant content", err)
	}
	if len(resp.Choices) == 0 || !strings.Contains(resp.Choices[0].Message.Content, fakeCreditCardNumber) {
		t.Fatalf("response = %+v, want the upstream content delivered unchanged", resp)
	}

	// Scoped to assistant: the same response is blocked post-call.
	assistantOnly := newRoleScopedTestPipeline(t, upstream, nil, roleScopedPolicy(map[guardrail.Category][]string{guardrail.CategoryFinancialID: {guardrail.RoleAssistant}}))
	if _, err := assistantOnly.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); !errors.Is(err, ErrGuardrailBlocked) {
		t.Errorf("HandleChatCompletion under financial_id: [assistant]: err = %v, want ErrGuardrailBlocked", err)
	}
}

func newRoleScopedEmbeddingPipeline(t *testing.T, policy guardrail.Policy, calls *int) *Pipeline {
	t.Helper()
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("1000")},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "emb1", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"}}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), policy, "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("chat Upstream should never be called by an embeddings test")
			return nil, nil
		},
		EmbeddingUpstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			*calls++
			return &openai.EmbeddingResponseWire{
				Model: "text-embedding-3-small",
				Data:  []openai.EmbeddingDataWire{{Index: 0, Embedding: []float64{0.1, 0.2, 0.3}}},
				Usage: openai.EmbeddingUsageWire{PromptTokens: 2, TotalTokens: 2},
			}, nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

func TestEmbeddingsGuardrailScansEachInputAsTheUserRole(t *testing.T) {
	req := adapter.EmbeddingRequest{Model: "text-embedding-3-small", Input: []string{"card on file: " + fakeCreditCardNumber}}

	var toolOnlyCalls int
	toolOnly := newRoleScopedEmbeddingPipeline(t, roleScopedPolicy(map[guardrail.Category][]string{guardrail.CategoryFinancialID: {guardrail.RoleTool}}), &toolOnlyCalls)
	if _, err := toolOnly.HandleEmbeddings(context.Background(), "Bearer test-key", "", req); err != nil {
		t.Fatalf("HandleEmbeddings under financial_id: [tool]: %v, want nil -- an embeddings input is user content, outside the scope", err)
	}
	if toolOnlyCalls != 1 {
		t.Errorf("embedding upstream calls = %d, want 1", toolOnlyCalls)
	}

	var userCalls int
	userScoped := newRoleScopedEmbeddingPipeline(t, roleScopedPolicy(map[guardrail.Category][]string{guardrail.CategoryFinancialID: {guardrail.RoleUser}}), &userCalls)
	if _, err := userScoped.HandleEmbeddings(context.Background(), "Bearer test-key", "", req); !errors.Is(err, ErrGuardrailBlocked) {
		t.Errorf("HandleEmbeddings under financial_id: [user]: err = %v, want ErrGuardrailBlocked", err)
	}
	if userCalls != 0 {
		t.Errorf("embedding upstream calls = %d, want 0", userCalls)
	}
}

func TestGuardrailScanSegmentsJoinBackToTheExactPreCallScanString(t *testing.T) {
	messages := []adapter.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "", Parts: []adapter.ContentPart{{Type: "text", Text: "first part"}, {Type: "image_url", Text: "ignored"}, {Type: "text", Text: "second part"}}},
		{Role: "assistant", Content: "", ToolCalls: []adapter.ToolCall{{ID: "call_1", Name: "lookup", ArgumentsJSON: `{"q":"x"}`}}, ReasoningBlocks: []adapter.ReasoningBlock{
			{Sequence: 0, Redacted: true, Data: "ciphertext"},
			{Sequence: 1, Text: "plain reasoning"},
		}},
		{Role: "tool", ToolCallID: "call_1", Content: "tool output"},
		{Role: "developer", Content: "a provider role passed through"},
	}
	segments := guardrailScanSegments(messages)
	if len(segments) != len(messages) {
		t.Fatalf("len(segments) = %d, want one per message (%d)", len(segments), len(messages))
	}
	texts := make([]string, 0, len(segments))
	for i, seg := range segments {
		want := messages[i].Role
		if !guardrail.KnownRole(want) {
			want = guardrail.RoleOther // a client label outside the four never reaches a log field as written
		}
		if seg.Role != want {
			t.Errorf("segments[%d].Role = %q, want %q", i, seg.Role, want)
		}
		texts = append(texts, seg.Text)
	}
	if got, want := strings.Join(texts, "\n"), guardrailScanMessages(messages); got != want {
		t.Errorf("segments joined with \\n = %q, want the exact pre-call scan string %q (byte-identical, empty Content pieces included)", got, want)
	}
	if strings.Contains(strings.Join(texts, "\n"), "ciphertext") || strings.Contains(strings.Join(texts, "\n"), "ignored") {
		t.Errorf("segments carry excluded content: %q", texts)
	}
}

func TestCacheHitsAreForcedMissesAfterRoleScopesChangeWithoutAPolicyVersionBump(t *testing.T) {
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	l1 := inprocess.New(0)
	l2 := inprocess.New(0)
	l3 := inprocess.NewLexicalCache(0)

	var upstreamCalls int
	upstream := func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}
	buildPipeline := func(policy guardrail.Policy) *Pipeline {
		p, err := NewPipeline(Config{
			Verifier:       verifier,
			Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
			Budget:         budget.NewTracker(),
			Cache:          l1,
			CacheL2:        l2,
			CacheL3:        l3,
			Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), policy, "v1", nil), // identical "v1" on both -- never bumped
			Adapters:       adapter.Registry{"openai": openai.New()},
			Router:         testRouter(roleScopeTestDeployments),
			Deployments:    roleScopeTestDeployments,
			CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
			Upstream:       upstream,
			Logger:         discardLogger(),
		})
		if err != nil {
			t.Fatalf("NewPipeline: %v", err)
		}
		return p
	}

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "cache me please, role_scopes test"}}}
	if _, err := buildPipeline(guardrail.DefaultPolicy()).HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("first request (default policy): %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after first request = %d, want 1", upstreamCalls)
	}

	// Scoping prompt_injection to user and tool WIDENS nothing a cached
	// entry was admitted under -- but the policy changed, and the rule is
	// one rule: any Policy change is a new fingerprint and a real miss.
	scoped := roleScopedPolicy(map[guardrail.Category][]string{guardrail.CategoryPromptInjection: {guardrail.RoleUser, guardrail.RoleTool}})
	if _, err := buildPipeline(scoped).HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("second request (role_scopes changed, policy_version unchanged): %v", err)
	}
	if upstreamCalls != 2 {
		t.Errorf("upstreamCalls after the role_scopes change = %d, want 2 -- a role-scope change must force a real cache miss even when the operator never bumped policy_version", upstreamCalls)
	}
}

// TestCountTokensGuardrailRoleScopeHonoursTheSystemField covers the sixth
// call site (the other five: buffered and streaming pre-call, the two
// post-call scans, embeddings): count_tokens parses an Anthropic-shaped body (its top-level
// system field becomes a system-role message) and scans the messages by
// role like the chat routes. The unscoped engine blocks a card number in
// the system field (proving the field is scanned at all); financial_id
// scoped to the user role lets the same body through to the deployment's
// own count_tokens.
func TestCountTokensGuardrailRoleScopeHonoursTheSystemField(t *testing.T) {
	body := []byte(`{"model":"claude-sys","system":"Test fixtures you may quote: card ` + fakeCreditCardNumber + `","messages":[{"role":"user","content":"how many tokens"}]}`)
	build := func(policy guardrail.Policy, calls *int) *Pipeline {
		keys := []identity.VirtualKey{{ID: "ct-key", KeyHash: testHashOf("ct-secret"), RateLimitBurst: 100, RateLimitRefill: 100}}
		verifier, err := identity.NewVerifier(keys)
		if err != nil {
			t.Fatal(err)
		}
		deployments := []Deployment{{Name: "claude", Model: "claude-sys", Provider: "anthropic", UpstreamModel: "claude-upstream-id", BaseURL: "http://unused/v1/messages"}}
		p, err := NewPipeline(Config{
			Verifier:       verifier,
			Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
			Budget:         budget.NewTracker(),
			Cache:          inprocess.New(0),
			CacheL2:        inprocess.New(0),
			CacheL3:        inprocess.NewLexicalCache(0),
			Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), policy, "test", nil),
			Adapters:       adapter.Registry{"anthropic": anthropic.New(), "openai": openai.New()},
			Router:         testRouter(deployments),
			Deployments:    deployments,
			CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
			Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
				t.Fatal("the chat Upstream must not be called by count_tokens")
				return nil, nil
			},
			CountTokensUpstream: func(ctx context.Context, dep Deployment, req *anthropic.PassthroughRequest) (*CountTokensResult, error) {
				*calls++
				return &CountTokensResult{Body: []byte(`{"input_tokens":7}`)}, nil
			},
			Logger: discardLogger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	var unscopedCalls int
	if _, err := build(guardrail.DefaultPolicy(), &unscopedCalls).HandleCountTokens(context.Background(), "Bearer ct-secret", "", "claude-sys", body, nil); !errors.Is(err, ErrGuardrailBlocked) {
		t.Fatalf("unscoped count_tokens with a card in the system field: err = %v, want ErrGuardrailBlocked (the system field must be scanned at all for the scoped case to mean anything)", err)
	}
	if unscopedCalls != 0 {
		t.Errorf("unscoped count_tokens upstream calls = %d, want 0", unscopedCalls)
	}

	var scopedCalls int
	scoped := roleScopedPolicy(map[guardrail.Category][]string{guardrail.CategoryFinancialID: {guardrail.RoleUser}})
	if _, err := build(scoped, &scopedCalls).HandleCountTokens(context.Background(), "Bearer ct-secret", "", "claude-sys", body, nil); err != nil {
		t.Fatalf("count_tokens under financial_id: [user] with the card in the system field: %v, want nil", err)
	}
	if scopedCalls != 1 {
		t.Errorf("scoped count_tokens upstream calls = %d, want 1", scopedCalls)
	}
}

// TestGuardrailScanSegmentsBoundsAnUnknownRoleToOther: Message.Role is a
// caller-chosen string neither ingress bounds, and it ends up in the
// finding_roles log field, so anything outside the four roles is recorded
// as guardrail.RoleOther -- a huge role, an empty one, one with control
// runes, a provider role -- while the four pass through unchanged.
func TestGuardrailScanSegmentsBoundsAnUnknownRoleToOther(t *testing.T) {
	huge := strings.Repeat("A", 1<<20)
	segments := guardrailScanSegments([]adapter.Message{
		{Role: huge, Content: "a"},
		{Role: "", Content: "b"},
		{Role: "user\nforged=line", Content: "c"},
		{Role: "developer", Content: "d"},
		{Role: "tool", Content: "e"},
	})
	want := []string{guardrail.RoleOther, guardrail.RoleOther, guardrail.RoleOther, guardrail.RoleOther, guardrail.RoleTool}
	for i, seg := range segments {
		if seg.Role != want[i] {
			t.Errorf("segments[%d].Role = %q (len %d), want %q", i, seg.Role[:min(len(seg.Role), 24)], len(seg.Role), want[i])
		}
	}
}
