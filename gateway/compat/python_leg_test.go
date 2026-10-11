package compat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const pythonLegTimeout = 5 * time.Minute

// pythonLegCases is how many pytest cases python/tests holds. Pinned so a
// mis-named test file cannot shrink the leg while the gate stays green;
// update it when a case is added there.
const pythonLegCases = 12

// runPythonLeg runs the official anthropic and openai Python packages' tests
// (python/tests) in their own uv project against whatever env names. uv is
// part of the gate: its absence fails the run rather than skipping it.
func runPythonLeg(t *testing.T, env []string) (string, error) {
	t.Helper()
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Fatalf("uv is not on PATH; the Python leg is part of the merge gate and does not skip: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pythonLegTimeout)
	defer cancel()
	// --tb=short: a long traceback prints the failing test's arguments, and
	// one of them is the run's gateway credential -- throwaway, but no reason
	// to put it in a CI log.
	cmd := exec.CommandContext(ctx, uv, "run", "--frozen", "--", "pytest", "-q", "--tb=short", "-p", "no:cacheprovider", "tests") //nolint:gosec // uv resolved from PATH, fixed arguments
	cmd.Dir = "python"
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// pythonEnv is the runner's environment minus anything that could steer the
// SDKs or that a test process has no business inheriting -- the SDKs' own
// ANTHROPIC_*/OPENAI_* variables, a stale KELVRAN_COMPAT_*, cloud and
// credential-shaped variables, and proxies (httpx, unlike Go's net/http, does
// not exempt loopback from HTTPS_PROXY) -- plus the gateway's address and
// credential when withGateway is set.
func pythonEnv(withGateway bool) []string {
	env := []string{"NO_PROXY=127.0.0.1,localhost"}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if dropFromPythonEnv(name) {
			continue
		}
		env = append(env, kv)
	}
	if withGateway {
		env = append(env,
			"KELVRAN_COMPAT_URL="+gatewayURL,
			"KELVRAN_COMPAT_CREDENTIAL="+gatewayCredential,
			"KELVRAN_COMPAT_MODEL="+modelAnthropic,
		)
	}
	return env
}

func dropFromPythonEnv(name string) bool {
	upper := strings.ToUpper(name)
	switch {
	case strings.HasPrefix(upper, "KELVRAN_COMPAT_"), strings.HasPrefix(upper, "ANTHROPIC_"), strings.HasPrefix(upper, "OPENAI_"), strings.HasPrefix(upper, "AWS_"):
		return true
	case strings.HasSuffix(upper, "_TOKEN"), strings.HasSuffix(upper, "_KEY"), strings.Contains(upper, "SECRET"):
		return true
	case upper == "HTTP_PROXY", upper == "HTTPS_PROXY", upper == "ALL_PROXY", upper == "NO_PROXY":
		return true
	}
	return false
}

func TestPythonLegRunsTheOfficialPackagesAgainstTheGateway(t *testing.T) {
	out, err := runPythonLeg(t, pythonEnv(true))
	if err != nil {
		t.Fatalf("python leg failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "skipped") {
		t.Errorf("python leg skipped something; the gate must exercise every case:\n%s", out)
	}
	if want := fmt.Sprintf("%d passed", pythonLegCases); !strings.Contains(out, want) {
		t.Errorf("python leg summary lacks %q; every case in python/tests must run:\n%s", want, lastLines(out, 3))
	}
	t.Logf("python leg:\n%s", lastLines(out, 3))
}

// The gate must bite: without a gateway the leg fails and names the variable,
// rather than collecting zero tests or skipping its way to green.
func TestPythonLegFailsNotSkipsWithoutAGateway(t *testing.T) {
	out, err := runPythonLeg(t, pythonEnv(false))
	if err == nil {
		t.Fatalf("python leg passed with no gateway configured:\n%s", out)
	}
	if !strings.Contains(out, "KELVRAN_COMPAT_URL is not set: the compat leg runs only against a live gateway") {
		t.Errorf("failure does not carry conftest's own message for the missing variable:\n%s", out)
	}
	// pytest's summary line: a session fixture that fails is reported as an
	// error at setup of every case that needs it ("12 errors"), so count that
	// as failing; nothing may pass, be skipped or collect into "no tests ran".
	failing := strings.Contains(out, " error") || strings.Contains(out, " failed")
	if !failing || strings.Contains(out, " passed") || strings.Contains(out, "skipped") || strings.Contains(out, "no tests ran") {
		t.Errorf("want a run where every case failed and none passed or skipped:\n%s", out)
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
