package anthropicmsgs

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// parseMessages maps Anthropic's messages[] onto canonical messages: a
// user message's tool_result blocks become role:"tool" messages (in order,
// before the remaining user content); an assistant message's thinking and
// redacted_thinking blocks become ReasoningBlocks whose Sequence counts the
// tool_use blocks seen before them (the anthropic adapter's own replay
// convention); tool_use becomes ToolCalls with the input bytes as sent.
// Strings in members the shadow has no home for -- on the message or on any
// known block -- are surfaced as text parts (see leftoverText).
func parseMessages(raw json.RawMessage, rec *recorder) ([]adapter.Message, error) {
	items, err := array(raw, "/messages")
	if err != nil {
		return nil, err
	}
	var out []adapter.Message
	for i, item := range items {
		ptr := fmt.Sprintf("/messages/%d", i)
		obj, err := object(item, ptr)
		if err != nil {
			return nil, err
		}
		var role string
		if r, ok := take(obj, "role"); ok {
			if err := decode(r, &role, ptr+"/role"); err != nil {
				return nil, err
			}
		}
		content, ok := take(obj, "content")
		if !ok {
			return nil, fmt.Errorf("%w: %s/content is required", ErrInvalidField, ptr)
		}
		extra := leftoverText(obj)
		rec.rest(ptr, obj)
		if isString(content) {
			var text string
			if err := decode(content, &text, ptr+"/content"); err != nil {
				return nil, err
			}
			msg := adapter.Message{Role: role, Content: text}
			if extra != "" && role == "system" {
				// A system message carries Content, never Parts (see
				// systemMessagesFromBlocks): the leftover text is its own
				// system message, as in the top-level system array.
				out = append(out, msg, adapter.Message{Role: "system", Content: extra})
				continue
			}
			if extra != "" {
				msg.Content = ""
				msg.Parts = []adapter.ContentPart{{Type: "text", Text: text}, {Type: "text", Text: extra}}
			}
			out = append(out, msg)
			continue
		}
		blocks, err := array(content, ptr+"/content")
		if err != nil {
			return nil, err
		}
		if role == "system" {
			// The same shape as the top-level system array, parsed the same
			// way (systemMessagesFromBlocks); the entry's own leftover members
			// follow as one more system message.
			msgs, err := systemMessagesFromBlocks(blocks, ptr+"/content", rec)
			if err != nil {
				return nil, err
			}
			if extra != "" {
				msgs = append(msgs, adapter.Message{Role: "system", Content: extra})
			}
			out = append(out, msgs...)
			continue
		}
		msgs, err := parseContentBlocks(role, blocks, extra, ptr+"/content", rec)
		if err != nil {
			return nil, err
		}
		out = append(out, msgs...)
	}
	return out, nil
}

// leftoverText is the guardrail's view of the members a parse function did
// not consume (what the recorder is about to report): every string in them,
// at any depth, newline-joined. Such members are model-visible on a
// passthrough hop (a document's title and context, a text block's citations,
// anything newer than this parser), so they are surfaced as a text part
// beside being recorded; a translate hop is refused anyway once
// UnknownFields is non-empty, so the extra part can never reach a provider.
func leftoverText(obj map[string]json.RawMessage) string {
	if len(obj) == 0 {
		return ""
	}
	return collectStrings(obj)
}

// blockAccumulator gathers one message's blocks into its canonical
// message(s).
type blockAccumulator struct {
	main     adapter.Message
	parts    []adapter.ContentPart
	toolMsgs []adapter.Message
}

