package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adminapi"
)

func runStatusCmd(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Status(args, IO{Stdout: &out, Stderr: &errb, Getenv: func(k string) string { return env[k] }})
	return code, out.String(), errb.String()
}

func runSpendCmd(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Spend(args, IO{Stdout: &out, Stderr: &errb, Getenv: func(k string) string { return env[k] }})
	return code, out.String(), errb.String()
}

// readyzServer answers /readyz the way the gateway does: 200 when ready, 503
// otherwise, with the per-model map; /v1/models without a bearer is 401.
func readyzServer(t *testing.T, ready bool, models map[string]bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": ready, "models": models})
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unauthorized", http.StatusUnauthorized) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

var spaceRuns = regexp.MustCompile(`[ \t]+`)

func collapse(s string) string { return spaceRuns.ReplaceAllString(s, " ") }

// checkGolden compares got with testdata/<name>; KELVRAN_CLI_UPDATE_GOLDEN=1
// rewrites it (the adminapi package's own convention).
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("KELVRAN_CLI_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil { //nolint:gosec // G301: a test fixture directory
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // G306: a committed golden file
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: the golden under testdata
	if err != nil {
		t.Fatalf("golden %s missing (%v); run once with KELVRAN_CLI_UPDATE_GOLDEN=1", name, err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
		t.Errorf("golden %s differs:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

// normaliseStatus replaces the per-run values (ports, temp paths) in a
// status --json document so the golden is stable.
func normaliseStatus(t *testing.T, raw, cfgPath string) []byte {
	t.Helper()
	var doc statusDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("status --json is not one document: %v\n%s", err, raw)
	}
	doc.Source = "<source>"
	if doc.Readyz != nil {
		doc.Readyz.URL = "<readyz>"
	}
	for i, n := range doc.Notes {
		doc.Notes[i] = strings.ReplaceAll(n, cfgPath, "<config>")
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func statusFake() *fakeAdmin {
	f := newFakeAdmin()
	f.entries = []adminapi.VirtualKeyListEntry{
		{ID: "team-alpha", BudgetUSD: "100", BudgetResetIntervalSeconds: resetMonthly, BudgetWarnPercent: 0.8, AllowedModels: []string{"gpt-4o"}, ExpiresAt: "2030-01-01T00:00:00Z"},
		{ID: "team-beta", BudgetUSD: "0"},
	}
	f.deployments = []adminapi.DeploymentEntry{
		{Name: "d1", Model: "gpt-4o", UpstreamModel: "gpt-4o", Provider: "openai", Kind: "chat", Healthy: true, Weight: 2, Sticky: true},
		{Name: "d2", Model: "gpt-4o", UpstreamModel: "gpt-4o-mini", Provider: "openai", Healthy: false, Weight: 1, LatencyFactorPercent: 25},
	}
	f.persistPath = "/var/lib/kelvran-gateway/identity.db"
	return f
}

func TestStatusOnlineReadsTheThreeRoutesAndTheDataPlane(t *testing.T) {
	f := statusFake()
	srv := f.serve(t)
	rz := readyzServer(t, false, map[string]bool{"gpt-4o": true, "embed": false})
	code, stdout, stderr := runStatusCmd(t, onlineEnv(), "--admin-url", srv.URL, "--url", rz.URL)
	if code != 1 || !strings.Contains(stderr, "is not ready") {
		t.Fatalf("a not-ready data plane exits 1: exit %d\n%s%s", code, stdout, stderr)
	}
	flat := collapse(stdout)
	for _, want := range []string{
		"kelvran status — online via " + srv.URL,
		"readyz: 503 not ready (" + rz.URL + "/readyz); models without a healthy deployment: embed",
		"listen_addr: 127.0.0.1:8080",
		"deployments: 2 configured, 1 healthy",
		"virtual keys: 2 live, 2 in the served config",
		"admin store: persist_path /var/lib/kelvran-gateway/identity.db",
		"name model upstream_model provider kind healthy weight latency_factor_percent sticky",
		"d1 gpt-4o gpt-4o openai chat yes 2 0 yes",
		"d2 gpt-4o gpt-4o-mini openai chat no 1 25 no",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("missing %q in\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, hashSecret("test-key")) || strings.Contains(stdout, "KeyHash") {
		t.Error("status must never carry a key hash")
	}
	// A ready data plane and no --url both exit 0.
	ready := readyzServer(t, true, map[string]bool{"gpt-4o": true})
	if code, stdout, _ := runStatusCmd(t, onlineEnv(), "--admin-url", srv.URL, "--url", ready.URL); code != 0 || !strings.Contains(collapse(stdout), "readyz: 200 ready (") {
		t.Errorf("ready: exit %d\n%s", code, stdout)
	}
	if code, stdout, _ := runStatusCmd(t, onlineEnv(), "--admin-url", srv.URL); code != 0 || !strings.Contains(stdout, "not probed (pass --url") {
		t.Errorf("no --url: exit %d\n%s", code, stdout)
	}
	// --json: one document, pinned by the golden.
	code, stdout, _ = runStatusCmd(t, onlineEnv(), "--admin-url", srv.URL, "--url", rz.URL, "--json")
	if code != 1 {
		t.Fatalf("--json with a not-ready plane still exits 1: %d", code)
	}
	checkGolden(t, "status_online.golden.json", normaliseStatus(t, stdout, ""))
}

func TestStatusOfflineReadsTheFile(t *testing.T) {
	p := writeKeysFixture(t, t.TempDir(), fixtureText(), 0o644)
	resolved, _ := filepath.EvalSymlinks(p)
	code, stdout, stderr := runStatusCmd(t, nil, "--config", p)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	flat := collapse(stdout)
	for _, want := range []string{
		"kelvran status — offline from " + resolved + "; a running gateway reflects this file only after restart",
		"readyz: not probed",
		"listen_addr: 127.0.0.1:8080",
		"deployments: 1 configured (static view)",
		"virtual keys: 3 in the file",
		"admin store: none (single-user: no admin section)",
		"this config has no `admin:` section (single-user mode)",
		"token_env: " + adminTokenEnvName,
		"name model upstream_model provider kind\n",
		"d1 gpt-4o gpt-4o openai chat",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("missing %q in\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "healthy") || strings.Contains(stdout, hashSecret("alpha")) {
		t.Errorf("offline shows the static columns only and never a hash:\n%s", stdout)
	}
	if strings.Count(stdout, "a running gateway reflects this file only after restart") != 1 {
		t.Errorf("the restart caveat is printed once in the text view:\n%s", stdout)
	}
	// An admin section without a token in reach: the note names the variable.
	team := writeKeysFixture(t, t.TempDir(), fixtureText(`admin:`, `  token_env: "DOCTOR_TEST_ADMIN"`), 0o644)
	if code, stdout, _ := runStatusCmd(t, nil, "--config", team); code != 0 || !strings.Contains(stdout, "export DOCTOR_TEST_ADMIN (the variable admin.token_env names)") || !strings.Contains(stdout, "admin store:   none (keys changed through the admin API are in-memory only)") {
		t.Errorf("team config offline: exit %d\n%s", code, stdout)
	}
	// --url against nothing: reported, exit 1, the rest of the view still printed.
	code, stdout, stderr = runStatusCmd(t, nil, "--config", p, "--url", "http://127.0.0.1:9")
	if code != 1 || !strings.Contains(collapse(stdout), "readyz: unreachable at http://127.0.0.1:9/readyz (") || !strings.Contains(stdout, "virtual keys:") || !strings.Contains(stderr, "is not ready") {
		t.Errorf("unreachable data plane: exit %d\n%s%s", code, stdout, stderr)
	}
	code, stdout, _ = runStatusCmd(t, nil, "--config", p, "--json")
	if code != 0 {
		t.Fatalf("--json: %d", code)
	}
	checkGolden(t, "status_offline.golden.json", normaliseStatus(t, stdout, resolved))
}

func TestStatusUsageAndGuards(t *testing.T) {
	if code, _, stderr := runStatusCmd(t, nil); code != 2 || !strings.Contains(stderr, "and no --config") {
		t.Errorf("neither token nor --config: exit %d %s", code, stderr)
	}
	if code, _, stderr := runStatusCmd(t, nil, "extra"); code != 2 || !strings.Contains(stderr, `unexpected argument "extra"`) {
		t.Errorf("positional: exit %d %s", code, stderr)
	}
	p := writeKeysFixture(t, t.TempDir(), fixtureText(), 0o644)
	if code, _, stderr := runStatusCmd(t, nil, "--config", p, "--url", "http://u:"+sentinel("pw")+"@127.0.0.1:8080"); code != 1 || !strings.Contains(stderr, "must not carry credentials") || strings.Contains(stderr, sentinel("pw")) {
		t.Errorf("--url with userinfo: exit %d %s", code, stderr)
	}
	if code, _, stderr := runStatusCmd(t, onlineEnv(), "--admin-url", "http://127.0.0.1:9"); code != 1 || !strings.Contains(stderr, "admin API unreachable at http://127.0.0.1:9") {
		t.Errorf("unreachable admin API: exit %d %s", code, stderr)
	}
	// The shared warning names the verb that ran, not keys.
	broken := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(broken, []byte("listen_addr: \"x\"\n"), 0o644); err != nil { //nolint:gosec // G306: a fixture
		t.Fatal(err)
	}
	if _, _, stderr := runStatusCmd(t, onlineEnv(), "--config", broken, "--admin-url", "http://127.0.0.1:9"); !strings.Contains(stderr, "kelvran status: warning: --config") || strings.Contains(stderr, "kelvran keys:") {
		t.Errorf("the warning must be prefixed with the running verb: %s", stderr)
	}
}

func TestSpendByKeyOnline(t *testing.T) {
	f := statusFake()
	srv := f.serve(t)
	code, stdout, stderr := runSpendCmd(t, onlineEnv(), "--admin-url", srv.URL)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	flat := collapse(stdout)
	for _, want := range []string{"key spent_usd budget_usd percent_used expires_at", "team-alpha 1.25 100 12.5% 2030-01-01T00:00:00Z", "team-beta 1.25 unlimited 12.5% never"} {
		if !strings.Contains(flat, want) {
			t.Errorf("missing %q in\n%s", want, stdout)
		}
	}
	if !strings.Contains(f.calls[len(f.calls)-1], "?include=spend") {
		t.Errorf("spend must ask for include=spend: %v", f.calls)
	}
	f.spendUnavailable = true
	code, stdout, stderr = runSpendCmd(t, onlineEnv(), "--admin-url", srv.URL)
	if code != 1 || !strings.Contains(collapse(stdout), "team-alpha n/a 100 n/a") || !strings.Contains(stderr, "spend unavailable for 1 key(s): budget backend error") {
		t.Errorf("spend_unavailable: exit %d\n%s%s", code, stdout, stderr)
	}
	code, stdout, _ = runSpendCmd(t, onlineEnv(), "--admin-url", srv.URL, "--json", "--by", "key")
	var got []adminapi.VirtualKeyListEntry
	if code != 1 || json.Unmarshal([]byte(stdout), &got) != nil || len(got) != 2 || !got[0].SpendUnavailable {
		t.Errorf("--json: exit %d\n%s", code, stdout)
	}
}

func TestSpendRefusals(t *testing.T) {
	for _, tc := range []struct {
		args []string
		env  map[string]string
		want int
		msg  string
	}{
		{[]string{"--by", "model"}, onlineEnv(), 2, "needs the spend ledger (plan item 13d"},
		{[]string{"--by", "tool"}, onlineEnv(), 2, "13d"},
		{[]string{"--by", "bogus"}, onlineEnv(), 2, `--by "bogus" is not one of`},
		{nil, nil, 2, "and no --config"},
		{[]string{"extra"}, nil, 2, `unexpected argument "extra"`},
		{[]string{"--admin-url", "http://127.0.0.1:9"}, onlineEnv(), 1, "admin API unreachable"},
	} {
		code, stdout, stderr := runSpendCmd(t, tc.env, tc.args...)
		if code != tc.want || !strings.Contains(stderr, tc.msg) || stdout != "" {
			t.Errorf("%v: exit %d (want %d), stdout %q, stderr %s", tc.args, code, tc.want, stdout, stderr)
		}
	}
	// Offline fails closed: spend lives only in the budget store.
	p := writeKeysFixture(t, t.TempDir(), fixtureText(), 0o644)
	if code, stdout, stderr := runSpendCmd(t, nil, "--config", p); code != 1 || stdout != "" || !strings.Contains(stderr, "this config has no `admin:` section (single-user mode)") {
		t.Errorf("offline spend: exit %d, stdout %q, stderr %s", code, stdout, stderr)
	}
	team := writeKeysFixture(t, t.TempDir(), fixtureText(`admin:`, `  token_env: "DOCTOR_TEST_ADMIN"`), 0o644)
	if code, _, stderr := runSpendCmd(t, nil, "--config", team); code != 1 || !strings.Contains(stderr, "export DOCTOR_TEST_ADMIN") {
		t.Errorf("offline spend with an admin section: exit %d, stderr %s", code, stderr)
	}
}

// A /readyz that is not the gateway's — a proxy page answering 200, or a
// body disagreeing with its status — must never read as ready.
func TestProbeReadyzFailsClosed(t *testing.T) {
	p := writeKeysFixture(t, t.TempDir(), fixtureText(), 0o644)
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>ok</html>")) }))
	t.Cleanup(html.Close)
	code, stdout, stderr := runStatusCmd(t, nil, "--config", p, "--url", html.URL)
	if code != 1 || !strings.Contains(collapse(stdout), "readyz: 200 not ready (") || !strings.Contains(stdout, "not the gateway's /readyz JSON") || !strings.Contains(stderr, "is not ready") {
		t.Errorf("a non-JSON 200 must fail closed: exit %d\n%s%s", code, stdout, stderr)
	}
	lying := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"ready":true,"models":{}}`))
	}))
	t.Cleanup(lying.Close)
	if code, stdout, _ := runStatusCmd(t, nil, "--config", p, "--url", lying.URL); code != 1 || !strings.Contains(collapse(stdout), "readyz: 503 not ready (") {
		t.Errorf("a 503 with ready:true is not ready: exit %d\n%s", code, stdout)
	}
	code, stdout, _ = runStatusCmd(t, nil, "--config", p, "--url", html.URL, "--json")
	var doc statusDoc
	if code != 1 || json.Unmarshal([]byte(stdout), &doc) != nil || doc.Readyz == nil || doc.Readyz.Ready || doc.Readyz.Status != 200 || doc.Readyz.Error == "" {
		t.Errorf("--json carries status 200, ready false and the error: %s", stdout)
	}
}

// A file may carry a pasted secret where a variable name or a host belongs;
// status and spend must not print it (doctor flags both as errors).
func TestStatusNeverEchoesASecretFromTheFile(t *testing.T) {
	pasted := "sk-ant-" + sentinel("token-env")
	storeURL := "redis" + ":" + "//:" + sentinel("redis-pw") + "@127.0.0.1:6379"
	p := writeKeysFixture(t, t.TempDir(), fixtureText(`admin:`, `  token_env: "`+pasted+`"`, `  redis_addr: "`+storeURL+`"`), 0o644)
	wantStore := "redis_addr " + "redis" + ":" + "//:" + "xxxxx@127.0.0.1:6379"
	for _, args := range [][]string{{"--config", p}, {"--config", p, "--json"}} {
		code, stdout, stderr := runStatusCmd(t, nil, args...)
		if code != 0 || strings.Contains(stdout+stderr, pasted) || strings.Contains(stdout+stderr, sentinel("redis-pw")) {
			t.Errorf("%v: a pasted value leaked: exit %d\n%s%s", args, code, stdout, stderr)
		}
		if !strings.Contains(stdout, "admin.token_env does not name an environment variable (value not shown)") || !strings.Contains(stdout, wantStore) {
			t.Errorf("%v: the remedy line and the store must be redacted forms:\n%s", args, stdout)
		}
	}
	if code, stdout, stderr := runSpendCmd(t, nil, "--config", p); code != 1 || strings.Contains(stdout+stderr, pasted) || !strings.Contains(stderr, "value not shown") {
		t.Errorf("spend offline: exit %d\n%s%s", code, stdout, stderr)
	}
}

// A body-read error can quote what the server sent (a chunked trailer that is
// the bearer with no colon); the unreachable branch's fallthrough must redact
// it like every other path.
func TestStatusRedactsTheBearerFromABodyReadError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nTrailer: X-Note\r\n\r\n0\r\n"+fakeToken()+"\r\n\r\n")
				_ = c.Close()
			}(c)
		}
	}()
	for _, run := range []func(*testing.T, map[string]string, ...string) (int, string, string){runStatusCmd, runSpendCmd} {
		code, stdout, stderr := run(t, onlineEnv(), "--admin-url", "http://"+ln.Addr().String())
		if code != 1 || stdout != "" || strings.Contains(stderr, fakeToken()) || !strings.Contains(stderr, "***") {
			t.Errorf("exit %d\nstdout %q\nstderr %q", code, stdout, stderr)
		}
	}
}
