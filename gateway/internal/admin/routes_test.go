package admin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/admin/auditstore"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// The tests in this file execute pprof's CPU profile and trace handlers
// (with seconds=1); none of them may call t.Parallel, or two profiles could
// overlap and net/http/pprof would answer 500 "cpu profiling already in use".

// fillPath substitutes a concrete value for every Go 1.22 wildcard a table
// row uses, so a request can be built from the pattern alone.
func fillPath(pattern string) string {
	return strings.NewReplacer("{name}", "x", "{id}", "x", "{label}", "x", "{version}", "1").Replace(pattern)
}

// probeQuery is appended to the request URL of the two pprof handlers whose
// default run time is 30 s (Profile) and 1 s (Trace): without seconds=1 the
// tier matrix below would take over a minute instead of about two seconds.
var probeQuery = map[string]string{
	"GET /admin/debug/pprof/profile": "seconds=1",
	"GET /admin/debug/pprof/trace":   "seconds=1",
}

func probePath(rt Route) string {
	p := fillPath(rt.Path)
	if q, ok := probeQuery[rt.Pattern()]; ok {
		p += "?" + q
	}
	return p
}

func openTestAuditStore(t *testing.T) *auditstore.Store {
	t.Helper()
	store, err := auditstore.Open(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("auditstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// routeCondition is one of the four registration conditions Handler can run
// under: the audit route needs a store, the thirteen pprof routes need
// admin.enable_pprof.
type routeCondition struct {
	name  string
	cfg   *controlplane.Config
	store *auditstore.Store
	want  int
}

func routeConditions(t *testing.T) []routeCondition {
	t.Helper()
	return []routeCondition{
		{"no audit store, pprof off", testConfig(), nil, 20},
		{"audit store, pprof off", testConfig(), openTestAuditStore(t), 21},
		{"no audit store, pprof on", pprofEnabledConfig(), nil, 33},
		{"audit store, pprof on", pprofEnabledConfig(), openTestAuditStore(t), 34},
	}
}

func patternSet(routes []Route) map[string]bool {
	set := make(map[string]bool, len(routes))
	for _, rt := range routes {
		set[rt.Pattern()] = true
	}
	return set
}

// TestAllRoutesIsTheDocumentsRouteSet pins the table's shape: 34 rows over 29
// distinct paths, every pattern and operationId unique, exactly one subtree.
func TestAllRoutesIsTheDocumentsRouteSet(t *testing.T) {
	all := AllRoutes()
	if len(all) != 34 {
		t.Fatalf("AllRoutes() has %d rows, want 34", len(all))
	}
	patterns, ids, paths, subtrees := map[string]bool{}, map[string]bool{}, map[string]bool{}, 0
	for _, rt := range all {
		if patterns[rt.Pattern()] {
			t.Errorf("duplicate pattern %q", rt.Pattern())
		}
		if ids[rt.OperationID] {
			t.Errorf("duplicate operationId %q", rt.OperationID)
		}
		patterns[rt.Pattern()], ids[rt.OperationID], paths[rt.Path] = true, true, true
		if rt.Subtree() {
			subtrees++
		}
		if len(rt.Tier.Tiers()) == 0 || rt.Tier.Tiers()[0] != "admin" {
			t.Errorf("%s: Tier %q admits %v, want admin first", rt.Pattern(), rt.Tier, rt.Tier.Tiers())
		}
	}
	if len(paths) != 29 {
		t.Errorf("AllRoutes() spans %d distinct paths, want 29", len(paths))
	}
	if subtrees != 1 {
		t.Errorf("AllRoutes() has %d subtree patterns, want exactly one (GET /admin/debug/pprof/)", subtrees)
	}
}

// TestRouteTableTiersFollowTheRBACBaseline pins every row's tier to the
// baseline the RBAC RFCs set (docs/rfcs/2026-09-09-gateway-admin-viewer-role.md,
// docs/rfcs/2026-09-20-gateway-admin-rbac-risk-tiering.md): every write is
// admin-only except exactly the three reversible, single-target operator
// routes; every read is admin-or-viewer except the two spend views, which add
// cost_viewer (the ledger view only with key=, decided in its handler — item
// 13d, G27); every pprof row is admin-only. The tier matrix cannot catch a
// table edit on its own because its oracle is the table.
func TestRouteTableTiersFollowTheRBACBaseline(t *testing.T) {
	operatorRoutes := map[string]bool{"rotateVirtualKey": true, "updateDeploymentWeight": true, "eraseCacheEntry": true}
	for _, rt := range AllRoutes() {
		want := TierAdminOrViewer
		switch {
		case strings.HasPrefix(rt.Path, "/admin/debug/pprof/"):
			want = TierAdmin
		case rt.Method != http.MethodGet && operatorRoutes[rt.OperationID]:
			want = TierAdminOrOperator
		case rt.Method != http.MethodGet:
			want = TierAdmin
		case rt.OperationID == "getVirtualKeySpend" || rt.OperationID == "getSpend":
			want = TierAdminViewerCostViewer
		}
		if rt.Tier != want {
			t.Errorf("%s (%s): tier %q, want %q under the RBAC baseline", rt.Pattern(), rt.OperationID, rt.Tier, want)
		}
		if rt.Condition == IfEnablePprof && !strings.HasPrefix(rt.Path, "/admin/debug/pprof/") || rt.Condition != IfEnablePprof && strings.HasPrefix(rt.Path, "/admin/debug/pprof/") {
			t.Errorf("%s: condition %q does not match its path", rt.Pattern(), rt.Condition)
		}
	}
	for _, id := range []string{"rotateVirtualKey", "updateDeploymentWeight", "eraseCacheEntry", "getVirtualKeySpend", "getSpend", "eraseSpendSession"} {
		found := false
		for _, rt := range AllRoutes() {
			found = found || rt.OperationID == id
		}
		if !found {
			t.Errorf("baseline names %q, which the table no longer has", id)
		}
	}
}

// TestRoutesMatchRegisteredPatternsForEveryCondition proves, for all four
// conditions, that Routes names exactly the patterns the mux resolves: every
// row of AllRoutes resolves to its own pattern iff Routes includes it, and to
// nothing otherwise. Resolution only -- no handler executes.
func TestRoutesMatchRegisteredPatternsForEveryCondition(t *testing.T) {
	for _, c := range routeConditions(t) {
		t.Run(c.name, func(t *testing.T) {
			routes := Routes(c.cfg, c.store != nil)
			if len(routes) != c.want {
				t.Errorf("Routes() has %d rows, want %d", len(routes), c.want)
			}
			registered := patternSet(routes)
			mux, ok := Handler(c.cfg, newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger(), c.store).(*http.ServeMux)
			if !ok {
				t.Fatal("Handler() no longer returns the bare *http.ServeMux")
			}
			var missing, extra []string
			for _, rt := range AllRoutes() {
				_, pattern := mux.Handler(httptest.NewRequest(rt.Method, fillPath(rt.Path), nil))
				switch {
				case registered[rt.Pattern()] && pattern != rt.Pattern():
					missing = append(missing, rt.Pattern()+" (mux resolved "+pattern+")")
				case !registered[rt.Pattern()] && pattern != "":
					extra = append(extra, rt.Pattern()+" (mux resolved "+pattern+")")
				}
			}
			sort.Strings(missing)
			sort.Strings(extra)
			for _, m := range missing {
				t.Errorf("in Routes, not resolved by the mux: + %s", m)
			}
			for _, e := range extra {
				t.Errorf("resolved by the mux, not in Routes: - %s", e)
			}
		})
	}
}

// TestRouteHandlersCoverEveryRoute: the handler map and the route table name
// the same operations under every condition, in both directions.
func TestRouteHandlersCoverEveryRoute(t *testing.T) {
	for _, c := range routeConditions(t) {
		t.Run(c.name, func(t *testing.T) {
			audit := auditLogger{logger: discardLogger(), enabled: c.cfg.Admin.EnableAuditLog, store: c.store}
			handlers := routeHandlers(c.cfg, newTestPipeline(t), audit, c.store)
			want := map[string]bool{}
			for _, rt := range Routes(c.cfg, c.store != nil) {
				want[rt.OperationID] = true
				if handlers[rt.OperationID] == nil {
					t.Errorf("route %s (%s) has no handler", rt.Pattern(), rt.OperationID)
				}
			}
			for id := range handlers {
				if !want[id] {
					t.Errorf("handler %q has no route under this condition", id)
				}
			}
		})
	}
}

// TestHandlerTierPerRouteFromTable drives every row with every credential:
// a credential is admitted (anything but 401) iff the row's Tier names it;
// the two 401 bodies are the wrappers' own. The rejected legs never reach a
// handler; the admitted ones do, with seconds=1 on the two slow pprof routes.
func TestHandlerTierPerRouteFromTable(t *testing.T) {
	creds := Credentials{
		Admin:      fakeAdminCredential(),
		Viewer:     "a-viewer-credential-for-tests",
		CostViewer: "a-cost-viewer-credential-for-tests",
		Operator:   "an-operator-credential-for-tests",
	}
	presented := map[string]string{
		"admin": creds.Admin, "viewer": creds.Viewer, "cost_viewer": creds.CostViewer, "operator": creds.Operator,
		"wrong": "not-any-configured-credential", "none": "",
	}
	h := Handler(pprofEnabledConfig(), newTestPipeline(t), creds, discardLogger(), openTestAuditStore(t))
	for _, rt := range AllRoutes() {
		admitted := map[string]bool{}
		for _, tier := range rt.Tier.Tiers() {
			admitted[tier] = true
		}
		for _, tier := range []string{"admin", "viewer", "cost_viewer", "operator", "wrong", "none"} {
			rec := doRequest(t, h, rt.Method, probePath(rt), presented[tier], "")
			switch {
			case admitted[tier] && rec.Code == http.StatusUnauthorized:
				t.Errorf("%s with the %s credential: 401, want admitted (tier %s)", rt.Pattern(), tier, rt.Tier)
			case !admitted[tier] && rec.Code != http.StatusUnauthorized:
				t.Errorf("%s with the %s credential: %d, want 401 (tier %s)", rt.Pattern(), tier, rec.Code, rt.Tier)
			case !admitted[tier]:
				want := "invalid admin token\n"
				if tier == "none" {
					want = "missing or malformed Authorization header\n"
				}
				if rec.Body.String() != want {
					t.Errorf("%s with the %s credential: body %q, want %q", rt.Pattern(), tier, rec.Body.String(), want)
				}
			}
		}
	}
}

// TestAdminServeMuxSemantics pins the Go 1.22+ ServeMux behaviours the
// OpenAPI document states in prose because its vocabulary cannot carry them:
// every GET also answers HEAD (empty body on the real server), a wrong
// method on a known path is net/http's plain-text 405 before any bearer
// check, /admin/debug/pprof/ is a subtree, and its slash-less form redirects
// with 307 under the pinned toolchain.
func TestAdminServeMuxSemantics(t *testing.T) {
	store := openTestAuditStore(t)
	h := Handler(pprofEnabledConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger(), store)
	mux := h.(*http.ServeMux)

	for _, rt := range AllRoutes() {
		if rt.Method != http.MethodGet {
			continue
		}
		if _, pattern := mux.Handler(httptest.NewRequest(http.MethodHead, fillPath(rt.Path), nil)); pattern != rt.Pattern() {
			t.Errorf("HEAD %s resolved to %q, want the GET pattern %q", fillPath(rt.Path), pattern, rt.Pattern())
		}
		get := doRequest(t, h, http.MethodGet, probePath(rt), fakeAdminCredential(), "")
		head := doRequest(t, h, http.MethodHead, probePath(rt), fakeAdminCredential(), "")
		if head.Code != get.Code {
			t.Errorf("HEAD %s: %d, want the GET's %d", probePath(rt), head.Code, get.Code)
		}
	}
	// The recorder keeps HEAD bodies; the real server strips them -- proven once.
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req, _ := http.NewRequest(method, srv.URL+"/admin/config", nil)
		req.Header.Set("Authorization", "Bearer "+fakeAdminCredential())
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s /admin/config: %v", method, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s /admin/config: %d, want 200", method, resp.StatusCode)
		}
		if method == http.MethodHead && len(body) != 0 {
			t.Errorf("HEAD /admin/config carried %d body bytes over the real server, want 0", len(body))
		}
		if method == http.MethodGet && len(body) == 0 {
			t.Error("GET /admin/config returned no body")
		}
	}

	// (b) wrong method on a known path: the mux answers before any bearer check.
	if _, pattern := mux.Handler(httptest.NewRequest(http.MethodPatch, "/admin/config", nil)); pattern != "" {
		t.Errorf("PATCH /admin/config resolved to %q, want no pattern", pattern)
	}
	rec := doRequest(t, h, http.MethodPatch, "/admin/config", "", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Body.String() != "Method Not Allowed\n" || rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("PATCH /admin/config without credentials: %d %q %q, want 405, net/http's plain-text body", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "HEAD") {
		t.Errorf("PATCH /admin/config: Allow %q, want GET and HEAD", allow)
	}

	// (c) the one subtree, and its slash-less redirect.
	if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, "/admin/debug/pprof/not-a-profile", nil)); pattern != "GET /admin/debug/pprof/" {
		t.Errorf("GET /admin/debug/pprof/not-a-profile resolved to %q, want the subtree pattern", pattern)
	}
	rec = doRequest(t, h, http.MethodGet, "/admin/debug/pprof", fakeAdminCredential(), "")
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != "/admin/debug/pprof/" {
		t.Errorf("GET /admin/debug/pprof (no slash): %d Location %q, want 307 to /admin/debug/pprof/ (the pinned toolchain's trailing-slash redirect)", rec.Code, rec.Header().Get("Location"))
	}

	// (d) pprof off: the subtree does not exist.
	off := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)
	if rec := doRequest(t, off, http.MethodGet, "/admin/debug/pprof/", fakeAdminCredential(), ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET /admin/debug/pprof/ with pprof off: %d, want 404", rec.Code)
	}
}

