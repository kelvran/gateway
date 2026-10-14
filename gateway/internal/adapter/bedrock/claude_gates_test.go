package bedrock

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

func bedrockGatesWire(t *testing.T, req adapter.ChatRequest) (*Request, string) {
	t.Helper()
	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider(%s): %v", req.Model, err)
	}
	native := nativeAny.(*Request)
	wire, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return native, string(wire)
}

// Proven on real Bedrock 2026-10-14 (Sonnet 5.5, Haiku 5.5, Fable 5.1, max_tokens
// 1): Converse's inferenceConfig.temperature is accepted only at 1, topP only at
// 0.99, both together a ValidationException. The adapter leaves a rejected value
// off the wire; the dataplane logs the drop.
func TestToProviderDropsRejectedSamplingOnBedrockClaude55(t *testing.T) {
	half, one, p9, p99 := 0.5, 1.0, 0.9, 0.99
	base := func() adapter.ChatRequest { return bedrockThinkingRequest("global.anthropic.claude-sonnet-5-5", nil) }

	req := base()
	req.Temperature = &half
	if native, _ := bedrockGatesWire(t, req); native.InferenceConfig != nil && native.InferenceConfig.Temperature != nil {
		t.Errorf("temperature 0.5 forwarded to Bedrock Sonnet 5.5: %+v", native.InferenceConfig)
	}
	req = base()
	req.Temperature = &one
	if native, _ := bedrockGatesWire(t, req); native.InferenceConfig == nil || native.InferenceConfig.Temperature == nil || *native.InferenceConfig.Temperature != 1 {
		t.Errorf("temperature 1 must be forwarded: %+v", native.InferenceConfig)
	}
	req = base()
	req.TopP = &p9
	if native, _ := bedrockGatesWire(t, req); native.InferenceConfig != nil && native.InferenceConfig.TopP != nil {
		t.Errorf("topP 0.9 forwarded to Bedrock Sonnet 5.5: %+v", native.InferenceConfig)
	}
	req = base()
	req.Temperature, req.TopP = &one, &p99
	native, _ := bedrockGatesWire(t, req)
	if native.InferenceConfig == nil || native.InferenceConfig.Temperature == nil || native.InferenceConfig.TopP != nil {
		t.Errorf("temperature 1 + topP 0.99 must keep temperature and drop topP: %+v", native.InferenceConfig)
	}
	// A 4.x family keeps both (regression guard beside the existing TopP test).
	req = bedrockThinkingRequest("global.anthropic.claude-sonnet-4-6", nil)
	req.Temperature, req.TopP = &half, &p9
	if native, _ := bedrockGatesWire(t, req); native.InferenceConfig == nil || native.InferenceConfig.Temperature == nil || native.InferenceConfig.TopP == nil {
		t.Errorf("Sonnet 4.6 must keep temperature and topP: %+v", native.InferenceConfig)
	}
}

// thinking disabled on Bedrock: Sonnet 5.5 -> between_tools (both halves proven
// 2026-10-14), Fable 5.1 -> dropped (disabled and between_tools are both 400s),
// Haiku 5.5 -> forwarded (accepted).
func TestToProviderThinkingDisabledTreatmentOnBedrock(t *testing.T) {
	cases := map[string]string{
		"global.anthropic.claude-sonnet-5-5": `"thinking":{"type":"between_tools"}`,
		"global.anthropic.claude-haiku-5-5":  `"thinking":{"type":"disabled"}`,
	}
	for model, want := range cases {
		req := bedrockThinkingRequest(model, &adapter.ThinkingConfig{Type: "disabled"})
		if _, w := bedrockGatesWire(t, req); !strings.Contains(w, want) {
			t.Errorf("%s: wire = %s, want %s", model, w, want)
		}
	}
	req := bedrockThinkingRequest("global.anthropic.claude-fable-5-1", &adapter.ThinkingConfig{Type: "disabled"})
	if _, w := bedrockGatesWire(t, req); strings.Contains(w, `"thinking"`) {
		t.Errorf("Fable 5.1: thinking disabled forwarded (a 400 upstream): %s", w)
	}
}

// top_k on Bedrock passes two gates: the per-generation table (which lists
// the probed 5.x families) and the fixed-sampling verdict, which also covers
// Mythos 5.1 -- a family the table never probed (Fable 5.1's twin by release
// note). Sonnet 4.6 keeps it (the existing TopK test pins the forward case).
func TestToProviderDropsTopKOnMythos51ThroughTheFixedSamplingVerdict(t *testing.T) {
	topK := 5
	req := bedrockThinkingRequest("global.anthropic.claude-mythos-5-1", nil)
	req.TopK = &topK
	if _, w := bedrockGatesWire(t, req); strings.Contains(w, `"top_k"`) {
		t.Errorf("top_k forwarded to Bedrock Mythos 5.1: %s", w)
	}
}

// Converse's "any" (canonical "required") is a ValidationException on the
// forced-tool-choice rejecters -- proven on Bedrock Sonnet 5.5 and Fable 5.1
// 2026-10-14 -- so the adapter must error there exactly as the "tool" branch
// already does, and keep forwarding for Haiku 5.5, which accepts it (proven).
func TestToolChoiceRequiredErrorsOnRejectingFamiliesAndForwardsOnHaiku55(t *testing.T) {
	tc := &adapter.ToolChoice{Mode: "required"}
	for _, model := range []string{"global.anthropic.claude-sonnet-5-5", "global.anthropic.claude-fable-5-1"} {
		if _, err := toolChoiceToProvider(tc, model); err == nil {
			t.Errorf("%s: tool_choice required must be an adapter error", model)
		}
	}
	wire, err := toolChoiceToProvider(tc, "global.anthropic.claude-haiku-5-5")
	if err != nil || wire == nil || wire.Any == nil {
		t.Errorf("Haiku 5.5: tool_choice required must forward as any; got %+v, %v", wire, err)
	}
}
