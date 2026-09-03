package adapter_test

import (
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/gateway/internal/adapter/gemini"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/adapter/openaicompat"
)

// TestAllFiveAdaptersRegister proves all five adapters (two real, three
// honest stubs) satisfy adapter.Adapter and compile registered together in
// a single lookup map, per Task 4's verify step. The production
// registration (cmd/gateway) is built in Task 8; this test only proves the
// interface-satisfaction compiles for every adapter package.
func TestAllFiveAdaptersRegister(t *testing.T) {
	registry := adapter.Registry{
		"openai":       openai.New(),
		"anthropic":    anthropic.New(),
		"gemini":       gemini.New(),
		"bedrock":      bedrock.New(),
		"openaicompat": openaicompat.New(),
	}

	if len(registry) != 5 {
		t.Fatalf("registry has %d adapters, want 5", len(registry))
	}
	for name, a := range registry {
		if a.Name() != name {
			t.Errorf("registry key %q has adapter with Name() = %q", name, a.Name())
		}
	}
}
