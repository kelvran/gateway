package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kelvran/gateway/gateway/internal/adminapi"
	"github.com/kelvran/gateway/gateway/internal/credentialstate"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/telemetry/exporterkind"
)

// packagedEnvFile is the credentials file the packaged unit loads with
// EnvironmentFile= (deploy/systemd/kelvran-gateway.service), beside the
// config; root-only (install -m 0600), so a non-root doctor run cannot read
// it.
func packagedEnvFile() string { return packagedConfigDir + "env" }

// packagedStateDir is the unit's StateDirectory, the one writable path under
// ProtectSystem=strict.
const packagedStateDir = "/var/lib/kelvran-gateway/"

// buildEnvSource merges --env-file values over the process environment. With
// no --env-file and a config under /etc/kelvran-gateway/, the packaged env
// file is read when readable; an unreadable one is one warning and leaves
// the run without an env source (RFC-3 decision 7, check 1).
func buildEnvSource(o doctorOptions, packaged bool, getenv func(string) string, r *doctorReport) (envSource, error) {
	src := envSource{files: map[string]string{}, getenv: getenv}
	if len(o.envFiles) > 0 {
		vars, err := loadEnvFiles(o.envFiles)
		if err != nil {
			return src, runtimeErr("%v", err)
		}
		src.files, src.hasFile = vars, true
		return src, nil
	}
	if !packaged {
		return src, nil
	}
	envFile := packagedEnvFile()
	f, err := os.Open(envFile) //nolint:gosec // G304: the fixed packaged path
	switch {
	case err == nil:
		vars, perr := parseEnvFile(f)
		_ = f.Close()
		if perr != nil {
			r.add("warning", "env.file", envFile+": "+perr.Error()+"; the unit's EnvironmentFile= would reject it too")
			return src, nil
		}
		src.files, src.hasFile = vars, true
	case errors.Is(err, fs.ErrPermission):
		r.add("warning", "env.file", "cannot read "+envFile+" (root-only); the *_env checks below cannot see the gateway's credentials — pass --env-file with a copy you can read, or run `sudo kelvran doctor …` (as root every file reads as readable, so the *_file rows then say less; the world-readable checks still apply)")
	case errors.Is(err, fs.ErrNotExist):
		// No credentials file installed yet: the *_env checks fall back to this process's environment.
	default:
		r.add("warning", "env.file", "cannot read "+envFile+": "+err.Error())
	}
	return src, nil
}

// envCheck is one *_env field: the config path for the check name, the
// variable it names, and whether an empty value is fatal at gateway startup.
type envCheck struct {
	check, variable string
	fatal           bool
	consequence     string
}

