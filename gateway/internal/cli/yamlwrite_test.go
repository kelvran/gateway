package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// The fixture carries every feature RFC-3 verification (e) names: a column-0
// comment and a blank line before the target entry, an indent-4 comment and a
// column-0 commented-out line between two nested content lines, a trailing
// indent-4 comment after the entry's last content line, column-0 commentary
// after the block before deployments:, a comment containing the literal
// `virtual_keys:`, a colon name, and a blank line between entries.
var fixtureLines = []string{
	`listen_addr: "127.0.0.1:8080"`,
	``,
	`# Virtual keys: a column-0 comment above the block.`,
	`# A stray mention of virtual_keys: inside a comment is not the section.`,
	``,
	`virtual_keys:`,
	`  # a comment immediately above the first entry`,
	`  team-alpha:`, // 7
	`    key_hash: "HASH_ALPHA"   # the alpha hash`,
	`    budget_usd: 100.0`,
	`    # an indent-4 comment between two nested content lines`,
	`# a column-0 commented-out line between two nested content lines`,
	`    budget_reset_interval_seconds: 2592000`,
	`    rate_limit:`,
	`      burst: 20`,
	`      refill_per_second: 10`, // 15: last content line of team-alpha
	`    # a trailing indent-4 comment after the entry's last content line`,
	``,
	`  team-beta:`,
	`    key_hash: "HASH_BETA"`,
	`  "my:key":`,
	`    key_hash: "HASH_COLON"`, // 21: last content line of the block
	``,
	`# Column-0 commentary after the block, introducing the next section.`,
	`# It must stay attached to deployments:.`,
	`deployments:`,
	`  d1:`,
	`    model: "gpt-4o"`,
	`    provider: "openai"`,
	`    upstream_model: "gpt-4o"`,
	`    base_url: "https://api.openai.com/v1/chat/completions"`,
	`    api_key_env: "DOCTOR_TEST_UPSTREAM"`,
	``,
	`price_table:`,
	`  gpt-4o:`,
	`    prompt_per_token: 0.0000025`,
	`    completion_per_token: 0.00001`,
}

func fixtureText(extra ...string) string {
	lines := append([]string(nil), fixtureLines...)
	lines = append(lines, extra...)
	text := strings.Join(lines, "\n") + "\n"
	text = strings.ReplaceAll(text, "HASH_ALPHA", hashSecret("alpha"))
	text = strings.ReplaceAll(text, "HASH_BETA", hashSecret("beta"))
	return strings.ReplaceAll(text, "HASH_COLON", hashSecret("colon"))
}

func loadFixture(t *testing.T, p string) *controlplane.Config {
	t.Helper()
	cfg, err := controlplane.Load(p)
	if err != nil {
		t.Fatalf("Load(%s): %v", p, err)
	}
	return cfg
}

// keyByName finds a loaded key; Load sorts VirtualKeys by name, so indices
// follow the alphabet ("my:key" < "team-alpha" < "team-beta"), not the file.
func keyByName(t *testing.T, cfg *controlplane.Config, name string) controlplane.VirtualKeyConfig {
	t.Helper()
	for _, vk := range cfg.VirtualKeys {
		if vk.Name == name {
			return vk
		}
	}
	t.Fatalf("no key %q in %+v", name, cfg.VirtualKeys)
	return controlplane.VirtualKeyConfig{}
}

