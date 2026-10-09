package telemetry

import (
	"context"
	"testing"
)

func TestAttributionContextRoundTrip(t *testing.T) {
	a := Attribution{ClientTool: ClientToolClaudeCode, SessionID: "s1", AgentID: "a1", RequestClass: "main", PrevToolCount: 2, PrevToolTotalMS: 30}
	ctx := WithAttribution(context.Background(), a)
	if got := AttributionFromContext(ctx); got != a {
		t.Errorf("AttributionFromContext = %+v, want %+v", got, a)
	}
	if got := AttributionFromContext(context.Background()); got != (Attribution{}) {
		t.Errorf("AttributionFromContext on a bare context = %+v, want the zero value", got)
	}
}

func TestAttributionWithoutIdentifiersKeepsBoundedFields(t *testing.T) {
	a := Attribution{
		ClientTool: ClientToolOpenAIPython, SessionID: "s", AgentID: "a", ParentAgentID: "p", ClaudeCodePromptID: "pr", AgentType: "Explore",
		RequestClass: "subagent", Compaction: "auto", ContextCompacted: "manual", PrevToolDurations: "Bash=1", PrevToolCount: 1, PrevToolTotalMS: 1,
	}
	if !a.HasIdentifiers() {
		t.Fatal("HasIdentifiers = false on a record with identifiers")
	}
	b := a.WithoutIdentifiers()
	if b.HasIdentifiers() {
		t.Errorf("WithoutIdentifiers left identifiers: %+v", b)
	}
	if b.ClientTool != a.ClientTool || b.RequestClass != a.RequestClass || b.Compaction != a.Compaction || b.ContextCompacted != a.ContextCompacted || b.PrevToolDurations != a.PrevToolDurations || b.PrevToolCount != a.PrevToolCount || b.PrevToolTotalMS != a.PrevToolTotalMS {
		t.Errorf("WithoutIdentifiers changed a bounded field: %+v", b)
	}
	if a.SessionID != "s" {
		t.Error("WithoutIdentifiers must return a copy, not mutate the receiver")
	}
}

func TestNormalizedAttributionDimensions(t *testing.T) {
	if got := NormalizedClientTool(""); got != ClientToolOther {
		t.Errorf("NormalizedClientTool(\"\") = %q, want %q", got, ClientToolOther)
	}
	if got := NormalizedClientTool(ClientToolCurl); got != ClientToolCurl {
		t.Errorf("NormalizedClientTool(curl) = %q", got)
	}
	if got := NormalizedRequestClass(""); got != RequestClassNone {
		t.Errorf("NormalizedRequestClass(\"\") = %q, want %q", got, RequestClassNone)
	}
	if got := NormalizedRequestClass("main"); got != "main" {
		t.Errorf("NormalizedRequestClass(main) = %q", got)
	}
}
