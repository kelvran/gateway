package bedrock

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// TestToProviderForwardsAnthropicBetasIntoAdditionalModelRequestFields: the
// canonical AnthropicBetas list (filled by the dataplane only under
// anthropic_beta_policy forward_known) is Converse's
// additionalModelRequestFields.anthropic_beta, in the order given; a nil
// list adds nothing, so a request carrying nothing else keeps the nil map
// every request golden relies on.
func TestToProviderForwardsAnthropicBetasIntoAdditionalModelRequestFields(t *testing.T) {
	req := adapter.ChatRequest{
		Model:          "global.anthropic.claude-sonnet-4-6",
		Messages:       []adapter.Message{{Role: "user", Content: "hi"}},
		AnthropicBetas: []string{"claude-code-20250219", "interleaved-thinking-2025-05-14"},
	}
	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	native := nativeAny.(*Request)
	got, _ := native.AdditionalModelRequestFields["anthropic_beta"].([]string)
	if want := req.AnthropicBetas; !reflect.DeepEqual(got, want) {
		t.Fatalf("anthropic_beta = %#v, want %#v (fields %v)", got, want, native.AdditionalModelRequestFields)
	}

	req.AnthropicBetas = nil
	nativeAny, err = New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider without betas: %v", err)
	}
	if fields := nativeAny.(*Request).AdditionalModelRequestFields; fields != nil {
		t.Fatalf("additionalModelRequestFields = %v without betas, want nil", fields)
	}
}

// TestAnthropicBetasMarshalAsTheArrayConverseAccepts pins the wire shape the
// 2026-10-10 Converse probes proved (item 11 slice S2): a JSON array of
// strings under additionalModelRequestFields.anthropic_beta.
func TestAnthropicBetasMarshalAsTheArrayConverseAccepts(t *testing.T) {
	nativeAny, err := New().ToProvider(adapter.ChatRequest{
		Model:          "global.anthropic.claude-sonnet-4-6",
		Messages:       []adapter.Message{{Role: "user", Content: "hi"}},
		AnthropicBetas: []string{"claude-code-20250219", "interleaved-thinking-2025-05-14"},
	})
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	wire, err := json.Marshal(nativeAny)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `"additionalModelRequestFields":{"anthropic_beta":["claude-code-20250219","interleaved-thinking-2025-05-14"]}`
	if !strings.Contains(string(wire), want) {
		t.Fatalf("wire lacks %s:\n%s", want, wire)
	}
}
