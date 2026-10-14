package adapter

import (
	"encoding/json"
	"testing"
)

func fptr(v float64) *float64 { return &v }
func iptr(v int) *int         { return &v }

// Proven on real Bedrock 2026-10-14 (max_tokens 1, Sonnet 5.5, Haiku 5.5,
// Fable 5.1): temperature is accepted only at 1, top_p only at 0.99, the two
// together are a 400 even at those values; top_k is a 400 for any value on
// the 5.x generation (the Haiku 5.5 migration guide; the Bedrock thinking
// table already drops it). Sonnet 5 was proven the same way by a direct
// Converse probe on 2026-10-14 (F5 had seen its temperature 0.3 rejected on
// 2026-10-08); Mythos 5.1 follows Fable 5.1 by Anthropic's own release note
// (not probeable here).
func TestClaudeSamplingDropsForFixedSamplingFamilies(t *testing.T) {
	models := []string{
		"claude-sonnet-5-5", "global.anthropic.claude-sonnet-5-5", "global.anthropic.claude-haiku-5-5-20261007-v1:0",
		"claude-haiku-5-5", "claude-fable-5-1", "claude-mythos-5-1", "claude-sonnet-5",
	}
	for _, model := range models {
		if d := ClaudeSamplingDropsFor(model, fptr(0.5), nil, nil); d.Temperature == "" || d.TopP != "" || d.TopK != "" {
			t.Errorf("%s temperature 0.5: drops = %+v, want only Temperature dropped", model, d)
		}
		if d := ClaudeSamplingDropsFor(model, fptr(1), nil, nil); d != (SamplingDrops{}) {
			t.Errorf("%s temperature 1: drops = %+v, want none", model, d)
		}
		if d := ClaudeSamplingDropsFor(model, nil, fptr(0.9), nil); d.TopP == "" || d.Temperature != "" {
			t.Errorf("%s top_p 0.9: drops = %+v, want only TopP dropped", model, d)
		}
		if d := ClaudeSamplingDropsFor(model, nil, fptr(0.99), nil); d != (SamplingDrops{}) {
			t.Errorf("%s top_p 0.99: drops = %+v, want none", model, d)
		}
		// Both at their only accepted values is still a 400: keep temperature, drop top_p.
		if d := ClaudeSamplingDropsFor(model, fptr(1), fptr(0.99), nil); d.Temperature != "" || d.TopP == "" {
			t.Errorf("%s temperature 1 + top_p 0.99: drops = %+v, want TopP dropped and Temperature kept", model, d)
		}
		if d := ClaudeSamplingDropsFor(model, nil, nil, iptr(5)); d.TopK == "" || d.Temperature != "" || d.TopP != "" {
			t.Errorf("%s top_k 5: drops = %+v, want only TopK dropped", model, d)
		}
	}
}

// Families outside the table, including Opus 5.5 (not probeable on this
// account and not documented as rejecting sampling), forward everything.
func TestClaudeSamplingDropsForLeavesOtherFamiliesAlone(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-6", "global.anthropic.claude-haiku-4-5-20251001-v1:0", "claude-opus-5-5", "gpt-4o", ""} {
		if d := ClaudeSamplingDropsFor(model, fptr(0.5), fptr(0.9), iptr(5)); d != (SamplingDrops{}) {
			t.Errorf("%s: drops = %+v, want none", model, d)
		}
	}
}

