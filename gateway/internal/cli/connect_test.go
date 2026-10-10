package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runConnectCmd(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Connect(args, IO{Stdout: &out, Stderr: &errb, Getenv: func(k string) string { return env[k] }})
	return code, out.String(), errb.String()
}

// connectKey is the virtual key the connect tests hand the CLI; a sentinel
// so its one permitted appearance can be counted.
func connectKey() string { return sentinel("virtual-key") }

func keyEnv(extra map[string]string) map[string]string {
	m := map[string]string{envAnthropicBearer: connectKey()}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func writeKeyFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(connectKey()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConnectUsageAndKeySources(t *testing.T) {
	for _, tc := range []struct {
		args []string
		env  map[string]string
		want int
		msg  string
	}{
		{nil, nil, 2, "usage: kelvran connect"},
		{[]string{"cursor"}, nil, 2, `unknown tool "cursor"`},
		{[]string{"claude", "extra"}, keyEnv(nil), 2, `unexpected argument "extra"`},
		{[]string{"claude", "--scope", "org"}, keyEnv(nil), 2, "--scope \"org\""},
		{[]string{"codex", "--write"}, keyEnv(nil), 2, "flag provided but not defined: -write"},
		{[]string{"claude"}, map[string]string{envOpenAIAPIVar: connectKey()}, 1, "no virtual key"},
		{[]string{"claude", "--url", "http://127.0.0.1:8080/v1"}, keyEnv(nil), 2, "without a path"},
		{[]string{"claude", "--url", "http://203.0.113.1:8080"}, keyEnv(nil), 1, "is not https and not loopback"},
		{[]string{"claude", "--url", "http://u:" + sentinel("pw") + "@127.0.0.1:8080"}, keyEnv(nil), 1, "must not carry credentials"},
		{[]string{"claude", "--write", "--check"}, keyEnv(nil), 2, "--check and --write are exclusive"},
		{[]string{"claude", "--force"}, keyEnv(nil), 2, "--force applies only with --write"},
		{[]string{"claude", "--scope", "project"}, keyEnv(nil), 2, "--scope applies only with --write"},
		{[]string{"claude", "--replace-credential"}, keyEnv(nil), 2, "--replace-credential applies only with --write"},
	} {
		code, stdout, stderr := runConnectCmd(t, tc.env, tc.args...)
		if code != tc.want || !strings.Contains(stderr, tc.msg) || (tc.want != 0 && strings.Contains(stdout, connectKey())) || strings.Contains(stderr, connectKey()) || strings.Contains(stderr, sentinel("pw")) {
			t.Errorf("%v: exit %d (want %d), stdout %q, stderr %q", tc.args, code, tc.want, stdout, stderr)
		}
	}
	// The three sources, in order; OPENAI_API_KEY alone is never used.
	if code, stdout, _ := runConnectCmd(t, nil, "claude", "--key-file", writeKeyFile(t)); code != 0 || strings.Count(stdout, connectKey()) != 1 || !strings.Contains(stdout, "the key came from the key file") {
		t.Errorf("--key-file: exit %d\n%s", code, stdout)
	}
	if code, stdout, _ := runConnectCmd(t, map[string]string{connectFileVar: writeKeyFile(t)}, "claude"); code != 0 || strings.Count(stdout, connectKey()) != 1 {
		t.Errorf("KELVRAN_KEY_FILE: exit %d\n%s", code, stdout)
	}
	if code, stdout, _ := runConnectCmd(t, keyEnv(nil), "claude"); code != 0 || strings.Count(stdout, connectKey()) != 1 || !strings.Contains(stdout, "the key came from "+envAnthropicBearer+" in this shell") {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: exit %d\n%s", code, stdout)
	}
	// A key the bearer alphabet cannot carry could never authenticate, and
	// a raw export line of it would carry shell syntax: refused naming the
	// source, never the value, before anything is printed.
	for _, bad := range []string{"abc; echo PWNED", "line-one\nline-two", "tab\tkey", "quote\"key"} {
		p := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(p, []byte(bad+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := runConnectCmd(t, nil, "codex", "--key-file", p)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "a bearer token cannot carry") || !strings.Contains(stderr, p) || strings.Contains(stderr, "PWNED") || strings.Contains(stderr, "line-two") || strings.Contains(stderr, "quote") {
			t.Errorf("key %q: exit %d\n%s%s", bad, code, stdout, stderr)
		}
	}
	if code, stdout, stderr := runConnectCmd(t, map[string]string{envAnthropicBearer: "with space"}, "claude"); code != 1 || stdout != "" || !strings.Contains(stderr, envAnthropicBearer+" in this shell") || strings.Contains(stderr, "with space") {
		t.Errorf("env key with a space: exit %d %s", code, stderr)
	}
	if code, _, stderr := runConnectCmd(t, nil, "claude", "--key-file", t.TempDir()); code != 1 || !strings.Contains(stderr, "not a regular file") {
		t.Errorf("a directory as the key file: exit %d %s", code, stderr)
	}
}

func TestConnectClaudePrintsTheBlockWithTheSecretOnce(t *testing.T) {
	code, stdout, stderr := runConnectCmd(t, keyEnv(map[string]string{envAnthropicAPIVar: sentinel("shell-api-key")}), "claude", "--url", "https://gw.example.invalid", "--discovery")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	if strings.Count(stdout, connectKey()) != 1 || strings.Contains(stderr, connectKey()) {
		t.Errorf("the secret must appear exactly once, on stdout:\n%s\n%s", stdout, stderr)
	}
	var doc struct {
		Env map[string]string `json:"env"`
	}
	start, end := strings.Index(stdout, "{"), strings.Index(stdout, "\n}\n")
	if start < 0 || end < 0 || json.Unmarshal([]byte(stdout[start:end+2]), &doc) != nil {
		t.Fatalf("no JSON env block in\n%s", stdout)
	}
	if doc.Env[envAnthropicBaseURL] != "https://gw.example.invalid" || doc.Env[envAnthropicBearer] != connectKey() || doc.Env[envHintHeaders] != "1" || doc.Env[envModelDiscovery] != "1" || len(doc.Env) != 4 {
		t.Errorf("env block = %v", doc.Env)
	}
	for _, want := range []string{`"claudeCode.environmentVariables": [`, `{"name": "ANTHROPIC_BASE_URL", "value": "https://gw.example.invalid"},`, `{"name": "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "value": "1"}` + "\n]", "export ANTHROPIC_BASE_URL=https://gw.example.invalid\n", "export CLAUDE_CODE_GATEWAY_HINT_HEADERS=1\n", "serves since gateway/v0.19.0"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in\n%s", want, stdout)
		}
	}
	if !strings.Contains(stderr, envAnthropicAPIVar+" is set in this shell too") || !strings.Contains(stderr, "env -u "+envAnthropicAPIVar+" claude") || strings.Contains(stderr, sentinel("shell-api-key")) {
		t.Errorf("the shell ANTHROPIC_API_KEY warning names the variable, never the value: %s", stderr)
	}
	// Without --discovery the block has three keys and the print-only exemption lets a LAN URL through for codex.
	if _, stdout, _ := runConnectCmd(t, keyEnv(nil), "claude"); strings.Contains(stdout, envModelDiscovery) {
		t.Errorf("--discovery off must not mention %s:\n%s", envModelDiscovery, stdout)
	}
}

func TestConnectPrintOnlyToolsPrintThePairOnce(t *testing.T) {
	for _, tool := range []string{"codex", "aider", "continue"} {
		code, stdout, stderr := runConnectCmd(t, keyEnv(nil), tool, "--url", "http://203.0.113.1:8080")
		if code != 0 || stderr != "" {
			t.Fatalf("%s: exit %d %s", tool, code, stderr)
		}
		if !strings.Contains(stdout, "export OPENAI_BASE_URL=http://203.0.113.1:8080/v1\n") || !strings.Contains(stdout, "export "+envOpenAIAPIVar+"="+connectKey()+"\n") || strings.Count(stdout, connectKey()) != 1 || !strings.Contains(stdout, "POST /v1/responses (the OpenAI Responses API) is a 404") || !strings.Contains(stdout, "# "+tool+" ") {
			t.Errorf("%s:\n%s", tool, stdout)
		}
	}
	home := t.TempDir()
	if code, _, _ := runConnectCmd(t, keyEnv(map[string]string{"HOME": home}), "codex", "--write"); code != 2 {
		t.Errorf("--write on a print-only tool is a usage error: %d", code)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Error("a print-only tool must write nothing")
	}
}

func readJSONFile(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // G304: test fixture
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return m
}

func TestConnectClaudeWriteUserScope(t *testing.T) {
	home := t.TempDir()
	env := keyEnv(map[string]string{"HOME": home})
	target := filepath.Join(home, ".claude", "settings.json")
	fixture := map[string]any{"permissions": map[string]any{"allow": []any{"Bash(ls:*)"}}, "env": map[string]any{"FOO": "bar"}, "model": "opus"}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	fb, _ := json.MarshalIndent(fixture, "", "  ")
	if err := os.WriteFile(target, fb, 0o644); err != nil { //nolint:gosec // G306: the fixture's mode is the point
		t.Fatal(err)
	}
	code, stdout, stderr := runConnectCmd(t, env, "claude", "--write")
	if code != 0 || !strings.Contains(stdout, "Wrote "+target+" (mode 0600)") || strings.Contains(stdout+stderr, connectKey()) {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	// Byte-identical to the documented block merged into the fixture.
	want := map[string]any{"permissions": fixture["permissions"], "model": "opus", "env": map[string]any{"FOO": "bar", envAnthropicBaseURL: defaultDataPlaneURL, envAnthropicBearer: connectKey(), envHintHeaders: "1"}}
	wb, _ := json.MarshalIndent(want, "", "  ")
	got, _ := os.ReadFile(target) //nolint:gosec // G304: test fixture
	if string(got) != string(wb)+"\n" {
		t.Errorf("written file differs:\n--- got\n%s\n--- want\n%s", got, wb)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %04o, want 0600", fi.Mode().Perm())
	}
	// Idempotent: a second run changes nothing and reports no replacement; a new URL is reported, never the token.
	before, _ := os.ReadFile(target) //nolint:gosec // G304: test fixture
	if _, _, stderr := runConnectCmd(t, env, "claude", "--write"); strings.Contains(stderr, "replaced") {
		t.Errorf("idempotent write must not report a replacement: %s", stderr)
	}
	if after, _ := os.ReadFile(target); string(after) != string(before) { //nolint:gosec // G304: test fixture
		t.Error("idempotent write changed the file")
	}
	if _, _, stderr := runConnectCmd(t, env, "claude", "--write", "--url", "https://gw.example.invalid"); !strings.Contains(stderr, "replaced ANTHROPIC_BASE_URL http://127.0.0.1:8080 → https://gw.example.invalid") || strings.Contains(stderr, connectKey()) {
		t.Errorf("a URL change is reported without the token: %s", stderr)
	}
	// The old URL is described as scheme://host at most: a pasted secret in
	// its userinfo, query or fragment never reaches stderr. Integers beyond
	// float64 in the file survive the round trip as written.
	mm := readJSONFile(t, target)
	mm["env"].(map[string]any)[envAnthropicBaseURL] = "https://u:" + sentinel("oldpw") + "@old.example:9?k=" + sentinel("oldq") + "#frag"
	mm["bigint"] = json.Number("12345678901234567890")
	mmb, _ := json.MarshalIndent(mm, "", "  ")
	if err := os.WriteFile(target, append(mmb, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runConnectCmd(t, env, "claude", "--write"); code != 0 || !strings.Contains(stderr, "replaced ANTHROPIC_BASE_URL a different value → http://127.0.0.1:8080") || strings.Contains(stderr, sentinel("oldpw")) || strings.Contains(stderr, sentinel("oldq")) || strings.Contains(stderr, "old.example") {
		t.Errorf("the old URL must be withheld: exit %d %s", code, stderr)
	}
	if got, _ := os.ReadFile(target); !strings.Contains(string(got), `"bigint": 12345678901234567890`) { //nolint:gosec // G304: test fixture
		t.Errorf("a large integer must survive as written:\n%s", got)
	}
	if _, _, stderr := runConnectCmd(t, env, "claude", "--write", "--url", "https://gw.example.invalid"); !strings.Contains(stderr, "replaced ANTHROPIC_BASE_URL http://127.0.0.1:8080 → https://gw.example.invalid") {
		t.Errorf("a plain old URL is shown as scheme://host: %s", stderr)
	}
	// Conflicts: env.ANTHROPIC_API_KEY and apiKeyHelper refuse without --replace-credential, and are removed exactly with it.
	for _, conflict := range []string{"env", settingsHelperField} {
		m := readJSONFile(t, target)
		if conflict == "env" {
			m["env"].(map[string]any)[envAnthropicAPIVar] = sentinel("conflict")
		} else {
			m[settingsHelperField] = "/bin/" + sentinel("conflict")
		}
		mb, _ := json.MarshalIndent(m, "", "  ")
		if err := os.WriteFile(target, append(mb, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		code, _, stderr := runConnectCmd(t, env, "claude", "--write")
		if code != 1 || !strings.Contains(stderr, "pass --replace-credential") || strings.Contains(stderr, sentinel("conflict")) {
			t.Errorf("%s conflict: exit %d %s", conflict, code, stderr)
		}
		if got, _ := os.ReadFile(target); string(got) != string(mb)+"\n" { //nolint:gosec // G304: test fixture
			t.Errorf("%s conflict: the file must be untouched", conflict)
		}
		if code, _, stderr := runConnectCmd(t, env, "claude", "--write", "--replace-credential"); code != 0 {
			t.Fatalf("--replace-credential: exit %d %s", code, stderr)
		}
		got := readJSONFile(t, target)
		if _, has := got["env"].(map[string]any)[envAnthropicAPIVar]; has {
			t.Error("env.ANTHROPIC_API_KEY must be gone")
		}
		if _, has := got[settingsHelperField]; has {
			t.Error("apiKeyHelper must be gone")
		}
		if got["model"] != "opus" || got["env"].(map[string]any)["FOO"] != "bar" {
			t.Errorf("other keys must survive: %v", got)
		}
	}
	// A credential in another scope's file: a warning, no refusal, that file untouched.
	t.Chdir(t.TempDir())
	local := filepath.Join(".claude", "settings.local.json")
	if err := os.MkdirAll(".claude", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte(`{"env":{"`+envAnthropicAPIVar+`":"`+sentinel("other-scope")+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ = os.ReadFile(local) //nolint:gosec // G304: test fixture
	code, _, stderr = runConnectCmd(t, env, "claude", "--write")
	if code != 0 || !strings.Contains(stderr, "also sets env."+envAnthropicAPIVar) || strings.Contains(stderr, sentinel("other-scope")) {
		t.Errorf("other scope: exit %d %s", code, stderr)
	}
	if after, _ := os.ReadFile(local); string(after) != string(before) { //nolint:gosec // G304: test fixture
		t.Error("the other scope's file must be untouched")
	}
}

// gitAvailable skips a test when git is not on PATH; the project-scope rule
// shells out to it.
func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// isolateGit points HOME, XDG_CONFIG_HOME and GIT_CONFIG_GLOBAL at empty temp
// locations so the developer's own global excludes cannot leak in (on this
// machine **/.claude/settings.local.json is globally ignored, which would make
// a negative row pass vacuously), and returns the temp global gitconfig path.
func isolateGit(t *testing.T) string {
	t.Helper()
	iso := t.TempDir()
	t.Setenv("HOME", iso)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(iso, "xdg"))
	gc := filepath.Join(iso, "gitconfig")
	if err := os.WriteFile(gc, []byte("[user]\n\tname = t\n\temail = t@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gc)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return gc
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", "-q", dir) //nolint:gosec // G204: a fixed argv in a test
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
}

func noTempSiblings(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(dir, ".claude"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temp file was left behind: %s", e.Name())
		}
	}
}

func TestConnectClaudeWriteProjectScopeFollowsGit(t *testing.T) {
	gitAvailable(t)
	gc := isolateGit(t)
	env := keyEnv(map[string]string{"HOME": os.Getenv("HOME")})
	// No coverage → refuse, remedy on stderr, nothing written.
	repo := t.TempDir()
	gitInit(t, repo)
	t.Chdir(repo)
	code, _, stderr := runConnectCmd(t, env, "claude", "--write", "--scope", "project")
	if code != 1 || !strings.Contains(stderr, "is not gitignored") || !strings.Contains(stderr, ">> .gitignore") {
		t.Errorf("uncovered: exit %d %s", code, stderr)
	}
	if _, err := os.Stat(projectLocalSettings); !os.IsNotExist(err) {
		t.Error("nothing may be written without coverage")
	}
	noTempSiblings(t, repo)
	// --force writes anyway, with the note.
	if code, _, stderr := runConnectCmd(t, env, "claude", "--write", "--scope", "project", "--force"); code != 0 || !strings.Contains(stderr, "written because of --force") {
		t.Errorf("--force: exit %d %s", code, stderr)
	}
	if err := os.Remove(projectLocalSettings); err != nil {
		t.Fatal(err)
	}
	// .gitignore coverage → writes 0600; a project .claude/settings.json is never written.
	if err := os.WriteFile(".gitignore", []byte(".claude/settings.local.json\n"), 0o644); err != nil { //nolint:gosec // G306: a fixture
		t.Fatal(err)
	}
	if code, stdout, stderr := runConnectCmd(t, env, "claude", "--write", "--scope", "project"); code != 0 || !strings.Contains(stdout, "Wrote "+filepath.Join(repo, projectLocalSettings)) {
		t.Fatalf("covered: exit %d\n%s%s", code, stdout, stderr)
	}
	if fi, err := os.Stat(projectLocalSettings); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(".claude", "settings.json")); !os.IsNotExist(err) {
		t.Error("a project's shared settings.json must never be written")
	}
	// Tracked despite a matching pattern → refuse with the git rm remedy.
	for _, args := range [][]string{{"add", "-f", projectLocalSettings}, {"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "x"}} {
		cmd := exec.Command("git", args...) //nolint:gosec // G204: fixed argv in a test
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if code, _, stderr := runConnectCmd(t, env, "claude", "--write", "--scope", "project"); code != 1 || !strings.Contains(stderr, "git rm --cached "+projectLocalSettings) {
		t.Errorf("tracked: exit %d %s", code, stderr)
	}
	// Coverage only through a core.excludesFile in the (isolated) global config → writes.
	repo2 := t.TempDir()
	gitInit(t, repo2)
	excl := filepath.Join(t.TempDir(), "excludes")
	if err := os.WriteFile(excl, []byte("**/.claude/settings.local.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gc, []byte("[core]\n\texcludesFile = "+excl+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo2)
	if code, _, stderr := runConnectCmd(t, env, "claude", "--write", "--scope", "project"); code != 0 {
		t.Errorf("core.excludesFile coverage: exit %d %s", code, stderr)
	}
	// A subdirectory whose parent .gitignore covers the pattern → writes.
	repo3 := t.TempDir()
	gitInit(t, repo3)
	if err := os.WriteFile(gc, []byte("[user]\n\tname = t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo3, ".gitignore"), []byte("**/.claude/settings.local.json\n"), 0o644); err != nil { //nolint:gosec // G306: a fixture
		t.Fatal(err)
	}
	sub := filepath.Join(repo3, "svc")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	if code, _, stderr := runConnectCmd(t, env, "claude", "--write", "--scope", "project"); code != 0 {
		t.Errorf("parent .gitignore coverage: exit %d %s", code, stderr)
	}
	// No .git anywhere → writes with the note.
	plain := t.TempDir()
	t.Chdir(plain)
	if code, _, stderr := runConnectCmd(t, env, "claude", "--write", "--scope", "project"); code != 0 || !strings.Contains(stderr, "not inside a git work tree") {
		t.Errorf("no repo: exit %d %s", code, stderr)
	}
	// git removed from PATH with a .git present → refuse.
	repo4 := t.TempDir()
	gitInit(t, repo4)
	t.Chdir(repo4)
	t.Setenv("PATH", t.TempDir())
	if code, _, stderr := runConnectCmd(t, env, "claude", "--write", "--scope", "project"); code != 1 || !strings.Contains(stderr, "git is not on PATH") {
		t.Errorf("git absent: exit %d %s", code, stderr)
	}
}

func TestConnectClaudeWriteRefusesASymlinkedSettingsFile(t *testing.T) {
	home := t.TempDir()
	env := keyEnv(map[string]string{"HOME": home})
	link := filepath.Join(home, ".claude", "settings.json")
	real := filepath.Join(t.TempDir(), "dotfiles-settings.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	realResolved, err := filepath.EvalSymlinks(real) // macOS temp dirs sit behind /private
	if err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runConnectCmd(t, env, "claude", "--write")
	if code != 1 || !strings.Contains(stderr, "symbolic link") || !strings.Contains(stderr, realResolved) || strings.Contains(stderr, connectKey()) {
		t.Fatalf("symlink: exit %d %s", code, stderr)
	}
	if fi, _ := os.Lstat(link); fi == nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the link must survive a refusal")
	}
	if got, _ := os.ReadFile(real); string(got) != "{}\n" { //nolint:gosec // G304: test fixture
		t.Error("the link's target must be untouched")
	}
	// --force writes through to the target; the link survives.
	if code, _, stderr := runConnectCmd(t, env, "claude", "--write", "--force"); code != 0 || !strings.Contains(stderr, "written through to "+realResolved) {
		t.Fatalf("--force: exit %d %s", code, stderr)
	}
	if fi, _ := os.Lstat(link); fi == nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("--force must keep the link")
	}
	if got := readJSONFile(t, real); got["env"].(map[string]any)[envAnthropicBearer] != connectKey() {
		t.Error("--force must write the link's target")
	}
	if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o600 {
		t.Errorf("target mode %04o, want 0600", fi.Mode().Perm())
	}
}

func TestConnectClaudeWriteUserScopeRefusesATrackedFile(t *testing.T) {
	gitAvailable(t)
	isolateGit(t)
	// HOME is a dotfiles work tree with .claude/settings.json committed.
	home := t.TempDir()
	gitInit(t, home)
	target := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "--", ".claude/settings.json"}, {"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "x"}} {
		cmd := exec.Command("git", args...) //nolint:gosec // G204: fixed argv in a test
		cmd.Dir = home
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	env := keyEnv(map[string]string{"HOME": home})
	code, _, stderr := runConnectCmd(t, env, "claude", "--write")
	if code != 1 || !strings.Contains(stderr, "tracked by git") || !strings.Contains(stderr, "git rm --cached") || strings.Contains(stderr, connectKey()) {
		t.Errorf("tracked user file: exit %d %s", code, stderr)
	}
	if got, _ := os.ReadFile(target); string(got) != "{}\n" { //nolint:gosec // G304: test fixture
		t.Error("a refused write must leave the file alone")
	}
	if code, _, stderr := runConnectCmd(t, env, "claude", "--write", "--force"); code != 0 || !strings.Contains(stderr, "TRACKED by git and was written because of --force") {
		t.Errorf("--force: exit %d %s", code, stderr)
	}
	// A stow-style HOME: ~/.claude is a symlinked directory into the tracked
	// repository; the question is asked at the resolved location.
	home2 := t.TempDir()
	if err := os.Symlink(filepath.Join(home, ".claude"), filepath.Join(home2, ".claude")); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runConnectCmd(t, keyEnv(map[string]string{"HOME": home2}), "claude", "--write"); code != 1 || !strings.Contains(stderr, "tracked by git") {
		t.Errorf("symlinked .claude directory: exit %d %s", code, stderr)
	}
}

func TestConnectProjectScopeIgnoresInheritedGitEnv(t *testing.T) {
	gitAvailable(t)
	isolateGit(t)
	env := keyEnv(map[string]string{"HOME": os.Getenv("HOME")})
	covered, uncovered := t.TempDir(), t.TempDir()
	gitInit(t, covered)
	gitInit(t, uncovered)
	if err := os.WriteFile(filepath.Join(covered, ".gitignore"), []byte(".claude/settings.local.json\n"), 0o644); err != nil { //nolint:gosec // G306: a fixture
		t.Fatal(err)
	}
	t.Chdir(uncovered)
	// A hook or git-* script exports these; a git that inherited them would
	// answer for the covered repository and the uncovered file would be written.
	t.Setenv("GIT_DIR", filepath.Join(covered, ".git"))
	t.Setenv("GIT_WORK_TREE", covered)
	if code, _, stderr := runConnectCmd(t, env, "claude", "--write", "--scope", "project"); code != 1 || !strings.Contains(stderr, "is not gitignored") {
		t.Errorf("inherited GIT_DIR: exit %d %s", code, stderr)
	}
	if _, err := os.Stat(projectLocalSettings); !os.IsNotExist(err) {
		t.Error("nothing may be written")
	}
}

func TestConnectProjectScopeRefusesAnUnreadableRepository(t *testing.T) {
	gitAvailable(t)
	isolateGit(t)
	env := keyEnv(map[string]string{"HOME": os.Getenv("HOME")})
	repo := t.TempDir()
	gitInit(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".claude/settings.local.json\n"), 0o644); err != nil { //nolint:gosec // G306: a fixture
		t.Fatal(err)
	}
	// A corrupt .git/config makes every git command die with 128 — the same
	// exit as "not a repository" — although the file is covered and a healthy
	// git would write.
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[core\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	code, _, stderr := runConnectCmd(t, env, "claude", "--write", "--scope", "project")
	if code != 1 || !strings.Contains(stderr, "could not read this repository") {
		t.Errorf("unreadable repository: exit %d %s", code, stderr)
	}
	if _, err := os.Stat(projectLocalSettings); !os.IsNotExist(err) {
		t.Error("nothing may be written")
	}
	if code, _, stderr := runConnectCmd(t, env, "claude", "--write", "--scope", "project", "--force"); code != 0 || !strings.Contains(stderr, "written because of --force") {
		t.Errorf("--force: exit %d %s", code, stderr)
	}
}

func TestConnectClaudeCheckReadsTheProbeHonestly(t *testing.T) {
	type probe struct {
		status int
		body   string
		auth   string
		seen   []byte
	}
	var p probe
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.auth = r.Header.Get("Authorization")
		p.seen, _ = io.ReadAll(r.Body)
		w.WriteHeader(p.status)
		_, _ = w.Write([]byte(p.body))
	}))
	t.Cleanup(srv.Close)
	env := keyEnv(nil)
	for _, tc := range []struct {
		status   int
		body     string
		wantExit int
		msg      string
	}{
		{http.StatusNotFound, "404 page not found\n", 1, "predates the Anthropic Messages route"},
		{http.StatusUnauthorized, `{"error":{"type":"authentication_error","code":"invalid_api_key","message":"invalid key"}}`, 1, "the key is not a virtual key for this gateway"},
		{http.StatusBadRequest, `{"error":{"type":"invalid_request_error","code":"model_not_found","message":"no deployment serves model"}}`, 0, "URL and credential are good"},
		{http.StatusOK, `{"id":"msg_1","content":[]}`, 0, "URL and credential are good"},
		{http.StatusBadGateway, "upstream down " + connectKey(), 1, "answered 502"},
		{http.StatusBadRequest, `{"error":{"type":"invalid_request_error","code":"invalid_request","message":"remodel the body"}}`, 1, "answered 400"},
		{http.StatusFound, "", 1, "point --url at the gateway itself"},
	} {
		p = probe{status: tc.status, body: tc.body}
		code, stdout, stderr := runConnectCmd(t, env, "claude", "--check", "--url", srv.URL)
		if code != tc.wantExit || !strings.Contains(stdout, tc.msg) || !strings.Contains(stdout, "settings: the probe did not exercise any settings file") || strings.Contains(stdout+stderr, connectKey()) {
			t.Errorf("%d: exit %d (want %d)\n%s%s", tc.status, code, tc.wantExit, stdout, stderr)
		}
		if p.auth != "Bearer "+connectKey() || !strings.Contains(string(p.seen), `"max_tokens":1`) || !strings.Contains(string(p.seen), checkProbeModel) {
			t.Errorf("%d: probe request auth=%q body=%s", tc.status, p.auth, p.seen)
		}
	}
	// The guard refuses a LAN URL before any request; the transport would fail the test.
	old := adminTransport
	t.Cleanup(func() { adminTransport = old })
	adminTransport = &guardTransport{t: t}
	if code, _, stderr := runConnectCmd(t, env, "claude", "--check", "--url", "http://203.0.113.1:8080"); code != 1 || !strings.Contains(stderr, "is not https and not loopback") {
		t.Errorf("guard: exit %d %s", code, stderr)
	}
}
