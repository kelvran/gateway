package dataplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// --- isSourceIPAllowed / resolveClientIP unit tests ---

func mustParseCIDR(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("net.ParseCIDR(%q): %v", cidr, err)
	}
	return network
}

func TestIsSourceIPAllowedNoConstraintAllowsEverything(t *testing.T) {
	vk := &identity.VirtualKey{}
	if !isSourceIPAllowed(vk, "203.0.113.9") {
		t.Error("isSourceIPAllowed with no AllowedSourceCIDRs constraint = false, want true")
	}
	if !isSourceIPAllowed(vk, "") {
		t.Error("isSourceIPAllowed with no constraint against an empty/unparseable IP = false, want true")
	}
}

func TestIsSourceIPAllowedMatchingCIDR(t *testing.T) {
	vk := &identity.VirtualKey{AllowedSourceCIDRs: []*net.IPNet{mustParseCIDR(t, "10.0.0.0/8")}}
	if !isSourceIPAllowed(vk, "10.1.2.3") {
		t.Error("isSourceIPAllowed(10.1.2.3) against 10.0.0.0/8 = false, want true")
	}
	if isSourceIPAllowed(vk, "203.0.113.9") {
		t.Error("isSourceIPAllowed(203.0.113.9) against 10.0.0.0/8 = true, want false")
	}
}

// TestIsSourceIPAllowedUnparseableIPFailsClosedAgainstARealConstraint
// proves the deliberate fail-closed posture: an IP that doesn't even
// parse (e.g. a caller passing something malformed through) never
// satisfies a real constraint, mirroring isRegionAllowed's own
// fail-closed shape for an empty region.
func TestIsSourceIPAllowedUnparseableIPFailsClosedAgainstARealConstraint(t *testing.T) {
	vk := &identity.VirtualKey{AllowedSourceCIDRs: []*net.IPNet{mustParseCIDR(t, "10.0.0.0/8")}}
	if isSourceIPAllowed(vk, "not-an-ip") {
		t.Error("isSourceIPAllowed(\"not-an-ip\") against a real constraint = true, want false (fail closed)")
	}
	if isSourceIPAllowed(vk, "") {
		t.Error("isSourceIPAllowed(\"\") against a real constraint = true, want false (fail closed)")
	}
}

func TestResolveClientIPStripsPort(t *testing.T) {
	if got := resolveClientIP("203.0.113.9:54321"); got != "203.0.113.9" {
		t.Errorf("resolveClientIP(203.0.113.9:54321) = %q, want 203.0.113.9", got)
	}
}

// TestResolveClientIPPassesThroughBareIP proves a RemoteAddr that isn't
// in host:port form (e.g. some test harnesses' constructed values) still
// reaches isSourceIPAllowed's own net.ParseIP unchanged, rather than
// being mangled by SplitHostPort's own error path.
func TestResolveClientIPPassesThroughBareIP(t *testing.T) {
	if got := resolveClientIP("203.0.113.9"); got != "203.0.113.9" {
		t.Errorf("resolveClientIP(203.0.113.9) = %q, want 203.0.113.9 unchanged", got)
	}
}

// TestResolveClientIPNeverConsultsForwardedHeader is a documentation-grade
// regression guard for the resolved design decision in
// identity.VirtualKey.AllowedSourceCIDRs' own doc comment: resolveClientIP
// takes only remoteAddr, with no header/context parameter at all, so a
// spoofable X-Forwarded-For header structurally cannot influence it --
// this test exists so that if a future change adds header-trust logic
// here, hitting this signature forces a deliberate decision rather than
// a silent one.
func TestResolveClientIPNeverConsultsForwardedHeader(t *testing.T) {
	spoofed := resolveClientIP("198.51.100.1:1234")
	if spoofed != "198.51.100.1" {
		t.Errorf("resolveClientIP(198.51.100.1:1234) = %q, want 198.51.100.1 -- the real TCP peer, never a header value", spoofed)
	}
}

// --- End-to-end dataplane tests, via the real HandleChatCompletion path ---

func sourceIPConstraintChatRequest() adapter.ChatRequest {
	return adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
}

// newSourceIPConstraintPipeline mirrors newRegionConstraintPipeline's own
// harness pattern exactly, for a single openai deployment.
func newSourceIPConstraintPipeline(t *testing.T, vk identity.VirtualKey, upstream UpstreamCaller) *Pipeline {
	t.Helper()
	deployments := []Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	verifier, err := identity.NewVerifier([]identity.VirtualKey{vk})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys([]identity.VirtualKey{vk})),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream:       upstream,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestHandleChatCompletionRejectsDisallowedSourceIP is the load-bearing
// proof for this feature: a request whose RemoteAddr falls outside vk's
// own AllowedSourceCIDRs must be rejected with ErrSourceIPNotAllowed,
// and must never reach the upstream call at all.
func TestHandleChatCompletionRejectsDisallowedSourceIP(t *testing.T) {
	authCred := "ip-constrained-cred"
	vk := identity.VirtualKey{
		ID: "ip-key", KeyHash: testHashOf(authCred),
		AllowedSourceCIDRs: []*net.IPNet{mustParseCIDR(t, "10.0.0.0/8")},
		RateLimitBurst:     100, RateLimitRefill: 100,
	}
	var called bool
	p := newSourceIPConstraintPipeline(t, vk, func(ctx context.Context, dep Deployment, req any) (any, error) {
		called = true
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	})

	_, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "203.0.113.9:1234", sourceIPConstraintChatRequest(), "")
	if !errors.Is(err, ErrSourceIPNotAllowed) {
		t.Fatalf("err = %v, want ErrSourceIPNotAllowed", err)
	}
	if called {
		t.Error("upstream was called for a disallowed source IP -- must never be attempted")
	}
}

// TestHandleChatCompletionAllowsMatchingSourceIP proves the positive
// case: a RemoteAddr inside vk's own AllowedSourceCIDRs succeeds
// normally.
func TestHandleChatCompletionAllowsMatchingSourceIP(t *testing.T) {
	authCred := "ip-constrained-cred-2"
	vk := identity.VirtualKey{
		ID: "ip-key-2", KeyHash: testHashOf(authCred),
		AllowedSourceCIDRs: []*net.IPNet{mustParseCIDR(t, "10.0.0.0/8")},
		RateLimitBurst:     100, RateLimitRefill: 100,
	}
	var called bool
	p := newSourceIPConstraintPipeline(t, vk, func(ctx context.Context, dep Deployment, req any) (any, error) {
		called = true
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	})

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "10.1.2.3:1234", sourceIPConstraintChatRequest(), ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if !called {
		t.Error("upstream was never called for an allowed source IP")
	}
}

// TestHandleChatCompletionUnconstrainedKeyUnaffectedBySourceIP proves an
// unconstrained key (the default) is never affected by this feature,
// regardless of RemoteAddr -- the same "empty means unrestricted"
// convention as regions/models.
func TestHandleChatCompletionUnconstrainedKeyUnaffectedBySourceIP(t *testing.T) {
	authCred := "ip-unconstrained-cred"
	vk := identity.VirtualKey{
		ID: "ip-key-3", KeyHash: testHashOf(authCred),
		RateLimitBurst: 100, RateLimitRefill: 100,
	}
	p := newSourceIPConstraintPipeline(t, vk, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	})

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "198.51.100.1:1234", sourceIPConstraintChatRequest(), ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v, want success -- an unconstrained key must be unaffected", err)
	}
}
