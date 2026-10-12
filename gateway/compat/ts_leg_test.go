//go:build compat_ts

package compat

// The nightly TypeScript leg (RFC-1 §11; item 11 slice S13; owner decision
// G15 of 2026-10-10 with the ADR 0003 test-tooling note): the official
// @anthropic-ai/sdk and Claude Code itself (claude -p) against the gateway the
// harness started. Behind a build tag because it needs node and an `npm ci`
// of .github/compat-ts/, which the PR gate does not have; the nightly
// workflow (.github/workflows/compat-ts-nightly.yml) runs it in a
// digest-pinned node container. It is non-blocking: the workflow has no
// pull_request or push trigger and is not among main's required status checks.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	tsDirEnv     = "KELVRAN_COMPAT_TS_DIR"
	tsLegTimeout = 4 * time.Minute
	// The variable names Claude Code reads (docs/how-to/clients/claude-code.md):
	// the gateway address without /v1, and the credential in its x-api-key form.
	claudeCodeBaseURLEnv    = "ANTHROPIC_BASE_URL"
	claudeCodeCredentialEnv = "ANTHROPIC_API_KEY"
	claudeCodeConfigDirEnv  = "CLAUDE_CONFIG_DIR"
)

// tsDir is the directory holding package.json, sdk.mjs and the installed
// node_modules -- .github/compat-ts at the repo root unless the env names it.
func tsDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv(tsDirEnv)
	if dir == "" {
		dir = filepath.Join("..", "..", ".github", "compat-ts")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("%s: %v", tsDirEnv, err)
	}
	if _, err := os.Stat(filepath.Join(abs, "node_modules", ".bin", "claude")); err != nil {
		t.Fatalf("%s has no installed node_modules (run `npm ci` there first): %v", abs, err)
	}
	return abs
}

// nodeEnv is the minimal environment for every node process the leg starts:
// the toolchain path, a private HOME (Claude Code writes its state there,
// never into the runner's), and no proxies for loopback.
func nodeEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"TMPDIR=" + home,
		"NO_PROXY=127.0.0.1,localhost",
	}
}

// sdkEnv adds the gateway's address and credential under the names sdk.mjs
// reads.
func sdkEnv(home string) []string {
	return append(nodeEnv(home),
		"KELVRAN_COMPAT_URL="+gatewayURL,
		"KELVRAN_COMPAT_CREDENTIAL="+gatewayCredential,
		"KELVRAN_COMPAT_MODEL="+modelAnthropic,
	)
}

// claudeCodeEnv adds what Claude Code reads and nothing else: its config dir
// under the private HOME, the gateway as ANTHROPIC_BASE_URL, the credential
// in its x-api-key form, the updater, telemetry and non-essential traffic off,
// CI, and -- for the how-to's remedy run only -- the pre-release betas off.
func claudeCodeEnv(home string, betasOff bool) []string {
	env := append(nodeEnv(home),
		claudeCodeConfigDirEnv+"="+filepath.Join(home, "cfg"),
		claudeCodeBaseURLEnv+"="+gatewayURL,
		claudeCodeCredentialEnv+"="+gatewayCredential,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_AUTOUPDATER=1",
		"DISABLE_TELEMETRY=1",
		"CI=true",
	)
	if betasOff {
		env = append(env, "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1")
	}
	return env
}

