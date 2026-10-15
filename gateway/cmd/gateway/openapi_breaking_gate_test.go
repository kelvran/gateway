package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The OpenAPI breaking gate (round-5 item 9, 2026-10-15): the contract tests
// in this package pin the document to the code, so a route deleted from both
// the route table and the document leaves every test green. The gate compares
// the document with the one shipped at the previous gateway/v* tag (oasdiff
// `breaking`, exit 1 on ERR) in CI's proto job and in the release build job.
// This test pins the gate's own inputs: the severity file that raises the
// VERSIONING § 2 surfaces oasdiff rates INFO/WARN by default, the waiver file
// that is empty unless a **BREAKING** entry is in flight, and the wiring.
const (
	breakingGateScript   = "../../../scripts/openapi-breaking.sh"
	breakingSeverityFile = "../../openapi/oasdiff-severity.txt"
	breakingWaiverFile   = "../../openapi/oasdiff-err-ignore.txt"
	unreleasedChangelog  = "../../changelog/unreleased.md"
	changelogDir         = "../../changelog"
	repoRoot             = "../../.."
	ciWorkflow           = "../../../.github/workflows/ci.yml"
	releaseWorkflow      = "../../../.github/workflows/release.yml"
)

// breakingSeverityPolicy is the § 2 surface oasdiff v1.33.0 does not rate ERR
// on its own: a non-success status removed (INFO), an operationId removed or
// renamed (INFO), an optional response header removed (INFO), a response enum
// value removed (INFO; the `type`/`code` vocabularies), an optional response
// property removed (INFO), a request parameter removed (WARN), a request
// member removed (WARN), a security requirement added to an existing
// operation (INFO).
var breakingSeverityPolicy = []string{
	"api-operation-id-removed",
	"api-security-added",
	"optional-response-header-removed",
	"request-parameter-removed",
	"request-property-removed",
	"response-non-success-status-removed",
	"response-optional-property-removed",
	"response-property-enum-value-removed",
}

