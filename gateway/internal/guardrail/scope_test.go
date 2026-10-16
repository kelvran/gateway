package guardrail

import (
	"context"
	"strings"
	"testing"
)

// recordingDetector captures the exact text each Detect call received, so
// a test can prove which role-tagged segments a scoped detector saw, and
// returns a fixed set of findings (offsets into that text) for role
// mapping.
type recordingDetector struct {
	name     string
	category Category
	findings []Finding
	texts    *[]string
}

func (r recordingDetector) Name() string       { return r.name }
func (r recordingDetector) Category() Category { return r.category }
func (r recordingDetector) Detect(_ context.Context, text string) ([]Finding, error) {
	*r.texts = append(*r.texts, text)
	return r.findings, nil
}

func fourRoleSegments() []Segment {
	return []Segment{
		{Role: RoleSystem, Text: "system-text"},
		{Role: RoleUser, Text: "user-text"},
		{Role: RoleAssistant, Text: "assistant-text"},
		{Role: RoleTool, Text: "tool-text"},
	}
}

func TestCheckSegmentsScopedDetectorSeesOnlyAdmittedRolesJoinedInOrder(t *testing.T) {
	var texts []string
	policy := DefaultPolicy()
	policy.RoleScopes = map[Category][]string{CategoryPromptInjection: {RoleTool, RoleUser}}
	e := NewEngine([]Detector{recordingDetector{name: "rec", category: CategoryPromptInjection, texts: &texts}}, policy, "test", nil)

	e.CheckSegments(context.Background(), fourRoleSegments())

	if len(texts) != 1 {
		t.Fatalf("Detect calls = %d, want exactly 1 (one joined text per detector, not one call per segment)", len(texts))
	}
	if want := "user-text\ntool-text"; texts[0] != want {
		t.Errorf("scoped detector saw %q, want %q -- only the user and tool segments, in message order, newline-joined", texts[0], want)
	}
}

func TestCheckSegmentsUnscopedCategorySeesEveryRoleExactlyAsCheckWould(t *testing.T) {
	var fromSegments, fromCheck []string
	joined := "system-text\nuser-text\nassistant-text\ntool-text"

	NewEngine([]Detector{recordingDetector{name: "rec", category: CategoryCredential, texts: &fromSegments}}, DefaultPolicy(), "test", nil).
		CheckSegments(context.Background(), fourRoleSegments())
	NewEngine([]Detector{recordingDetector{name: "rec", category: CategoryCredential, texts: &fromCheck}}, DefaultPolicy(), "test", nil).
		Check(context.Background(), joined)

	if len(fromSegments) != 1 || fromSegments[0] != joined {
		t.Errorf("unscoped detector saw %q via CheckSegments, want the byte-identical newline join %q", fromSegments, joined)
	}
	if len(fromCheck) != 1 || fromCheck[0] != fromSegments[0] {
		t.Errorf("Check(text) and CheckSegments(segments) must hand an unscoped detector the same bytes: %q vs %q", fromCheck, fromSegments)
	}
}

func TestCheckSegmentsFindingCarriesTheRoleOfTheSegmentItStartsIn(t *testing.T) {
	// The detector sees "user-text\ntool-text" (the scope admits user and
	// tool); a finding at [10,14) lies inside "tool-text", which starts at
	// byte 10 of that joined text; one at [0,4) lies inside "user-text".
	var texts []string
	policy := DefaultPolicy()
	policy.RoleScopes = map[Category][]string{CategoryGovernmentID: {RoleUser, RoleTool}}
	det := recordingDetector{name: "rec", category: CategoryGovernmentID, texts: &texts, findings: []Finding{
		{Category: CategoryGovernmentID, Detector: "rec", Start: 10, End: 14},
		{Category: CategoryGovernmentID, Detector: "rec", Start: 0, End: 4},
	}}
	v := NewEngine([]Detector{det}, policy, "test", nil).CheckSegments(context.Background(), fourRoleSegments())

	if len(v.Findings) != 2 {
		t.Fatalf("Findings = %+v, want 2", v.Findings)
	}
	if v.Findings[0].Role != RoleTool {
		t.Errorf("finding at [10,14) Role = %q, want %q (the segment byte 10 of the joined text belongs to)", v.Findings[0].Role, RoleTool)
	}
	if v.Findings[1].Role != RoleUser {
		t.Errorf("finding at [0,4) Role = %q, want %q", v.Findings[1].Role, RoleUser)
	}
	if !v.Blocked {
		t.Error("Blocked = false, want true: government_id is Block tier and the findings are inside admitted roles")
	}
}

func TestCheckSegmentsScopedDetectorIsSkippedWhenNoSegmentIsAdmitted(t *testing.T) {
	var texts []string
	policy := DefaultPolicy()
	policy.RoleScopes = map[Category][]string{CategoryCredential: {RoleTool}}
	det := recordingDetector{name: "rec", category: CategoryCredential, texts: &texts, findings: []Finding{{Category: CategoryCredential, Detector: "rec"}}}
	v := NewEngine([]Detector{det}, policy, "test", nil).CheckSegments(context.Background(), []Segment{
		{Role: RoleSystem, Text: "system-text"}, {Role: RoleUser, Text: "user-text"},
	})
	if len(texts) != 0 {
		t.Errorf("Detect was called %d time(s) with %q, want 0: no segment carries an admitted role", len(texts), texts)
	}
	if v.Blocked || len(v.Findings) != 0 || v.DetectorError != nil {
		t.Errorf("Verdict = %+v, want clean: a skipped detector contributes nothing", v)
	}
}

