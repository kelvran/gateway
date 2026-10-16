package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
)

// roleScopeTestCardNumber is the standard Visa test number -- Luhn-valid,
// never a real account -- so the financial_id detector fires on it.
const roleScopeTestCardNumber = "4111111111111111"

func TestNewGuardrailEngineAppliesRoleScopesToThePolicy(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	unscoped, err := newGuardrailEngine(controlplane.GuardrailsConfig{}, logger)
	if err != nil {
		t.Fatalf("newGuardrailEngine (unscoped): %v", err)
	}
	scoped, err := newGuardrailEngine(controlplane.GuardrailsConfig{
		RoleScopes: map[string][]string{"financial_id": {"user", "tool"}},
	}, logger)
	if err != nil {
		t.Fatalf("newGuardrailEngine (scoped): %v", err)
	}
	if scoped.Version() == unscoped.Version() {
		t.Errorf("Version() = %q for both engines, want the role scope folded into the policy fingerprint", scoped.Version())
	}

	text := "card on file: " + roleScopeTestCardNumber
	if v := scoped.CheckSegments(context.Background(), []guardrail.Segment{{Role: guardrail.RoleSystem, Text: text}}); v.Blocked {
		t.Errorf("a card number in a system segment was blocked under financial_id: [user, tool]; want the system role left unscanned")
	}
	if v := scoped.CheckSegments(context.Background(), []guardrail.Segment{{Role: guardrail.RoleUser, Text: text}}); !v.Blocked {
		t.Errorf("a card number in a user segment was not blocked under financial_id: [user, tool]")
	}
	if v := unscoped.CheckSegments(context.Background(), []guardrail.Segment{{Role: guardrail.RoleSystem, Text: text}}); !v.Blocked {
		t.Errorf("the unscoped engine must still block a card number in a system segment (today's behaviour)")
	}
	if strings.Contains(buf.String(), "guardrail_config_unknown") {
		t.Errorf("unexpected config warning for a valid scope: %s", buf.String())
	}
}

func TestNewGuardrailEngineWarnsAndSkipsAnUnknownRoleScopeCategory(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	unscoped, err := newGuardrailEngine(controlplane.GuardrailsConfig{}, logger)
	if err != nil {
		t.Fatalf("newGuardrailEngine (unscoped): %v", err)
	}
	engine, err := newGuardrailEngine(controlplane.GuardrailsConfig{
		RoleScopes: map[string][]string{"not_a_category": {"user"}},
	}, logger)
	if err != nil {
		t.Fatalf("newGuardrailEngine: %v, want nil -- an unknown category is logged and skipped, matching category_overrides", err)
	}
	if !strings.Contains(buf.String(), "guardrail_config_unknown_category") || !strings.Contains(buf.String(), "not_a_category") {
		t.Errorf("log = %q, want a guardrail_config_unknown_category line naming not_a_category", buf.String())
	}
	if engine.Version() != unscoped.Version() {
		t.Errorf("Version() = %q, want %q: a skipped scope must leave the policy fingerprint unchanged", engine.Version(), unscoped.Version())
	}
}

// TestGuardrailRoleScopeVocabularyMatchesTheEngine pins controlplane's own
// role list (it is a pure leaf and cannot import internal/guardrail) to the
// engine's: every role the loader accepts is one the engine knows, and
// every role the engine knows is one the loader accepts.
func TestGuardrailRoleScopeVocabularyMatchesTheEngine(t *testing.T) {
	accepted := controlplane.KnownGuardrailRoles()
	for _, r := range accepted {
		if !guardrail.KnownRole(r) {
			t.Errorf("controlplane accepts role %q but guardrail.KnownRole rejects it", r)
		}
	}
	for _, r := range []string{guardrail.RoleSystem, guardrail.RoleUser, guardrail.RoleAssistant, guardrail.RoleTool} {
		found := false
		for _, a := range accepted {
			found = found || a == r
		}
		if !found {
			t.Errorf("guardrail knows role %q but controlplane.KnownGuardrailRoles() = %v omits it", r, accepted)
		}
	}
	if len(accepted) != 4 {
		t.Errorf("controlplane.KnownGuardrailRoles() = %v, want exactly the four message roles", accepted)
	}
}
