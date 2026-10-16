package controlplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeGuardrailScopesConfig writes a minimal, otherwise-valid config whose
// guardrails: block is exactly the given YAML fragment (already indented
// two spaces per level under guardrails:), and returns its path.
func writeGuardrailScopesConfig(t *testing.T, guardrailsBlock string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nguardrails:\n" + guardrailsBlock +
		"deployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestLoadGuardrailsRoleScopesParsesAMappingOfRoleToTrue(t *testing.T) {
	// The parser has no YAML lists, so a role set is a mapping of
	// role -> true, exactly like a virtual key's allowed_models.
	path := writeGuardrailScopesConfig(t, "  role_scopes:\n    prompt_injection:\n      user: true\n      tool: true\n    credential:\n      user: true\n      assistant: false\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with a guardrails.role_scopes block: %v", err)
	}
	if got := strings.Join(cfg.Guardrails.RoleScopes["prompt_injection"], ","); got != "tool,user" {
		t.Errorf(`Guardrails.RoleScopes["prompt_injection"] = %q, want "tool,user" (sorted, only the true entries)`, got)
	}
	if got := strings.Join(cfg.Guardrails.RoleScopes["credential"], ","); got != "user" {
		t.Errorf(`Guardrails.RoleScopes["credential"] = %q, want "user" -- a false entry is not in the scope`, got)
	}
}

func TestLoadWithoutRoleScopesLeavesTheMapNil(t *testing.T) {
	path := writeGuardrailScopesConfig(t, "  policy_version: \"v2\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Guardrails.RoleScopes != nil {
		t.Errorf("Guardrails.RoleScopes = %v, want nil when the key is absent (every role scanned, today's behaviour)", cfg.Guardrails.RoleScopes)
	}
}

func TestLoadRejectsARoleScopeEntryThatIsNotABool(t *testing.T) {
	// The over-nesting mistake category_overrides already rejects, one
	// level down: a role written as a nested mapping or a bare string.
	path := writeGuardrailScopesConfig(t, "  role_scopes:\n    prompt_injection:\n      user: \"yes\"\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "role_scopes") {
		t.Fatalf("Load with role_scopes.prompt_injection.user: \"yes\" = %v, want a loud error naming role_scopes", err)
	}
}

func TestLoadRejectsAnUnknownRoleInARoleScope(t *testing.T) {
	// A typo ("tools") would otherwise narrow the category to nothing
	// but the roles that happened to be spelled right -- a security
	// control silently narrowed is a load error, never a warning.
	path := writeGuardrailScopesConfig(t, "  role_scopes:\n    prompt_injection:\n      tools: true\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "tools") {
		t.Fatalf("Load with an unknown role \"tools\" = %v, want a loud error naming the role", err)
	}
}

func TestLoadRejectsARoleScopeThatAdmitsNoRole(t *testing.T) {
	// An empty mapping, or one whose entries are all false, would switch
	// the category off entirely; category_overrides has no "off" action
	// on purpose, so role_scopes must not become one by accident.
	for name, block := range map[string]string{
		"all false": "  role_scopes:\n    credential:\n      user: false\n",
		"empty":     "  role_scopes:\n    credential:\n",
	} {
		path := writeGuardrailScopesConfig(t, block)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "credential") {
			t.Errorf("%s: Load = %v, want a loud error naming the category whose scope admits no role", name, err)
		}
	}
}

func TestLoadRejectsARoleScopeWrittenAsAString(t *testing.T) {
	path := writeGuardrailScopesConfig(t, "  role_scopes:\n    prompt_injection: \"user, tool\"\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "prompt_injection") {
		t.Fatalf("Load with role_scopes.prompt_injection as a string = %v, want a loud error: the value is a mapping of role -> true", err)
	}
}