var (
	breakingMarker  = regexp.MustCompile(`(?i)\*\*breaking\*\*`)
	changelogEntry  = regexp.MustCompile(`(?m)^\s*[-*] `) // preflight check 2's definition of "holds entries"
	datedChangelog  = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)\.md$`)
	oasdiffConfigRE = regexp.MustCompile(`^\.?oasdiff\.`)
)

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: fixed repository paths above
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(b)
}

// TestOpenAPIBreakingGateSeverityFile: exactly the policy above, one `<id> err`
// per line, no comments and no blank lines (oasdiff refuses both with exit 106).
func TestOpenAPIBreakingGateSeverityFile(t *testing.T) {
	lines := strings.Split(strings.TrimRight(readRepoFile(t, breakingSeverityFile), "\n"), "\n")
	var ids []string
	for i, l := range lines {
		fields := strings.Fields(l)
		if len(fields) != 2 || fields[1] != "err" || strings.HasPrefix(l, "#") || strings.TrimSpace(l) == "" {
			t.Fatalf("line %d %q: want `<check-id> err` (oasdiff accepts no comments or blank lines)", i+1, l)
		}
		ids = append(ids, fields[0])
	}
	sort.Strings(ids)
	want := append([]string(nil), breakingSeverityPolicy...)
	sort.Strings(want)
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("severity file raises %v, want %v", ids, want)
	}
}

// newestDatedChangelog returns the highest X.Y.Z.md under gateway/changelog.
func newestDatedChangelog(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(changelogDir)
	if err != nil {
		t.Fatal(err)
	}
	var best string
	var bestKey [3]int
	for _, e := range entries {
		m := datedChangelog.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		var key [3]int
		for i := range key {
			key[i], _ = strconv.Atoi(m[i+1])
		}
		if best == "" || key[0] > bestKey[0] || (key[0] == bestKey[0] && (key[1] > bestKey[1] || (key[1] == bestKey[1] && key[2] > bestKey[2]))) {
			best, bestKey = e.Name(), key
		}
	}
	if best == "" {
		t.Fatalf("no dated changelog under %s", changelogDir)
	}
	return filepath.Join(changelogDir, best)
}

// TestOpenAPIBreakingGateWaiverIsEmptyUnlessABreakIsInFlight: the ignore file
// waives sanctioned breaks, so it may hold lines only while a **BREAKING**
// entry is in flight. Where that entry lives follows RELEASE.md: in
// unreleased.md while the line is open; after the release-prep commit moves
// the entries into the dated file and resets unreleased.md (preflight check 2
// requires the reset), in the newest gateway/changelog/X.Y.Z.md -- the same
// file preflight check 8 reads at the tag. The lines leave in the post-tag
// follow-up commit (RELEASE.md gateway step 7). A new entry landing in
// unreleased.md between the prep commit and that follow-up flips the source
// back and fails this test while the waiver is legitimate -- the accepted
// trade-off (a stale marker in the last dated file must not sanction future
// waivers); RELEASE.md pushes the tag right after the prep commit, so the
// window is small.
func TestOpenAPIBreakingGateWaiverIsEmptyUnlessABreakIsInFlight(t *testing.T) {
	waiver := strings.TrimSpace(readRepoFile(t, breakingWaiverFile))
	if waiver == "" {
		return
	}
	unreleased := readRepoFile(t, unreleasedChangelog)
	source := unreleasedChangelog
	if !changelogEntry.MatchString(unreleased) {
		source = newestDatedChangelog(t)
	}
	if !breakingMarker.MatchString(readRepoFile(t, source)) {
		t.Errorf("gateway/openapi/oasdiff-err-ignore.txt holds %d line(s) but %s has no **BREAKING** entry; a waiver is only for a sanctioned break in flight, and its lines leave in the post-tag follow-up commit (RELEASE.md gateway step 7)", len(strings.Split(waiver, "\n")), filepath.Base(source))
	}
}

// TestOpenAPIBreakingGateIsWired: the script exists, is executable, and its
// code lines carry the pin, the flags, the policy files, the config refusal
// and the release-tag base rule; both workflows run it (the proto job on every
// push and pull request, the release build job at the tag); and no oasdiff
// config file sits at the repository root, where the tool would read it.
func TestOpenAPIBreakingGateIsWired(t *testing.T) {
	info, err := os.Stat(breakingGateScript)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("%s is not executable", breakingGateScript)
	}
	// Anchored on the script's code lines, not its comments: the header
	// describes the same flags in prose, so a substring match would stay
	// green with the command line itself weakened.
	script := readRepoFile(t, breakingGateScript)
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)^doc=gateway/openapi/kelvran-gateway\.openapi\.yaml$`),
		regexp.MustCompile(`(?m)^severity=gateway/openapi/oasdiff-severity\.txt$`),
		regexp.MustCompile(`(?m)^waiver=gateway/openapi/oasdiff-err-ignore\.txt$`),
		regexp.MustCompile(`(?m)^oasdiff_module=github\.com/oasdiff/oasdiff@v1\.33\.0$`),
		regexp.MustCompile(`(?m)^unset OASDIFF_CONFIG$`),
		regexp.MustCompile(`(?m)^for stray in \.oasdiff\.\* oasdiff\.\*; do$`),
		regexp.MustCompile(`(?m)^\s*base=\$\(git -c versionsort\.suffix=- tag --list 'gateway/v\*' --sort=-v:refname --no-contains HEAD \| grep -E "\$release_tag" \| head -1 \|\| true\)$`),
		regexp.MustCompile(`(?m)^git rev-parse --verify -q "\$base\^\{commit\}" >/dev/null \|\| fail `),
		regexp.MustCompile(`(?m)^trap 'rm -rf "\$bin_dir"' EXIT$`),
		regexp.MustCompile(`(?m)^GOBIN="\$bin_dir" go install "\$oasdiff_module"$`),
		regexp.MustCompile(`(?m)^args=\(breaking --fail-on ERR --allow-external-refs=false --severity-levels "\$severity"\)$`),
		regexp.MustCompile(`(?m)^\s*args\+=\(--err-ignore "\$waiver"\)$`),
	} {
		if !want.MatchString(script) {
			t.Errorf("scripts/openapi-breaking.sh has no line matching %s", want)
		}
	}
	// The run line itself, not a mention: release.yml's step also names the
	// script in its hashFiles guard and its comment.
	runsGate := regexp.MustCompile(`(?m)^\s*run: scripts/openapi-breaking\.sh$`)
	for name, path := range map[string]string{"ci.yml (proto job)": ciWorkflow, "release.yml (build job)": releaseWorkflow} {
		if !runsGate.MatchString(readRepoFile(t, path)) {
			t.Errorf("%s has no `run: scripts/openapi-breaking.sh` step", name)
		}
	}
	entries, err := os.ReadDir(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if oasdiffConfigRE.MatchString(e.Name()) {
			t.Errorf("%s at the repository root would reconfigure oasdiff behind the gate's flags", e.Name())
		}
	}
}