func envChecks(cfg *controlplane.Config) []envCheck {
	var out []envCheck
	add := func(check, variable string, fatal bool, consequence string) {
		if variable != "" {
			out = append(out, envCheck{check, variable, fatal, consequence})
		}
	}
	for _, d := range cfg.Deployments {
		p := "deployments." + d.Name + "."
		if d.APIKeyFile == "" {
			add(p+"api_key_env", d.APIKeyEnv, false, "the gateway starts with a warning and every call to this deployment fails")
		}
		if d.AccessKeyIDFile == "" {
			add(p+"access_key_id_env", d.AccessKeyIDEnv, false, "the gateway starts with a warning and every call to this deployment fails")
		}
		if d.SecretAccessKeyFile == "" {
			add(p+"secret_access_key_env", d.SecretAccessKeyEnv, false, "the gateway starts with a warning and every call to this deployment fails")
		}
		if d.SessionTokenFile == "" {
			add(p+"session_token_env", d.SessionTokenEnv, false, "the session token is optional, but a named variable that is unset signs requests without it")
		}
	}
	if g := cfg.Guardrails.BedrockGuardrails; g != nil {
		if g.AccessKeyIDFile == "" {
			add("guardrails.bedrock_guardrails.access_key_id_env", g.AccessKeyIDEnv, false, "the detector cannot sign its requests")
		}
		if g.SecretAccessKeyFile == "" {
			add("guardrails.bedrock_guardrails.secret_access_key_env", g.SecretAccessKeyEnv, false, "the detector cannot sign its requests")
		}
		if g.SessionTokenFile == "" {
			add("guardrails.bedrock_guardrails.session_token_env", g.SessionTokenEnv, false, "the session token is optional, but a named variable that is unset signs requests without it")
		}
	}
	if e := cfg.Guardrails.EmbedSim; e != nil {
		// embedsim.New embeds its whole corpus at construction, so an empty
		// credential is a fatal startup error, unlike a deployment's.
		if e.AccessKeyIDFile == "" {
			add("guardrails.embed_sim.access_key_id_env", e.AccessKeyIDEnv, true, "the embedder signs its corpus embedding at construction and the gateway refuses to start")
		}
		if e.SecretAccessKeyFile == "" {
			add("guardrails.embed_sim.secret_access_key_env", e.SecretAccessKeyEnv, true, "the embedder signs its corpus embedding at construction and the gateway refuses to start")
		}
		if e.SessionTokenFile == "" {
			add("guardrails.embed_sim.session_token_env", e.SessionTokenEnv, false, "the session token is optional, but a named variable that is unset signs requests without it")
		}
	}
	add("admin.token_env", cfg.Admin.TokenEnv, true, "the gateway refuses to start rather than run an unauthenticated admin server")
	add("admin.viewer_token_env", cfg.Admin.ViewerTokenEnv, true, "the gateway refuses to start rather than run an unauthenticated viewer tier")
	add("admin.cost_viewer_token_env", cfg.Admin.CostViewerTokenEnv, true, "the gateway refuses to start rather than run an unauthenticated cost-viewer tier")
	add("admin.operator_token_env", cfg.Admin.OperatorTokenEnv, true, "the gateway refuses to start rather than run an unauthenticated operator tier")
	add("admin.redis_password_env", cfg.Admin.Redis.PasswordEnv, false, "the identity store's Redis connection authenticates with an empty password")
	add("budget.redis_password_env", cfg.Budget.Redis.PasswordEnv, false, "the budget backend's Redis connection authenticates with an empty password")
	add("rate_limit.redis_password_env", cfg.RateLimit.Redis.PasswordEnv, false, "the rate limiter's Redis connection authenticates with an empty password")
	add("config_propagation.redis_password_env", cfg.ConfigPropagation.Redis.PasswordEnv, false, "the propagation channel's Redis connection authenticates with an empty password")
	if cfg.ConfigPropagation.RedisAddr != "" {
		add("config_propagation.signing_secret_env", cfg.ConfigPropagation.SigningSecretEnv, true, "the gateway refuses to run an unauthenticated cross-instance mutation channel")
	}
	add("alerting.webhook_url_env", cfg.Alerting.WebhookURLEnv, false, "the gateway starts with a warning and the alerting notifier is disabled")
	add("alerting.signing_secret_env", cfg.Alerting.SigningSecretEnv, false, "webhook deliveries go out unsigned")
	return out
}

// envNameRe is the shape of an environment-variable NAME. A `*_env` field
// holding anything else is almost always a pasted value, which -validate
// accepts and which must not be echoed.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// checkEnvVariables reports every *_env variable the config names that is
// unset or empty in the env source. With an env file the severity mirrors
// the gateway's own startup — error where it refuses to start, warning where
// it starts and logs — because the file may not be the whole environment (a
// unit can add Environment= lines). With --strict-env the operator declares
// this shell IS the gateway's whole environment, so every miss is an error
// (RFC-3 decision 7: a miss in the single-user flow exits 1). Without a
// source every miss is a warning, because the gateway may receive the
// variable from EnvironmentFile=, `docker run -e` or envFrom.
func checkEnvVariables(cfg *controlplane.Config, src envSource, strict bool, r *doctorReport) {
	hasSource := src.hasFile || strict
	for _, c := range envChecks(cfg) {
		if !envNameRe.MatchString(c.variable) {
			r.add("error", c.check, "does not name an environment variable (letters, digits and _ only, not starting with a digit) — it looks like a pasted value; *_env fields name the variable that holds the secret, never the secret (the value is not shown)")
			continue
		}
		if src.isSet(c.variable) {
			continue
		}
		if !hasSource {
			r.add("warning", c.check, c.variable+" is not set in this process environment; the gateway may receive it from EnvironmentFile=, `docker run -e`/compose `env_file:` or `envFrom:` — rerun with --env-file, inside the gateway's container (`docker exec <ctr> /kelvran doctor --config /config.yaml --strict-env`, `kubectl exec … -- /kelvran doctor … --strict-env`), or with --strict-env if this shell is the gateway's environment")
			continue
		}
		where := "the env file"
		if !src.hasFile {
			where = "this environment (--strict-env)"
		}
		severity := "warning"
		if c.fatal || strict {
			severity = "error"
		}
		r.add(severity, c.check, c.variable+" is unset or empty in "+where+": "+c.consequence)
	}
}

