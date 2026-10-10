package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

// upstreamVars are every credential variable init reads; tests set each to
// a distinct sentinel so a leak into stdout, stderr or the file is caught.
var upstreamVars = []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"}

func sentinel(name string) string { return "sentinel-" + strings.ToLower(name) + "-value" }

// envWith builds a fake environment holding a sentinel for each named
// variable (names are built from the list, never written as `NAME: value`).
func envWith(names ...string) map[string]string {
	env := map[string]string{}
	for _, n := range names {
		env[n] = sentinel(n)
	}
	return env
}

func allSentinels() map[string]string { return envWith(upstreamVars...) }

// runInit drives Init with a deterministic random source: the first 32
// bytes are 0x11 (so the virtual-key secret is 64 "1"s), the next 32 are
// 0x22 (the admin token is 64 "2"s).
func runInit(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	seq := bytes.Repeat([]byte{0x11}, 32)
	seq = append(seq, bytes.Repeat([]byte{0x22}, 32)...)
	code = Init(args, IO{Stdout: &out, Stderr: &errb, Getenv: func(k string) string { return env[k] }, Rand: bytes.NewReader(seq)})
	return code, out.String(), errb.String()
}

var (
	keyHex   = strings.Repeat("1", 64)
	adminHex = strings.Repeat("2", 64)
)

func loadWritten(t *testing.T, path string) *controlplane.Config {
	t.Helper()
	cfg, err := controlplane.Load(path)
	if err != nil {
		t.Fatalf("Load(%s): %v", path, err)
	}
	if err := controlplane.Validate(cfg, adapter.ProviderNames()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return cfg
}

func loadYAML(t *testing.T, yaml string) *controlplane.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return loadWritten(t, p)
}

func TestInitWritesALoadableValidatedConfigPerProvider(t *testing.T) {
	for _, provider := range []string{"anthropic", "openai", "gemini", "bedrock"} {
		t.Run(provider, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "config.yaml")
			args := []string{"--out", out, "--provider", provider}
			if provider == "bedrock" {
				args = append(args, "--region", "us-east-1")
			}
			code, stdout, stderr := runInit(t, allSentinels(), args...)
			if code != 0 {
				t.Fatalf("exit %d; stderr: %s", code, stderr)
			}
			st, err := os.Stat(out)
			if err != nil || st.Mode().Perm() != 0o644 {
				t.Fatalf("stat %s: %v mode %v, want 0644", out, err, st.Mode())
			}
			cfg := loadWritten(t, out)
			if cfg.Telemetry.Exporter != "none" {
				t.Errorf("telemetry.exporter = %q, want none", cfg.Telemetry.Exporter)
			}
			if len(cfg.VirtualKeys) != 1 || cfg.VirtualKeys[0].KeyHash != identity.HashSecret(keyHex) || !cfg.VirtualKeys[0].BudgetUSD.IsZero() {
				t.Errorf("virtual_keys = %+v, want one key hashed from the printed secret with no budget", cfg.VirtualKeys)
			}
			wantDir, _ := filepath.EvalSymlinks(filepath.Dir(out)) // macOS temp dirs sit under a symlinked /var
			if cfg.Admin.TokenEnv != "KELVRAN_ADMIN_TOKEN" || cfg.Admin.PersistPath != filepath.Join(wantDir, "kelvran-identity.db") {
				t.Errorf("team mode admin = %+v, want token_env KELVRAN_ADMIN_TOKEN and an absolute persist_path beside the config", cfg.Admin)
			}
			if cfg.ListenAddr != "127.0.0.1:8080" {
				t.Errorf("listen_addr = %q", cfg.ListenAddr)
			}
			if len(cfg.Deployments) != 1 {
				t.Fatalf("deployments = %d, want 1", len(cfg.Deployments))
			}
			d := cfg.Deployments[0]
			if d.Provider != provider || d.Model != defaultModel[provider] {
				t.Errorf("deployment = %+v", d)
			}
			if _, ok := cfg.PriceTable[d.Model]; !ok {
				t.Errorf("price_table lacks %s", d.Model)
			}
			if provider == "bedrock" {
				if d.Model == d.UpstreamModel || !strings.HasSuffix(d.BaseURL, "/model/"+d.UpstreamModel+"/converse") || d.Region != "us-east-1" || d.AccessKeyIDEnv != "AWS_ACCESS_KEY_ID" || d.SecretAccessKeyEnv != "AWS_SECRET_ACCESS_KEY" || d.SessionTokenEnv != "AWS_SESSION_TOKEN" {
					t.Errorf("bedrock deployment = %+v, want canonical != upstream, a /converse URL, the region and the three AWS variables", d)
				}
			} else if d.UpstreamModel != d.Model || d.APIKeyEnv == "" {
				t.Errorf("%s deployment = %+v, want upstream == model and an api_key_env", provider, d)
			}
			if !strings.Contains(stdout, "export KELVRAN_KEY="+keyHex) {
				t.Errorf("stdout must print the secret once as an export: %s", stdout)
			}
			// The Claude Code comment names the route as served (item 11 slice
			// S10a); the previous wording survived two slices because nothing
			// pinned it.
			if !strings.Contains(stdout, "posts to /v1/messages, served since gateway/v0.19.0") {
				t.Errorf("stdout must say the Anthropic Messages route is served: %s", stdout)
			}
		})
	}
}