func TestOfflineDeleteRemovesExactlyTheSpan(t *testing.T) {
	p := writeKeysFixture(t, t.TempDir(), fixtureText(), 0o644)
	before := readFile(t, p)
	code, stdout, stderr := runKeysCmd(t, nil, "delete", "team-alpha", "--config", p)
	if code != 0 || !strings.Contains(stdout, `Deleted virtual key "team-alpha" from`) {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	// Expected: the original minus lines 7..15 (the entry line through its last content line).
	lines := strings.Split(before, "\n")
	want := strings.Join(append(append([]string(nil), lines[:7]...), lines[16:]...), "\n")
	if got := readFile(t, p); got != want {
		t.Errorf("delete did not remove exactly the span:\n--- got\n%s\n--- want\n%s", got, want)
	}
	after := readFile(t, p)
	for _, kept := range []string{"# a comment immediately above the first entry", "# a trailing indent-4 comment after the entry's last content line", "# It must stay attached to deployments:.", "# A stray mention of virtual_keys: inside a comment"} {
		if !strings.Contains(after, kept) {
			t.Errorf("a line outside the span was lost: %q", kept)
		}
	}
	for _, gone := range []string{"# an indent-4 comment between", "# a column-0 commented-out line", "team-alpha"} {
		if strings.Contains(after, gone) {
			t.Errorf("a line inside the span survived: %q", gone)
		}
	}
	cfg := loadFixture(t, p)
	if len(cfg.VirtualKeys) != 2 || cfg.VirtualKeys[0].Name != "my:key" || cfg.VirtualKeys[1].Name != "team-beta" {
		t.Errorf("keys after delete: %+v", cfg.VirtualKeys)
	}
	if !strings.Contains(stderr, "takes effect on the next gateway start") || strings.Contains(stderr, "persisted store") || strings.Contains(stderr, "in-memory only") {
		t.Errorf("a single-user config prints only the restart caveat: %s", stderr)
	}
}

func TestOfflineRotateChangesOnlyTheHashToken(t *testing.T) {
	p := writeKeysFixture(t, t.TempDir(), fixtureText(), 0o644)
	before := strings.Split(readFile(t, p), "\n")
	code, stdout, stderr := runKeysCmd(t, nil, "rotate", "team-alpha", "--config", p)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	after := strings.Split(readFile(t, p), "\n")
	if len(after) != len(before) {
		t.Fatalf("line count changed: %d -> %d", len(before), len(after))
	}
	newHash := hashSecret(fixedSecretHex())
	for i := range before {
		switch i {
		case 8:
			if want := `    key_hash: "` + newHash + `"   # the alpha hash`; after[i] != want {
				t.Errorf("key_hash line = %q, want %q", after[i], want)
			}
		default:
			if after[i] != before[i] {
				t.Errorf("line %d changed: %q -> %q", i+1, before[i], after[i])
			}
		}
	}
	if strings.Count(stdout, fixedSecretHex()) != 1 || strings.Contains(stderr, fixedSecretHex()) || strings.Contains(strings.Join(after, "\n"), fixedSecretHex()) {
		t.Errorf("the secret must appear once on stdout and nowhere else:\n%s\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "hard cut") || !strings.Contains(stdout, "a hard cut at the next gateway start") {
		t.Errorf("offline rotate must say it is a hard cut: %s %s", stdout, stderr)
	}
	if vk := keyByName(t, loadFixture(t, p), "team-alpha"); vk.KeyHash != newHash {
		t.Errorf("Load sees %q", vk.KeyHash)
	}
}

func TestOfflineCreatePlacesTheEntryBeforeTheBannerAndQuotesAColonName(t *testing.T) {
	p := writeKeysFixture(t, t.TempDir(), fixtureText(), 0o644)
	code, stdout, stderr := runKeysCmd(t, nil, "create", "my:key2", "--config", p, "--budget", "5", "--models", "gpt-4o", "--reset", "weekly", "--warn", "0.5", "--billing-subject", "acct-9")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	after := readFile(t, p)
	wantBlock := "    key_hash: \"" + hashSecret("colon") + "\"\n\n  \"my:key2\":\n    key_hash: \"" + hashSecret(fixedSecretHex()) + "\"\n    budget_usd: 5\n    budget_reset_interval_seconds: 604800\n    budget_warn_percent: 0.5\n    allowed_models:\n      gpt-4o: true\n    billing_subject_id: \"acct-9\"\n\n# Column-0 commentary after the block"
	if !strings.Contains(after, wantBlock) {
		t.Errorf("the entry must sit after the last content line and before the banner:\n%s", after)
	}
	cfg := loadFixture(t, p)
	var got *controlplane.VirtualKeyConfig
	for i := range cfg.VirtualKeys {
		if cfg.VirtualKeys[i].Name == "my:key2" {
			got = &cfg.VirtualKeys[i]
		}
	}
	if got == nil || !got.BudgetUSD.Equal(decimalFromString(t, "5")) || got.BudgetResetIntervalSeconds != 604800 || got.BudgetWarnPercent != 0.5 || len(got.AllowedModels) != 1 || got.AllowedModels[0] != "gpt-4o" || got.BillingSubjectID != "acct-9" {
		t.Errorf("Load round-trip: %+v", got)
	}
	if strings.Count(stdout, fixedSecretHex()) != 1 || !strings.Contains(stdout, "ANTHROPIC_BASE_URL=http://127.0.0.1:8080") {
		t.Errorf("stdout: %s", stdout)
	}
	// A second create of the same name refuses before any write.
	before := readFile(t, p)
	code, _, stderr = runKeysCmd(t, nil, "create", "my:key2", "--config", p)
	if code != 1 || !strings.Contains(stderr, "already exists") || strings.Contains(stderr, "duplicate key") || readFile(t, p) != before {
		t.Errorf("existing name: exit %d, stderr %s", code, stderr)
	}
	// --replace rewrites it: delete + append, every unspecified field dropped.
	code, _, _ = runKeysCmd(t, nil, "create", "my:key2", "--config", p, "--replace")
	if code != 0 {
		t.Fatalf("--replace: exit %d", code)
	}
	cfg = loadFixture(t, p)
	for _, vk := range cfg.VirtualKeys {
		if vk.Name == "my:key2" && (vk.BudgetUSD.IsPositive() || len(vk.AllowedModels) != 0) {
			t.Errorf("--replace must drop the old fields: %+v", vk)
		}
	}
}

func TestOfflineFourSpaceIndentAndQuotedName(t *testing.T) {
	text := strings.Join([]string{
		`listen_addr: "127.0.0.1:8080"`,
		`virtual_keys:`,
		`    'quoted-name':`,
		`        key_hash: "` + hashSecret("q") + `" # trailing`,
		`    other:`,
		`        key_hash: "` + hashSecret("o") + `"`,
		`deployments:`,
		`    d1:`,
		`        model: "gpt-4o"`,
		`        provider: "openai"`,
		`        upstream_model: "gpt-4o"`,
		`        base_url: "https://api.openai.com/v1/chat/completions"`,
		`        api_key_env: "DOCTOR_TEST_UPSTREAM"`,
		`price_table:`,
		`    gpt-4o:`,
		`        prompt_per_token: 0.0000025`,
		`        completion_per_token: 0.00001`,
	}, "\n") + "\n"
	p := writeKeysFixture(t, t.TempDir(), text, 0o644)
	if code, _, stderr := runKeysCmd(t, nil, "rotate", "quoted-name", "--config", p); code != 0 {
		t.Fatalf("rotate: exit %d %s", code, stderr)
	}
	if !strings.Contains(readFile(t, p), `        key_hash: "`+hashSecret(fixedSecretHex())+`" # trailing`) {
		t.Errorf("rotate must keep the 8-space indent and the trailing comment:\n%s", readFile(t, p))
	}
	if code, _, stderr := runKeysCmd(t, nil, "create", "third", "--config", p, "--budget", "1"); code != 0 {
		t.Fatalf("create: exit %d %s", code, stderr)
	}
	if !strings.Contains(readFile(t, p), "\n    third:\n        key_hash: \""+hashSecret(fixedSecretHex())+"\"\n        budget_usd: 1\ndeployments:") {
		t.Errorf("create must use the file's 4/8 indents and sit before deployments:\n%s", readFile(t, p))
	}
	if cfg := loadFixture(t, p); len(cfg.VirtualKeys) != 3 {
		t.Errorf("keys: %+v", cfg.VirtualKeys)
	}
}

func TestOfflineRefusalsLeaveTheFileUntouched(t *testing.T) {
	p := writeKeysFixture(t, t.TempDir(), fixtureText(), 0o644)
	before := readFile(t, p)
	for _, tc := range []struct {
		args []string
		want int
		msg  string
	}{
		{[]string{"create", "has#hash", "--config", p}, 2, "create it online"},
		{[]string{"create", `"wrapped"`, "--config", p}, 2, "create it online"},
		{[]string{"create", "a:b#c", "--config", p}, 2, "create it online"},
		{[]string{"create", "ok", "--config", p, "--models", "bad:model"}, 2, "cannot be a key"},
		{[]string{"rotate", "nobody", "--config", p}, 1, `virtual key "nobody" not found`},
		{[]string{"delete", "nobody", "--config", p}, 1, `virtual key "nobody" not found`},
		{[]string{"rotate", "team-alpha", "--config", p, "--grace", "10m"}, 2, "no offline meaning"},
		{[]string{"create", "late", "--config", p, "--expires", "2000-01-01T00:00:00Z"}, 1, "not in the future"},
	} {
		code, _, stderr := runKeysCmd(t, nil, tc.args...)
		if code != tc.want || !strings.Contains(stderr, tc.msg) {
			t.Errorf("%v: exit %d (want %d), stderr %s", tc.args, code, tc.want, stderr)
		}
		if readFile(t, p) != before {
			t.Fatalf("%v: the file changed", tc.args)
		}
	}
	// The last key cannot be deleted: the gateway would refuse to start.
	single := writeKeysFixture(t, t.TempDir(), strings.Replace(strings.Replace(fixtureText(), "  team-beta:\n    key_hash: \""+hashSecret("beta")+"\"\n", "", 1), "  \"my:key\":\n    key_hash: \""+hashSecret("colon")+"\"\n", "", 1), 0o644)
	if code, _, stderr := runKeysCmd(t, nil, "delete", "team-alpha", "--config", single); code != 1 || !strings.Contains(stderr, "only virtual key") {
		t.Errorf("deleting the last key: exit %d, stderr %s", code, stderr)
	}
	// A file Load rejects is refused untouched, with the redacted loader error.
	broken := writeKeysFixture(t, t.TempDir(), strings.Replace(fixtureText(), "deployments:\n", "deployments: {}\n", 1), 0o644)
	brokenBefore := readFile(t, broken)
	if code, _, stderr := runKeysCmd(t, nil, "create", "x", "--config", broken); code != 1 || !strings.Contains(stderr, "does not load") || readFile(t, broken) != brokenBefore {
		t.Errorf("unloadable config: exit %d, stderr %s", code, stderr)
	}
}

func TestOfflineKeepsModeAndSymlink(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o600} {
		p := writeKeysFixture(t, t.TempDir(), fixtureText(), mode)
		if code, _, stderr := runKeysCmd(t, nil, "rotate", "team-beta", "--config", p); code != 0 {
			t.Fatalf("mode %04o: exit %d %s", mode, code, stderr)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != mode {
			t.Errorf("mode %04o became %04o", mode, fi.Mode().Perm())
		}
		if os.Geteuid() != 0 {
			continue
		}
		if uid, gid, ok := fileOwner(fi); !ok || uid != 0 || gid != os.Getgid() {
			t.Errorf("root-run ownership: %d:%d ok=%v", uid, gid, ok)
		}
	}
	dir := t.TempDir()
	target := writeKeysFixture(t, dir, fixtureText(), 0o644)
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runKeysCmd(t, nil, "delete", "team-beta", "--config", link); code != 0 {
		t.Fatalf("via symlink: exit %d %s", code, stderr)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink must survive: %v %v", fi, err)
	}
	if strings.Contains(readFile(t, target), "team-beta") {
		t.Error("the target must have been rewritten")
	}
}

func TestOfflineCaveatsFollowTheAdminSection(t *testing.T) {
	cases := map[string][]string{
		"single-user":   nil,
		"team-no-store": {`admin:`, `  token_env: "DOCTOR_TEST_ADMIN"`},
		"persisted":     {`admin:`, `  token_env: "DOCTOR_TEST_ADMIN"`, `  persist_path: "/var/lib/kelvran-gateway/identity.db"`},
		"redis":         {`admin:`, `  token_env: "DOCTOR_TEST_ADMIN"`, `  redis_addr: "127.0.0.1:6379"`},
	}
	for name, extra := range cases {
		p := writeKeysFixture(t, t.TempDir(), fixtureText(extra...), 0o644)
		code, _, stderr := runKeysCmd(t, nil, "create", "fresh", "--config", p)
		if code != 0 {
			t.Fatalf("%s: exit %d %s", name, code, stderr)
		}
		restart := strings.Contains(stderr, "takes effect on the next gateway start")
		store := strings.Contains(stderr, `if "fresh" was ever created, rotated or deleted through the admin API, the persisted store (`)
		memory := strings.Contains(stderr, "in-memory only and this file wins on restart")
		switch name {
		case "single-user":
			if !restart || store || memory {
				t.Errorf("%s: %s", name, stderr)
			}
		case "team-no-store":
			if !restart || store || !memory {
				t.Errorf("%s: %s", name, stderr)
			}
		case "persisted":
			if !restart || !store || memory || !strings.Contains(stderr, "admin.persist_path /var/lib/kelvran-gateway/identity.db") {
				t.Errorf("%s: %s", name, stderr)
			}
		case "redis":
			if !restart || !store || memory || !strings.Contains(stderr, "admin.redis_addr 127.0.0.1:6379") {
				t.Errorf("%s: %s", name, stderr)
			}
		}
	}
}

func TestOfflineRotateOfAnExpiredKeyNeedsExpires(t *testing.T) {
	past := `    expires_at: "2000-01-01T00:00:00Z"`
	text := strings.Replace(fixtureText(), "  team-beta:\n    key_hash: \""+hashSecret("beta")+"\"\n", "  team-beta:\n    key_hash: \""+hashSecret("beta")+"\"\n"+past+"\n", 1)
	p := writeKeysFixture(t, t.TempDir(), text, 0o644)
	// Load itself accepts the past value: an already-expired key must not stop the gateway.
	if vk := keyByName(t, loadFixture(t, p), "team-beta"); vk.ExpiresAt.IsZero() {
		t.Fatalf("fixture must carry the past expiry: %+v", vk)
	}
	before := readFile(t, p)
	if code, _, stderr := runKeysCmd(t, nil, "rotate", "team-beta", "--config", p); code != 1 || !strings.Contains(stderr, "expired at 2000-01-01T00:00:00Z; pass --expires") || readFile(t, p) != before {
		t.Errorf("rotate without --expires: exit %d, stderr %s", code, stderr)
	}
	if code, _, stderr := runKeysCmd(t, nil, "rotate", "team-beta", "--config", p, "--expires", "2000-01-02T00:00:00Z"); code != 1 || !strings.Contains(stderr, "not in the future") || readFile(t, p) != before {
		t.Errorf("a past --expires: exit %d, stderr %s", code, stderr)
	}
	code, stdout, stderr := runKeysCmd(t, nil, "rotate", "team-beta", "--config", p, "--expires", "2030-06-01T00:00:00Z")
	if code != 0 || !strings.Contains(stdout, "expires_at: 2030-06-01T00:00:00Z") {
		t.Fatalf("rotate with --expires: exit %d %s %s", code, stdout, stderr)
	}
	if after := readFile(t, p); !strings.Contains(after, `    expires_at: "2030-06-01T00:00:00Z"`) || strings.Contains(after, "2000-01-01") {
		t.Errorf("the expires_at token must be rewritten in place:\n%s", after)
	}
	if vk := keyByName(t, loadFixture(t, p), "team-beta"); !vk.ExpiresAt.Equal(time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)) || vk.KeyHash != hashSecret(fixedSecretHex()) {
		t.Errorf("Load after rotate: %+v", vk)
	}
	// An entry without expires_at gains the line right after key_hash.
	code, _, stderr = runKeysCmd(t, nil, "rotate", "my:key", "--config", p, "--expires", "2031-01-01T00:00:00Z")
	if code != 0 || !strings.Contains(readFile(t, p), "  \"my:key\":\n    key_hash: \""+hashSecret(fixedSecretHex())+"\"\n    expires_at: \"2031-01-01T00:00:00Z\"\n") {
		t.Errorf("inserted expires_at: exit %d %s\n%s", code, stderr, readFile(t, p))
	}
}

func TestOfflineListReadsTheFile(t *testing.T) {
	p := writeKeysFixture(t, t.TempDir(), fixtureText(), 0o644)
	resolved, _ := filepath.EvalSymlinks(p)
	code, stdout, _ := runKeysCmd(t, nil, "list", "--config", p)
	if code != 0 || !strings.Contains(stdout, "from "+resolved) || !strings.Contains(stdout, "team-alpha  100 ") || !strings.Contains(stdout, "monthly") || !strings.Contains(stdout, "my:key") {
		t.Errorf("offline list: exit %d\n%s", code, stdout)
	}
	if code, _, stderr := runKeysCmd(t, nil, "list", "--config", p, "--spend"); code != 1 || !strings.Contains(stderr, "needs the admin API") {
		t.Errorf("offline --spend must fail closed: exit %d, %s", code, stderr)
	}
	code, stdout, _ = runKeysCmd(t, nil, "list", "--config", p, "--json")
	if code != 0 || !strings.Contains(stdout, `"id": "team-alpha"`) || strings.Contains(stdout, hashSecret("alpha")) {
		t.Errorf("offline --json lists ids and never hashes: exit %d\n%s", code, stdout)
	}
}

func TestCheckRewriteFailsClosed(t *testing.T) {
	p := writeKeysFixture(t, t.TempDir(), fixtureText(), 0o644)
	orig := loadFixture(t, p)
	same := []byte(readFile(t, p))
	// Identity rewrite with the right expectation passes.
	if err := checkRewrite(orig, same, p, append([]controlplane.VirtualKeyConfig(nil), orig.VirtualKeys...)); err != nil {
		t.Errorf("identity: %v", err)
	}
	// The same bytes with an expectation naming a different key: the delta check refuses.
	wrong := append([]controlplane.VirtualKeyConfig(nil), orig.VirtualKeys...)
	wrong[0].Name = "renamed"
	if err := checkRewrite(orig, same, p, wrong); err == nil || !strings.Contains(err.Error(), "differ from the intended change") {
		t.Errorf("wrong delta: %v", err)
	}
	// A rewrite that touches a field outside virtual_keys is refused.
	outside := []byte(strings.Replace(string(same), `listen_addr: "127.0.0.1:8080"`, `listen_addr: "127.0.0.1:9090"`, 1))
	if err := checkRewrite(orig, outside, p, append([]controlplane.VirtualKeyConfig(nil), orig.VirtualKeys...)); err == nil || !strings.Contains(err.Error(), "outside virtual_keys") {
		t.Errorf("outside change: %v", err)
	}
	// A candidate that does not load is refused with the loader's error redacted.
	if err := checkRewrite(orig, []byte("listen_addr: \"x\"\nvirtual_keys:\n  a:\n    budget_usd: 1\n"), p, nil); err == nil || !strings.Contains(err.Error(), "would not load") {
		t.Errorf("unloadable candidate: %v", err)
	}
}

func TestParseYAMLDocRefusesWhatTheLoaderRefuses(t *testing.T) {
	if _, err := parseYAMLDoc([]byte("a:\n\tb: 1\n")); err == nil || !strings.Contains(err.Error(), "tab") {
		t.Errorf("tab indent: %v", err)
	}
	if _, err := parseYAMLDoc([]byte("just words\n")); err == nil || !strings.Contains(err.Error(), "key: value") {
		t.Errorf("no colon: %v", err)
	}
	d, err := parseYAMLDoc([]byte("virtual_keys:\n  a:\n    key_hash: \"x\"\nvirtual_keys:\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.virtualKeysBlock(); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Errorf("duplicate section: %v", err)
	}
	d, _ = parseYAMLDoc([]byte("listen_addr: \"x\"\n# virtual_keys:\n"))
	if _, err := d.virtualKeysBlock(); err == nil || !strings.Contains(err.Error(), "no virtual_keys section") {
		t.Errorf("section only in a comment: %v", err)
	}
	d, _ = parseYAMLDoc([]byte("virtual_keys:\n    a:\n        key_hash: \"x\"\n  b:\n    key_hash: \"y\"\n"))
	if _, err := d.virtualKeysBlock(); err == nil || !strings.Contains(err.Error(), "irregular indentation") {
		t.Errorf("mixed entry indents: %v", err)
	}
}
