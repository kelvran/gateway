package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/admin"
)

// The reference pages' route tables are checked against the Go route tables
// (plan item 14 slice S4): a check, not a generator, because the pages carry
// prose the tables do not. The repository's promoted gotcha is doc-vs-code
// staleness, so a route added to the code without its row here is red.

const (
	dataPlaneReferencePage = "../../../docs/reference/data-plane-api.md"
	adminReferencePage     = "../../../docs/reference/admin-api.md"
)

// markdownTable returns the data rows (cells trimmed, backticks stripped) of
// the first table in page whose header row equals header, or fails.
func markdownTable(t *testing.T, name string, page []byte, header string) [][]string {
	t.Helper()
	lines := strings.Split(string(page), "\n")
	for i, line := range lines {
		if normalizeRow(line) != normalizeRow(header) {
			continue
		}
		var rows [][]string
		for _, l := range lines[i+2:] {
			if !strings.HasPrefix(strings.TrimSpace(l), "|") {
				break
			}
			cells := strings.Split(strings.Trim(strings.TrimSpace(l), "|"), "|")
			for j := range cells {
				cells[j] = strings.TrimSpace(strings.ReplaceAll(cells[j], "`", ""))
			}
			rows = append(rows, cells)
		}
		return rows
	}
	t.Fatalf("%s: no table with header %q", name, header)
	return nil
}

var spaceRun = regexp.MustCompile(`\s+`)

func normalizeRow(s string) string { return spaceRun.ReplaceAllString(strings.TrimSpace(s), " ") }

// tierCells maps the admin page's Tiers cell (spaces removed, lower-cased) to
// the route table's tier.
var tierCells = map[string]admin.Tier{
	"admin":                   admin.TierAdmin,
	"admin,viewer":            admin.TierAdminOrViewer,
	"admin,operator":          admin.TierAdminOrOperator,
	"admin,viewer,costviewer": admin.TierAdminViewerCostViewer,
}

// TestReferenceDocsRouteTablesMatchTheCode: the data-plane page's route table
// and the admin page's route index together name exactly the routes the Go
// tables register (one row per METHOD path; a cell "GET, POST" is two rows),
// every admin row's Tiers cell is the row's tier, and the admin page's pprof
// table names exactly the pprof routes.
func TestReferenceDocsRouteTablesMatchTheCode(t *testing.T) {
	dataPlanePage, err := os.ReadFile(dataPlaneReferencePage)
	if err != nil {
		t.Fatal(err)
	}
	adminPage, err := os.ReadFile(adminReferencePage)
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]string{} // METHOD path -> page: row
	for _, row := range markdownTable(t, "data-plane-api.md", dataPlanePage, "| Method | Path | Auth | Success | Wrong method |") {
		for _, m := range strings.Split(row[0], ",") {
			documented[strings.TrimSpace(m)+" "+row[1]] = "data-plane-api.md: " + row[1]
		}
	}
	docTier := map[string]string{}
	for _, row := range markdownTable(t, "admin-api.md", adminPage, "| Method | Path | Tiers | Success | Errors | Audit event |") {
		for _, m := range strings.Split(row[0], ",") {
			key := strings.TrimSpace(m) + " " + row[1]
			documented[key] = "admin-api.md route index: " + row[1]
			docTier[key] = row[2]
		}
	}
	pprofTable := map[string]bool{}
	for _, row := range markdownTable(t, "admin-api.md", adminPage, "| Method | Path | Handler |") {
		for _, m := range strings.Split(row[0], ",") {
			pprofTable[strings.TrimSpace(m)+" "+row[1]] = true
		}
	}
	registered := map[string]admin.Tier{}
	for _, rt := range dataPlaneRoutes {
		registered[rt.Method+" "+rt.Path] = ""
	}
	for _, rt := range admin.AllRoutes() {
		registered[rt.Pattern()] = rt.Tier
	}
	var missing, extra, wrongTier []string
	for key, tier := range registered {
		isPprof := strings.Contains(key, " /admin/debug/pprof/")
		if isPprof && !pprofTable[key] {
			missing = append(missing, key+" (pprof table)")
		}
		if _, ok := documented[key]; !ok {
			missing = append(missing, key)
			continue
		}
		cell, ok := docTier[key]
		if !ok {
			continue
		}
		got, known := tierCells[strings.ToLower(strings.ReplaceAll(cell, " ", ""))]
		switch {
		case !known:
			wrongTier = append(wrongTier, key+": Tiers cell "+cell+" is not one of the four tier spellings")
		case got != tier:
			wrongTier = append(wrongTier, key+": Tiers cell "+cell+" is "+string(got)+", the route table says "+string(tier))
		}
	}
	for key, where := range documented {
		if _, ok := registered[key]; !ok {
			extra = append(extra, key+" ("+where+")")
		}
	}
	for key := range pprofTable {
		if _, ok := registered[key]; !ok || !strings.Contains(key, " /admin/debug/pprof/") {
			extra = append(extra, key+" (admin-api.md pprof table)")
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	sort.Strings(wrongTier)
	for _, m := range missing {
		t.Errorf("registered, not in the reference tables: + %s", m)
	}
	for _, e := range extra {
		t.Errorf("in a reference table, not registered: - %s", e)
	}
	for _, w := range wrongTier {
		t.Error(w)
	}
	if want := len(dataPlaneRoutes) + len(admin.AllRoutes()); len(registered) != want || want < 30 {
		t.Errorf("%d registered routes compared, want %d distinct (a duplicate pattern or a lost table)", len(registered), want)
	}
}