func TestInitSecretHygiene(t *testing.T) {
	for _, mode := range []string{"team", "single-user", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "config.yaml")
			args := []string{"--out", out, "--provider", "anthropic"}
			switch mode {
			case "single-user":
				args = append(args, "--single-user")
			case "dry-run":
				args = append(args, "--dry-run")
			}
			code, stdout, stderr := runInit(t, allSentinels(), args...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			file, _ := os.ReadFile(out) //nolint:gosec // G304: a path inside t.TempDir()
			// The secrets live on exactly one stream: stdout normally, stderr
			// under --dry-run (stdout is then the YAML alone).
			secretStream, otherStream := stdout, stderr
			if mode == "dry-run" {
				secretStream, otherStream = stderr, stdout
				if entries, _ := os.ReadDir(dir); len(entries) != 0 {
					t.Errorf("--dry-run left files behind: %v", entries)
				}
			}
			if n := strings.Count(secretStream, keyHex); n != 1 {
				t.Errorf("secret appears %d times on its stream, want exactly once", n)
			}
			if strings.Contains(otherStream, keyHex) || strings.Contains(string(file), keyHex) {
				t.Error("the secret must never reach the other stream or the file")
			}
			wantToken := 1
			if mode == "single-user" {
				wantToken = 0
			}
			if n := strings.Count(secretStream, adminHex); n != wantToken {
				t.Errorf("admin token appears %d times on its stream, want %d", n, wantToken)
			}
			if strings.Contains(otherStream, adminHex) || strings.Contains(string(file), adminHex) {
				t.Error("the admin token must never reach the other stream or the file")
			}
			if wantToken == 1 && !regexp.MustCompile(`(?m)^\s*export KELVRAN_ADMIN_TOKEN=`).MatchString(secretStream) {
				t.Errorf("team mode must print the admin token as an export line: %s", secretStream)
			}
			for _, v := range upstreamVars {
				for where, text := range map[string]string{"stdout": stdout, "stderr": stderr, "file": string(file)} {
					if strings.Contains(text, sentinel(v)) {
						t.Errorf("upstream credential %s leaked into %s", v, where)
					}
				}
			}
		})
	}
}

