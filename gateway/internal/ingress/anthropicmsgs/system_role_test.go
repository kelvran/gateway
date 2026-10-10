package anthropicmsgs

import "testing"

// A role:"system" entry inside messages[] -- Claude Code's mid-conversation
// system message, sent under its mid-conversation-system beta -- is parsed
// exactly like the top-level system array: one system message per text
// block, Content filled, cache_control kept, never Parts. Every adapter's
// system hoist reads Content, so a Parts-only system message reached Bedrock
// as an empty {} block ("The system field can't be null", live 2026-10-11).
func TestParseSystemRoleEntryInsideMessagesMirrorsTopLevelSystem(t *testing.T) {
	req, pt, err := Parse([]byte(`{"model":"m","max_tokens":8,"system":"Be terse.","messages":[{"role":"user","content":"x"},{"role":"system","content":[{"type":"text","text":"Reminder A","cache_control":{"type":"ephemeral"}},{"type":"text","text":"Reminder B"}]},{"role":"user","content":"y"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if pt != nil && (len(pt.UnknownFields) != 0 || len(pt.UntranslatableBlocks) != 0) {
		t.Fatalf("a text-only system entry recorded unknown members: %v / %v", keysOf(pt.UnknownFields), pt.UntranslatableBlocks)
	}
	want := []struct {
		role, content string
		cached        bool
	}{
		{"system", "Be terse.", false},
		{"user", "x", false},
		{"system", "Reminder A", true},
		{"system", "Reminder B", false},
		{"user", "y", false},
	}
	if len(req.Messages) != len(want) {
		t.Fatalf("messages = %+v, want %d messages (one per system block, none merged)", req.Messages, len(want))
	}
	for i, w := range want {
		m := req.Messages[i]
		if m.Role != w.role || m.Content != w.content || len(m.Parts) != 0 || (m.CacheControl != nil) != w.cached {
			t.Errorf("messages[%d] = %+v, want role %q content %q cached %v and no Parts", i, m, w.role, w.content, w.cached)
		}
	}
}

// A non-text block inside such an entry has no canonical home, like a
// non-text block in the top-level system array: it is listed as
// untranslatable, recorded whole, and its strings become a system message for
// the guardrail; the text blocks around it keep their own messages.
func TestParseSystemRoleEntryNonTextBlockIsUntranslatable(t *testing.T) {
	req, pt, err := Parse([]byte(`{"model":"m","max_tokens":8,"messages":[{"role":"system","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"after"}]},{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if pt == nil || len(pt.UntranslatableBlocks) != 1 || pt.UntranslatableBlocks[0] != "/messages/0/content/0" {
		t.Fatalf("untranslatable = %+v, want exactly /messages/0/content/0", pt)
	}
	if _, ok := pt.UnknownFields["/messages/0/content/0"]; !ok {
		t.Errorf("unknown fields = %v, want the block recorded whole under its pointer", keysOf(pt.UnknownFields))
	}
	if len(req.Messages) != 3 || req.Messages[0].Role != "system" || req.Messages[0].Content == "" || len(req.Messages[0].Parts) != 0 {
		t.Fatalf("messages[0] = %+v, want the image block's strings as a system message for the guardrail", req.Messages)
	}
	if req.Messages[1].Role != "system" || req.Messages[1].Content != "after" || len(req.Messages[1].Parts) != 0 || req.Messages[2].Role != "user" {
		t.Errorf("messages[1:] = %+v, want the text block as its own system message, then the user turn", req.Messages[1:])
	}
}
