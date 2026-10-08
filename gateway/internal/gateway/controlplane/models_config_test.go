package controlplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modelsConfigSkeleton = "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\ndeployments:\n  d1:\n    model: \"planner\"\n    provider: \"openai\"\n    upstream_model: \"gpt-4.1\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n  d2:\n    model: \"titan-embed\"\n    provider: \"openai\"\n    kind: \"embedding\"\n    upstream_model: \"text-embedding-3-small\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"

func loadModelsConfig(t *testing.T, modelsSection string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(modelsConfigSkeleton+modelsSection), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return Load(path)
}

// TestLoadModelsSectionParsesDisplayMetadata: the optional `models:` section
// supplies display_name/description per canonical model for GET /v1/models;
// a model without an entry simply has none.
func TestLoadModelsSectionParsesDisplayMetadata(t *testing.T) {
	cfg, err := loadModelsConfig(t, "models:\n  planner:\n    display_name: \"Planner (Claude Sonnet)\"\n    description: \"Routes to Bedrock with an OpenAI fallback.\"\n  titan-embed:\n    display_name: \"Titan Embeddings v2\"\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Models["planner"]; got.DisplayName != "Planner (Claude Sonnet)" || got.Description != "Routes to Bedrock with an OpenAI fallback." {
		t.Errorf("Models[planner] = %+v", got)
	}
	if got := cfg.Models["titan-embed"]; got.DisplayName != "Titan Embeddings v2" || got.Description != "" {
		t.Errorf("Models[titan-embed] = %+v, want display name only", got)
	}
	if len(cfg.Models) != 2 {
		t.Errorf("len(Models) = %d, want 2", len(cfg.Models))
	}
}

// TestLoadWithoutModelsSectionLeavesModelsNil: the section is optional.
func TestLoadWithoutModelsSectionLeavesModelsNil(t *testing.T) {
	cfg, err := loadModelsConfig(t, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Models != nil {
		t.Errorf("Models = %+v, want nil when the section is absent", cfg.Models)
	}
}

// TestLoadModelsSectionRejectsUnknownModel: an entry for a model no
// deployment serves is a load error, never silently ignored -- the same
// rule tpm_accounting-without-tpm_capacity follows.
func TestLoadModelsSectionRejectsUnknownModel(t *testing.T) {
	_, err := loadModelsConfig(t, "models:\n  gpt-4o:\n    display_name: \"GPT-4o\"\n")
	if err == nil || !strings.Contains(err.Error(), `models entry "gpt-4o" does not match any deployment's model`) {
		t.Fatalf("err = %v, want the unknown-model load error", err)
	}
}

// TestLoadModelsSectionRejectsUnknownField: a misspelt field (the
// doc-vs-code trap) fails to load instead of being dropped.
func TestLoadModelsSectionRejectsUnknownField(t *testing.T) {
	_, err := loadModelsConfig(t, "models:\n  planner:\n    displayname: \"Planner\"\n")
	if err == nil || !strings.Contains(err.Error(), `unknown field "displayname"`) {
		t.Fatalf("err = %v, want the unknown-field load error", err)
	}
}

// TestLoadModelsSectionRejectsNonMapping: a scalar where a mapping is
// expected is a clear error.
func TestLoadModelsSectionRejectsNonMapping(t *testing.T) {
	_, err := loadModelsConfig(t, "models:\n  planner: \"Planner\"\n")
	if err == nil || !strings.Contains(err.Error(), `models entry "planner" must be a mapping`) {
		t.Fatalf("err = %v, want the must-be-a-mapping load error", err)
	}
}