func TestInitWarnsOnAnOAuthTokenWithoutEchoingIt(t *testing.T) {
	const anthropicVar = "ANTHROPIC_API_KEY"
	env := allSentinels()
	env[anthropicVar] = oauthTokenPrefix + "-" + sentinel("oauth")
	code, stdout, stderr := runInit(t, env, "--dry-run", "--provider", "anthropic")
	if code != 0 {
		t.Fatalf("Stage 1 warns and continues; exit %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, anthropicVar) || !strings.Contains(stderr, oauthTokenPrefix) {
		t.Errorf("warning must name the variable and the prefix: %s", stderr)
	}
	if strings.Contains(stdout+stderr, sentinel("oauth")) {
		t.Error("the token value must never be echoed")
	}
	env[anthropicVar] = "sk-ant-" + sentinel("api")
	if _, _, stderr := runInit(t, env, "--dry-run", "--provider", "anthropic"); strings.Contains(stderr, "OAuth") {
		t.Errorf("an ordinary sk-ant- key must not trigger the warning: %s", stderr)
	}
}

func TestInitAutoDetectsProvidersFromTheEnvironment(t *testing.T) {
	cases := []struct {
		name      string
		set       []string
		providers []string
		geminiEnv string
	}{
		{"anthropic alone", []string{"ANTHROPIC_API_KEY"}, []string{"anthropic"}, ""},
		{"openai alone", []string{"OPENAI_API_KEY"}, []string{"openai"}, ""},
		{"gemini alone", []string{"GEMINI_API_KEY"}, []string{"gemini"}, "GEMINI_API_KEY"},
		{"google alone", []string{"GOOGLE_API_KEY"}, []string{"gemini"}, "GOOGLE_API_KEY"},
		{"both google vars", []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}, []string{"gemini"}, "GEMINI_API_KEY"},
		{"aws pair", []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}, []string{"bedrock"}, ""},
		{"aws id alone is not a pair", []string{"AWS_ACCESS_KEY_ID", "OPENAI_API_KEY"}, []string{"openai"}, ""},
		{"several", []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"}, []string{"anthropic", "openai", "gemini"}, "GEMINI_API_KEY"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := envWith(c.set...)
			env["AWS_REGION"] = "eu-west-1"
			out := filepath.Join(t.TempDir(), "config.yaml")
			code, _, stderr := runInit(t, env, "--out", out, "--single-user")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			cfg := loadWritten(t, out)
			var got []string
			for _, d := range cfg.Deployments {
				got = append(got, d.Provider)
				if d.Provider == "gemini" && d.APIKeyEnv != c.geminiEnv {
					t.Errorf("gemini api_key_env = %q, want %q", d.APIKeyEnv, c.geminiEnv)
				}
				if d.Provider == "bedrock" && (d.Region != "eu-west-1" || d.SessionTokenEnv != "") {
					t.Errorf("bedrock from AWS_REGION without a session token: %+v", d)
				}
			}
			sort.Strings(got)
			want := append([]string(nil), c.providers...)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") { // the loader sorts deployments by name
				t.Errorf("providers = %v, want %v", got, want)
			}
			if cfg.Admin.TokenEnv != "" {
				t.Errorf("--single-user must write no admin section: %+v", cfg.Admin)
			}
		})
	}
	code, _, stderr := runInit(t, map[string]string{}, "--dry-run")
	if code != 1 {
		t.Errorf("no credential anywhere: exit %d, want 1", code)
	}
	for _, v := range append(upstreamVars[:6:6], "--provider") {
		if !strings.Contains(stderr, v) {
			t.Errorf("the no-provider error must list %s: %s", v, stderr)
		}
	}
}

