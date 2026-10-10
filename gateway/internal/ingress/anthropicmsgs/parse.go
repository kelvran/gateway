package anthropicmsgs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// defaultSchemaName is the JSONSchema.Name the shadow carries for
// output_config.format: Anthropic's wire has no name, OpenAI's json_schema
// requires one, and a translate hop needs something stable to send.
const defaultSchemaName = "output"

// maxEndUserIDBytes bounds metadata.user_id, which becomes a cache scope and
// a log field downstream; Anthropic documents the same 256-character cap.
const maxEndUserIDBytes = 256

// maxErrorFragment bounds how much of the body an error message may echo (a
// member name, a pointer segment, a tool_choice type): the handler puts the
// message into the 400 body and a log line.
const maxErrorFragment = 128

// Parse decodes an Anthropic Messages request body into the canonical
// shadow and its passthrough record (RFC-1 §1), also attached as
// ChatRequest.Passthrough. Every known member maps to the shadow; every
// member the shadow has no home for is recorded in Passthrough.UnknownFields
// under its JSON pointer with its raw value, at every depth; a content block
// with no canonical form (tool_reference, or a type newer than this parser)
// is listed in UntranslatableBlocks, recorded whole in UnknownFields (so the
// RFC-1 §8 fingerprint distinguishes two such blocks), and its strings, at
// any depth, are surfaced as a text part so the guardrail scan sees what the
// model will.
// The body is kept byte-for-byte in RawBody for the passthrough path, after
// checkBody has refused duplicate members and over-deep nesting.
// DisableCacheControlAutoPopulate is set: the client owns cache_control on
// this ingress (RFC-1 §8).
func Parse(body []byte) (adapter.ChatRequest, *adapter.Passthrough, error) {
	if err := checkBody(body); err != nil {
		return adapter.ChatRequest{}, nil, err
	}
	top, err := object(body, "")
	if err != nil {
		return adapter.ChatRequest{}, nil, err
	}
	rec := &recorder{unknown: map[string]json.RawMessage{}}
	req := adapter.ChatRequest{DisableCacheControlAutoPopulate: true}
	pt := &adapter.Passthrough{Format: Format, RawBody: append(json.RawMessage(nil), body...)}

	msgsRaw, err := parseRequired(top, &req)
	if err != nil {
		return adapter.ChatRequest{}, nil, err
	}
	if raw, ok := take(top, "system"); ok {
		msgs, err := parseSystem(raw, rec)
		if err != nil {
			return adapter.ChatRequest{}, nil, err
		}
		req.Messages = append(req.Messages, msgs...)
	}
	msgs, err := parseMessages(msgsRaw, rec)
	if err != nil {
		return adapter.ChatRequest{}, nil, err
	}
	req.Messages = append(req.Messages, msgs...)
	if err := parseSampling(top, &req); err != nil {
		return adapter.ChatRequest{}, nil, err
	}
	if raw, ok := take(top, "tools"); ok {
		if req.Tools, err = parseTools(raw, rec); err != nil {
			return adapter.ChatRequest{}, nil, err
		}
	}
	if raw, ok := take(top, "tool_choice"); ok {
		if req.ToolChoice, err = parseToolChoice(raw, rec); err != nil {
			return adapter.ChatRequest{}, nil, err
		}
	}
	if raw, ok := take(top, "thinking"); ok {
		if err := parseThinking(raw, rec, &req); err != nil {
			return adapter.ChatRequest{}, nil, err
		}
	}
	if raw, ok := take(top, "output_config"); ok {
		if err := parseOutputConfig(raw, rec, &req); err != nil {
			return adapter.ChatRequest{}, nil, err
		}
	}
	if raw, ok := take(top, "metadata"); ok {
		if pt.EndUserID, err = parseMetadata(raw, rec); err != nil {
			return adapter.ChatRequest{}, nil, err
		}
	}
	rec.rest("", top)
	if len(rec.unknown) > 0 {
		pt.UnknownFields = rec.unknown
	}
	pt.UntranslatableBlocks = rec.untranslatable
	req.Passthrough = pt
	return req, pt, nil
}

// parseRequired consumes model and max_tokens and takes messages (returned
// raw; it is parsed after system so system messages come first). JSON null
// counts as absent for all three, as it does at Anthropic (take's rule).
func parseRequired(top map[string]json.RawMessage, req *adapter.ChatRequest) (json.RawMessage, error) {
	raw, ok := take(top, "model")
	if !ok {
		return nil, ErrMissingModel
	}
	if err := decode(raw, &req.Model, "/model"); err != nil {
		return nil, err
	}
	raw, ok = take(top, "max_tokens")
	if !ok {
		return nil, ErrMissingMaxTokens
	}
	var n int
	if err := decode(raw, &n, "/max_tokens"); err != nil {
		return nil, err
	}
	req.MaxTokens = &n
	msgs, ok := take(top, "messages")
	if !ok {
		return nil, ErrMissingMessages
	}
	return msgs, nil
}