// TestHandlerRegistersOnlyThroughTheRouteTable is the AST guard: Handler
// registers through exactly one mux.Handle(rt.Pattern(), ...) call and no
// HandleFunc, and no non-test file but routes.go holds an admin pattern
// literal -- a hand-added registration fails here before the contract test.
func TestHandlerRegistersOnlyThroughTheRouteTable(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "admin.go", nil, 0)
	if err != nil {
		t.Fatalf("parse admin.go: %v", err)
	}
	var handlerDecl *ast.FuncDecl
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == "Handler" && fd.Recv == nil {
			handlerDecl = fd
		}
	}
	if handlerDecl == nil {
		t.Fatal("admin.go has no top-level func Handler")
	}
	handleCalls, handleFuncCalls := 0, 0
	ast.Inspect(handlerDecl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "mux" {
			return true
		}
		switch sel.Sel.Name {
		case "HandleFunc":
			handleFuncCalls++
		case "Handle":
			handleCalls++
			arg, ok := call.Args[0].(*ast.CallExpr)
			argSel, selOK := arg.Fun.(*ast.SelectorExpr)
			if !ok || !selOK || argSel.Sel.Name != "Pattern" {
				t.Errorf("mux.Handle at %s: first argument is not rt.Pattern()", fset.Position(call.Pos()))
			}
		}
		return true
	})
	if handleCalls != 1 || handleFuncCalls != 0 {
		t.Errorf("Handler has %d mux.Handle and %d mux.HandleFunc calls, want exactly 1 and 0", handleCalls, handleFuncCalls)
	}

	literal := regexp.MustCompile(`^(GET|HEAD|POST|PUT|PATCH|DELETE) /admin`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "routes.go" {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if ok && lit.Kind == token.STRING && literal.MatchString(strings.Trim(lit.Value, "`\"")) {
				t.Errorf("%s: admin pattern literal %s outside routes.go", fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
}