func TestInitOpenAICompatRules(t *testing.T) {
	out := filepath.Join(t.TempDir(), "config.yaml")
	code, _, stderr := runInit(t, nil, "--out", out, "--provider", "openaicompat", "--base-url", "http://127.0.0.1:11434/v1", "--models", "llama3.2", "--price", "llama3.2=0,0")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	cfg := loadWritten(t, out)
	d := cfg.Deployments[0]
	if !d.AllowInsecureHTTP || d.BaseURL != "http://127.0.0.1:11434/v1/chat/completions" || d.Model != "llama3.2" || d.APIKeyEnv != "OPENAI_API_KEY" {
		t.Errorf("openaicompat deployment = %+v", d)
	}
	if p := cfg.PriceTable["llama3.2"]; !p.PromptPerToken.IsZero() || !p.CompletionPerToken.IsZero() {
		t.Errorf("price = %+v, want the explicit zero", p)
	}
	if code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "openaicompat", "--base-url", "http://127.0.0.1:11434/v1", "--price", "llama3.2=0,0"); code != 2 || !strings.Contains(stderr, "--models") {
		t.Errorf("without --models: exit %d, stderr %s", code, stderr)
	}
	if code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "openaicompat", "--base-url", "http://127.0.0.1:11434/v1", "--models", "llama3.2"); code != 1 || !strings.Contains(stderr, "--price llama3.2=") {
		t.Errorf("without --price: exit %d, stderr %s", code, stderr)
	}
	if code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "openaicompat", "--base-url", "https://llm.example/v1/chat/completions", "--models", "m", "--price", "m=0.000001,0.000002"); code != 0 || strings.Contains(stderr, "warning") {
		t.Errorf("an https base URL that already ends in /chat/completions: exit %d, stderr %s", code, stderr)
	}
}

func TestInitRefusesAnUnpricedModelUnlessPriced(t *testing.T) {
	code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "openai", "--models", "gpt-4o,x-unknown")
	if code != 1 || !strings.Contains(stderr, `"x-unknown"`) || !strings.Contains(stderr, "--price x-unknown=") || !strings.Contains(stderr, priceTableAsOf) {
		t.Errorf("unpriced model: exit %d, stderr %s", code, stderr)
	}
	code, stdout, stderr := runInit(t, nil, "--dry-run", "--provider", "openai", "--models", "gpt-4o,x-unknown", "--price", "x-unknown=0.000001,0.000002")
	if code != 0 {
		t.Fatalf("priced: exit %d: %s", code, stderr)
	}
	cfg := loadYAML(t, stdout)
	if p, ok := cfg.PriceTable["x-unknown"]; !ok || p.PromptPerToken.String() != "0.000001" {
		t.Errorf("price_table[x-unknown] = %+v", p)
	}
	if len(cfg.Deployments) != 2 {
		t.Errorf("deployments = %d, want 2", len(cfg.Deployments))
	}
}

func TestInitRefusesToOverwriteWithoutForce(t *testing.T) {
	out := filepath.Join(t.TempDir(), "config.yaml")
	original := "listen_addr: \":1\"\n"
	if err := os.WriteFile(out, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runInit(t, nil, "--out", out, "--provider", "openai")
	if code != 1 || !strings.Contains(stderr, "--force") {
		t.Errorf("existing file: exit %d, stderr %s", code, stderr)
	}
	if b, _ := os.ReadFile(out); string(b) != original { //nolint:gosec // G304: a path inside t.TempDir()
		t.Errorf("the existing file must be byte-identical, got %q", b)
	}
	if code, _, stderr := runInit(t, nil, "--out", out, "--provider", "openai", "--force"); code != 0 {
		t.Fatalf("--force: exit %d: %s", code, stderr)
	}
	st, _ := os.Stat(out)
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode after --force = %v, want 0644", st.Mode())
	}
	loadWritten(t, out)
}

