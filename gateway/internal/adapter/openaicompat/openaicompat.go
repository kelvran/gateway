// Package openaicompat is an honest stub for the generic OpenAI-compatible
// provider adapter (self-hosted vLLM/Ollama/TGI/etc.).
//
// Not implemented this pass — per docs/rfcs/2026-09-02-initial-code-scaffolding.md.
// Note, though: per gateway/ARCHITECTURE.md's "Canonical Schema & Provider
// Adapters" section, this is the adapter most worth implementing for real
// first once scaffolding is done — self-hosted runtimes already speak the
// canonical (OpenAI Chat-Completions) dialect natively, so this adapter
// would end up nearly identical to internal/adapter/openai. That's exactly
// why it's *not* done this pass: a second near-identity adapter wouldn't
// prove anything new about the seam that the openai adapter doesn't already
// prove. Both methods return a clear, typed error rather than a silent
// no-op or fake success.
package openaicompat

import (
	"fmt"

	"github.com/kelvran/gateway/internal/adapter"
)

// Adapter is an unimplemented stub satisfying adapter.Adapter.
type Adapter struct{}

// New constructs an OpenAI-compatible Adapter stub.
func New() *Adapter {
	return &Adapter{}
}

// Name implements adapter.Adapter.
func (a *Adapter) Name() string {
	return "openaicompat"
}

// ToProvider implements adapter.Adapter as an honest "not implemented"
// stub.
func (a *Adapter) ToProvider(adapter.ChatRequest) (any, error) {
	return nil, fmt.Errorf("adapter %q not implemented (scaffolding pass, see docs/rfcs/2026-09-02-initial-code-scaffolding.md)", a.Name())
}

// FromProvider implements adapter.Adapter as an honest "not implemented"
// stub.
func (a *Adapter) FromProvider(any) (adapter.ChatResponse, error) {
	return adapter.ChatResponse{}, fmt.Errorf("adapter %q not implemented (scaffolding pass, see docs/rfcs/2026-09-02-initial-code-scaffolding.md)", a.Name())
}