// checkOAuthPrefix is gate G33 in its Stage 1 form: a Claude subscription
// OAuth token (sk-ant-oat…) offered as an upstream API key draws a warning
// naming the variable, never its value.
func checkOAuthPrefix(cfg *controlplane.Config, src envSource, r *doctorReport) {
	for _, d := range cfg.Deployments {
		if d.APIKeyEnv == "" || d.APIKeyFile != "" {
			continue
		}
		if envNameRe.MatchString(d.APIKeyEnv) && strings.HasPrefix(src.value(d.APIKeyEnv), oauthTokenPrefix) {
			r.add("warning", "deployments."+d.Name+".api_key_env", d.APIKeyEnv+" holds a value starting with "+oauthTokenPrefix+": a Claude subscription OAuth token is not an API key, and routing it through a gateway is own-token, single-user use outside this gateway's scope today; use an API key from the Anthropic Console")
		}
	}
}

// checkLossyIngress is the deferral RFC-3 recorded (item 11 slice S9a): a
// chat deployment that is not anthropic and leaves
// accept_lossy_anthropic_ingress unset will answer a Claude Code request
// carrying fields the canonical schema cannot hold with a 400 naming them
// once the /v1/messages route is served (RFC-1 §6, decision Q5); the operator
// either accepts the lossy translation or has the client drop the fields.
// One warning per such deployment, none for anthropic or flagged ones.
func checkLossyIngress(cfg *controlplane.Config, r *doctorReport) {
	for _, d := range cfg.Deployments {
		if d.Provider == "anthropic" || d.AcceptLossyAnthropicIngress || d.Kind == "embedding" {
			continue
		}
		r.add("warning", "deployments."+d.Name+".accept_lossy_anthropic_ingress", "Claude Code requests carrying fields the canonical schema cannot hold will be rejected 400 on this "+d.Provider+" deployment once /v1/messages is served; set accept_lossy_anthropic_ingress: true to translate them with those fields dropped, or set CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1 on the client")
	}
}

// pathCheck is one file or directory the config names.
type pathCheck struct {
	check, path string
	credential  bool // a *_file credential: must be readable and non-empty
	writable    bool // the gateway writes here (persist paths, audit log, backups)
}

