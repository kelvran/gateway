package anthropic

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// The two fixtures are real Bedrock InvokeModel bodies captured on
// 2026-10-14 (round-5 Tier A item 6, by the item's Bedrock probe and its
// Sonnet 5.5 re-probe with a reasoning prompt):
// a signed thinking block from a first turn, replayed under a CHANGED system
// prompt with thinking.block_binding.prefix_mismatch_behavior "drop_block"
// and the thinking-binding-controls-2026-08-01 beta. Anthropic's native
// shape, as Bedrock InvokeModel returns it; only the thinking signatures were
// replaced by a placeholder (an opaque account-bound token the test never
// reads). They PROVE that Claude Sonnet 5.5 and Claude Haiku 5.5 run the
// preserved-thinking prefix check -- the reason the adapter's gate now names
// them -- and that the array decodes as documented.
func TestFromProviderDecodesTheRealPrefixDropsOfSonnet55AndHaiku55(t *testing.T) {
	for family, name := range map[string]string{
		"claude-sonnet-5-5": "response_anthropic_native_binding_drop_sonnet55.json",
		"claude-haiku-5-5":  "response_anthropic_native_binding_drop_haiku55.json",
	} {
		t.Run(family, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			var native Response
			if err := json.Unmarshal(raw, &native); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !strings.Contains(native.Model, family) {
				t.Fatalf("fixture model = %q, want the %s family", native.Model, family)
			}
			resp, err := New().FromProvider(&native)
			if err != nil {
				t.Fatalf("FromProvider: %v", err)
			}
			want := []adapter.InputTransformation{{Type: "thinking_dropped", Path: "messages.1.content.0", Reason: "prefix_binding_mismatch"}}
			if len(resp.InputTransformations) != 1 || resp.InputTransformations[0] != want[0] {
				t.Errorf("InputTransformations = %+v, want %+v", resp.InputTransformations, want)
			}
			// Haiku 5.5 thought again on the replay (90 thinking tokens); Sonnet 5.5
			// answered without thinking (0). Either way the subset invariant holds.
			if resp.Usage.ReasoningTokens < 0 || resp.Usage.ReasoningTokens > resp.Usage.CompletionTokens {
				t.Errorf("ReasoningTokens = %d (completion %d): not a subset", resp.Usage.ReasoningTokens, resp.Usage.CompletionTokens)
			}
			// The gate must name this family, or the request that produced this
			// body would never have carried block_binding and the beta.
			if !modelSupportsThinkingBlockBinding(native.Model) {
				t.Errorf("modelSupportsThinkingBlockBinding(%q) = false; the fixture proves the model runs the check", native.Model)
			}
		})
	}
}

// organization_binding_mismatch is the third documented reason (the page,
// 2026-10-14: "the block came from another account"; Sonnet 5.5 and Haiku
// 5.5 blocks are account-bound). DOCUMENTED, not captured live -- producing
// one needs a block signed under another account -- so this pins only that
// the adapter passes the reason through verbatim, as the page asks
// ("Ignore entries whose type or reason you don't recognize, because later
// checks add values").
func TestFromProviderPassesTheOrganizationBindingReasonThrough(t *testing.T) {
	native := &Response{
		ID: "msg_org", Model: "claude-haiku-5-5", StopReason: "end_turn",
		Content: []ContentBlock{{Type: "text", Text: "ok"}},
		InputTransformations: []InputTransformationWire{
			{Type: "thinking_dropped", Path: "messages.1.content.0", Reason: "organization_binding_mismatch"},
		},
	}
	resp, err := New().FromProvider(native)
	if err != nil {
		t.Fatalf("FromProvider: %v", err)
	}
	want := adapter.InputTransformation{Type: "thinking_dropped", Path: "messages.1.content.0", Reason: "organization_binding_mismatch"}
	if len(resp.InputTransformations) != 1 || resp.InputTransformations[0] != want {
		t.Errorf("InputTransformations = %+v, want %+v", resp.InputTransformations, want)
	}
}
