package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// cfgOpts shapes a small config for the doctor tests; every credential
// variable is named through a parameter so no literal `name: "value"` pair
// sits in the source.
type cfgOpts struct {
	listen           string
	keyEnvName       string // deployment api_key_env (openai)
	keyFile          string // deployment api_key_file instead of the env var
	insecureHTTP     bool
	insecureURL      string // overrides the http base_url when insecureHTTP is set
	rawBaseURL       string // base_url written verbatim, WITHOUT allow_insecure_http
	adminTokenEnv    string // "" = no admin section
	adminPersist     string
	adminAudit       string
	adminBackup      string
	deploymentName   string // "" = d1
	propagationRedis string // config_propagation.redis_addr, deliberately without signing_secret_env
	telemetry        string // "" = section absent; "-" = present but invalid name
	noPrice          bool
	provider         string // "" = openai; "anthropic" switches provider and base_url
	acceptLossy      bool   // accept_lossy_anthropic_ingress: true on the deployment
}

func kv(indent int, key, value string) string {
	return strings.Repeat("  ", indent) + key + ": \"" + value + "\""
}

func cfgYAML(o cfgOpts) string {
	listen := o.listen
	if listen == "" {
		listen = "127.0.0.1:8080"
	}
	dep := o.deploymentName
	if dep == "" {
		dep = "d1"
	}
	provider := o.provider
	if provider == "" {
		provider = "openai"
	}
	lines := []string{kv(0, "listen_addr", listen), "virtual_keys:", "  k:", kv(2, "key_hash", strings.Repeat("ab", 32)), "deployments:", "  " + dep + ":",
		kv(2, "model", "gpt-4o"), kv(2, "provider", provider), kv(2, "upstream_model", "gpt-4o")}
	switch {
	case o.rawBaseURL != "":
		lines = append(lines, kv(2, "base_url", o.rawBaseURL))
	case o.insecureHTTP:
		u := o.insecureURL
		if u == "" {
			u = "http://127.0.0.1:9/v1/chat/completions"
		}
		lines = append(lines, kv(2, "base_url", u), "    allow_insecure_http: true")
	case provider == "anthropic":
		lines = append(lines, kv(2, "base_url", "https://api.anthropic.com/v1/messages"))
	default:
		lines = append(lines, kv(2, "base_url", "https://api.openai.com/v1/chat/completions"))
	}
	if o.acceptLossy {
		lines = append(lines, "    accept_lossy_anthropic_ingress: true")
	}
	if o.keyFile != "" {
		lines = append(lines, kv(2, "api_key_file", o.keyFile))
	} else {
		name := o.keyEnvName
		if name == "" {
			name = "DOCTOR_TEST_UPSTREAM"
		}
		lines = append(lines, kv(2, "api_key_env", name))
	}
	if o.adminTokenEnv != "" {
		lines = append(lines, "admin:", kv(1, "token_env", o.adminTokenEnv))
		if o.adminPersist != "" {
			lines = append(lines, kv(1, "persist_path", o.adminPersist))
		}
		if o.adminAudit != "" {
			lines = append(lines, kv(1, "audit_log_path", o.adminAudit))
		}
		if o.adminBackup != "" {
			lines = append(lines, kv(1, "backup_dir", o.adminBackup))
		}
	}
	if o.propagationRedis != "" {
		lines = append(lines, "config_propagation:", kv(1, "redis_addr", o.propagationRedis))
	}
	switch o.telemetry {
	case "":
	case "-":
		lines = append(lines, "telemetry:", kv(1, "exporter", "carrier-pigeon"))
	default:
		lines = append(lines, "telemetry:", kv(1, "exporter", o.telemetry))
	}
	if !o.noPrice {
		lines = append(lines, "price_table:", "  gpt-4o:", "    prompt_per_token: 0.0000025", "    completion_per_token: 0.00001")
	}
	return strings.Join(lines, "\n") + "\n"
}

func writeCfg(t *testing.T, dir string, o cfgOpts) string {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(cfgYAML(o)), 0o644); err != nil { //nolint:gosec // G306: the config is world-readable by design
		t.Fatal(err)
	}
	return p
}

func runDoctorCmd(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Doctor(args, IO{Stdout: &out, Stderr: &errb, Getenv: func(k string) string { return env[k] }})
	return code, out.String(), errb.String()
}

