package main

// Tests for the guardrail-subsystem credential hot-reload feature's
// WIRING: controlplane.GuardrailsConfig's *_file fields ->
// newGuardrailEngine -> a real detector satisfying credentialReloader.
// The reload mechanism's own concurrency/timing behavior is proven
// directly against bedrockguard.Detector/embedsim.BedrockEmbedder in
// their own packages' credential_reload_test.go files; this file's job
// is narrower and complementary, mirroring credential_file_test.go's
// own stated division of labor for deployments.

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
)

// TestNewGuardrailEngineWiresBedrockGuardrailsFileFieldsIntoAReloadableDetector
// proves a GuardrailsConfig with only *_file fields set (no *_env)
// produces a detector satisfying credentialReloader.
func TestNewGuardrailEngineWiresBedrockGuardrailsFileFieldsIntoAReloadableDetector(t *testing.T) {
	cfg := controlplane.GuardrailsConfig{
		BedrockGuardrails: &controlplane.BedrockGuardrailsConfig{
			Region:              "us-east-1",
			AccessKeyIDFile:     "/var/run/secrets/access-key-id",
			SecretAccessKeyFile: "/var/run/secrets/secret-access-key",
			GuardrailID:         "gr-abc123",
			GuardrailVersion:    "1",
		},
	}
	engine, err := newGuardrailEngine(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newGuardrailEngine: %v", err)
	}

	if !anyDetectorSatisfiesCredentialReloader(engine) {
		t.Error("no detector satisfies credentialReloader, want the bedrock_guardrails detector to")
	}
}

// TestNewGuardrailEngineWiresEmbedSimFileFieldsIntoAReloadableDetector
// mirrors the bedrock_guardrails test above for embed_sim -- but unlike
// that one, newGuardrailEngine's embed_sim branch has no test-injectable
// BaseURL: embedsim.New eagerly embeds its whole corpus at construction
// time (one real Embed call per entry, per that function's own doc
// comment), so this genuinely reaches real AWS Bedrock with no way to
// substitute a fake server without changing newGuardrailEngine's own
// production signature (out of this feature's scope, per the plan).
// Skipped by default; set RUN_LIVE_LLM_TESTS=1 plus real
// AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY env vars to run it, mirroring
// bedrockguard_live_test.go's own identical constraint and convention.
// The real env-var VALUES are written to real temp files here (not
// hardcoded fake paths) specifically so this test exercises the actual
// *_file wiring end to end, not just the pre-existing *_env path.
func TestNewGuardrailEngineWiresEmbedSimFileFieldsIntoAReloadableDetector(t *testing.T) {
	if os.Getenv("RUN_LIVE_LLM_TESTS") != "1" {
		t.Skip("requires real AWS Bedrock access (embedsim.New eagerly embeds its corpus at construction time); set RUN_LIVE_LLM_TESTS=1 plus AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY to run")
	}
	accessKeyID := os.Getenv("AWS_ACCESS_KEY_ID")
	secretAccessKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	if accessKeyID == "" || secretAccessKey == "" {
		t.Fatal("RUN_LIVE_LLM_TESTS=1 requires AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY to be set")
	}

	dir := t.TempDir()
	accessKeyIDFile := filepath.Join(dir, "access-key-id")
	secretAccessKeyFile := filepath.Join(dir, "secret-access-key")
	if err := os.WriteFile(accessKeyIDFile, []byte(accessKeyID), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(secretAccessKeyFile, []byte(secretAccessKey), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := controlplane.GuardrailsConfig{
		EmbedSim: &controlplane.EmbedSimConfig{
			Region:              "us-east-1",
			AccessKeyIDFile:     accessKeyIDFile,
			SecretAccessKeyFile: secretAccessKeyFile,
			SimilarityThreshold: 0.82,
		},
	}
	engine, err := newGuardrailEngine(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newGuardrailEngine: %v", err)
	}

	if !anyDetectorSatisfiesCredentialReloader(engine) {
		t.Error("no detector satisfies credentialReloader, want the embed_sim detector to")
	}
}

// TestNewGuardrailEngineDetectorsNeverSatisfyCredentialReloaderWhenNeitherConfigured
// proves the no-op case: guardrail.DefaultDetectors() alone (no
// bedrock_guardrails, no embed_sim) never satisfies credentialReloader
// -- run()'s own reload-loop discovery loop correctly starts zero
// goroutines for a config that never opted into either subsystem.
func TestNewGuardrailEngineDetectorsNeverSatisfyCredentialReloaderWhenNeitherConfigured(t *testing.T) {
	engine, err := newGuardrailEngine(controlplane.GuardrailsConfig{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newGuardrailEngine: %v", err)
	}

	if anyDetectorSatisfiesCredentialReloader(engine) {
		t.Error("a detector satisfies credentialReloader with neither bedrock_guardrails nor embed_sim configured, want none to")
	}
}

func anyDetectorSatisfiesCredentialReloader(engine *guardrail.Engine) bool {
	for _, det := range engine.Detectors() {
		if _, ok := det.(credentialReloader); ok {
			return true
		}
	}
	return false
}