// parseSampling consumes temperature, top_p, top_k, stop_sequences, stream.
func parseSampling(top map[string]json.RawMessage, req *adapter.ChatRequest) error {
	if raw, ok := take(top, "temperature"); ok {
		var f float64
		if err := decode(raw, &f, "/temperature"); err != nil {
			return err
		}
		req.Temperature = &f
	}
	if raw, ok := take(top, "top_p"); ok {
		var f float64
		if err := decode(raw, &f, "/top_p"); err != nil {
			return err
		}
		req.TopP = &f
	}
	if raw, ok := take(top, "top_k"); ok {
		var n int
		if err := decode(raw, &n, "/top_k"); err != nil {
			return err
		}
		req.TopK = &n
	}
	if raw, ok := take(top, "stop_sequences"); ok {
		var seqs []string
		if err := decode(raw, &seqs, "/stop_sequences"); err != nil {
			return err
		}
		req.StopSequences = adapter.StopSequences(seqs)
	}
	if raw, ok := take(top, "stream"); ok {
		if err := decode(raw, &req.Stream, "/stream"); err != nil {
			return err
		}
	}
	return nil
}

// recorder collects what the shadow could not hold.
type recorder struct {
	unknown        map[string]json.RawMessage
	untranslatable []string
}

// rest records every member still in obj -- everything a parse function did
// not take -- under ptr. A null member is absent (take's rule) and is not
// recorded.
func (r *recorder) rest(ptr string, obj map[string]json.RawMessage) {
	for k, v := range obj {
		if isNull(v) {
			continue
		}
		r.unknown[ptr+"/"+escapePointer(k)] = v
	}
}

// escapePointer applies RFC 6901's two escapes.
func escapePointer(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

// object decodes raw as a JSON object into a member map, naming ptr on
// failure.
func object(raw []byte, ptr string) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("%w: %s is not a JSON object", errFor(ptr), ptrOrBody(ptr))
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("%w: %s: %s", errFor(ptr), ptrOrBody(ptr), describe(err))
	}
	return obj, nil
}

// describe renders a decode error without echoing the body: a type mismatch
// names only the expected Go type (encoding/json's own message can carry the
// offending literal verbatim), anything else is truncated.
func describe(err error) string {
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) && ute.Type != nil {
		return "expected " + ute.Type.String()
	}
	return truncate(err.Error())
}

// truncate cuts s to maxErrorFragment bytes on a rune boundary, marking the
// cut.
func truncate(s string) string {
	if len(s) <= maxErrorFragment {
		return s
	}
	cut := maxErrorFragment
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// errFor is ErrInvalidBody for the body itself, ErrInvalidField below it.
func errFor(ptr string) error {
	if ptr == "" {
		return ErrInvalidBody
	}
	return ErrInvalidField
}

func ptrOrBody(ptr string) string {
	if ptr == "" {
		return "the body"
	}
	return ptr
}

// take removes and returns a member, so what remains in the map is exactly
// what the recorder reports. A JSON null is absent -- Anthropic reads it so,
// and encoding/json would otherwise decode it as a silent no-op into 0 or ""
// -- so a null member is removed and reported missing.
func take(obj map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	raw, ok := obj[key]
	if !ok {
		return nil, false
	}
	delete(obj, key)
	if isNull(raw) {
		return nil, false
	}
	return raw, true
}

// decode unmarshals raw into v, naming ptr on a type mismatch.
func decode(raw json.RawMessage, v any, ptr string) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%w: %s: %s", ErrInvalidField, ptr, describe(err))
	}
	return nil
}

// array decodes raw as a JSON array of raw elements.
func array(raw json.RawMessage, ptr string) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%w: %s is not a JSON array: %s", ErrInvalidField, ptr, describe(err))
	}
	return items, nil
}

func isString(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '"'
}

func isNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// compact returns raw JSON without insignificant whitespace, as sent.
func compact(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// parseCacheControl maps Anthropic's {type, ttl} onto the canonical
// CacheControl (ttl only; type is always "ephemeral"), recording any other
// member.
func parseCacheControl(raw json.RawMessage, ptr string, rec *recorder) (*adapter.CacheControl, error) {
	obj, err := object(raw, ptr)
	if err != nil {
		return nil, err
	}
	take(obj, "type")
	cc := &adapter.CacheControl{}
	if ttl, ok := take(obj, "ttl"); ok {
		if err := decode(ttl, &cc.TTL, ptr+"/ttl"); err != nil {
			return nil, err
		}
	}
	rec.rest(ptr, obj)
	return cc, nil
}

// parseSystem: a string is one system message; an array is one message per
// block, each keeping its own cache_control and never merged. A block of a
// type other than text has no canonical form: it is listed as
// untranslatable, recorded whole, and its strings become a system message
// for the guardrail; a text block's unconsumed members become one too.
func parseSystem(raw json.RawMessage, rec *recorder) ([]adapter.Message, error) {
	if isString(raw) {
		var text string
		if err := decode(raw, &text, "/system"); err != nil {
			return nil, err
		}
		return []adapter.Message{{Role: "system", Content: text}}, nil
	}
	items, err := array(raw, "/system")
	if err != nil {
		return nil, err
	}
	out := make([]adapter.Message, 0, len(items))
	for i, item := range items {
		ptr := fmt.Sprintf("/system/%d", i)
		obj, err := object(item, ptr)
		if err != nil {
			return nil, err
		}
		kind, err := blockType(obj, ptr)
		if err != nil {
			return nil, err
		}
		if kind != "text" {
			rec.untranslatable = append(rec.untranslatable, ptr)
			rec.unknown[ptr] = item
			if text := collectStrings(obj); text != "" {
				out = append(out, adapter.Message{Role: "system", Content: text})
			}
			continue
		}
		msg := adapter.Message{Role: "system"}
		if t, ok := take(obj, "text"); ok {
			if err := decode(t, &msg.Content, ptr+"/text"); err != nil {
				return nil, err
			}
		}
		if cc, ok := take(obj, "cache_control"); ok {
			if msg.CacheControl, err = parseCacheControl(cc, ptr+"/cache_control", rec); err != nil {
				return nil, err
			}
		}
		extra := leftoverText(obj)
		rec.rest(ptr, obj)
		out = append(out, msg)
		if extra != "" {
			out = append(out, adapter.Message{Role: "system", Content: extra})
		}
	}
	return out, nil
}

// blockType takes a block's type member.
func blockType(obj map[string]json.RawMessage, ptr string) (string, error) {
	var kind string
	if t, ok := take(obj, "type"); ok {
		if err := decode(t, &kind, ptr+"/type"); err != nil {
			return "", err
		}
	}
	return kind, nil
}

// parseTools maps tools[] onto ToolDefs: name, description, input_schema
// (its bytes as sent), cache_control, strict. An entry whose type is not
// custom (a server tool, a built-in) has no ToolDef form and is recorded
// whole under its pointer.
func parseTools(raw json.RawMessage, rec *recorder) ([]adapter.ToolDef, error) {
	items, err := array(raw, "/tools")
	if err != nil {
		return nil, err
	}
	out := make([]adapter.ToolDef, 0, len(items))
	for i, item := range items {
		ptr := fmt.Sprintf("/tools/%d", i)
		obj, err := object(item, ptr)
		if err != nil {
			return nil, err
		}
		if t, ok := take(obj, "type"); ok {
			var kind string
			if err := decode(t, &kind, ptr+"/type"); err != nil {
				return nil, err
			}
			if kind != "custom" {
				rec.unknown[ptr] = item
				continue
			}
		}
		td := adapter.ToolDef{}
		if v, ok := take(obj, "name"); ok {
			if err := decode(v, &td.Name, ptr+"/name"); err != nil {
				return nil, err
			}
		}
		if v, ok := take(obj, "description"); ok {
			if err := decode(v, &td.Description, ptr+"/description"); err != nil {
				return nil, err
			}
		}
		if v, ok := take(obj, "input_schema"); ok {
			td.ParametersJSON = compact(v)
		}
		if v, ok := take(obj, "strict"); ok {
			if err := decode(v, &td.Strict, ptr+"/strict"); err != nil {
				return nil, err
			}
		}
		if c, ok := take(obj, "cache_control"); ok {
			if td.CacheControl, err = parseCacheControl(c, ptr+"/cache_control", rec); err != nil {
				return nil, err
			}
		}
		rec.rest(ptr, obj)
		out = append(out, td)
	}
	return out, nil
}

// parseToolChoice is the inverse of the anthropic adapter's forward table:
// auto→auto, any→required, none→none, tool→tool (with name).
func parseToolChoice(raw json.RawMessage, rec *recorder) (*adapter.ToolChoice, error) {
	obj, err := object(raw, "/tool_choice")
	if err != nil {
		return nil, err
	}
	kind, err := blockType(obj, "/tool_choice")
	if err != nil {
		return nil, err
	}
	tc := &adapter.ToolChoice{}
	switch kind {
	case "auto":
		tc.Mode = "auto"
	case "any":
		tc.Mode = "required"
	case "none":
		tc.Mode = "none"
	case "tool":
		tc.Mode = "tool"
		if v, ok := take(obj, "name"); ok {
			if err := decode(v, &tc.ToolName, "/tool_choice/name"); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("%w: /tool_choice/type %q is not auto, any, none or tool", ErrInvalidField, truncate(kind))
	}
	if v, ok := take(obj, "disable_parallel_tool_use"); ok {
		if err := decode(v, &tc.DisableParallelToolUse, "/tool_choice/disable_parallel_tool_use"); err != nil {
			return nil, err
		}
	}
	rec.rest("/tool_choice", obj)
	return tc, nil
}

// parseThinking maps thinking{type, budget_tokens} onto ChatRequest.Thinking
// and block_binding.prefix_mismatch_behavior onto ThinkingBindingMode
// (error→strict, drop_block→non_strict; any other value is recorded).
func parseThinking(raw json.RawMessage, rec *recorder, req *adapter.ChatRequest) error {
	obj, err := object(raw, "/thinking")
	if err != nil {
		return err
	}
	th := &adapter.ThinkingConfig{}
	if v, ok := take(obj, "type"); ok {
		if err := decode(v, &th.Type, "/thinking/type"); err != nil {
			return err
		}
	}
	if v, ok := take(obj, "budget_tokens"); ok {
		if err := decode(v, &th.BudgetTokens, "/thinking/budget_tokens"); err != nil {
			return err
		}
	}
	if bb, ok := take(obj, "block_binding"); ok {
		bobj, err := object(bb, "/thinking/block_binding")
		if err != nil {
			return err
		}
		if v, ok := take(bobj, "prefix_mismatch_behavior"); ok {
			var behavior string
			if err := decode(v, &behavior, "/thinking/block_binding/prefix_mismatch_behavior"); err != nil {
				return err
			}
			switch behavior {
			case "error":
				req.ThinkingBindingMode = "strict"
			case "drop_block":
				req.ThinkingBindingMode = "non_strict"
			default:
				rec.unknown["/thinking/block_binding/prefix_mismatch_behavior"] = v
			}
		}
		rec.rest("/thinking/block_binding", bobj)
	}
	rec.rest("/thinking", obj)
	req.Thinking = th
	return nil
}

// parseOutputConfig maps output_config.format onto ResponseFormat (the
// inverse of the adapter's OutputFormat, named defaultSchemaName) and
// output_config.effort onto Effort; any other member is recorded.
func parseOutputConfig(raw json.RawMessage, rec *recorder, req *adapter.ChatRequest) error {
	obj, err := object(raw, "/output_config")
	if err != nil {
		return err
	}
	if f, ok := take(obj, "format"); ok {
		fobj, err := object(f, "/output_config/format")
		if err != nil {
			return err
		}
		rf := &adapter.ResponseFormat{}
		if rf.Type, err = blockType(fobj, "/output_config/format"); err != nil {
			return err
		}
		if v, ok := take(fobj, "schema"); ok {
			rf.JSONSchema = &adapter.JSONSchema{Name: defaultSchemaName, Schema: json.RawMessage(compact(v))}
		}
		rec.rest("/output_config/format", fobj)
		req.ResponseFormat = rf
	}
	if v, ok := take(obj, "effort"); ok {
		if err := decode(v, &req.Effort, "/output_config/effort"); err != nil {
			return err
		}
	}
	rec.rest("/output_config", obj)
	return nil
}

// parseMetadata consumes metadata.user_id (at most maxEndUserIDBytes); every
// other member is recorded.
func parseMetadata(raw json.RawMessage, rec *recorder) (string, error) {
	obj, err := object(raw, "/metadata")
	if err != nil {
		return "", err
	}
	var userID string
	if u, ok := take(obj, "user_id"); ok {
		if err := decode(u, &userID, "/metadata/user_id"); err != nil {
			return "", err
		}
		if len(userID) > maxEndUserIDBytes {
			return "", fmt.Errorf("%w: /metadata/user_id exceeds %d bytes", ErrInvalidField, maxEndUserIDBytes)
		}
	}
	rec.rest("/metadata", obj)
	return userID, nil
}
