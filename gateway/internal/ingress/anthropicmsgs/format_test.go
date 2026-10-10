package anthropicmsgs

import (
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// The anthropic adapter decides the passthrough path on this exact string
// (adapter.IngressFormatAnthropicMessages); the two constants must never
// drift apart.
func TestFormatMatchesTheAdapterConstant(t *testing.T) {
	if Format != adapter.IngressFormatAnthropicMessages {
		t.Fatalf("Format = %q, adapter.IngressFormatAnthropicMessages = %q", Format, adapter.IngressFormatAnthropicMessages)
	}
}
