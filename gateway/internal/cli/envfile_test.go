package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEnvFileReadsTheSystemdGrammar(t *testing.T) {
	const src = "# credentials\n; also a comment\n\nPLAIN=one\nDQ=\"two words\"\nSQ='three'\nCONT=four \\\nfive\nEMPTY=\nSPACED = six \n"
	got, err := parseEnvFile(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"PLAIN": "one", "DQ": "two words", "SQ": "three", "CONT": "four \nfive", "EMPTY": "", "SPACED": "six"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("parsed %d entries, want %d: %v", len(got), len(want), got)
	}
}

func TestParseEnvFileRejectsMalformedLines(t *testing.T) {
	for _, src := range []string{"NOEQUALS\n", "=novalue\n", "HAS SPACE=x\n", "TRAIL=x \\\n"} {
		if _, err := parseEnvFile(strings.NewReader(src)); err == nil {
			t.Errorf("%q parsed without error", src)
		}
	}
}

func TestEnvSourceFileValuesOverrideTheProcessAndEmptyMeansUnset(t *testing.T) {
	src := envSource{files: map[string]string{"A": "file", "B": ""}, getenv: func(k string) string { return map[string]string{"B": "proc", "C": "proc"}[k] }, hasFile: true}
	if !src.isSet("A") || src.value("A") != "file" {
		t.Error("A must come from the file")
	}
	if src.isSet("B") {
		t.Error("an empty file value counts as unset even when the process has it (the file is what the gateway would see)")
	}
	if !src.isSet("C") || src.isSet("D") {
		t.Error("C comes from the process; D is unset everywhere")
	}
}

func TestLoadEnvFilesLaterFilesWinAndErrorsNameTheFile(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.env")
	b := filepath.Join(dir, "b.env")
	if err := os.WriteFile(a, []byte("X=1\nY=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("Y=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadEnvFiles([]string{a, b})
	if err != nil || got["X"] != "1" || got["Y"] != "2" {
		t.Errorf("merge = %v, %v", got, err)
	}
	if _, err := loadEnvFiles([]string{filepath.Join(dir, "missing.env")}); err == nil || !strings.Contains(err.Error(), "missing.env") {
		t.Errorf("a missing file must be an error naming it: %v", err)
	}
}