func TestTypeScriptLegSDK(t *testing.T) {
	dir := tsDir(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("node is not on PATH; the TypeScript leg does not skip: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), tsLegTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, filepath.Join(dir, "sdk.mjs")) //nolint:gosec // node from PATH, a fixed script in the leg's own directory
	cmd.Dir = dir
	cmd.Env = sdkEnv(t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("@anthropic-ai/sdk leg failed: %v\n%s", err, out)
	}
	summary, ok := sdkSummary(string(out))
	if !ok || summary.Passed != sdkLegChecks || summary.Failed != 0 {
		t.Errorf("@anthropic-ai/sdk leg summary = %+v (found: %t), want %d passed and 0 failed:\n%s", summary, ok, sdkLegChecks, out)
	}
	t.Logf("@anthropic-ai/sdk leg:\n%s", lastLines(string(out), 2))
}

// sdkLegChecks is how many check() calls sdk.mjs makes; pinned so a dropped
// check cannot shrink the leg silently. Update it with the script.
const sdkLegChecks = 7

// sdkSummary finds sdk.mjs's final summary line, whatever its key order.
func sdkSummary(out string) (struct {
	Summary bool `json:"summary"`
	Passed  int  `json:"passed"`
	Failed  int  `json:"failed"`
}, bool) {
	var summary struct {
		Summary bool `json:"summary"`
		Passed  int  `json:"passed"`
		Failed  int  `json:"failed"`
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if json.Unmarshal([]byte(line), &summary) == nil && summary.Summary {
			return summary, true
		}
	}
	return summary, false
}

// claudeCodeResult is the shape of `claude -p --output-format json`.
type claudeCodeResult struct {
	Type     string `json:"type"`
	Subtype  string `json:"subtype"`
	IsError  bool   `json:"is_error"`
	Result   string `json:"result"`
	NumTurns int    `json:"num_turns"`
}

// Claude Code is the P0 client of the Anthropic ingress. `claude -p` runs one
// headless turn against the gateway with the credential in its x-api-key
// form, the updater and telemetry off, stdin at /dev/null (in print mode a
// non-TTY stdin is read until EOF, and a never-closing pipe hangs it), and a
// private HOME -- twice: with CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1, the
// how-to's remedy for a translate hop, and with Claude Code's own defaults
// (every beta and body member it ships with, which an anthropic deployment
// receives verbatim), because the system-parts regression of 2026-10-11 was
// found only by a defaults run. Each turn must reach the deployment at least
// once and end without an error; what the deployment saw is pinned the way
// the Go SDK test pins it, and each turn leaves its own artifacts.
func TestTypeScriptLegClaudeCode(t *testing.T) {
	dir := tsDir(t)
	cases := []struct {
		name, suffix string
		betasOff     bool
	}{
		{"betas off (the how-to's remedy)", "", true},
		{"defaults", "-defaults", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runClaudeCodeTurn(t, dir, tc.suffix, tc.betasOff)
		})
	}
}

func runClaudeCodeTurn(t *testing.T, dir, artifactSuffix string, betasOff bool) {
	t.Helper()
	anthropicMock.reset()
	home := claudeCodeHome(t)
	ctx, cancel := context.WithTimeout(context.Background(), legTimeout())
	defer cancel()
	claude := filepath.Join(dir, "node_modules", ".bin", "claude")
	cmd := exec.CommandContext(ctx, claude, "-p", "say ok", "--model", modelAnthropic, "--output-format", "json") //nolint:gosec // the leg's own installed binary, fixed arguments
	cmd.Dir = home
	cmd.Env = claudeCodeEnv(home, betasOff)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	writeArtifact(t, "claude-code"+artifactSuffix+"-stdout.json", stdout.String())
	writeArtifact(t, "claude-code"+artifactSuffix+"-stderr.txt", stderr.String())
	writeArtifact(t, "claude-code"+artifactSuffix+"-upstream-trace.txt", upstreamTrace(anthropicMock.all()))
	if err != nil {
		t.Fatalf("claude -p failed: %v\n--- stdout ---\n%s\n--- stderr ---\n%s", err, stdout.String(), lastLines(stderr.String(), 20))
	}
	// The home's nine-digit run must have reached the gateway inside a
	// /v1/messages body (Claude Code writes its paths into the system prompt),
	// or a passing turn proves nothing about the ssn rule it exists to prove.
	carried := false
	for _, r := range anthropicMock.all() {
		if r.Path == "/v1/messages" && r.bodyHas(claudeCodeHomeDigitRun) {
			carried = true
			break
		}
	}
	if !carried {
		t.Fatalf("no /v1/messages request body carried the home's digit run %q; the leg no longer exercises the ssn rule", claudeCodeHomeDigitRun)
	}
	var result claudeCodeResult
	if err := json.Unmarshal([]byte(stdout.String()), &result); err != nil {
		t.Fatalf("claude -p output is not one JSON object (%v):\n%s", err, stdout.String())
	}
	if result.Type != "result" || result.IsError || !strings.Contains(result.Result, expectedStreamText) {
		t.Errorf("claude -p result = %+v, want a non-error result carrying the mock's text %q", result, expectedStreamText)
	}

	// What the deployment saw: the deployment's credential (never the
	// client's), Claude Code's anthropic-beta values forwarded verbatim on the
	// passthrough path (the User-Agent is not forwarded: the deployment sees
	// Go's), and the model rewritten.
	posts := 0
	for _, fwd := range anthropicMock.all() {
		if fwd.Path != "/v1/messages" {
			continue
		}
		posts++
		if fwd.Header.Get("X-Api-Key") != upstreamCredential || fwd.Header.Get("Authorization") != "" {
			t.Errorf("upstream auth headers: x-api-key is the deployment's: %t, authorization present: %t", fwd.Header.Get("X-Api-Key") == upstreamCredential, fwd.Header.Get("Authorization") != "")
		}
		assertClientCredentialNotForwarded(t, fwd)
		// Claude Code's own beta value is dated and changes with its releases
		// (Dependabot bumps the pin); the pin here is that the header reached
		// the deployment at all, with the value family Claude Code sends.
		if !strings.Contains(fwd.Header.Get("Anthropic-Beta"), claudeCodeBetaMarker) {
			t.Errorf("anthropic-beta %q did not reach the deployment verbatim", fwd.Header.Get("Anthropic-Beta"))
		}
		if fwd.jsonBody(t)["model"] != upstreamModelID {
			t.Errorf("forwarded model %v, want %q", fwd.jsonBody(t)["model"], upstreamModelID)
		}
	}
	if posts == 0 {
		t.Fatal("Claude Code never reached the deployment through the gateway")
	}
	counts := anthropicMock.pathCounts()
	writeArtifact(t, "claude-code"+artifactSuffix+"-route-counts.json", mustJSON(counts))
	t.Logf("claude -p (betas off: %t): %d turns, upstream requests by path: %s", betasOff, result.NumTurns, mustJSON(counts))
}

// writeArtifact leaves a file in the artifact dir when the run names one.
func writeArtifact(t *testing.T, name, content string) {
	t.Helper()
	dir := os.Getenv(artifactDirEnv)
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // the operator names the artifact dir on purpose
		t.Logf("artifact dir: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil { //nolint:gosec // same
		t.Logf("artifact %s: %v", name, err)
	}
}

// all returns a copy of every request since the last reset, in order.
func (rec *recorder) all() []recordedRequest {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]recordedRequest, len(rec.reqs))
	copy(out, rec.reqs)
	return out
}

