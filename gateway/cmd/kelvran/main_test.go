package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunWithoutACommandPrintsUsageAndExits2(t *testing.T) {
	var out, errb strings.Builder
	if code := run(nil, &out, &errb); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if out.Len() != 0 || !strings.Contains(errb.String(), "usage: kelvran") {
		t.Errorf("usage must go to stderr only: stdout=%q stderr=%q", out.String(), errb.String())
	}
}

func TestRunUnknownCommandExits2(t *testing.T) {
	var out, errb strings.Builder
	if code := run([]string{"frobnicate"}, &out, &errb); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), `unknown command "frobnicate"`) {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunVersionPrintsTheBuildIdentityOnStdout(t *testing.T) {
	for _, arg := range []string{"-version", "--version", "version"} {
		var out, errb strings.Builder
		if code := run([]string{arg}, &out, &errb); code != 0 {
			t.Errorf("%s: exit = %d", arg, code)
		}
		if !strings.HasPrefix(out.String(), "kelvran dev (") || errb.Len() != 0 {
			t.Errorf("%s: stdout=%q stderr=%q", arg, out.String(), errb.String())
		}
	}
}

func TestRunHelpExits0(t *testing.T) {
	var out, errb strings.Builder
	if code := run([]string{"help"}, &out, &errb); code != 0 || !strings.Contains(errb.String(), "init") {
		t.Errorf("help: exit=%d stderr=%q", code, errb.String())
	}
}

// TestInitDryRunOutputPassesTheGatewaysValidate is the acceptance job's
// pipe, in-process: `kelvran init --dry-run --provider openai` must print a
// YAML document that the real kelvran-gateway binary's -validate accepts.
func TestInitDryRunOutputPassesTheGatewaysValidate(t *testing.T) {
	dir := t.TempDir()
	gateway := filepath.Join(dir, "kelvran-gateway")
	build := exec.Command("go", "build", "-o", gateway, "../gateway") //nolint:gosec // G204: a fixed go build of a sibling package into this test's temp dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the gateway binary: %v\n%s", err, out)
	}
	var yaml, errb strings.Builder
	if code := run([]string{"init", "--dry-run", "--provider", "openai"}, &yaml, &errb); code != 0 {
		t.Fatalf("init --dry-run: exit %d, stderr %s", code, errb.String())
	}
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(yaml.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	validate := exec.Command(gateway, "-config", cfg, "-validate") //nolint:gosec // G204: the binary this test just built, with fixed flags
	out, err := validate.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "config is valid" {
		t.Fatalf("-validate on init's output: %v\n%s\n--- yaml ---\n%s", err, out, yaml.String())
	}
}

func TestRunDoctorDispatchesAndReportsAMissingConfig(t *testing.T) {
	var out, errb strings.Builder
	if code := run([]string{"doctor", "--config", filepath.Join(t.TempDir(), "absent.yaml")}, &out, &errb); code != 1 {
		t.Errorf("exit = %d, want 1 (the config does not load)", code)
	}
	if !strings.Contains(out.String(), "config.load") {
		t.Errorf("stdout must carry the config.load row: %q", out.String())
	}
}