func TestCheckSegmentsSegmentWithoutAKnownRoleIsSeenByEveryDetector(t *testing.T) {
	// An empty Role (Check's own single-text path) or a role outside the
	// four message roles (a provider role the adapters pass through
	// unchanged) is admitted by every detector: a scope can only exclude
	// text whose role is known, never hide text a caller could not
	// attribute.
	var texts []string
	policy := DefaultPolicy()
	policy.RoleScopes = map[Category][]string{CategoryCredential: {RoleTool}}
	NewEngine([]Detector{recordingDetector{name: "rec", category: CategoryCredential, texts: &texts}}, policy, "test", nil).
		CheckSegments(context.Background(), []Segment{
			{Role: RoleUser, Text: "user-text"},
			{Text: "unscoped-text"},
			{Role: "developer", Text: "developer-text"},
		})
	if len(texts) != 1 || texts[0] != "unscoped-text\ndeveloper-text" {
		t.Errorf("scoped detector saw %q, want exactly the role-less and unknown-role segments %q", texts, "unscoped-text\ndeveloper-text")
	}
}

func TestCheckStillRunsEveryDetectorOnTheWholeText(t *testing.T) {
	var texts []string
	policy := DefaultPolicy()
	policy.RoleScopes = map[Category][]string{CategoryCredential: {RoleTool}}
	v := NewEngine([]Detector{recordingDetector{name: "rec", category: CategoryCredential, texts: &texts,
		findings: []Finding{{Category: CategoryCredential, Detector: "rec", Start: 0, End: 3}}}}, policy, "test", nil).
		Check(context.Background(), "plain text")
	if len(texts) != 1 || texts[0] != "plain text" {
		t.Errorf("Check handed %q to a scoped detector, want the whole text (Check carries no role, so no scope applies)", texts)
	}
	if len(v.Findings) != 1 || v.Findings[0].Role != "" {
		t.Errorf("Findings = %+v, want one finding with an empty Role", v.Findings)
	}
}

func TestNewEngineVersionChangesWithRoleScopesAndIgnoresTheirOrder(t *testing.T) {
	base := NewEngine(nil, DefaultPolicy(), "v1", nil).Version()

	scoped := DefaultPolicy()
	scoped.RoleScopes = map[Category][]string{CategoryPromptInjection: {RoleUser, RoleTool}}
	scopedVersion := NewEngine(nil, scoped, "v1", nil).Version()
	if scopedVersion == base {
		t.Errorf("Version() = %q with a role scope, want it to differ from the unscoped %q: a scope changes what is scanned, so cache entries written under the old policy must miss", scopedVersion, base)
	}
	if !strings.HasPrefix(scopedVersion, "v1#policy-") {
		t.Errorf("Version() = %q, want the existing v1#policy- shape", scopedVersion)
	}

	reordered := DefaultPolicy()
	reordered.RoleScopes = map[Category][]string{CategoryPromptInjection: {RoleTool, RoleUser}}
	if got := NewEngine(nil, reordered, "v1", nil).Version(); got != scopedVersion {
		t.Errorf("Version() = %q for the same scope listed in another order, want %q: the fingerprint must not depend on list order", got, scopedVersion)
	}

	otherCategory := DefaultPolicy()
	otherCategory.RoleScopes = map[Category][]string{CategoryCredential: {RoleUser, RoleTool}}
	if got := NewEngine(nil, otherCategory, "v1", nil).Version(); got == scopedVersion {
		t.Errorf("Version() = %q for a different scoped category, want a different fingerprint", got)
	}
}

func TestVerdictFindingRolesIsDistinctFirstSeenAndSkipsEmpty(t *testing.T) {
	v := Verdict{Findings: []Finding{{Role: RoleTool}, {Role: ""}, {Role: RoleUser}, {Role: RoleTool}}}
	if got := v.FindingRoles(); len(got) != 2 || got[0] != RoleTool || got[1] != RoleUser {
		t.Errorf("FindingRoles() = %v, want [tool user]", got)
	}
	if empty := (Verdict{}).FindingRoles(); empty == nil || len(empty) != 0 {
		t.Errorf("FindingRoles() on an empty verdict = %#v, want a non-nil empty slice (it is passed straight into a log field)", empty)
	}
}

func TestKnownRoleAcceptsTheFourMessageRolesOnly(t *testing.T) {
	for _, r := range []string{RoleSystem, RoleUser, RoleAssistant, RoleTool} {
		if !KnownRole(r) {
			t.Errorf("KnownRole(%q) = false, want true", r)
		}
	}
	for _, r := range []string{"", "tools", "User", "developer", "function"} {
		if KnownRole(r) {
			t.Errorf("KnownRole(%q) = true, want false", r)
		}
	}
}

func TestCheckSegmentsEmptyScopeIsTheSameAsNoScope(t *testing.T) {
	// A nil or empty slice through the Go API is "no entry", never "off":
	// every role is admitted and the policy fingerprint is the unscoped one.
	var texts []string
	policy := DefaultPolicy()
	policy.RoleScopes = map[Category][]string{CategoryCredential: {}}
	e := NewEngine([]Detector{recordingDetector{name: "rec", category: CategoryCredential, texts: &texts}}, policy, "v1", nil)
	e.CheckSegments(context.Background(), fourRoleSegments())
	if len(texts) != 1 || texts[0] != "system-text\nuser-text\nassistant-text\ntool-text" {
		t.Errorf("detector under an empty scope saw %q, want every role joined", texts)
	}
	if got, want := e.Version(), NewEngine(nil, DefaultPolicy(), "v1", nil).Version(); got != want {
		t.Errorf("Version() = %q with an empty scope, want the unscoped %q: an empty entry must hash as no entry", got, want)
	}
}