// parseContentBlocks converts one message's block array; extra is the
// message-level leftover text, surfaced last.
func parseContentBlocks(role string, blocks []json.RawMessage, extra, ptr string, rec *recorder) ([]adapter.Message, error) {
	acc := &blockAccumulator{main: adapter.Message{Role: role}}
	for j, block := range blocks {
		bptr := fmt.Sprintf("%s/%d", ptr, j)
		obj, err := object(block, bptr)
		if err != nil {
			return nil, err
		}
		kind, err := blockType(obj, bptr)
		if err != nil {
			return nil, err
		}
		if err := acc.add(kind, block, obj, bptr, rec); err != nil {
			return nil, err
		}
	}
	acc.addText(extra)
	return acc.messages(), nil
}

// addText appends a surfaced text part when text is non-empty.
func (a *blockAccumulator) addText(text string) {
	if text != "" {
		a.parts = append(a.parts, adapter.ContentPart{Type: "text", Text: text})
	}
}

// add folds one block into the accumulator; raw is the block as sent, for
// the untranslatable case.
func (a *blockAccumulator) add(kind string, raw json.RawMessage, obj map[string]json.RawMessage, bptr string, rec *recorder) error {
	switch kind {
	case "text":
		part, extra, err := parseTextBlock(obj, bptr, rec)
		if err != nil {
			return err
		}
		a.parts = append(a.parts, part)
		a.addText(extra)
	case "image", "document":
		part, extra, err := parseMediaBlock(kind, obj, bptr, rec)
		if err != nil {
			return err
		}
		a.parts = append(a.parts, part)
		a.addText(extra)
	case "tool_use":
		return a.addToolUse(obj, bptr, rec)
	case "thinking":
		rb := adapter.ReasoningBlock{Sequence: len(a.main.ToolCalls)}
		if v, ok := take(obj, "thinking"); ok {
			if err := decode(v, &rb.Text, bptr+"/thinking"); err != nil {
				return err
			}
		}
		if v, ok := take(obj, "signature"); ok {
			if err := decode(v, &rb.Signature, bptr+"/signature"); err != nil {
				return err
			}
		}
		a.addText(leftoverText(obj))
		rec.rest(bptr, obj)
		a.main.ReasoningBlocks = append(a.main.ReasoningBlocks, rb)
	case "redacted_thinking":
		rb := adapter.ReasoningBlock{Sequence: len(a.main.ToolCalls), Redacted: true}
		if v, ok := take(obj, "data"); ok {
			if err := decode(v, &rb.Data, bptr+"/data"); err != nil {
				return err
			}
		}
		a.addText(leftoverText(obj))
		rec.rest(bptr, obj)
		a.main.ReasoningBlocks = append(a.main.ReasoningBlocks, rb)
	case "tool_result":
		msg, err := parseToolResult(obj, bptr, rec)
		if err != nil {
			return err
		}
		a.toolMsgs = append(a.toolMsgs, msg)
	default:
		// tool_reference (Claude Code's tool search) and any newer block
		// type: no canonical form, so the block is listed as untranslatable,
		// recorded whole (type included) so the §8 fingerprint tells two such
		// blocks apart, and every string in it, at any depth, becomes a text
		// part the guardrail scans.
		rec.untranslatable = append(rec.untranslatable, bptr)
		rec.unknown[bptr] = raw
		a.addText(collectStrings(obj))
	}
	return nil
}

func (a *blockAccumulator) addToolUse(obj map[string]json.RawMessage, bptr string, rec *recorder) error {
	tc := adapter.ToolCall{ArgumentsJSON: "{}"}
	if v, ok := take(obj, "id"); ok {
		if err := decode(v, &tc.ID, bptr+"/id"); err != nil {
			return err
		}
	}
	if v, ok := take(obj, "name"); ok {
		if err := decode(v, &tc.Name, bptr+"/name"); err != nil {
			return err
		}
	}
	if v, ok := take(obj, "input"); ok {
		tc.ArgumentsJSON = compact(v)
	}
	if c, ok := take(obj, "cache_control"); ok {
		cc, err := parseCacheControl(c, bptr+"/cache_control", rec)
		if err != nil {
			return err
		}
		a.main.CacheControl = cc
	}
	a.addText(leftoverText(obj))
	rec.rest(bptr, obj)
	a.main.ToolCalls = append(a.main.ToolCalls, tc)
	return nil
}