func TestInitPrintsClientURLsFromTheListenAddress(t *testing.T) {
	cases := []struct{ listen, anthropic, openai string }{
		{"", "http://127.0.0.1:8080", "http://127.0.0.1:8080/v1"},
		{":9000", "http://127.0.0.1:9000", "http://127.0.0.1:9000/v1"},
		{"0.0.0.0:9000", "http://127.0.0.1:9000", "http://127.0.0.1:9000/v1"},
	}
	for _, c := range cases {
		args := []string{"--dry-run", "--provider", "openai"}
		if c.listen != "" {
			args = append(args, "--listen", c.listen)
		}
		code, _, stderr := runInit(t, nil, args...)
		if code != 0 {
			t.Fatalf("--listen %q: exit %d: %s", c.listen, code, stderr)
		}
		if !strings.Contains(stderr, "ANTHROPIC_BASE_URL="+c.anthropic+"\n") || !strings.Contains(stderr, "OPENAI_BASE_URL="+c.openai+"\n") {
			t.Errorf("--listen %q: next steps lack the expected URLs: %s", c.listen, stderr)
		}
		// ":9000" and "0.0.0.0:9000" both bind every interface; only the
		// default 127.0.0.1 is loopback.
		if nonLoopback := c.listen != ""; strings.Contains(stderr, "not loopback-only") != nonLoopback {
			t.Errorf("--listen %q: loopback warning presence wrong: %s", c.listen, stderr)
		}
	}
}

func TestInitDryRunPrintsOnlyTheYAMLOnStdoutAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "config.yaml")
	code, stdout, stderr := runInit(t, allSentinels(), "--dry-run", "--out", out, "--provider", "anthropic")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("--dry-run must write nothing; stat err = %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("--dry-run left files behind: %v", entries)
	}
	if strings.Contains(stdout, "Next steps") || strings.Contains(stdout, "export ") {
		t.Errorf("stdout must be the YAML only (it is piped into -validate): %s", stdout)
	}
	if !strings.Contains(stderr, "Next steps") || !strings.Contains(stderr, "nothing written") {
		t.Errorf("next steps and the dry-run notice go to stderr: %s", stderr)
	}
	cfg := loadYAML(t, stdout)
	if cfg.Deployments[0].Provider != "anthropic" {
		t.Errorf("stdout YAML = %+v", cfg.Deployments)
	}
}

func TestInitBedrockRegionAndIDRules(t *testing.T) {
	code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "bedrock")
	if code != 1 || !strings.Contains(stderr, "--region") {
		t.Errorf("no region anywhere: exit %d, stderr %s", code, stderr)
	}
	code, stdout, stderr := runInit(t, map[string]string{"AWS_DEFAULT_REGION": "ap-south-1"}, "--dry-run", "--provider", "bedrock")
	if code != 0 || loadYAML(t, stdout).Deployments[0].Region != "ap-south-1" {
		t.Errorf("AWS_DEFAULT_REGION fallback: exit %d, stderr %s", code, stderr)
	}
	if code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "bedrock", "--region", "us-east-1", "--base-url", "https://x"); code != 2 || !strings.Contains(stderr, "--base-url") {
		t.Errorf("--base-url with bedrock: exit %d, stderr %s", code, stderr)
	}
	const legacy = "claude-3-7-sonnet"
	if code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "bedrock", "--region", "us-east-1", "--models", legacy, "--price", legacy+"=0.000003,0.000015"); code != 1 || !strings.Contains(stderr, "--upstream-model "+legacy+"=") {
		t.Errorf("unmapped bedrock model: exit %d, stderr %s", code, stderr)
	}
	const legacyBedrock = "anthropic.claude-3-7-sonnet-20250219-v1:0"
	code, stdout, stderr = runInit(t, nil, "--dry-run", "--provider", "bedrock", "--region", "us-east-1", "--models", legacy, "--price", legacy+"=0.000003,0.000015", "--upstream-model", legacy+"="+legacyBedrock)
	if code != 0 {
		t.Fatalf("with --upstream-model: exit %d: %s", code, stderr)
	}
	d := loadYAML(t, stdout).Deployments[0]
	if d.UpstreamModel != legacyBedrock || !strings.Contains(d.BaseURL, "/model/"+legacyBedrock+"/converse") {
		t.Errorf("override not applied: %+v", d)
	}
}