func TestDoctorCleanConfigHasNoFindings(t *testing.T) {
	cfg := writeCfg(t, t.TempDir(), cfgOpts{acceptLossy: true, telemetry: "none"})
	code, stdout, stderr := runDoctorCmd(t, envWith("DOCTOR_TEST_UPSTREAM"), "--config", cfg)
	if code != 0 || !strings.Contains(stdout, "no findings") || stderr != "" {
		t.Errorf("clean config: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
}

func TestDoctorUnsetVariableSeveritiesFollowTheGatewaysStartup(t *testing.T) {
	dir := t.TempDir()
	cfg := writeCfg(t, dir, cfgOpts{acceptLossy: true, telemetry: "none", adminTokenEnv: "DOCTOR_TEST_ADMIN", adminPersist: filepath.Join(dir, "id.db")})
	// No env source: every miss is a warning that names the EnvironmentFile= fallback.
	code, stdout, _ := runDoctorCmd(t, nil, "--config", cfg)
	if code != 0 || strings.Count(stdout, "warning") < 2 || !strings.Contains(stdout, "EnvironmentFile=") || strings.Contains(stdout, "error ") {
		t.Errorf("no source: exit %d\n%s", code, stdout)
	}
	// --strict-env: this shell is declared the gateway's whole environment, so every miss is an error (RFC-3 decision 7).
	code, stdout, _ = runDoctorCmd(t, nil, "--config", cfg, "--strict-env")
	if code != 1 || !strings.Contains(stdout, "error     admin.token_env") || !strings.Contains(stdout, "refuses to start") || !strings.Contains(stdout, "error     deployments.d1.api_key_env") || strings.Contains(stdout, "warning   ") {
		t.Errorf("--strict-env: exit %d\n%s", code, stdout)
	}
	// An env file that sets both: no findings. One that sets the admin token to empty: error.
	envFile := filepath.Join(dir, "env")
	if err := os.WriteFile(envFile, []byte("DOCTOR_TEST_UPSTREAM=\"x\"\nDOCTOR_TEST_ADMIN=y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stdout, _ := runDoctorCmd(t, nil, "--config", cfg, "--env-file", envFile); code != 0 || !strings.Contains(stdout, "no findings") {
		t.Errorf("env file resolving both: exit %d\n%s", code, stdout)
	}
	// An env file may not be the whole environment, so a deployment credential it lacks keeps startup's severity: a warning.
	if err := os.WriteFile(envFile, []byte("DOCTOR_TEST_ADMIN=y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stdout, _ := runDoctorCmd(t, nil, "--config", cfg, "--env-file", envFile); code != 0 || !strings.Contains(stdout, "warning   deployments.d1.api_key_env") {
		t.Errorf("env file lacking the deployment credential: exit %d\n%s", code, stdout)
	}
	if err := os.WriteFile(envFile, []byte("DOCTOR_TEST_UPSTREAM=x\nDOCTOR_TEST_ADMIN=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stdout, _ := runDoctorCmd(t, nil, "--config", cfg, "--env-file", envFile); code != 1 || !strings.Contains(stdout, "unset or empty in the env file") {
		t.Errorf("empty value in the env file: exit %d\n%s", code, stdout)
	}
	if code, _, stderr := runDoctorCmd(t, nil, "--config", cfg, "--env-file", filepath.Join(dir, "missing.env")); code != 1 || !strings.Contains(stderr, "missing.env") {
		t.Errorf("missing --env-file: exit %d, stderr %s", code, stderr)
	}
}

func TestDoctorOAuthWarningNamesTheVariableNeverTheValue(t *testing.T) {
	cfg := writeCfg(t, t.TempDir(), cfgOpts{acceptLossy: true, telemetry: "none", keyEnvName: "ANTHROPIC_API_KEY"})
	env := map[string]string{"ANTHROPIC_API_KEY": oauthTokenPrefix + "-" + sentinel("oauth")}
	for _, args := range [][]string{{"--config", cfg}, {"--config", cfg, "--json"}} {
		code, stdout, stderr := runDoctorCmd(t, env, args...)
		if code != 0 || !strings.Contains(stdout, "ANTHROPIC_API_KEY") || !strings.Contains(stdout, oauthTokenPrefix) {
			t.Errorf("%v: exit %d\n%s", args, code, stdout)
		}
		if strings.Contains(stdout+stderr, sentinel("oauth")) {
			t.Errorf("%v: the value leaked", args)
		}
	}
	env["ANTHROPIC_API_KEY"] = "sk-ant-" + sentinel("api")
	if _, stdout, _ := runDoctorCmd(t, env, "--config", cfg); strings.Contains(stdout, oauthTokenPrefix) || !strings.Contains(stdout, "no findings") {
		t.Errorf("an ordinary key must not warn: %s", stdout)
	}
}

func TestDoctorPackagedLayoutChecks(t *testing.T) {
	root := t.TempDir()
	etc := filepath.Join(root, "etc", "kelvran-gateway")
	if err := os.MkdirAll(etc, 0o750); err != nil {
		t.Fatal(err)
	}
	// doctor resolves the config directory's symlinks (macOS keeps temp dirs
	// under a symlinked /var), so the packaged prefix must be the resolved one.
	resolvedEtc, err := filepath.EvalSymlinks(etc)
	if err != nil {
		t.Fatal(err)
	}
	old := packagedConfigDir
	packagedConfigDir = resolvedEtc + string(filepath.Separator)
	t.Cleanup(func() { packagedConfigDir = old })

	// A credential file literally under /tmp (t.TempDir() is elsewhere on
	// macOS), so the PrivateTmp rule fires on the absolute path.
	tmpDir, err := os.MkdirTemp("/tmp", "kelvran-doctor-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
	keyFile := filepath.Join(tmpDir, "upstream.key")
	if err := os.WriteFile(keyFile, []byte("k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := writeCfg(t, etc, cfgOpts{telemetry: "none", keyFile: keyFile, adminTokenEnv: "DOCTOR_TEST_ADMIN", adminPersist: "/home/op/id.db", adminAudit: "/var/log/kelvran/audit.jsonl"})
	if err := os.Chmod(cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	// The packaged env file is readable (we own it) and resolves the admin token.
	envFile := filepath.Join(etc, "env")
	if err := os.WriteFile(envFile, []byte("DOCTOR_TEST_ADMIN=t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := runDoctorCmd(t, nil, "--config", cfg)
	for _, want := range []string{
		"error     config.mode",          // 0600 config under the package layout
		"warning   admin.persist_path",   // /home is hidden by ProtectHome and outside the StateDirectory
		"warning   admin.audit_log_path", // outside /var/lib/kelvran-gateway
		"not world-readable",             // the 0600 credential file
		"ProtectHome=yes",                // the key file under /tmp (the real /tmp prefix is checked on the absolute path)
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("packaged layout: missing %q in\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "admin.token_env") {
		t.Errorf("the auto-detected env file must resolve the admin token:\n%s", stdout)
	}
	if code != 1 {
		t.Errorf("a 0600 config under the package layout is an error: exit %d", code)
	}
	// An unreadable env file: one warning, and the token check falls back to the process environment.
	if err := os.Chmod(envFile, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(envFile, 0o600) })
	if os.Geteuid() == 0 {
		t.Skip("root can read a 0000 file; the EACCES branch is unreachable")
	}
	_, stdout, _ = runDoctorCmd(t, nil, "--config", cfg)
	if !strings.Contains(stdout, "cannot read "+packagedConfigDir+"env") || !strings.Contains(stdout, "sudo kelvran doctor") || !strings.Contains(stdout, "DOCTOR_TEST_ADMIN is not set in this process environment") {
		t.Errorf("unreadable env file: want one warning and the no-source fallback:\n%s", stdout)
	}
}

func TestDoctorPriceTelemetryAdminAndListenChecks(t *testing.T) {
	dir := t.TempDir()
	env := envWith("DOCTOR_TEST_UPSTREAM", "DOCTOR_TEST_ADMIN")
	cfg := writeCfg(t, dir, cfgOpts{noPrice: true})
	code, stdout, _ := runDoctorCmd(t, env, "--config", cfg)
	if code != 0 || !strings.Contains(stdout, "warning   price_table.gpt-4o") || !strings.Contains(stdout, "info      telemetry.exporter") {
		t.Errorf("unpriced model + absent telemetry: exit %d\n%s", code, stdout)
	}
	cfg = writeCfg(t, dir, cfgOpts{telemetry: "-"})
	if code, stdout, _ := runDoctorCmd(t, env, "--config", cfg); code != 1 || !strings.Contains(stdout, "error     telemetry.exporter") || !strings.Contains(stdout, `"carrier-pigeon"`) {
		t.Errorf("bogus exporter: exit %d\n%s", code, stdout)
	}
	cfg = writeCfg(t, dir, cfgOpts{telemetry: "none", adminTokenEnv: "DOCTOR_TEST_ADMIN"})
	if _, stdout, _ := runDoctorCmd(t, env, "--config", cfg); !strings.Contains(stdout, "warning   admin.persistence") {
		t.Errorf("admin without a store:\n%s", stdout)
	}
	cfg = writeCfg(t, dir, cfgOpts{telemetry: "none", listen: "0.0.0.0:8080", insecureHTTP: true})
	if _, stdout, _ := runDoctorCmd(t, env, "--config", cfg); !strings.Contains(stdout, "warning   listen_addr") || !strings.Contains(stdout, "deployments.d1.allow_insecure_http") {
		t.Errorf("single-user non-loopback listen + insecure http:\n%s", stdout)
	}
	cfg = writeCfg(t, dir, cfgOpts{telemetry: "none", listen: "0.0.0.0:8080", adminTokenEnv: "DOCTOR_TEST_ADMIN", adminPersist: filepath.Join(dir, "id.db")})
	if _, stdout, _ := runDoctorCmd(t, env, "--config", cfg); strings.Contains(stdout, "listen_addr") {
		t.Errorf("team mode: a non-loopback listen is not a single-user finding:\n%s", stdout)
	}
}

func TestDoctorProbesTheDataPlane(t *testing.T) {
	cfg := writeCfg(t, t.TempDir(), cfgOpts{telemetry: "none"})
	env := envWith("DOCTOR_TEST_UPSTREAM")
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readyz":
			w.WriteHeader(http.StatusOK)
		case "/v1/models":
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer good.Close()
	code, stdout, _ := runDoctorCmd(t, env, "--config", cfg, "--url", good.URL)
	if code != 0 || !strings.Contains(stdout, "info      probe.readyz") || !strings.Contains(stdout, "info      probe.auth") {
		t.Errorf("good gateway: exit %d\n%s", code, stdout)
	}
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer open.Close()
	if code, stdout, _ := runDoctorCmd(t, env, "--config", cfg, "--url", open.URL); code != 1 || !strings.Contains(stdout, "error     probe.auth") || !strings.Contains(stdout, "want 401") {
		t.Errorf("a server that serves /v1/models without a bearer: exit %d\n%s", code, stdout)
	}
	// A server that answers 404 everywhere (the wrong address, or not a Kelvran gateway).
	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()
	if code, stdout, _ := runDoctorCmd(t, env, "--config", cfg, "--url", notFound.URL); code != 1 || !strings.Contains(stdout, "error     probe.readyz") || !strings.Contains(stdout, "error     probe.auth") || !strings.Contains(stdout, "answered 404, want 401") {
		t.Errorf("a 404 route: exit %d\n%s", code, stdout)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	if code, stdout, _ := runDoctorCmd(t, env, "--config", cfg, "--url", dead.URL); code != 1 || !strings.Contains(stdout, "error     probe.readyz") {
		t.Errorf("unreachable: exit %d\n%s", code, stdout)
	}
}

func TestDoctorProbesTheAdminAPIWithAResolvedToken(t *testing.T) {
	cfg := writeCfg(t, t.TempDir(), cfgOpts{telemetry: "none", adminTokenEnv: "DOCTOR_TEST_ADMIN", adminPersist: "/var/lib/kelvran-gateway/identity.db"})
	const tok = "tok-doctor-one"
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("Authorization") != "Bearer "+tok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/admin/config":
			_, _ = w.Write([]byte(`{"ListenAddr":"127.0.0.1:8080"}`))
		case "/admin/deployments":
			_, _ = w.Write([]byte(`[{"name":"d1","model":"gpt-4o","healthy":true,"weight":1},{"name":"d2","model":"gpt-4o","healthy":false,"weight":1}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	env := map[string]string{"DOCTOR_TEST_UPSTREAM": "x", "DOCTOR_TEST_ADMIN": tok}
	code, stdout, stderr := runDoctorCmd(t, env, "--config", cfg, "--admin-url", srv.URL)
	if code != 0 || !strings.Contains(stdout, "info      admin.config") || !strings.Contains(stdout, "warning   admin.deployments.d2") || !strings.Contains(stdout, "1 of 2 deployments healthy") {
		t.Errorf("admin probe: exit %d\n%s", code, stdout)
	}
	if !strings.Contains(stderr, "sending the admin token to http://127.0.0.1:") || strings.Contains(stdout+stderr, tok) {
		t.Errorf("the host note must appear and the token never: %q", stderr)
	}
	// No token anywhere: an error finding, no request sent.
	before := hits
	if code, stdout, _ := runDoctorCmd(t, envWith("DOCTOR_TEST_UPSTREAM"), "--config", cfg, "--admin-url", srv.URL); code != 1 || !strings.Contains(stdout, "error     admin.probe") || hits != before {
		t.Errorf("no token: exit %d, hits %d→%d\n%s", code, before, hits, stdout)
	}
	// Wrong token: the config probe fails with the status.
	env["DOCTOR_TEST_ADMIN"] = "wrong"
	if code, stdout, _ := runDoctorCmd(t, env, "--config", cfg, "--admin-url", srv.URL); code != 1 || !strings.Contains(stdout, "error     admin.config") || !strings.Contains(stdout, "HTTP 401") {
		t.Errorf("wrong token: exit %d\n%s", code, stdout)
	}
	// A non-loopback http admin URL is refused before any request leaves.
	before = hits
	if code, _, stderr := runDoctorCmd(t, env, "--config", cfg, "--admin-url", "http://203.0.113.1:8081"); code != 1 || !strings.Contains(stderr, "is not https and not loopback; pass --allow-insecure-http") || hits != before {
		t.Errorf("URL guard: exit %d, stderr %s", code, stderr)
	}
}

func TestDoctorJSONOutputIsOneDocument(t *testing.T) {
	cfg := writeCfg(t, t.TempDir(), cfgOpts{acceptLossy: true, noPrice: true})
	code, stdout, _ := runDoctorCmd(t, nil, "--config", cfg, "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stdout)
	}
	var rep doctorReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("not one JSON document: %v\n%s", err, stdout)
	}
	wantCfg, _ := filepath.EvalSymlinks(cfg)
	if rep.Config != wantCfg || rep.Warnings != 2 || rep.Info != 1 || rep.Errors != 0 || len(rep.Findings) != 3 {
		t.Errorf("report = %+v", rep)
	}
	for _, f := range rep.Findings {
		if f.Severity == "" || f.Check == "" || f.Detail == "" {
			t.Errorf("incomplete finding %+v", f)
		}
	}
}

func TestDoctorUsageAndLoadErrors(t *testing.T) {
	dir := t.TempDir()
	code, stdout, _ := runDoctorCmd(t, nil, "--config", filepath.Join(dir, "missing.yaml"))
	if code != 1 || !strings.Contains(stdout, "error     config.load") {
		t.Errorf("missing config: exit %d\n%s", code, stdout)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("listen_addr: \":1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stdout, _ := runDoctorCmd(t, nil, "--config", filepath.Join(dir, "bad.yaml")); code != 1 || !strings.Contains(stdout, "config.load") || !strings.Contains(stdout, "virtual_keys") {
		t.Errorf("unloadable config: exit %d\n%s", code, stdout)
	}
	if code, _, stderr := runDoctorCmd(t, nil, "--frob"); code != 2 || !strings.Contains(stderr, "usage: kelvran doctor") {
		t.Errorf("unknown flag: exit %d, stderr %s", code, stderr)
	}
	if code, _, stderr := runDoctorCmd(t, nil, "extra"); code != 2 || !strings.Contains(stderr, "unexpected argument") {
		t.Errorf("positional: exit %d, stderr %s", code, stderr)
	}
	if code, stdout, stderr := runDoctorCmd(t, nil, "-h"); code != 0 || stdout != "" || !strings.Contains(stderr, "usage: kelvran doctor") {
		t.Errorf("-h: exit %d stdout %q", code, stdout)
	}
}

// TestEnvChecksSeveritiesMirrorStartup pins, over a constructed Config, which
// unresolved variables the gateway refuses to start on (cmd/gateway/main.go)
// and which it only warns about.
func TestEnvChecksSeveritiesMirrorStartup(t *testing.T) {
	cfg := &controlplane.Config{
		Deployments: []controlplane.DeploymentConfig{{Name: "d", APIKeyEnv: "D_KEY_VAR", SessionTokenEnv: "D_SESSION_VAR"}, {Name: "f", APIKeyEnv: "IGNORED_BECAUSE_FILE_WINS", APIKeyFile: "/x"}},
	}
	cfg.Guardrails.BedrockGuardrails = &controlplane.BedrockGuardrailsConfig{AccessKeyIDEnv: "BG_ID_VAR", SecretAccessKeyEnv: "BG_SECRET_VAR"}
	cfg.Guardrails.EmbedSim = &controlplane.EmbedSimConfig{AccessKeyIDEnv: "ES_ID_VAR", SecretAccessKeyEnv: "ES_SECRET_VAR"}
	cfg.Admin.TokenEnv, cfg.Admin.ViewerTokenEnv = "ADMIN_VAR", "VIEWER_VAR"
	cfg.ConfigPropagation.RedisAddr, cfg.ConfigPropagation.SigningSecretEnv = "redis:6379", "SIGNING_VAR"
	cfg.Budget.Redis.PasswordEnv, cfg.Alerting.WebhookURLEnv = "BUDGET_PW_VAR", "WEBHOOK_VAR"
	got := map[string]bool{}
	for _, c := range envChecks(cfg) {
		got[c.variable] = c.fatal
	}
	want := map[string]bool{
		"D_KEY_VAR": false, "D_SESSION_VAR": false, "BG_ID_VAR": false, "BG_SECRET_VAR": false,
		"ES_ID_VAR": true, "ES_SECRET_VAR": true, // embedsim.New embeds the corpus at construction
		"ADMIN_VAR": true, "VIEWER_VAR": true, "SIGNING_VAR": true,
		"BUDGET_PW_VAR": false, "WEBHOOK_VAR": false,
	}
	for v, fatal := range want {
		gotFatal, ok := got[v]
		if !ok {
			t.Errorf("%s is not checked", v)
			continue
		}
		if gotFatal != fatal {
			t.Errorf("%s fatal = %v, want %v", v, gotFatal, fatal)
		}
	}
	if _, ok := got["IGNORED_BECAUSE_FILE_WINS"]; ok {
		t.Error("a *_env beside a set *_file must not be checked (the file wins at startup)")
	}
	// Without redis_addr the signing secret is irrelevant.
	cfg.ConfigPropagation.RedisAddr = ""
	for _, c := range envChecks(cfg) {
		if c.variable == "SIGNING_VAR" {
			t.Error("signing_secret_env must only be checked when config_propagation.redis_addr is set")
		}
	}
}

func TestParseEnvFileExplainsAnExportLine(t *testing.T) {
	_, err := parseEnvFile(strings.NewReader("export A=1\n"))
	if err == nil || !strings.Contains(err.Error(), "export") || !strings.Contains(err.Error(), "KEY=value") {
		t.Errorf("an export line must be refused with a pointer to the accepted shape: %v", err)
	}
}

func TestDoctorJSONKeysAreStable(t *testing.T) {
	cfg := writeCfg(t, t.TempDir(), cfgOpts{noPrice: true})
	_, stdout, _ := runDoctorCmd(t, nil, "--config", cfg, "--json")
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &top); err != nil {
		t.Fatal(err)
	}
	keys := func(m map[string]json.RawMessage) string {
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}
	if got := keys(top); got != "config,errors,findings,info,warnings" {
		t.Errorf("top-level keys = %s", got)
	}
	var findings []map[string]json.RawMessage
	if err := json.Unmarshal(top["findings"], &findings); err != nil || len(findings) == 0 {
		t.Fatalf("findings: %v", err)
	}
	if got := keys(findings[0]); got != "check,detail,severity" {
		t.Errorf("finding keys = %s", got)
	}
}

func TestDoctorRefusesAnEnvFileAsConfigAndWithholdsLoaderLineContent(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	leak := sentinel("pasted-into-env")
	if err := os.WriteFile(envFile, []byte("# creds\nKELVRAN_ADMIN_TOKEN="+leak+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Pointed at a file the loader cannot parse: the row names the line, never its content.
	for _, args := range [][]string{{"--config", envFile}, {"--config", envFile, "--json"}} {
		code, stdout, stderr := runDoctorCmd(t, nil, args...)
		if code != 1 || !strings.Contains(stdout, "config.load") || !strings.Contains(stdout, "line 2") || !strings.Contains(stdout, "withheld") {
			t.Errorf("%v: exit %d\n%s", args, code, stdout)
		}
		if strings.Contains(stdout+stderr, leak) {
			t.Errorf("%v: the env file's line leaked into the output", args)
		}
	}
	// The same path named as an env file, or the packaged env file, is refused before Load.
	if code, stdout, stderr := runDoctorCmd(t, nil, "--config", envFile, "--env-file", envFile); code != 2 || !strings.Contains(stderr, "is an environment file") || stdout != "" || strings.Contains(stderr, leak) {
		t.Errorf("--config == --env-file: exit %d, stdout %q, stderr %s", code, stdout, stderr)
	}
	// A config that is a symlink to the env file: the check resolves the file itself, not only its directory.
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(envFile, link); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runDoctorCmd(t, nil, "--config", link, "--env-file", envFile); code != 2 || !strings.Contains(stderr, "is an environment file") || strings.Contains(stderr, leak) {
		t.Errorf("--config <symlink to the env file>: exit %d, stderr %s", code, stderr)
	}
	old := packagedConfigDir
	resolved, _ := filepath.EvalSymlinks(dir)
	packagedConfigDir = resolved + string(filepath.Separator)
	t.Cleanup(func() { packagedConfigDir = old })
	if code, _, stderr := runDoctorCmd(t, nil, "--config", envFile); code != 2 || !strings.Contains(stderr, "is an environment file") || strings.Contains(stderr, leak) {
		t.Errorf("--config <packaged env file>: exit %d, stderr %s", code, stderr)
	}
}

func TestDoctorNeverEchoesTheBearerFromAnAdminErrorBody(t *testing.T) {
	cfg := writeCfg(t, t.TempDir(), cfgOpts{telemetry: "none", adminTokenEnv: "DOCTOR_TEST_ADMIN", adminPersist: "/var/lib/kelvran-gateway/identity.db"})
	const tok = "tok-doctor-echo-" + "sentinel"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A misbehaving server: a 400 whose body echoes the request's headers, raw and base64, with the
		// raw copy pushed past the 120-rune cut so that only redaction-before-truncation can hide it.
		bearer := r.Header.Get("Authorization")
		b64 := base64.StdEncoding.EncodeToString([]byte(strings.TrimPrefix(bearer, "Bearer ")))
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("<html>debug: b64=" + b64 + " " + strings.Repeat("x", 100) + " Authorization=" + bearer + "\x1b[2K\nsecond line</html>")) //nolint:gosec // G705: a deliberately misbehaving server that echoes the request's bearer back
	}))
	defer srv.Close()
	env := map[string]string{"DOCTOR_TEST_UPSTREAM": "x", "DOCTOR_TEST_ADMIN": tok}
	code, stdout, stderr := runDoctorCmd(t, env, "--config", cfg, "--admin-url", srv.URL)
	if code != 1 || !strings.Contains(stdout, "error     admin.config") || !strings.Contains(stdout, "HTTP 400") {
		t.Errorf("400 body: exit %d\n%s", code, stdout)
	}
	if strings.Contains(stdout+stderr, tok) || strings.Contains(stdout, "\x1b") || strings.Contains(stdout, "second line") {
		t.Errorf("the bearer, control characters or later lines leaked: %q", stdout)
	}
	if b64 := base64.StdEncoding.EncodeToString([]byte(tok)); strings.Contains(stdout, b64) || strings.Contains(stdout, tok[:8]) {
		t.Errorf("an encoded or truncated copy of the bearer leaked: %q", stdout)
	}
	// 401: status only, no body at all.
	unauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized: "+r.Header.Get("Authorization"), http.StatusUnauthorized)
	}))
	defer unauth.Close()
	if _, stdout, _ := runDoctorCmd(t, env, "--config", cfg, "--admin-url", unauth.URL); !strings.Contains(stdout, "HTTP 401") || strings.Contains(stdout, "unauthorized:") || strings.Contains(stdout, tok) {
		t.Errorf("401 must be status-only: %s", stdout)
	}
}

func TestDoctorDoesNotFollowRedirectsOnEitherClient(t *testing.T) {
	cfg := writeCfg(t, t.TempDir(), cfgOpts{telemetry: "none", adminTokenEnv: "DOCTOR_TEST_ADMIN", adminPersist: "/var/lib/kelvran-gateway/identity.db"})
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits++; w.WriteHeader(http.StatusOK) }))
	defer target.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound) //nolint:gosec // G710: the target is this test's own httptest server
	}))
	defer redirecting.Close()
	env := map[string]string{"DOCTOR_TEST_UPSTREAM": "x", "DOCTOR_TEST_ADMIN": "tok-redirect"}
	code, stdout, _ := runDoctorCmd(t, env, "--config", cfg, "--admin-url", redirecting.URL, "--url", redirecting.URL)
	if code != 1 || !strings.Contains(stdout, "HTTP 302 (a redirect, not followed") || !strings.Contains(stdout, "answered 302, a redirect (not followed)") {
		t.Errorf("redirects must be reported, not followed: exit %d\n%s", code, stdout)
	}
	if targetHits != 0 {
		t.Errorf("the redirect target received %d request(s); the bearer would have travelled with them", targetHits)
	}
}

func TestDoctorFlagsAPastedValueInAnEnvFieldWithoutEchoingIt(t *testing.T) {
	leak := "sk-ant-api03-" + sentinel("pasted")
	cfg := writeCfg(t, t.TempDir(), cfgOpts{telemetry: "none", keyEnvName: leak})
	code, stdout, stderr := runDoctorCmd(t, nil, "--config", cfg, "--json")
	if code != 1 || !strings.Contains(stdout, "deployments.d1.api_key_env") || !strings.Contains(stdout, "looks like a pasted value") {
		t.Errorf("pasted value: exit %d\n%s", code, stdout)
	}
	if strings.Contains(stdout+stderr, sentinel("pasted")) {
		t.Errorf("the pasted value was echoed: %s", stdout)
	}
}

func TestDoctorRedactsUserinfoInProbeURLsAndBaseURLs(t *testing.T) {
	cfg := writeCfg(t, t.TempDir(), cfgOpts{telemetry: "none", insecureHTTP: true, insecureURL: "http://user:" + sentinel("pw") + "@127.0.0.1:9/v1/chat/completions"})
	_, stdout, _ := runDoctorCmd(t, envWith("DOCTOR_TEST_UPSTREAM"), "--config", cfg)
	if !strings.Contains(stdout, "allow_insecure_http") || strings.Contains(stdout, sentinel("pw")) || !strings.Contains(stdout, "user:xxxxx@") {
		t.Errorf("the base_url must be printed redacted: %s", stdout)
	}
	for _, flag := range []string{"--url", "--admin-url"} {
		code, stdout, stderr := runDoctorCmd(t, map[string]string{"DOCTOR_TEST_UPSTREAM": "x", "KELVRAN_ADMIN_TOKEN": "t"}, "--config", cfg, flag, "http://u:"+sentinel("pw")+"@127.0.0.1:9/x?k=v")
		if strings.Contains(stdout+stderr, sentinel("pw")) {
			t.Errorf("%s: the password was echoed: %s %s", flag, stdout, stderr)
		}
		if !strings.Contains(stdout+stderr, "must not carry credentials") {
			t.Errorf("%s: a URL with userinfo must be refused: exit %d, %s %s", flag, code, stdout, stderr)
		}
	}
}

func TestDoctorReportsAMissingPropagationSigningSecretKey(t *testing.T) {
	cfg := writeCfg(t, t.TempDir(), cfgOpts{telemetry: "none", propagationRedis: "127.0.0.1:6379"})
	code, stdout, _ := runDoctorCmd(t, envWith("DOCTOR_TEST_UPSTREAM"), "--config", cfg)
	if code != 1 || !strings.Contains(stdout, "error     config_propagation.signing_secret_env") || !strings.Contains(stdout, "missing while redis_addr is set") {
		t.Errorf("redis_addr without a signing_secret_env key is startup-fatal: exit %d\n%s", code, stdout)
	}
}

func TestDoctorJSONFindingsIsAnArrayWhenEmpty(t *testing.T) {
	cfg := writeCfg(t, t.TempDir(), cfgOpts{acceptLossy: true, telemetry: "none"})
	code, stdout, _ := runDoctorCmd(t, envWith("DOCTOR_TEST_UPSTREAM"), "--config", cfg, "--json")
	if code != 0 || !strings.Contains(stdout, `"findings":[]`) {
		t.Errorf("a clean config must emit an empty array, never null: exit %d\n%s", code, stdout)
	}
}

func TestDoctorEscapesControlCharactersInTheTable(t *testing.T) {
	cfg := writeCfg(t, t.TempDir(), cfgOpts{telemetry: "none", deploymentName: "d\x1b[2Kx"})
	for _, args := range [][]string{{"--config", cfg}, {"--config", cfg, "--json"}} {
		_, stdout, stderr := runDoctorCmd(t, nil, args...)
		if strings.Contains(stdout+stderr, "\x1b") {
			t.Errorf("%v: a raw escape reached the output: %q", args, stdout)
		}
		if !strings.Contains(stdout, `\u001b`) {
			t.Errorf("%v: the escaped form must still identify the key: %s", args, stdout)
		}
	}
}

func TestDoctorCleansPathsBeforeThePackagedLayoutRules(t *testing.T) {
	root := t.TempDir()
	etc := filepath.Join(root, "etc", "kelvran-gateway")
	if err := os.MkdirAll(etc, 0o750); err != nil {
		t.Fatal(err)
	}
	resolvedEtc, err := filepath.EvalSymlinks(etc)
	if err != nil {
		t.Fatal(err)
	}
	old := packagedConfigDir
	packagedConfigDir = resolvedEtc + string(filepath.Separator)
	t.Cleanup(func() { packagedConfigDir = old })
	cfg := writeCfg(t, etc, cfgOpts{telemetry: "none", adminTokenEnv: "DOCTOR_TEST_ADMIN",
		adminPersist: "/var/lib/kelvran-gateway/../../../home/op/id.db", // cleans to /home/op/id.db
		adminAudit:   "/tmp/../var/lib/kelvran-gateway/audit.jsonl",     // cleans into the state directory
		adminBackup:  "/var/lib/kelvran-gateway"})                       // the state directory itself
	_, stdout, _ := runDoctorCmd(t, envWith("DOCTOR_TEST_UPSTREAM", "DOCTOR_TEST_ADMIN"), "--config", cfg)
	if !strings.Contains(stdout, "warning   admin.persist_path") || !strings.Contains(stdout, "ProtectHome=yes") {
		t.Errorf("a path that climbs out of the state directory must be flagged:\n%s", stdout)
	}
	if strings.Contains(stdout, "admin.audit_log_path") || strings.Contains(stdout, "admin.backup_dir") {
		t.Errorf("paths that clean into the state directory must not be flagged:\n%s", stdout)
	}
}

func TestDoctorResolvesTheAdminTokenFromTheEnvFile(t *testing.T) {
	dir := t.TempDir()
	cfg := writeCfg(t, dir, cfgOpts{telemetry: "none", adminTokenEnv: "DOCTOR_TEST_ADMIN", adminPersist: filepath.Join(dir, "id.db")})
	const tok = "tok-from-the-" + "env-file"
	envFile := filepath.Join(dir, "env")
	if err := os.WriteFile(envFile, []byte("DOCTOR_TEST_UPSTREAM=x\nDOCTOR_TEST_ADMIN="+tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/admin/deployments" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	// sudo drops the caller's environment: the process env is empty and only the env file holds the token.
	code, stdout, _ := runDoctorCmd(t, nil, "--config", cfg, "--env-file", envFile, "--admin-url", srv.URL)
	if code != 0 || !strings.Contains(stdout, "info      admin.config") || strings.Contains(stdout, "admin.probe") {
		t.Errorf("the admin token must resolve from the env file: exit %d\n%s", code, stdout)
	}
}

func TestDoctorRedactsAPasswordInALoadErrorsURL(t *testing.T) {
	// Load echoes base_url verbatim when it is http without allow_insecure_http; the password must not follow it into a finding.
	cfg := writeCfg(t, t.TempDir(), cfgOpts{telemetry: "none", rawBaseURL: "http://user:" + sentinel("pw") + "@127.0.0.1:9/v1/chat/completions"})
	for _, args := range [][]string{{"--config", cfg}, {"--config", cfg, "--json"}} {
		code, stdout, stderr := runDoctorCmd(t, nil, args...)
		if code != 1 || !strings.Contains(stdout, "config.load") || !strings.Contains(stdout, "is not https") || !strings.Contains(stdout, "user:xxxxx@") {
			t.Errorf("%v: exit %d\n%s", args, code, stdout)
		}
		if strings.Contains(stdout+stderr, sentinel("pw")) {
			t.Errorf("%v: the password leaked through the load error: %s", args, stdout)
		}
	}
	// A percent-encoded "@" makes url.Parse fail without a literal "@" (the wrapped url.Error echoes the URL twice): withheld whole.
	// A query string can carry a key (Gemini-style ?key=): withheld.
	for _, raw := range []string{"http://user:" + sentinel("pw") + "%40127.0.0.1:9/v1", "http://127.0.0.1:9/v1?key=" + sentinel("pw")} {
		cfg := writeCfg(t, t.TempDir(), cfgOpts{telemetry: "none", rawBaseURL: raw})
		code, stdout, stderr := runDoctorCmd(t, nil, "--config", cfg)
		if code != 1 || !strings.Contains(stdout, "config.load") || !strings.Contains(stdout, "withheld") || strings.Contains(stdout+stderr, sentinel("pw")) {
			t.Errorf("%s: exit %d\n%s%s", strings.ReplaceAll(raw, sentinel("pw"), "<pw>"), code, stdout, stderr)
		}
	}
}

func TestRedactURLsWithholdsTheParsersEchoedFragments(t *testing.T) {
	const pw = "PW-SENTINEL"
	in := `controlplane: deployment "d1" has an unparseable base_url "http://u:` + pw + `%40h:9/v1": parse "http://u:` + pw + `%40h:9/v1": invalid port ":` + pw + `%40h:9" after host`
	got := redactURLs(in)
	if strings.Contains(got, pw) || !strings.Contains(got, `"<URL withheld>"`) || !strings.Contains(got, `"<URL fragment withheld>"`) || !strings.Contains(got, `deployment "d1"`) {
		t.Errorf("redactURLs = %q", got)
	}
	if got := redactURLs(`deployment "d1" base_url "http://u:` + pw + `@h/x" is not https`); strings.Contains(got, pw) || !strings.Contains(got, `"http://u:xxxxx@h/x"`) {
		t.Errorf("redactURLs = %q", got)
	}
	if in, got := `deployment "d1" names provider "nope"`, redactURLs(`deployment "d1" names provider "nope"`); got != in {
		t.Errorf("a message without a URL must pass through unchanged: %q", got)
	}
	// A plain URL carries nothing sensitive, so a name that is one of its substrings stays legible…
	if in, got := `deployment "v1" base_url "http://h/v1" is not https`, redactURLs(`deployment "v1" base_url "http://h/v1" is not https`); got != in {
		t.Errorf("a name inside a plain URL must not be withheld: %q", got)
	}
	// …while a deployment named after its own password is withheld, because printing the name would print the password.
	if got := redactURLs(`deployment "` + pw + `" base_url "http://u:` + pw + `@h/x" is not https`); strings.Contains(got, pw) || !strings.Contains(got, `"<URL fragment withheld>"`) {
		t.Errorf("a name equal to the password must be withheld: %q", got)
	}
}

func TestRedactOneURLFailsClosed(t *testing.T) {
	for raw, want := range map[string]string{
		"https://h/x":                 "https://h/x",
		"http://user:pw@h/x":          "http://user:xxxxx@h/x",
		"http://user:pw%40h":          "<URL withheld>",
		"https://h/x?key=s":           "https://h/x?<query withheld>",
		"https://h/x?":                "https://h/x?<query withheld>",
		"https://h/x#frag":            "https://h/x",
		"https://user:pw@h/x?k=v#f":   "https://user:xxxxx@h/x?<query withheld>",
		"deployments.d1 is not a URL": "<URL withheld>",
		"/just/a/path?k=v":            "<URL withheld>",
	} {
		if got := redactOneURL(raw); got != want {
			t.Errorf("redactOneURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestDoctorEscapesTheFlagPackagesOwnErrors(t *testing.T) {
	code, stdout, stderr := runDoctorCmd(t, nil, "--bogus\x1b[2K")
	if code != 2 || stdout != "" || strings.Contains(stderr, "\x1b") || !strings.Contains(stderr, `\u001b`) {
		t.Errorf("unknown flag with an escape: exit %d, stderr %q", code, stderr)
	}
}

func TestDoctorEscapesControlCharactersOnStderrToo(t *testing.T) {
	dir := t.TempDir()
	cfg := writeCfg(t, dir, cfgOpts{telemetry: "none"})
	// A usage error echoing a path: --config pointed at an env file whose name carries an escape.
	envFile := filepath.Join(dir, "env\x1b[2K")
	if err := os.WriteFile(envFile, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runDoctorCmd(t, nil, "--config", envFile, "--env-file", envFile)
	if code != 2 || strings.Contains(stdout+stderr, "\x1b") || !strings.Contains(stderr, `\u001b`) {
		t.Errorf("usage error: exit %d, stderr %q", code, stderr)
	}
	// A runtime error echoing a path: a missing --env-file.
	code, stdout, stderr = runDoctorCmd(t, nil, "--config", cfg, "--env-file", filepath.Join(dir, "missing\x1b.env"))
	if code != 1 || strings.Contains(stdout+stderr, "\x1b") || !strings.Contains(stderr, `\u001b`) {
		t.Errorf("runtime error: exit %d, stderr %q", code, stderr)
	}
	// The stderr host note and the probe detail: a host carrying a Unicode line separator passes url.Parse and the https guard.
	_, stdout, stderr = runDoctorCmd(t, map[string]string{"DOCTOR_TEST_UPSTREAM": "x", "KELVRAN_ADMIN_TOKEN": "t"}, "--config", cfg, "--admin-url", "https://x\u2028y.invalid:9")
	if strings.Contains(stdout+stderr, "\u2028") || !strings.Contains(stderr, `\u2028`) {
		t.Errorf("admin host: stdout %q stderr %q", stdout, stderr)
	}
}

// TestDoctorLossyIngressWarningPerNonAnthropicDeployment: a deployment that
// is not anthropic and has accept_lossy_anthropic_ingress unset draws one
// warning naming the key and the client-side remedy; anthropic deployments
// and flagged ones draw none (item 11 slice S9a; the deferral RFC-3
// recorded).
func TestDoctorLossyIngressWarningPerNonAnthropicDeployment(t *testing.T) {
	env := map[string]string{"DOCTOR_TEST_UPSTREAM": "k"}
	for name, tc := range map[string]struct {
		opts cfgOpts
		warn bool
	}{
		"openai unflagged":    {cfgOpts{telemetry: "none"}, true},
		"openai flagged":      {cfgOpts{telemetry: "none", acceptLossy: true}, false},
		"anthropic unflagged": {cfgOpts{telemetry: "none", provider: "anthropic"}, false},
	} {
		cfg := writeCfg(t, t.TempDir(), tc.opts)
		code, stdout, _ := runDoctorCmd(t, env, "--config", cfg)
		if code != 0 {
			t.Errorf("%s: exit %d\n%s", name, code, stdout)
		}
		got := strings.Contains(stdout, "deployments.d1.accept_lossy_anthropic_ingress")
		if got != tc.warn {
			t.Errorf("%s: warning present = %v, want %v\n%s", name, got, tc.warn, stdout)
		}
		if tc.warn && (!strings.Contains(stdout, "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1") || !strings.Contains(stdout, "400")) {
			t.Errorf("%s: the warning must name the 400 and the client-side remedy\n%s", name, stdout)
		}
	}
}
