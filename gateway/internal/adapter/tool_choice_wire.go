package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

// ErrInvalidToolChoice is returned when an inbound tool_choice is neither a
// canonical object, one of OpenAI's bare strings, nor OpenAI's function
// object. It is a client request-shape error (the handler maps it to 400),
// never an upstream failure.
var ErrInvalidToolChoice = errors.New("adapter: invalid tool_choice")

// maxEchoedToolChoiceLen bounds how much of a client-supplied string an
// error message repeats back, matching the 128-rune cap upstream_error.go
// applies to provider-authored text: the message is for the client, but
// it is also logged, and an unbounded echo is a log-flooding vector.
const maxEchoedToolChoiceLen = 128

// echo returns s quoted for an error message, truncated to
// maxEchoedToolChoiceLen runes with control characters replaced, so a
// hostile tool_choice value cannot flood or forge log lines.
func echo(s string) string {
	if !utf8.ValidString(s) {
		s = string([]rune(s)) // replaces invalid bytes with U+FFFD
	}
	runes := []rune(s)
	if len(runes) > maxEchoedToolChoiceLen {
		runes = append(runes[:maxEchoedToolChoiceLen], '…')
	}
	for i, r := range runes {
		if unicode.IsControl(r) {
			runes[i] = '?'
		}
	}
	return fmt.Sprintf("%q", string(runes))
}

// toolChoiceWire is the union of the three inbound shapes ToolChoice accepts:
//
//   - the canonical object {"mode","tool_name","disable_parallel_tool_use"}
//     (docs/rfcs/2026-09-14-gateway-tool-choice-normalization.md);
//   - OpenAI's bare strings "auto" | "required" | "none";
//   - OpenAI's function object {"type":"function","function":{"name":...}}.
//
// Anthropic's own object dialect ({"type":"auto"|"any"|"tool"}) is deliberately
// NOT accepted on this OpenAI-shaped route: Anthropic clients arrive through
// the planned /v1/messages ingress with its own translation, and a third
// inbound dialect would make {"type":"tool"} ambiguous with OpenAI's
// {"type":"function"} with no requesting client (round-4 plan, item 8a).
type toolChoiceWire struct {
	Mode                   string `json:"mode"`
	ToolName               string `json:"tool_name"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	Type                   string `json:"type"`
	Function               *struct {
		Name string `json:"name"`
	} `json:"function"`
}

// UnmarshalJSON implements json.Unmarshaler for the inbound tool_choice
// field. MarshalJSON is intentionally NOT overridden: the canonical object is
// what re-encoding must produce, because the idempotency fingerprint is
// sha256(json.Marshal(req)) (dataplane claimIdempotency) and a wire-shape
// change there would turn every retry across a rolling deploy into a
// spurious fingerprint mismatch.
func (tc *ToolChoice) UnmarshalJSON(data []byte) error {
	// Bare string: OpenAI's "auto" | "required" | "none".
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidToolChoice, err)
		}
		switch s {
		case "auto", "required", "none":
			*tc = ToolChoice{Mode: s}
			return nil
		}
		return fmt.Errorf("%w: unknown string %s (accepted: \"auto\", \"required\", \"none\", {\"type\":\"function\",\"function\":{\"name\":...}}, or {\"mode\":...})", ErrInvalidToolChoice, echo(s))
	}
	var w toolChoiceWire
	if err := json.Unmarshal(data, &w); err != nil {
		// encoding/json's type-mismatch text names Go struct internals
		// ("Go struct field toolChoiceWire.function ..."); the client gets
		// the accepted shapes instead, never the implementation's layout.
		return fmt.Errorf("%w: tool_choice must be \"auto\", \"required\", \"none\", {\"type\":\"function\",\"function\":{\"name\":...}} or {\"mode\":...}", ErrInvalidToolChoice)
	}
	switch {
	case w.Mode != "" && w.Type != "":
		return fmt.Errorf("%w: both \"mode\" (canonical) and \"type\" (OpenAI) are set; send one shape", ErrInvalidToolChoice)
	case w.Type != "":
		// OpenAI's function object.
		if w.Type != "function" {
			switch w.Type {
			case "allowed_tools", "custom":
				// OpenAI's newer tool_choice types. Not supported yet: no
				// provider adapter maps them, and silently downgrading to
				// "auto" would be exactly the quiet-degradation the RFC rejects.
				return fmt.Errorf("%w: tool_choice type %q is not supported by this gateway yet (accepted: \"function\"; bare strings \"auto\"/\"required\"/\"none\"; or the canonical {\"mode\":...} object)", ErrInvalidToolChoice, w.Type)
			}
			return fmt.Errorf("%w: unsupported \"type\" %s (accepted: \"function\"; bare strings \"auto\"/\"required\"/\"none\"; or the canonical {\"mode\":...} object)", ErrInvalidToolChoice, echo(w.Type))
		}
		if w.Function == nil || w.Function.Name == "" {
			return fmt.Errorf("%w: {\"type\":\"function\"} requires a non-empty function.name", ErrInvalidToolChoice)
		}
		*tc = ToolChoice{Mode: "tool", ToolName: w.Function.Name}
		return nil
	case w.Mode != "":
		// Canonical object.
		switch w.Mode {
		case "auto", "required", "none":
		case "tool":
			if w.ToolName == "" {
				return fmt.Errorf("%w: mode \"tool\" requires a non-empty tool_name", ErrInvalidToolChoice)
			}
		default:
			return fmt.Errorf("%w: unknown mode %s (accepted: \"auto\", \"required\", \"none\", \"tool\")", ErrInvalidToolChoice, echo(w.Mode))
		}
		*tc = ToolChoice{Mode: w.Mode, ToolName: w.ToolName, DisableParallelToolUse: w.DisableParallelToolUse}
		return nil
	default:
		return fmt.Errorf("%w: expected \"auto\", \"required\", \"none\", {\"type\":\"function\",\"function\":{\"name\":...}} or {\"mode\":...}", ErrInvalidToolChoice)
	}
}

// ValidateToolChoice checks the one cross-field rule the wire parser cannot:
// a forced tool must be one of the tools the request actually offers,
// otherwise the provider would reject the call much later with a less
// useful error. Nil tool_choice is a no-op; "auto"/"required"/"none" without
// tools is left to the provider (OpenAI accepts "none" without tools and
// rejects "required"; mirroring every provider's table here would drift).
func ValidateToolChoice(tc *ToolChoice, tools []ToolDef) error {
	if tc == nil || tc.Mode != "tool" {
		return nil
	}
	for _, t := range tools {
		if t.Name == tc.ToolName {
			return nil
		}
	}
	return fmt.Errorf("%w: tool_choice names %s but tools[] does not define it", ErrInvalidToolChoice, echo(tc.ToolName))
}