func TestInitPersistPathRules(t *testing.T) {
	code, stdout, stderr := runInit(t, nil, "--dry-run", "--provider", "openai", "--out", "/etc/kelvran-gateway/config.yaml")
	if code != 0 || loadYAML(t, stdout).Admin.PersistPath != "/var/lib/kelvran-gateway/identity.db" {
		t.Errorf("packaged layout: exit %d, stderr %s, yaml %s", code, stderr, stdout)
	}
	if code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "openai", "--persist-path", "relative.db"); code != 2 || !strings.Contains(stderr, "absolute") {
		t.Errorf("relative --persist-path: exit %d, stderr %s", code, stderr)
	}
	code, stdout, stderr = runInit(t, nil, "--dry-run", "--provider", "openai", "--persist-path", "/var/tmp/x.db")
	if code != 0 || loadYAML(t, stdout).Admin.PersistPath != "/var/tmp/x.db" {
		t.Errorf("explicit --persist-path: exit %d, %s", code, stderr)
	}
	code, stdout, stderr = runInit(t, nil, "--dry-run", "--provider", "openai", "--no-persist")
	if code != 0 || loadYAML(t, stdout).Admin.PersistPath != "" || !strings.Contains(stderr, "not survive a restart") {
		t.Errorf("--no-persist: exit %d, stderr %s", code, stderr)
	}
	if code, _, _ := runInit(t, nil, "--dry-run", "--provider", "openai", "--single-user", "--no-persist"); code != 2 {
		t.Errorf("--no-persist with --single-user: exit %d, want 2", code)
	}
}

func TestInitBudgetFlag(t *testing.T) {
	code, stdout, stderr := runInit(t, nil, "--dry-run", "--provider", "openai", "--budget", "25.50")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := loadYAML(t, stdout).VirtualKeys[0].BudgetUSD.String(); got != "25.5" {
		t.Errorf("budget_usd = %s, want 25.5", got)
	}
	for _, bad := range []string{"0", "-1", "abc"} {
		if code, _, _ := runInit(t, nil, "--dry-run", "--provider", "openai", "--budget", bad); code != 2 {
			t.Errorf("--budget %q: exit %d, want 2", bad, code)
		}
	}
}

func TestInitUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"bad provider", nil, []string{"--provider", "cohere"}, "--provider"},
		{"models with several detected providers", envWith("ANTHROPIC_API_KEY", "OPENAI_API_KEY"), []string{"--models", "m"}, "--models"},
		{"base-url with anthropic", nil, []string{"--provider", "anthropic", "--base-url", "https://x"}, "--base-url"},
		{"upstream-model with openai", nil, []string{"--provider", "openai", "--upstream-model", "a=b"}, "--upstream-model"},
		{"region with openai", nil, []string{"--provider", "openai", "--region", "us-east-1"}, "--region"},
		{"malformed price", nil, []string{"--provider", "openai", "--price", "gpt-4o=1"}, "--price"},
		{"negative price", nil, []string{"--provider", "openai", "--price", "gpt-4o=-1,2"}, "--price"},
		{"duplicate model", nil, []string{"--provider", "openai", "--models", "gpt-4o,gpt-4o"}, "twice"},
		{"bad listen", nil, []string{"--provider", "openai", "--listen", "nope"}, "--listen"},
		{"positional argument", nil, []string{"--provider", "openai", "extra"}, "unexpected argument"},
		{"unknown flag", nil, []string{"--provider", "openai", "--frob"}, "frob"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, stdout, stderr := runInit(t, c.env, append([]string{"--dry-run"}, c.args...)...)
			if code != 2 {
				t.Errorf("exit %d, want 2 (stderr %s)", code, stderr)
			}
			if !strings.Contains(stderr, c.want) || !strings.Contains(stderr, "usage: kelvran init") {
				t.Errorf("stderr must name %q and print usage: %s", c.want, stderr)
			}
			if stdout != "" {
				t.Errorf("nothing on stdout for a usage error: %q", stdout)
			}
		})
	}
}

