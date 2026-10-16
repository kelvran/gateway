package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/admin/auditstore"
)

const auditTestKeyVar = "AUDIT_TEST_SIGNING_KEY"

func auditTestEnv(extra map[string]string) IO {
	return IO{Stdout: &strings.Builder{}, Stderr: &strings.Builder{}, Getenv: func(k string) string {
		if k == auditTestKeyVar {
			return "cli-test-key-material"
		}
		return extra[k]
	}}
}

func out(env IO) (string, string) {
	return env.Stdout.(*strings.Builder).String(), env.Stderr.(*strings.Builder).String()
}

func signedAuditFile(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	store, err := auditstore.Open(path, []byte("cli-test-key-material"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := store.Append("admin_config_read", "authorized_by", "admin"); err != nil {
			t.Fatal(err)
		}
	}
	_ = store.Close()
	return path
}

func TestAuditVerifyReportsAnIntactChain(t *testing.T) {
	path := signedAuditFile(t, 3)
	env := auditTestEnv(nil)
	code := Audit([]string{"verify", "--file", path, "--key-env", auditTestKeyVar}, env)
	stdout, stderr := out(env)
	if code != 0 || !strings.Contains(stdout, "OK") || !strings.Contains(stdout, "3 lines") {
		t.Errorf("exit %d, stdout %q, stderr %q; want 0 and an OK summary naming 3 lines", code, stdout, stderr)
	}
	if strings.Contains(stderr, "unchecked") {
		t.Errorf("with a key every MAC is checked: %q", stderr)
	}
}

func TestAuditVerifyJSONReportsTheCounts(t *testing.T) {
	path := signedAuditFile(t, 3)
	env := auditTestEnv(nil)
	if code := Audit([]string{"verify", "--file", path, "--key-env", auditTestKeyVar, "--json"}, env); code != 0 {
		_, stderr := out(env)
		t.Fatalf("exit %d: %s", code, stderr)
	}
	stdout, _ := out(env)
	var rep struct {
		OK            bool   `json:"ok"`
		Lines         int    `json:"lines"`
		SignedLines   int    `json:"signed_lines"`
		UncheckedMACs int    `json:"unchecked_macs"`
		LastSeq       uint64 `json:"last_seq"`
		HeadHash      string `json:"head_hash"`
		Problems      []any  `json:"problems"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("stdout is not the JSON report: %v\n%s", err, stdout)
	}
	if !rep.OK || rep.Lines != 3 || rep.SignedLines != 3 || rep.UncheckedMACs != 0 || rep.LastSeq != 3 || len(rep.HeadHash) != 64 || rep.Problems == nil {
		t.Errorf("report = %+v, want ok, 3 lines, 3 signed, last seq 3, a 64-hex head and a non-null problems array", rep)
	}
}

func TestAuditVerifyExitsOneOnATamperedFile(t *testing.T) {
	path := signedAuditFile(t, 3)
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a t.TempDir() path this test created
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), `"authorized_by":"admin"`, `"authorized_by":"nobody"`, 1)
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil { //nolint:gosec // G703: the same t.TempDir() path, rewritten to simulate tampering
		t.Fatal(err)
	}
	env := auditTestEnv(nil)
	code := Audit([]string{"verify", "--file", path, "--key-env", auditTestKeyVar}, env)
	stdout, _ := out(env)
	if code != 1 || !strings.Contains(stdout, "mac_invalid") || !strings.Contains(stdout, "line 1") || !strings.Contains(stdout, "prev_hash") {
		t.Errorf("exit %d, stdout %q; want 1 with the MAC failure at line 1 and the broken link at line 2 listed", code, stdout)
	}
}

func TestAuditVerifyWithoutAKeyNotesUncheckedSignatures(t *testing.T) {
	path := signedAuditFile(t, 2)
	env := auditTestEnv(nil)
	code := Audit([]string{"verify", "--file", path}, env)
	stdout, stderr := out(env)
	if code != 0 || !strings.Contains(stdout, "OK") {
		t.Errorf("exit %d, stdout %q: the chain holds, so the exit is 0", code, stdout)
	}
	if !strings.Contains(stderr, "2 signed lines") || !strings.Contains(stderr, "--key-env") {
		t.Errorf("stderr must say the signatures went unchecked and how to check them: %q", stderr)
	}
}

func TestAuditVerifyResolvesFileAndKeyFromConfig(t *testing.T) {
	path := signedAuditFile(t, 2)
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	content := "listen_addr: \":8080\"\nvirtual_keys:\n  team-alpha:\n    key_hash: \"aa\"\nadmin:\n  token_env: \"ADMIN_TOK\"\n  audit_log_path: \"" + path + "\"\n  audit_signing_secret_env: \"" + auditTestKeyVar + "\"\ndeployments:\n  d1:\n    model: \"m\"\n    provider: \"openai\"\n    upstream_model: \"m\"\n    base_url: \"https://x\"\n    api_key_env: \"X\"\n"
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env := auditTestEnv(nil)
	code := Audit([]string{"verify", "--config", cfg, "--json"}, env)
	stdout, stderr := out(env)
	if code != 0 || !strings.Contains(stdout, `"signed_lines":2`) || !strings.Contains(stdout, `"unchecked_macs":0`) {
		t.Errorf("exit %d, stdout %q, stderr %q; want the file and the key both taken from the config", code, stdout, stderr)
	}
}

func TestAuditVerifyUsageAndRuntimeErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  IO
		code int
	}{
		{"no subcommand", nil, auditTestEnv(nil), 2},
		{"unknown subcommand", []string{"frobnicate"}, auditTestEnv(nil), 2},
		{"neither file nor config", []string{"verify"}, auditTestEnv(nil), 2},
		{"positional argument", []string{"verify", "--file", "x", "extra"}, auditTestEnv(nil), 2},
		{"missing file", []string{"verify", "--file", filepath.Join(t.TempDir(), "absent.jsonl")}, auditTestEnv(nil), 1},
		{"key variable set but empty", []string{"verify", "--file", signedAuditFile(t, 1), "--key-env", "EMPTY_KEY_VAR"}, auditTestEnv(nil), 1},
		{"prefix flag without a key", []string{"verify", "--file", signedAuditFile(t, 1), "--allow-unsigned-prefix"}, auditTestEnv(nil), 2},
	}
	for _, c := range cases {
		if code := Audit(c.args, c.env); code != c.code {
			stdout, stderr := out(c.env)
			t.Errorf("%s: exit %d, want %d (stdout %q, stderr %q)", c.name, code, c.code, stdout, stderr)
		}
	}
}

func unsignedAuditFile(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	store, err := auditstore.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := store.Append("admin_config_read", "authorized_by", "viewer"); err != nil {
			t.Fatal(err)
		}
	}
	_ = store.Close()
	return path
}

func TestAuditVerifyWithAKeyRejectsUnsignedChainedLinesUnlessAllowed(t *testing.T) {
	// Two lines chained before the key was configured, then one signed under it.
	path := unsignedAuditFile(t, 2)
	store, err := auditstore.Open(path, []byte("cli-test-key-material"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("admin_config_read", "authorized_by", "admin"); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	env := auditTestEnv(nil)
	code := Audit([]string{"verify", "--file", path, "--key-env", auditTestKeyVar}, env)
	stdout, stderr := out(env)
	if code != 1 || strings.Count(stdout, "mac_missing") != 2 || !strings.Contains(stdout, "FAIL") {
		t.Errorf("exit %d, stdout %q, stderr %q; want 1 and mac_missing on the two unsigned lines: a key was given", code, stdout, stderr)
	}
	env = auditTestEnv(nil)
	code = Audit([]string{"verify", "--file", path, "--key-env", auditTestKeyVar, "--allow-unsigned-prefix", "--json"}, env)
	stdout, stderr = out(env)
	if code != 0 || !strings.Contains(stdout, `"ok":true`) || !strings.Contains(stdout, `"signed_lines":1`) || !strings.Contains(stdout, `"unsigned_chained_lines":2`) {
		t.Errorf("exit %d, stdout %q, stderr %q; want 0 and the unsigned count in the JSON report", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "2 chained lines carry no mac") {
		t.Errorf("stderr must say the key could not be checked against those lines: %q", stderr)
	}
	// Nothing signed at all is never a prefix, flag or no flag.
	env = auditTestEnv(nil)
	if code := Audit([]string{"verify", "--file", unsignedAuditFile(t, 2), "--key-env", auditTestKeyVar, "--allow-unsigned-prefix"}, env); code != 1 {
		stdout, stderr := out(env)
		t.Errorf("a key on a file with no signed line must fail even with the flag: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	} else if stdout, _ := out(env); !strings.Contains(stdout, "file: no_signed_lines") {
		t.Errorf("the whole-file problem must be named: %q", stdout)
	}
	env = auditTestEnv(nil)
	if code := Audit([]string{"verify", "--file", signedAuditFile(t, 2), "--key-env", auditTestKeyVar, "--allow-unsigned-prefix"}, env); code != 0 {
		stdout, stderr := out(env)
		t.Errorf("a fully signed file verifies with the flag too: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	} else if _, stderr := out(env); strings.Contains(stderr, "carry no mac") {
		t.Errorf("no note when every chained line is signed: %q", stderr)
	}
}

func TestAuditVerifyNamesAWholeFileProblem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.jsonl")
	legacy := "{\"time\":\"2026-10-01T00:00:00Z\",\"msg\":\"admin_config_read\",\"fields\":{\"authorized_by\":\"admin\"}}\n"
	if err := os.WriteFile(path, []byte(legacy+legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	env := auditTestEnv(nil)
	code := Audit([]string{"verify", "--file", path, "--key-env", auditTestKeyVar}, env)
	stdout, stderr := out(env)
	if code != 1 || !strings.Contains(stdout, "file: no_signed_lines") || !strings.Contains(stdout, "2 legacy") {
		t.Errorf("exit %d, stdout %q, stderr %q; want 1 and the whole-file problem named without a line number", code, stdout, stderr)
	}
}

func TestAuditVerifyNotesAnEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := auditTestEnv(nil)
	code := Audit([]string{"verify", "--file", path, "--key-env", auditTestKeyVar}, env)
	stdout, stderr := out(env)
	if code != 0 || !strings.Contains(stdout, "0 lines") || !strings.Contains(stderr, "0 lines: a new or emptied file") {
		t.Errorf("exit %d, stdout %q, stderr %q; want 0 with a stderr note that an empty file is new or emptied", code, stdout, stderr)
	}
	env = auditTestEnv(nil)
	if code := Audit([]string{"verify", "--file", path}, env); code != 0 {
		t.Errorf("without a key too: exit %d", code)
	} else if _, stderr := out(env); !strings.Contains(stderr, "0 lines: a new or emptied file") {
		t.Errorf("the note does not depend on a key: %q", stderr)
	}
}