// messages lays the accumulated blocks out: tool messages first (a tool
// result must precede the user content that follows it), then the main
// message. A lone plain text part collapses to Content -- the shape every
// adapter handles natively -- while anything else stays as Parts so block
// boundaries, cache_control and a plaintext document's media_type survive.
func (a *blockAccumulator) messages() []adapter.Message {
	out := append([]adapter.Message(nil), a.toolMsgs...)
	main := a.main
	switch {
	case len(a.parts) == 1 && isPlainText(a.parts[0]):
		main.Content = a.parts[0].Text
	case len(a.parts) > 0:
		main.Parts = a.parts
	}
	if main.Content != "" || main.Parts != nil || len(main.ToolCalls) > 0 || len(main.ReasoningBlocks) > 0 {
		out = append(out, main)
	}
	return out
}

// isPlainText reports a text part carrying nothing but its text.
func isPlainText(p adapter.ContentPart) bool {
	return p.Type == "text" && p.CacheControl == nil && p.MediaType == "" && p.Data == "" && p.URL == ""
}

// parseTextBlock maps a text block onto a text part, returning the leftover
// text of its unconsumed members (citations, say) for surfacing.
func parseTextBlock(obj map[string]json.RawMessage, bptr string, rec *recorder) (adapter.ContentPart, string, error) {
	part := adapter.ContentPart{Type: "text"}
	if t, ok := take(obj, "text"); ok {
		if err := decode(t, &part.Text, bptr+"/text"); err != nil {
			return part, "", err
		}
	}
	if c, ok := take(obj, "cache_control"); ok {
		cc, err := parseCacheControl(c, bptr+"/cache_control", rec)
		if err != nil {
			return part, "", err
		}
		part.CacheControl = cc
	}
	extra := leftoverText(obj)
	rec.rest(bptr, obj)
	return part, extra, nil
}

// parseMediaBlock maps an image/document block's source onto a ContentPart,
// returning the leftover text of its unconsumed members at both levels
// (title, context, a content-composed source, ...) for surfacing. A document
// whose source.type is "text" carries plaintext in data -- which the
// canonical document part would treat as base64 and the guardrail would
// skip -- so it becomes a text part instead, keeping its media_type so the
// shadow still differs from a plain text block.
func parseMediaBlock(kind string, obj map[string]json.RawMessage, ptr string, rec *recorder) (adapter.ContentPart, string, error) {
	part := adapter.ContentPart{Type: kind}
	var sourceExtra string
	if src, ok := take(obj, "source"); ok {
		sobj, err := object(src, ptr+"/source")
		if err != nil {
			return part, "", err
		}
		srcType, err := blockType(sobj, ptr+"/source")
		if err != nil {
			return part, "", err
		}
		for key, dst := range map[string]*string{"media_type": &part.MediaType, "data": &part.Data, "url": &part.URL} {
			if v, ok := take(sobj, key); ok {
				if err := decode(v, dst, ptr+"/source/"+key); err != nil {
					return part, "", err
				}
			}
		}
		if srcType == "text" && kind == "document" {
			// MediaType stays so the shadow differs from a plain text block
			// (the cache keys must not equate a document with an utterance).
			part = adapter.ContentPart{Type: "text", Text: part.Data, MediaType: part.MediaType}
		}
		sourceExtra = leftoverText(sobj)
		rec.rest(ptr+"/source", sobj)
	}
	if c, ok := take(obj, "cache_control"); ok {
		cc, err := parseCacheControl(c, ptr+"/cache_control", rec)
		if err != nil {
			return part, "", err
		}
		part.CacheControl = cc
	}
	extra := joinText(leftoverText(obj), sourceExtra)
	rec.rest(ptr, obj)
	return part, extra, nil
}

