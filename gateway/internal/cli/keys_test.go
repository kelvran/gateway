package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedRand yields a deterministic 32-byte secret: 64 hex "42"s.
func fixedRand() io.Reader { return bytes.NewReader(bytes.Repeat([]byte{0x42}, 64)) }

func fixedSecretHex() string { return strings.Repeat("42", 32) }

func runKeysCmd(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Keys(args, IO{Stdout: &out, Stderr: &errb, Getenv: func(k string) string { return env[k] }, Rand: fixedRand()})
	return code, out.String(), errb.String()
}

func TestKeysUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
		msg  string
	}{
		{nil, 2, "usage: kelvran keys"},
		{[]string{"frob"}, 2, `unknown verb "frob"`},
		{[]string{"create"}, 2, "a key name is required"},
		{[]string{"create", "a", "b"}, 2, `unexpected argument "b"`},
		{[]string{"list", "extra"}, 2, `unexpected argument "extra"`},
		{[]string{"-h"}, 0, "usage: kelvran keys"},
		{[]string{"create", "x", "--budget", "-1"}, 2, "--budget must not be negative"},
		{[]string{"create", "x", "--budget", "ten"}, 2, "is not a decimal amount"},
		{[]string{"create", "x", "--reset", "yearly"}, 2, "--reset \"yearly\""},
		{[]string{"create", "x", "--warn", "80"}, 2, "--warn \"80\" must be a fraction"},
		{[]string{"create", "x", "--warn", "NaN"}, 2, "--warn \"NaN\" must be a fraction"},
		{[]string{"create", "x", "--models", "a,,b"}, 2, "empty entry"},
		{[]string{"create", "x", "--models", "a,a"}, 2, `lists "a" twice`},
		{[]string{"create", "x", "--expires", "soon"}, 2, "--expires \"soon\" is not RFC 3339"},
		{[]string{"create", "x", "--expires", "2000-01-01T00:00:00Z"}, 1, "is not in the future"},
		{[]string{"rotate", "x", "--grace", "1500ms"}, 2, "whole seconds"},
		{[]string{"rotate", "x", "--grace", "-1s"}, 2, "non-negative"},
		{[]string{"create", " x"}, 2, "surrounding whitespace"},
		{[]string{"list"}, 2, "no admin token resolved"},
		{[]string{"list", "--spend", "--json"}, 2, "KELVRAN_ADMIN_TOKEN) and no --config"},
	} {
		code, stdout, stderr := runKeysCmd(t, nil, tc.args...)
		if code != tc.want || !strings.Contains(stderr, tc.msg) || (tc.want != 0 && stdout != "") {
			t.Errorf("%v: exit %d (want %d), stdout %q, stderr %q", tc.args, code, tc.want, stdout, stderr)
		}
	}
}

func TestParseExpiresAndGrace(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 500, time.UTC)
	for in, want := range map[string]string{
		"30d":                       "2026-11-09T12:00:00Z",
		"720h":                      "2026-11-09T12:00:00Z",
		"2030-01-01T00:00:00Z":      "2030-01-01T00:00:00Z",
		"2030-01-01T05:30:00+05:30": "2030-01-01T00:00:00Z", // rendered in UTC
		"":                          "",
	} {
		got, err := parseExpires(in, now)
		if err != nil {
			t.Errorf("parseExpires(%q): %v", in, err)
			continue
		}
		if s := (keySpec{expiresAt: got}).expiresText(); s != want {
			t.Errorf("parseExpires(%q) = %q, want %q", in, s, want)
		}
	}
	if _, err := parseExpires("2026-10-10T12:00:00Z", now); err == nil || !strings.Contains(err.Error(), "not in the future") {
		t.Errorf("an instant equal to now must be refused: %v", err)
	}
	if _, err := parseExpires("0d", now); err == nil {
		t.Error("0d must be refused")
	}
	for in, want := range map[string]int{"": 0, "10m": 600, "1h30m": 5400, "0s": 0} {
		if got, err := parseGrace(in); err != nil || got != want {
			t.Errorf("parseGrace(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}

func TestSpellEntryName(t *testing.T) {
	for in, want := range map[string]string{
		"team-beta":   "team-beta",
		"my:key":      `"my:key"`,
		"'odd":        `"'odd"`,
		"a b":         "a b",
		"日本":          "日本",
		"has.dots_ok": "has.dots_ok",
	} {
		if got, err := spellEntryName(in); err != nil || got != want {
			t.Errorf("spellEntryName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", " lead", "trail ", "has#hash", "new\nline", "cr\r", `"wrapped"`, `'wrapped'`, `co:lon"quote`} {
		if _, err := spellEntryName(in); err == nil || !strings.Contains(err.Error(), "create it online") {
			t.Errorf("spellEntryName(%q) must refuse with the online pointer: %v", in, err)
		}
	}
}

func TestReplaceValueTokenKeepsQuotingSpacingAndComments(t *testing.T) {
	for raw, want := range map[string]string{
		`    key_hash: "old"   # note`: `    key_hash: "new"   # note`,
		`key_hash:   'old'`:            `key_hash:   'new'`,
		`key_hash: old # c`:            `key_hash: new # c`,
		"key_hash: \"old\"\r":          "key_hash: \"new\"\r",
		`billing: "sub:123"#acct`:      `billing: "new"#acct`,
	} {
		colon := strings.Index(raw, ":")
		got, err := replaceValueToken(raw, colon, "new")
		if err != nil || got != want {
			t.Errorf("replaceValueToken(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := replaceValueToken("key_hash:   # nothing", 8, "x"); err == nil {
		t.Error("a line with no value must be refused")
	}
}

func TestResetNameRoundTrip(t *testing.T) {
	for secs, want := range map[int]string{0: "never", 86400: "daily", 604800: "weekly", 2592000: "monthly", 3600: "3600s"} {
		if got := resetName(secs); got != want {
			t.Errorf("resetName(%d) = %q, want %q", secs, got, want)
		}
	}
}

// writeKeysFixture writes text to dir/config.yaml with mode and returns the path.
func writeKeysFixture(t *testing.T, dir, text string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(text), mode); err != nil { //nolint:gosec // G306: the fixture's mode is the point
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // G304: test fixture
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