// pathCounts is the number of requests per path since the last reset -- the
// nightly leg records it as an artifact (Claude Code's turn count per route is
// tracked, never asserted).
func (rec *recorder) pathCounts() map[string]int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	counts := map[string]int{}
	for _, r := range rec.reqs {
		counts[r.Path]++
	}
	return counts
}

// claudeCodeHomeDigitRun is a word-bounded nine-digit run with a valid SSN
// shape (area 349, group 52, serial 1234: inside every allowed range) that
// the home directory's path carries ON PURPOSE. Claude Code writes its own
// paths into the system prompt (the memory directory, the working
// directory) and the gateway's default guardrail scans that text; until
// 2026-10-12 the ssn detector blocked such a run outright, so the leg gave
// Claude Code a letters-only home and tested the dice rather than the
// gateway. The detector now needs an SSN cue before a bare run
// (internal/guardrail/ssn.go), and this run proves it on the real binary:
// a regression in that rule blocks every turn of the leg with 400
// content_policy_violation.
const claudeCodeHomeDigitRun = "349521234"

// claudeCodeHome is a private HOME whose path carries
// claudeCodeHomeDigitRun followed by random letters for uniqueness.
func claudeCodeHome(t *testing.T) string {
	t.Helper()
	const letters = "abcdefghijklmnopqrstuvwxyz"
	for attempt := 0; attempt < 16; attempt++ {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			t.Fatalf("random: %v", err)
		}
		suffix := make([]byte, len(raw))
		for i, b := range raw {
			suffix[i] = letters[int(b)%len(letters)]
		}
		home := filepath.Join(os.TempDir(), "kelvran-compat-claude-home-"+claudeCodeHomeDigitRun+"-"+string(suffix))
		if err := os.Mkdir(home, 0o700); err == nil {
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			return home
		}
	}
	t.Fatal("no free home directory after 16 attempts")
	return ""
}

// legTimeout is tsLegTimeout unless KELVRAN_COMPAT_TS_TIMEOUT names a Go
// duration -- a short run is how a looping client is diagnosed from the trace.
func legTimeout() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("KELVRAN_COMPAT_TS_TIMEOUT")); err == nil && d > 0 {
		return d
	}
	return tsLegTimeout
}

// upstreamTrace is one line per request the deployment saw -- path, user
// agent, stream flag, tool count, message count, the last message's role and
// content types -- enough to read a client's turn structure, never a body.
func upstreamTrace(reqs []recordedRequest) string {
	var b strings.Builder
	for i, r := range reqs {
		var body struct {
			Stream   bool              `json:"stream"`
			Tools    []json.RawMessage `json:"tools"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(r.Body, &body)
		lastRole, lastTypes := "", ""
		if n := len(body.Messages); n > 0 {
			lastRole = body.Messages[n-1].Role
			// Anthropic's content is either a string or an array of blocks.
			var blocks []struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(body.Messages[n-1].Content, &blocks) == nil {
				types := make([]string, 0, len(blocks))
				for _, bl := range blocks {
					types = append(types, bl.Type)
				}
				lastTypes = strings.Join(types, ",")
			} else {
				lastTypes = "string"
			}
		}
		fmt.Fprintf(&b, "%d %s ua=%q stream=%t tools=%d messages=%d last=%s[%s] bytes=%d\n",
			i+1, r.Path, r.Header.Get("User-Agent"), body.Stream, len(body.Tools), len(body.Messages), lastRole, lastTypes, len(r.Body))
	}
	return b.String()
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