// joinText newline-joins the non-empty strings.
func joinText(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n")
}

// parseToolResult maps a tool_result block onto a role:"tool" message: a
// string content is Content, an array is Parts (text, image, document;
// anything else untranslatable), is_error is ToolResultIsError. Leftover
// text at any level is surfaced as a part of the tool message.
func parseToolResult(obj map[string]json.RawMessage, ptr string, rec *recorder) (adapter.Message, error) {
	msg := adapter.Message{Role: "tool"}
	if v, ok := take(obj, "tool_use_id"); ok {
		if err := decode(v, &msg.ToolCallID, ptr+"/tool_use_id"); err != nil {
			return msg, err
		}
	}
	if v, ok := take(obj, "is_error"); ok {
		if err := decode(v, &msg.ToolResultIsError, ptr+"/is_error"); err != nil {
			return msg, err
		}
	}
	if c, ok := take(obj, "cache_control"); ok {
		cc, err := parseCacheControl(c, ptr+"/cache_control", rec)
		if err != nil {
			return msg, err
		}
		msg.CacheControl = cc
	}
	content, ok := take(obj, "content")
	extra := leftoverText(obj)
	rec.rest(ptr, obj)
	if ok {
		if isString(content) {
			if err := decode(content, &msg.Content, ptr+"/content"); err != nil {
				return msg, err
			}
		} else if err := parseToolResultBlocks(&msg, content, ptr, rec); err != nil {
			return msg, err
		}
	}
	if extra != "" {
		if msg.Content != "" {
			msg.Parts = append([]adapter.ContentPart{{Type: "text", Text: msg.Content}}, msg.Parts...)
			msg.Content = ""
		}
		msg.Parts = append(msg.Parts, adapter.ContentPart{Type: "text", Text: extra})
	}
	return msg, nil
}

// parseToolResultBlocks converts a tool_result's array content into Parts.
func parseToolResultBlocks(msg *adapter.Message, content json.RawMessage, ptr string, rec *recorder) error {
	blocks, err := array(content, ptr+"/content")
	if err != nil {
		return err
	}
	addText := func(text string) {
		if text != "" {
			msg.Parts = append(msg.Parts, adapter.ContentPart{Type: "text", Text: text})
		}
	}
	for k, block := range blocks {
		bptr := fmt.Sprintf("%s/content/%d", ptr, k)
		bobj, err := object(block, bptr)
		if err != nil {
			return err
		}
		kind, err := blockType(bobj, bptr)
		if err != nil {
			return err
		}
		switch kind {
		case "text":
			part, extra, err := parseTextBlock(bobj, bptr, rec)
			if err != nil {
				return err
			}
			msg.Parts = append(msg.Parts, part)
			addText(extra)
		case "image", "document":
			part, extra, err := parseMediaBlock(kind, bobj, bptr, rec)
			if err != nil {
				return err
			}
			msg.Parts = append(msg.Parts, part)
			addText(extra)
		default:
			rec.untranslatable = append(rec.untranslatable, bptr)
			rec.unknown[bptr] = block
			addText(collectStrings(bobj))
		}
	}
	return nil
}

// collectStrings joins every string value in a block, at any depth --
// objects in key order, arrays in element order -- so the guardrail scan
// sees what the model will. Depth is bounded by checkBody, which also
// validated the syntax, so the Unmarshal here cannot fail.
func collectStrings(obj map[string]json.RawMessage) string {
	var out []string
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var v any
		if json.Unmarshal(obj[k], &v) == nil {
			out = walkStrings(v, out)
		}
	}
	return strings.Join(out, "\n")
}

func walkStrings(v any, out []string) []string {
	switch t := v.(type) {
	case string:
		if t != "" {
			out = append(out, t)
		}
	case []any:
		for _, e := range t {
			out = walkStrings(e, out)
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = walkStrings(t[k], out)
		}
	}
	return out
}