func pathChecks(cfg *controlplane.Config) []pathCheck {
	var out []pathCheck
	add := func(check, path string, credential, writable bool) {
		if path != "" {
			out = append(out, pathCheck{check, path, credential, writable})
		}
	}
	for _, d := range cfg.Deployments {
		p := "deployments." + d.Name + "."
		add(p+"api_key_file", d.APIKeyFile, true, false)
		add(p+"access_key_id_file", d.AccessKeyIDFile, true, false)
		add(p+"secret_access_key_file", d.SecretAccessKeyFile, true, false)
		add(p+"session_token_file", d.SessionTokenFile, true, false)
		if t := d.TLSConfig; t != nil {
			add(p+"tls.ca_cert_path", t.CACertPath, false, false)
			add(p+"tls.client_cert_path", t.ClientCertPath, false, false)
			add(p+"tls.client_key_path", t.ClientKeyPath, false, false)
		}
	}
	if g := cfg.Guardrails.BedrockGuardrails; g != nil {
		add("guardrails.bedrock_guardrails.access_key_id_file", g.AccessKeyIDFile, true, false)
		add("guardrails.bedrock_guardrails.secret_access_key_file", g.SecretAccessKeyFile, true, false)
		add("guardrails.bedrock_guardrails.session_token_file", g.SessionTokenFile, true, false)
	}
	if e := cfg.Guardrails.EmbedSim; e != nil {
		add("guardrails.embed_sim.access_key_id_file", e.AccessKeyIDFile, true, false)
		add("guardrails.embed_sim.secret_access_key_file", e.SecretAccessKeyFile, true, false)
		add("guardrails.embed_sim.session_token_file", e.SessionTokenFile, true, false)
		add("guardrails.embed_sim.corpus_path", e.CorpusPath, false, false)
	}
	if m := cfg.Admin.MTLSConfig; m != nil {
		add("admin.mtls.ca_cert_path", m.CACertPath, false, false)
		add("admin.mtls.server_cert_path", m.ServerCertPath, false, false)
		add("admin.mtls.server_key_path", m.ServerKeyPath, false, false)
	}
	add("admin.persist_path", cfg.Admin.PersistPath, false, true)
	add("admin.audit_log_path", cfg.Admin.AuditLogPath, false, true)
	add("admin.backup_dir", cfg.Admin.BackupDir, false, true)
	add("budget.persist_path", cfg.Budget.PersistPath, false, true)
	add("prompt.persist_path", cfg.Prompt.PersistPath, false, true)
	return out
}

// checkFiles reports credential files that cannot be read as the invoking
// user (the gateway reads them as its own uid in its own mount namespace,
// so a miss here is a warning, not proof) and, for the packaged layout,
// paths the unit's hardening hides (ProtectHome, PrivateTmp), writable paths
// outside the StateDirectory (ProtectSystem=strict), credential files the
// DynamicUser could not read, and a config file that is not world-readable.
func checkFiles(cfg *controlplane.Config, cfgAbs string, packaged bool, r *doctorReport) {
	for _, c := range pathChecks(cfg) {
		if c.credential {
			if _, err := credentialstate.ReadFile(c.path); err != nil {
				r.add("warning", c.check, c.path+": cannot be read as this user or is empty ("+err.Error()+"); the gateway reads it as its own uid in its own mount namespace — check there")
			}
		} else if !c.writable {
			if _, err := os.Stat(c.path); err != nil {
				r.add("warning", c.check, c.path+": "+err.Error()+" (checked as this user; the gateway reads it as its own uid)")
			}
		}
		if !packaged {
			continue
		}
		clean := filepath.Clean(c.path)
		if hiddenByUnit(clean) {
			r.add("warning", c.check, c.path+" is under /home, /root or /tmp, which the unit hides (ProtectHome=yes, PrivateTmp=yes); place it under "+packagedStateDir+" or /etc/kelvran-gateway/")
		}
		if c.writable && !underDir(clean, packagedStateDir) {
			r.add("warning", c.check, c.path+" is outside "+packagedStateDir+", the only path the unit can write (ProtectSystem=strict, StateDirectory=kelvran-gateway)")
		}
		if c.credential {
			if st, err := os.Stat(c.path); err == nil && st.Mode().Perm()&0o004 == 0 {
				r.add("warning", c.check, c.path+" is not world-readable; the unit's DynamicUser is allocated at start, so only a world-readable file (or a group the unit joins) is readable by it")
			}
		}
	}
	if packaged {
		if st, err := os.Stat(cfgAbs); err == nil && st.Mode().Perm()&0o004 == 0 {
			r.add("error", "config.mode", fmt.Sprintf("%s has mode %04o: unreadable by the unit's DynamicUser; ExecStartPre -validate will fail (keep it 0644 — it holds hashes and variable names, never a secret)", cfgAbs, st.Mode().Perm()))
		}
	}
}