// The comparison is exact float64 equality: the JSON literals 1 and 0.99
// decode to the constants, a near miss is a different value and is dropped
// (never rounded into the accepted value and forwarded).
func TestClaudeSamplingDropsForNearMissValues(t *testing.T) {
	var fromJSON struct {
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
	}
	if err := json.Unmarshal([]byte(`{"temperature":1,"top_p":0.99}`), &fromJSON); err != nil {
		t.Fatal(err)
	}
	if d := ClaudeSamplingDropsFor("claude-sonnet-5-5", fromJSON.Temperature, nil, nil); d != (SamplingDrops{}) {
		t.Errorf("JSON temperature 1: drops = %+v, want none", d)
	}
	if d := ClaudeSamplingDropsFor("claude-sonnet-5-5", nil, fromJSON.TopP, nil); d != (SamplingDrops{}) {
		t.Errorf("JSON top_p 0.99: drops = %+v, want none", d)
	}
	if d := ClaudeSamplingDropsFor("claude-sonnet-5-5", fptr(1.0000001), fptr(0.9899999999), nil); d.Temperature == "" || d.TopP == "" {
		t.Errorf("near misses: drops = %+v, want both dropped", d)
	}
}

// Assistant prefill (a conversation ending on an assistant turn) is a 400 on
// Sonnet 5.5, Haiku 5.5, Fable 5.1, Sonnet 5 and Sonnet 4.6 (all proven on
// Bedrock 2026-10-14; the Sonnet 5.5 migration guide names the last two),
// and on Mythos 5.1 (Fable 5.1's twin).
func TestClaudeRejectsAssistantPrefill(t *testing.T) {
	for model, want := range map[string]bool{
		"claude-sonnet-5-5": true, "global.anthropic.claude-haiku-5-5": true, "claude-fable-5-1": true, "claude-mythos-5-1": true,
		"claude-sonnet-5": true, "claude-sonnet-4-6": true,
		"claude-haiku-4-5": false, "global.anthropic.claude-haiku-4-5-20251001-v1:0": false, "claude-opus-5-5": false, "gpt-4o": false, "": false,
	} {
		if got := ClaudeRejectsAssistantPrefill(model); got != want {
			t.Errorf("ClaudeRejectsAssistantPrefill(%q) = %v, want %v", model, got, want)
		}
	}
}

// thinking.type "disabled": Sonnet 5.5 rejects it and documents
// "between_tools" as the way to turn up-front thinking off (both proven on
// Bedrock 2026-10-14); Fable 5.1 rejects both disabled and between_tools
// (proven) -- thinking is always on, so the field is dropped; Opus 5.5 and
// Mythos 5.1 are documented the same way; Haiku 5.5 accepts disabled (proven).
func TestClaudeThinkingDisabledTreatment(t *testing.T) {
	for model, want := range map[string]string{
		"claude-sonnet-5-5": ThinkingDisabledBetweenTools, "global.anthropic.claude-sonnet-5-5": ThinkingDisabledBetweenTools,
		"claude-fable-5-1": ThinkingDisabledDrop, "claude-mythos-5-1": ThinkingDisabledDrop, "claude-opus-5-5": ThinkingDisabledDrop,
		"claude-haiku-5-5": ThinkingDisabledForward, "claude-sonnet-4-6": ThinkingDisabledForward, "gpt-4o": ThinkingDisabledForward,
	} {
		if got := ClaudeThinkingDisabledTreatment(model); got != want {
			t.Errorf("ClaudeThinkingDisabledTreatment(%q) = %q, want %q", model, got, want)
		}
	}
}

// Sonnet 5.5 joins the forced-tool-choice rejecters (release notes 2026-09-28;
// proven on Bedrock 2026-10-14: tool_choice any is a 400); Haiku 5.5 accepts a
// forced choice (its migration guide; proven) and stays off the list.
func TestForcedToolChoiceListCoversSonnet55AndNotHaiku55(t *testing.T) {
	for model, want := range map[string]bool{
		"claude-sonnet-5-5": true, "global.anthropic.claude-sonnet-5-5": true, "claude-fable-5-1": true, "claude-opus-5-5": true,
		"claude-haiku-5-5": false, "global.anthropic.claude-haiku-5-5": false, "claude-sonnet-4-6": false,
	} {
		if got := AnthropicModelRejectsForcedToolChoice(model); got != want {
			t.Errorf("AnthropicModelRejectsForcedToolChoice(%q) = %v, want %v", model, got, want)
		}
	}
}
