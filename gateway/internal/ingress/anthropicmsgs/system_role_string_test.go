package anthropicmsgs

import "testing"

// The string-content form of the same entry: a role:"system" message whose
// content is a plain string keeps it as Content, and an unknown member on the
// entry becomes one more system message (its text for the guardrail) rather
// than turning the message into a Parts pair -- the shape a user or
// assistant entry takes -- so no system message ever carries Parts.
func TestParseSystemRoleEntryStringContentWithLeftoverStaysContent(t *testing.T) {
	req, pt, err := Parse([]byte(`{"model":"m","max_tokens":8,"messages":[{"role":"system","content":"Reminder","vendor_note":"keep"},{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if pt == nil || len(pt.UnknownFields) != 1 {
		t.Fatalf("unknown fields = %+v, want the entry's one unknown member recorded", pt)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %+v, want system Reminder, system leftover, user x", req.Messages)
	}
	for i, m := range req.Messages[:2] {
		if m.Role != "system" || m.Content == "" || len(m.Parts) != 0 {
			t.Errorf("messages[%d] = %+v, want a system message with Content and no Parts", i, m)
		}
	}
	if req.Messages[0].Content != "Reminder" || req.Messages[1].Content != "keep" {
		t.Errorf("contents = %q / %q, want Reminder / keep", req.Messages[0].Content, req.Messages[1].Content)
	}
}