// underDir reports whether cleanPath (already filepath.Clean'ed) is dir
// itself or below it; dir carries a trailing slash. Cleaning first keeps
// /var/lib/kelvran-gateway/../../../home/x from passing as the state
// directory and /tmp/../etc/x from reading as /tmp.
func underDir(cleanPath, dir string) bool {
	return cleanPath == strings.TrimSuffix(dir, "/") || strings.HasPrefix(cleanPath, dir)
}

func hiddenByUnit(cleanPath string) bool {
	for _, dir := range []string{"/home/", "/root/", "/tmp/"} {
		if underDir(cleanPath, dir) {
			return true
		}
	}
	return false
}

// checkPricesAndTelemetry: every deployment model priced (an unpriced model
// never decrements a budget); telemetry.exporter one of the accepted names
// (the gateway refuses to start otherwise), with an info when the section
// is absent and the default stdout exporter will share stdout with the logs.
func checkPricesAndTelemetry(cfg *controlplane.Config, r *doctorReport) {
	seen := map[string]bool{}
	for _, d := range cfg.Deployments {
		if seen[d.Model] {
			continue
		}
		seen[d.Model] = true
		if _, ok := cfg.PriceTable[d.Model]; !ok {
			r.add("warning", "price_table."+d.Model, "no price_table entry: requests for this model cost $0 and never decrement a budget")
		}
	}
	switch {
	case !exporterkind.Valid(cfg.Telemetry.Exporter):
		r.add("error", "telemetry.exporter", fmt.Sprintf("%q is not one of %s; the gateway refuses to start (kelvran-gateway -validate does not check this)", cfg.Telemetry.Exporter, exporterkind.WantList()))
	case cfg.Telemetry.Exporter == "":
		r.add("info", "telemetry.exporter", "absent: the default stdout exporter interleaves spans and a 60 s metrics dump with the JSON logs on stdout; set exporter: \"none\", or \"otlp\" with otlp_endpoint")
	}
}

// checkAdminAndListen: an admin section without a persistent store loses
// admin-made key changes at restart; a non-loopback listen_addr in
// single-user mode (no admin section) and allow_insecure_http are warnings.
func checkAdminAndListen(cfg *controlplane.Config, r *doctorReport) {
	if cfg.Admin.TokenEnv != "" && cfg.Admin.PersistPath == "" && cfg.Admin.RedisAddr == "" {
		r.add("warning", "admin.persistence", "admin section without persist_path or redis_addr: virtual keys created, rotated or deleted through the admin API are lost at restart")
	}
	if cfg.Admin.TokenEnv == "" {
		if host, _, err := net.SplitHostPort(cfg.ListenAddr); err == nil && !isLoopbackHost(host) {
			r.add("warning", "listen_addr", cfg.ListenAddr+" is not loopback-only in single-user mode (no admin section): every host that can reach it can present a virtual key")
		}
	}
	for _, d := range cfg.Deployments {
		if d.AllowInsecureHTTP {
			r.add("warning", "deployments."+d.Name+".allow_insecure_http", "requests to "+redactedURL(d.BaseURL)+" leave this host in clear text, credential included")
		}
	}
}

// checkPropagationKey: config_propagation.redis_addr without a
// signing_secret_env KEY is fatal at startup (cmd/gateway refuses to run an
// unauthenticated cross-instance mutation channel) and invisible to
// -validate; the *_env check above cannot see an absent key.
func checkPropagationKey(cfg *controlplane.Config, r *doctorReport) {
	if cfg.ConfigPropagation.RedisAddr != "" && cfg.ConfigPropagation.SigningSecretEnv == "" {
		r.add("error", "config_propagation.signing_secret_env", "missing while redis_addr is set: the gateway refuses to run an unauthenticated cross-instance mutation channel (kelvran-gateway -validate does not check this)")
	}
}