func TestInitHelpExitsZero(t *testing.T) {
	code, stdout, stderr := runInit(t, nil, "-h")
	if code != 0 || stdout != "" || !strings.Contains(stderr, "usage: kelvran init") {
		t.Errorf("-h: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestInitForceWritesThroughASymlinkedOutAndKeepsTheLink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.yaml")
	link := filepath.Join(dir, "link.yaml")
	if err := os.WriteFile(real, []byte("listen_addr: \":1\"\n"), 0o644); err != nil { //nolint:gosec // G306: a fixture that must be world-readable like the real config
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if code, _, stderr := runInit(t, nil, "--out", link, "--provider", "openai"); code != 1 || !strings.Contains(stderr, "--force") {
		t.Errorf("symlinked --out without --force: exit %d, stderr %s", code, stderr)
	}
	if code, _, stderr := runInit(t, nil, "--out", link, "--provider", "openai", "--force"); code != 0 {
		t.Fatalf("--force through the link: exit %d: %s", code, stderr)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link must survive --force: %v %v", fi, err)
	}
	cfg := loadWritten(t, real)
	if len(cfg.Deployments) != 1 || cfg.Deployments[0].Provider != "openai" {
		t.Errorf("the target must hold the new config: %+v", cfg.Deployments)
	}
	if st, _ := os.Stat(real); st.Mode().Perm() != 0o644 {
		t.Errorf("target mode = %v, want 0644", st.Mode())
	}
}

func TestInitRejectsABaseURLWithCredentialsQueryOrFragmentWithoutEchoingIt(t *testing.T) {
	for _, u := range []string{"http://user:p4ss-" + sentinel("pw") + "@127.0.0.1:1/v1", "http://127.0.0.1:1/v1?key=" + sentinel("q"), "http://127.0.0.1:1/v1#" + sentinel("frag")} {
		code, stdout, stderr := runInit(t, nil, "--dry-run", "--provider", "openaicompat", "--base-url", u, "--models", "m", "--price", "m=0,0")
		if code != 2 || !strings.Contains(stderr, "--base-url must not carry") {
			t.Errorf("%q: exit %d, stderr %s", u, code, stderr)
		}
		for _, leak := range []string{sentinel("pw"), sentinel("q"), sentinel("frag"), "p4ss"} {
			if strings.Contains(stdout+stderr, leak) {
				t.Errorf("the URL (and whatever it carries) must never be echoed: %s", stderr)
			}
		}
	}
	if code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "openaicompat", "--base-url", "ftp://user:p4ss@h/x", "--models", "m", "--price", "m=0,0"); code != 2 || strings.Contains(stderr, "p4ss") {
		t.Errorf("a non-http URL must be refused without echo: exit %d, stderr %s", code, stderr)
	}
}

func TestInitRejectsAMalformedRegion(t *testing.T) {
	if code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "bedrock", "--region", "us-east-1.evil.example/x"); code != 2 || !strings.Contains(stderr, "not an AWS region id") {
		t.Errorf("--region with a dot and a slash: exit %d, stderr %s", code, stderr)
	}
	if code, _, stderr := runInit(t, map[string]string{"AWS_REGION": "evil.example"}, "--dry-run", "--provider", "bedrock"); code != 1 || !strings.Contains(stderr, "--region") {
		t.Errorf("AWS_REGION with a dot: exit %d, stderr %s", code, stderr)
	}
	for _, ok := range []string{"us-east-1", "eu-west-2", "us-gov-west-1", "ap-southeast-2", "eu-isoe-west-1"} {
		if code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "bedrock", "--region", ok); code != 0 {
			t.Errorf("--region %s: exit %d, stderr %s", ok, code, stderr)
		}
	}
}

