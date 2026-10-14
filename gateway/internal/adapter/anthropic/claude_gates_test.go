package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

func claudeGatesWire(t *testing.T, req adapter.ChatRequest) string {
	t.Helper()
	nativeAny, err := New().ToProvider(req)
	if err != nil {
		t.Fatalf("ToProvider(%s): %v", req.Model, err)
	}
	wire, err := json.Marshal(nativeAny)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(wire)
}

// The Claude 5.x fixed-sampling rule on the direct API (round-5 Tier A item 2):
// temperature only at 1, top_p only at 0.99, never both, no top_k. A value the
// model would 400 on is left off the wire; the dataplane logs the drop.
func TestToProviderDropsRejectedSamplingOnClaude55(t *testing.T) {
	half, one, p9, p99 := 0.5, 1.0, 0.9, 0.99
	topK := 5
	base := func() adapter.ChatRequest { return thinkingBindingChatRequest("claude-sonnet-5-5", "") }

	req := base()
	req.Temperature = &half
	if w := claudeGatesWire(t, req); strings.Contains(w, `"temperature"`) {
		t.Errorf("temperature 0.5 forwarded to Sonnet 5.5: %s", w)
	}
	req = base()
	req.Temperature = &one
	if w := claudeGatesWire(t, req); !strings.Contains(w, `"temperature":1`) {
		t.Errorf("temperature 1 must be forwarded: %s", w)
	}
	req = base()
	req.TopP = &p9
	if w := claudeGatesWire(t, req); strings.Contains(w, `"top_p"`) {
		t.Errorf("top_p 0.9 forwarded to Sonnet 5.5: %s", w)
	}
	req = base()
	req.TopP = &p99
	if w := claudeGatesWire(t, req); !strings.Contains(w, `"top_p":0.99`) {
		t.Errorf("top_p 0.99 must be forwarded: %s", w)
	}
	req = base()
	req.Temperature, req.TopP = &one, &p99
	if w := claudeGatesWire(t, req); !strings.Contains(w, `"temperature":1`) || strings.Contains(w, `"top_p"`) {
		t.Errorf("temperature 1 + top_p 0.99 must keep temperature and drop top_p: %s", w)
	}
	req = base()
	req.TopK = &topK
	if w := claudeGatesWire(t, req); strings.Contains(w, `"top_k"`) {
		t.Errorf("top_k forwarded to Sonnet 5.5: %s", w)
	}
}

// A family outside the table keeps every sampling field (the existing tests
// pin that for 4.6; this pins Opus 5.5, which is deliberately not gated).
func TestToProviderKeepsSamplingOutsideTheFixedFamilies(t *testing.T) {
	half, p9 := 0.5, 0.9
	topK := 5
	for _, model := range []string{"claude-sonnet-4-6", "claude-opus-5-5"} {
		req := thinkingBindingChatRequest(model, "")
		req.Temperature, req.TopP, req.TopK = &half, &p9, &topK
		w := claudeGatesWire(t, req)
		for _, want := range []string{`"temperature":0.5`, `"top_p":0.9`, `"top_k":5`} {
			if !strings.Contains(w, want) {
				t.Errorf("%s: wire lacks %s: %s", model, want, w)
			}
		}
	}
}

// thinking.type disabled: rewritten to between_tools on Sonnet 5.5, dropped on
// Fable 5.1 / Opus 5.5 (always-on adaptive thinking), forwarded on Haiku 5.5.
func TestToProviderThinkingDisabledTreatmentByFamily(t *testing.T) {
	for model, want := range map[string]string{
		"claude-sonnet-5-5": `"thinking":{"type":"between_tools"}`,
		"claude-haiku-5-5":  `"thinking":{"type":"disabled"}`,
	} {
		req := thinkingBindingChatRequest(model, "")
		req.Thinking = &adapter.ThinkingConfig{Type: "disabled"}
		if w := claudeGatesWire(t, req); !strings.Contains(w, want) {
			t.Errorf("%s: wire = %s, want %s", model, w, want)
		}
	}
	for _, model := range []string{"claude-fable-5-1", "claude-opus-5-5"} {
		req := thinkingBindingChatRequest(model, "")
		req.Thinking = &adapter.ThinkingConfig{Type: "disabled"}
		w := claudeGatesWire(t, req)
		if strings.Contains(w, `"type":"disabled"`) {
			t.Errorf("%s: thinking disabled forwarded (the model rejects it): %s", model, w)
		}
	}
}

// Sonnet 5.5 is on the forced-tool-choice rejecters list: the adapter errors
// instead of relaying a 400, exactly as it does for Fable 5.1 and Opus 5.5.
func TestToProviderRejectsForcedToolChoiceOnSonnet55(t *testing.T) {
	req := thinkingBindingChatRequest("claude-sonnet-5-5", "")
	req.Tools = []adapter.ToolDef{{Name: "t", Description: "d", ParametersJSON: `{"type":"object","properties":{}}`}}
	req.ToolChoice = &adapter.ToolChoice{Mode: "required"}
	if _, err := New().ToProvider(req); err == nil {
		t.Fatal("forced tool_choice on Sonnet 5.5 must be an adapter error, not a relayed 400")
	}
	req.Model = "claude-haiku-5-5"
	if _, err := New().ToProvider(req); err != nil {
		t.Fatalf("Haiku 5.5 accepts a forced tool_choice: %v", err)
	}
}
