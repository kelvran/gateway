// Package gemini is an honest stub for the Gemini provider adapter.
//
// Not implemented this pass — per docs/rfcs/2026-09-02-initial-code-scaffolding.md,
// this pass proves the adapter pattern via one near-identity adapter
// (openai) and one genuine-translation adapter (anthropic); a third real
// adapter would not prove anything new about the seam. Both methods
// return a clear, typed error rather than a silent no-op or fake success.
package gemini

import (
	"fmt"

	"github.com/kelvran/gateway/internal/adapter"
)

// Adapter is an unimplemented stub satisfying adapter.Adapter.
type Adapter struct{}

// New constructs a Gemini Adapter stub.
func New() *Adapter {
	return &Adapter{}
}

// Name implements adapter.Adapter.
func (a *Adapter) Name() string {
	return "gemini"
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
