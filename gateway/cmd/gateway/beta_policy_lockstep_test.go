package main

import (
	"testing"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
)

// TestAnthropicBetaPolicyConstantsAgree keeps the dataplane's copy of the
// forward_known policy value in lockstep with the control plane's: the
// dataplane does not import controlplane, and main.go copies the loaded
// string onto dataplane.Deployment.AnthropicBetaPolicy verbatim, so a drift
// between the two would silently turn forward_known into strip.
func TestAnthropicBetaPolicyConstantsAgree(t *testing.T) {
	if dataplane.AnthropicBetaPolicyForwardKnown != controlplane.AnthropicBetaPolicyForwardKnown {
		t.Fatalf("dataplane %q != controlplane %q", dataplane.AnthropicBetaPolicyForwardKnown, controlplane.AnthropicBetaPolicyForwardKnown)
	}
	if controlplane.AnthropicBetaPolicyForwardKnown != "forward_known" {
		t.Fatalf("the documented value is forward_known, got %q", controlplane.AnthropicBetaPolicyForwardKnown)
	}
}