// probeDataPlane: /readyz must answer 200 and a bearer-less /v1/models must
// answer 401 (the listener is up and auth is enforced). Both requests carry
// no credential, so --url is exempt from the credential-URL guard.
func probeDataPlane(ctx context.Context, base string, r *doctorReport) {
	if err := guardHTTPURL("--url", base); err != nil {
		r.add("error", "probe.url", err.Error())
		return
	}
	code, err := dataPlaneProbe(ctx, base, "/readyz")
	switch {
	case err != nil:
		r.add("error", "probe.readyz", "GET "+base+"/readyz: "+err.Error())
	case code >= 300 && code < 400:
		r.add("error", "probe.readyz", fmt.Sprintf("GET %s/readyz answered %d, a redirect (not followed) — is this the gateway's listener?", base, code))
	case code != http.StatusOK:
		r.add("error", "probe.readyz", fmt.Sprintf("GET %s/readyz answered %d, want 200 (not ready: a deployment excluded by health probes, or the wrong address)", base, code))
	default:
		r.add("info", "probe.readyz", "GET "+base+"/readyz answered 200")
	}
	code, err = dataPlaneProbe(ctx, base, "/v1/models")
	switch {
	case err != nil:
		r.add("error", "probe.auth", "GET "+base+"/v1/models: "+err.Error())
	case code == http.StatusUnauthorized:
		r.add("info", "probe.auth", "GET "+base+"/v1/models without a bearer answered 401: the listener is up and virtual-key auth is enforced")
	default:
		r.add("error", "probe.auth", fmt.Sprintf("GET %s/v1/models without a bearer answered %d, want 401 — is this a Kelvran gateway?", base, code))
	}
}

func guardHTTPURL(flag, raw string) error {
	if err := guardCredentialURL(flag, raw, true); err != nil {
		return err
	}
	return nil
}

// probeAdmin resolves the token (decision 3) and the guarded URL, then GET
// /admin/config (200 = token and listener good) and GET /admin/deployments,
// flagging every deployment the router has excluded. The token's variables
// are looked up in the env source (the env file first, then this process),
// so `sudo kelvran doctor --admin-url …` under the package layout finds the
// token the unit's EnvironmentFile= holds although sudo dropped the caller's
// environment.
func probeAdmin(ctx context.Context, o doctorOptions, cfg *controlplane.Config, src envSource, env IO, r *doctorReport) error {
	base, err := resolveAdminURL(o.adminURL, o.allowInsecure, env.Getenv)
	if err != nil {
		return runtimeErr("%v", err)
	}
	token, source, err := resolveAdminToken(o.adminTokenFile, cfg.Admin.TokenEnv, src.value)
	if err != nil {
		return runtimeErr("%v", err)
	}
	if token == "" {
		r.add("error", "admin.probe", "no admin token resolved from --admin-token-file, KELVRAN_ADMIN_TOKEN_FILE, the config's admin.token_env variable or KELVRAN_ADMIN_TOKEN (each looked up in the env file, then this process); the admin probes were skipped")
		return nil
	}
	c := newAdminClient(base, token, env.Stderr)
	// Belt and braces: no detail may carry the token — raw, URL-encoded or
	// base64 — whatever a server echoed; the body summary already did this
	// before its cut, this covers every other path into a detail.
	add := func(severity, check, detail string) {
		for _, s := range c.secrets {
			detail = strings.ReplaceAll(detail, s, "***")
		}
		r.add(severity, check, detail)
	}
	if _, err := c.getJSON(ctx, "/admin/config", nil); err != nil {
		add("error", "admin.config", "GET "+base+"/admin/config with the token from "+source+": "+err.Error())
		return nil
	}
	add("info", "admin.config", "GET "+base+"/admin/config answered 200 with the token from "+source)
	var deployments []adminapi.DeploymentEntry
	if _, err := c.getJSON(ctx, "/admin/deployments", &deployments); err != nil {
		add("error", "admin.deployments", "GET "+base+"/admin/deployments: "+err.Error())
		return nil
	}
	healthy := 0
	for _, d := range deployments {
		if d.Healthy {
			healthy++
			continue
		}
		add("warning", "admin.deployments."+d.Name, "excluded by the health prober (healthy: false); requests for "+d.Model+" fall back to other deployments or fail")
	}
	add("info", "admin.deployments", fmt.Sprintf("%d of %d deployments healthy", healthy, len(deployments)))
	return nil
}