func TestInitRejectsShellSyntaxInPrintedValuesAndQuotesThePath(t *testing.T) {
	if code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "openai", "--listen", "$(id):8080"); code != 2 || !strings.Contains(stderr, "--listen host") {
		t.Errorf("--listen with shell syntax: exit %d, stderr %s", code, stderr)
	}
	if code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "openai", "--listen", "127.0.0.1:$(id)"); code != 2 || !strings.Contains(stderr, "--listen") || strings.Contains(stderr, "$(id)/v1") {
		t.Errorf("--listen with shell syntax in the PORT: exit %d, stderr %s", code, stderr)
	}
	if code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "openai", "--listen", "gateway.internal:8080"); code != 0 || !strings.Contains(stderr, "ANTHROPIC_BASE_URL=http://gateway.internal:8080\n") {
		t.Errorf("a hostname listen address must work: exit %d, stderr %s", code, stderr)
	}
	if code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "openai", "--out", "it's.yaml"); code != 2 || !strings.Contains(stderr, "--out must not contain") {
		t.Errorf("--out with a quote: exit %d, stderr %s", code, stderr)
	}
	spaced := filepath.Join(t.TempDir(), "x dir; echo pwned", "config.yaml")
	code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "openai", "--out", spaced)
	if code != 0 || !strings.Contains(stderr, "kelvran-gateway -config '"+spaced+"' -validate\n") {
		t.Errorf("the printed -config path must be single-quoted: exit %d, stderr %s", code, stderr)
	}
}

func TestInitNeverEchoesAValueTheYAMLBuilderRefuses(t *testing.T) {
	code, stdout, stderr := runInit(t, nil, "--dry-run", "--provider", "bedrock", "--region", "us-east-1", "--upstream-model", "claude-sonnet-5-5=x#"+sentinel("id"))
	if code != 1 || !strings.Contains(stderr, "upstream_model") {
		t.Errorf("a # in a value: exit %d, stderr %s", code, stderr)
	}
	if strings.Contains(stdout+stderr, sentinel("id")) {
		t.Errorf("the refused value must not be echoed: %s", stderr)
	}
}

func TestInitRejectsModelsThatCollideOnTheDeploymentKey(t *testing.T) {
	code, _, stderr := runInit(t, nil, "--dry-run", "--provider", "openai", "--models", "a/b,a-b", "--price", "a/b=0,0", "--price", "a-b=0,0")
	if code != 2 || !strings.Contains(stderr, `"a/b"`) || !strings.Contains(stderr, `"a-b"`) || !strings.Contains(stderr, "openai-a-b") {
		t.Errorf("colliding deployment keys must be a usage error naming both models and the key: exit %d, stderr %s", code, stderr)
	}
	if strings.Contains(stderr, "a bug in kelvran init") {
		t.Errorf("a flag problem must not be attributed to a bug: %s", stderr)
	}
}

func TestInitResolvesASymlinkedOutDirectoryBeforeThePersistPathRule(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	code, stdout, stderr := runInit(t, nil, "--dry-run", "--provider", "openai", "--out", filepath.Join(link, "config.yaml"))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	// macOS puts t.TempDir() under a symlinked /var; compare resolved paths.
	wantDir, _ := filepath.EvalSymlinks(real)
	if got := loadYAML(t, stdout).Admin.PersistPath; got != filepath.Join(wantDir, "kelvran-identity.db") {
		t.Errorf("persist_path = %q, want it beside the resolved directory %q", got, wantDir)
	}
}

func TestInitRefusesADirectoryAsOut(t *testing.T) {
	dir := t.TempDir()
	for _, force := range []bool{false, true} {
		args := []string{"--out", dir, "--provider", "openai"}
		if force {
			args = append(args, "--force")
		}
		if code, _, stderr := runInit(t, nil, args...); code != 1 || !strings.Contains(stderr, "is a directory") {
			t.Errorf("--out <directory> (force=%v): exit %d, stderr %s", force, code, stderr)
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if code, _, stderr := runInit(t, nil, "--out", link, "--provider", "openai", "--force"); code != 1 || !strings.Contains(stderr, "is a directory") {
		t.Errorf("--out <symlink to a directory> --force: exit %d, stderr %s", code, stderr)
	}
}
