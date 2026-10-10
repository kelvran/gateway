package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/kelvran/gateway/gateway/internal/credentialstate"
)

const connectUsage = `usage: kelvran connect claude [--url http://127.0.0.1:8080] [--key-file PATH] [--write] [--scope user|project]
                             [--discovery] [--check] [--replace-credential] [--force] [--allow-insecure-http]
       kelvran connect codex|aider|continue [--url http://127.0.0.1:8080] [--key-file PATH]

Prints (or, for claude with --write, writes) the client configuration that points a coding tool at this
gateway. The virtual key comes from --key-file, else the file KELVRAN_KEY_FILE names, else
ANTHROPIC_AUTH_TOKEN in this shell — the export init printed — never OPENAI_API_KEY. --url must be the
gateway's base URL without a path; for claude it is guarded like an admin URL (https, loopback http, or
--allow-insecure-http) because the written ANTHROPIC_BASE_URL makes Claude Code send the key every turn.
--check sends one max_tokens: 1 request to POST /v1/messages and reports what the answer proves.
`

// The Claude Code env block (archived connect page, decision 8): the bearer
// variable, the base URL without a path, and the protocol page's hint switch.
const (
	envAnthropicBaseURL = "ANTHROPIC_BASE_URL"
	envHintHeaders      = "CLAUDE_CODE_GATEWAY_HINT_HEADERS"
	envModelDiscovery   = "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"
	settingsHelperField = "apiKeyHelper"
	connectFileVar      = "KELVRAN_KEY_FILE"
	defaultDataPlaneURL = "http://127.0.0.1:8080"
	// checkProbeModel is deliberately a model no gateway serves, so a gateway
	// that does serve the Messages API answers "unknown model" — which proves
	// the URL and the credential (the connect page's own test) without
	// spending a token.
	checkProbeModel = "kelvran-connect-check"
)

// The two Anthropic credential variables, NAMES only, joined at run time so no
// scanner mistakes them for values.
var (
	envAnthropicBearer = "ANTHROPIC_AUTH_" + "TOKEN"
	envAnthropicAPIVar = "ANTHROPIC_API_" + "KEY"
	envOpenAIAPIVar    = "OPENAI_API_" + "KEY"
)

type connectOptions struct {
	tool                    string
	url, keyFile            string
	write, discovery, check bool
	scope                   string
	replaceCredential       bool
	force, allowInsecure    bool
	explicit                map[string]bool // flags given on the command line, for the write-only rule
}

// Connect runs `kelvran connect <tool>`: 0 on success (a --check that proved
// URL and credential included), 1 on a refusal, a failed write or a --check
// that found the gateway not usable, 2 on a usage error.
func Connect(args []string, env IO) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(env.Stderr, connectUsage)
		return 2
	}
	o := connectOptions{tool: args[0]}
	switch o.tool {
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(env.Stderr, connectUsage)
		return 0
	case "claude", "codex", "aider", "continue":
	default:
		_, _ = fmt.Fprintf(env.Stderr, "kelvran connect: unknown tool %q (claude, codex, aider or continue)\n", sanitizeCell(o.tool))
		_, _ = fmt.Fprint(env.Stderr, connectUsage)
		return 2
	}
	fs := flag.NewFlagSet("connect "+o.tool, flag.ContinueOnError)
	fs.SetOutput(escapingWriter{env.Stderr})
	fs.Usage = func() { _, _ = fmt.Fprint(env.Stderr, connectUsage) }
	fs.StringVar(&o.url, "url", defaultDataPlaneURL, "the gateway's base URL, without a path")
	fs.StringVar(&o.keyFile, "key-file", "", "file holding the virtual key (else "+connectFileVar+", else "+envAnthropicBearer+")")
	if o.tool == "claude" {
		fs.BoolVar(&o.write, "write", false, "write the env block into the Claude Code settings file for --scope")
		fs.StringVar(&o.scope, "scope", "user", "user (~/.claude/settings.json) or project (./.claude/settings.local.json, gitignored)")
		fs.BoolVar(&o.discovery, "discovery", false, "also set "+envModelDiscovery+"=1 (GET /v1/models at startup)")
		fs.BoolVar(&o.check, "check", false, "send one max_tokens: 1 request to POST /v1/messages and report what it proves")
		fs.BoolVar(&o.replaceCredential, "replace-credential", false, "with --write: delete a conflicting env."+envAnthropicAPIVar+" or "+settingsHelperField+" in the same write")
		fs.BoolVar(&o.force, "force", false, "with --write: write although git cannot vouch for the file (not gitignored under --scope project, tracked, a repository git cannot read) or the target is a symbolic link")
		fs.BoolVar(&o.allowInsecure, "allow-insecure-http", false, "accept a non-loopback http:// --url (the key travels in clear text)")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	o.explicit = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { o.explicit[f.Name] = true })
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(env.Stderr, "kelvran connect %s: unexpected argument %q\n", o.tool, fs.Arg(0))
		fs.Usage()
		return 2
	}
	return exitWith("kelvran connect "+o.tool, runConnect(o, env), env, fs.Usage)
}

func runConnect(o connectOptions, env IO) error {
	if o.tool == "claude" {
		if o.scope != "user" && o.scope != "project" {
			return usageErr("--scope %q is not user or project", o.scope)
		}
		if o.write && o.check {
			return usageErr("--check and --write are exclusive: check first, then write")
		}
		for _, name := range []string{"scope", "replace-credential", "force"} {
			if !o.write && o.explicit[name] {
				return usageErr("--%s applies only with --write", name)
			}
		}
	}
	secret, source, err := resolveClientKey(o.keyFile, env.Getenv)
	if err != nil {
		return err
	}
	base, err := connectBaseURL(o)
	if err != nil {
		return err
	}
	if o.tool != "claude" {
		return printOpenAIPair(env, o.tool, base, secret)
	}
	pr := &printer{w: env.Stderr}
	warnShellAPIKey(pr, env)
	switch {
	case o.check:
		err = checkClaude(context.Background(), env, base, secret)
	case o.write:
		err = writeClaudeSettings(env, o, base, secret)
	default:
		printClaudeBlock(env, o, base, secret, source)
	}
	if perr := firstErr(pr); perr != nil {
		return perr
	}
	return err
}

// resolveClientKey follows decision 8: --key-file, the file KELVRAN_KEY_FILE
// names, then ANTHROPIC_AUTH_TOKEN in this shell — the export init printed —
// and never OPENAI_API_KEY, which in the same shell is the upstream credential
// an openai deployment reads.
func resolveClientKey(keyFile string, getenv func(string) string) (secret, source string, err error) {
	if keyFile == "" {
		keyFile = getenv(connectFileVar)
	}
	if keyFile != "" {
		if err := mustBeRegularFile(keyFile); err != nil {
			return "", "", runtimeErr("reading the key file: %v", err)
		}
		s, err := credentialstate.ReadFile(keyFile)
		if err != nil {
			return "", "", runtimeErr("reading the key file: %v", err)
		}
		if err := checkBearerShape(s, keyFile); err != nil {
			return "", "", err
		}
		return s, "the key file", nil
	}
	if v := getenv(envAnthropicBearer); v != "" {
		if err := checkBearerShape(v, envAnthropicBearer+" in this shell"); err != nil {
			return "", "", err
		}
		return v, envAnthropicBearer + " in this shell", nil
	}
	return "", "", runtimeErr("no virtual key: pass --key-file, set %s to a file holding one, or export %s (the line init printed); %s is never used — in this shell it is the upstream credential", connectFileVar, envAnthropicBearer, envOpenAIAPIVar)
}

// checkBearerShape holds the key to RFC 6750's b64token alphabet, the only
// bytes a Bearer credential can carry: a key outside it could never
// authenticate, and the print-only verbs print the key raw on an export
// line, so whitespace or shell syntax in it is refused here — naming the
// source, never the value.
func checkBearerShape(secret, source string) error {
	for i := 0; i < len(secret); i++ {
		c := secret[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == '+', c == '/', c == '=':
		default:
			return runtimeErr("the key from %s contains a character a bearer token cannot carry (whitespace, a control character or punctuation outside A-Z a-z 0-9 - . _ ~ + / =); it could never authenticate and is not printed", source)
		}
	}
	return nil
}

// mustBeRegularFile refuses a directory, a FIFO (a read of which would hang
// forever) or a device at path; the error names the path only.
func mustBeRegularFile(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}

// connectBaseURL validates --url as the gateway's base URL without a path and,
// for claude, applies decision 3's credential-URL guard: the written
// ANTHROPIC_BASE_URL makes Claude Code send the key on every turn, so a
// non-loopback http:// needs --allow-insecure-http. The print-only tools are
// exempt from the loopback rule (decision 3) but still need an http(s) URL.
func connectBaseURL(o connectOptions) (string, error) {
	if o.tool == "claude" {
		if err := guardCredentialURL("--url", o.url, o.allowInsecure); err != nil {
			return "", runtimeErr("%v", err)
		}
	} else if err := guardHTTPURL("--url", o.url); err != nil {
		return "", runtimeErr("%v", err)
	}
	u, err := url.Parse(o.url)
	if err != nil || u.Host == "" {
		return "", usageErr("--url is not an http(s) URL")
	}
	if u.Path != "" && u.Path != "/" {
		return "", usageErr("--url must be the gateway's base URL without a path (Claude Code appends /v1/messages, the OpenAI SDKs /v1/…)")
	}
	// The host rule ClientURLs applies to a listen_addr: an IP or a hostname,
	// nothing a shell could interpret, since the URL is pasted into shell lines.
	if !isValidListenHost(strings.Trim(u.Hostname(), "[]")) {
		return "", usageErr("--url host %q must be an IP address or a hostname", u.Hostname())
	}
	return u.Scheme + "://" + u.Host, nil
}

// printOpenAIPair is `connect codex|aider|continue`: the OpenAI pair, one
// pointer at the tool's own configuration, and the gateway fact about
// /v1/responses (decision 8, G32: print-only until the tools' pages are
// archived). The secret appears exactly once.
func printOpenAIPair(env IO, tool, base, secret string) error {
	// Nothing about these tools is archived in this repository (G32), so the
	// pointer asserts nothing about where each one reads its configuration.
	pointer := tool + ": see its own documentation for where it takes an OpenAI-compatible base URL and API key"
	out := &printer{w: env.Stdout}
	out.printf("# %s — in the shell that starts it (not the gateway's: %s there is the upstream credential an openai deployment reads):\n", tool, envOpenAIAPIVar)
	out.printf("export OPENAI_BASE_URL=%s/v1\n", sanitizeCell(base))
	out.printf("export %s=%s\n", envOpenAIAPIVar, secret)
	out.println("# " + pointer + ".")
	out.println("# Under /v1 this gateway serves POST /v1/chat/completions, POST /v1/embeddings and GET /v1/models only; POST /v1/responses (the OpenAI Responses API) is a 404 — if your tool defaults to the Responses API, switch it to Chat Completions per its own documentation.")
	return firstErr(out)
}

// claudeEnvBlock is the env block decision 8 specifies.
func claudeEnvBlock(base, secret string, discovery bool) map[string]string {
	m := map[string]string{envAnthropicBaseURL: base, envAnthropicBearer: secret, envHintHeaders: "1"}
	if discovery {
		m[envModelDiscovery] = "1"
	}
	return m
}

// claudeEnvNames is the block's keys in the order the connect page shows them.
func claudeEnvNames(discovery bool) []string {
	names := []string{envAnthropicBaseURL, envAnthropicBearer, envHintHeaders}
	if discovery {
		names = append(names, envModelDiscovery)
	}
	return names
}

// printClaudeBlock prints the settings env block (the secret appears here,
// once), then the VS Code and shell forms referring to it, then the honest
// state of the Messages API.
func printClaudeBlock(env IO, o connectOptions, base, secret, source string) {
	out := &printer{w: env.Stdout}
	block := claudeEnvBlock(base, secret, o.discovery)
	b, _ := json.MarshalIndent(map[string]any{"env": block}, "", "  ")
	out.printf("# Claude Code settings env block (~/.claude/settings.json, or ./.claude/settings.local.json for one project — never a project's committed .claude/settings.json); the key came from %s:\n", source)
	out.print(string(b) + "\n")
	out.println("# VS Code (settings.json), the same values:")
	out.println(`"claudeCode.environmentVariables": [`)
	names := claudeEnvNames(o.discovery)
	for i, name := range names {
		value := block[name]
		if name == envAnthropicBearer {
			value = "<the " + envAnthropicBearer + " value above>"
		}
		comma := ","
		if i == len(names)-1 {
			comma = ""
		}
		out.printf("  {\"name\": %q, \"value\": %q}%s\n", name, sanitizeCell(value), comma)
	}
	out.println("]")
	out.println("# Or the shell that starts claude (one session):")
	for _, name := range names {
		value := block[name]
		if name == envAnthropicBearer {
			value = "<the value above>"
		}
		out.printf("export %s=%s\n", name, sanitizeCell(value))
	}
	out.println("# This gateway does not serve POST /v1/messages yet (plan item 11): Claude Code's native Anthropic mode cannot reach it until it does — `kelvran connect claude --check` reports the live state. A pasted export persists in shell history; `read -rs` keeps it out.")
	if err := firstErr(out); err != nil {
		_, _ = fmt.Fprintln(env.Stderr, "kelvran connect claude:", sanitizeCell(err.Error()))
	}
}

// warnShellAPIKey: ANTHROPIC_API_KEY set in this shell also reaches Claude
// Code (in x-api-key, which this gateway reads only on GET /v1/models, and
// only when Authorization is absent) — and init's own
// single-user flow exports it as the gateway's upstream credential.
func warnShellAPIKey(pr *printer, env IO) {
	if env.Getenv(envAnthropicAPIVar) == "" {
		return
	}
	pr.printf("kelvran connect claude: warning: %s is set in this shell too; Claude Code also sends it (in x-api-key; Authorization wins, so set exactly one credential variable — a wrong ANTHROPIC_AUTH_TOKEN is a 401 with no fallback) — start claude from a shell without it (`env -u %s claude`) or the gateway from a separate shell (init's single-user flow exports it as the upstream credential)\n", envAnthropicAPIVar, envAnthropicAPIVar)
}

// checkClaude is the connect page's own probe: POST /v1/messages with
// max_tokens: 1 and a bearer, read honestly — today this gateway answers
// 404 (plan item 11, gate G8).
func checkClaude(ctx context.Context, env IO, base, secret string) error {
	body, _ := json.Marshal(map[string]any{"model": checkProbeModel, "max_tokens": 1, "messages": []map[string]string{{"role": "user", "content": "ping"}}})
	status, resp, err := dataPlanePost(ctx, base, "/v1/messages", body, secret)
	out := &printer{w: env.Stdout}
	secrets := tokenVariants(secret)
	var verdict error
	switch {
	case err != nil:
		out.printf("probe: POST %s/v1/messages failed: %s\n", sanitizeCell(base), sanitizeCell(redactSecrets(err.Error(), secrets)))
		verdict = runtimeErr("the gateway at %s could not be reached", base)
	case status == http.StatusNotFound:
		out.printf("probe: POST %s/v1/messages answered 404 — this gateway does not serve the Anthropic Messages API yet (plan item 11, gate G8); Claude Code's native mode will not work until it does\n", sanitizeCell(base))
		verdict = runtimeErr("the Anthropic Messages API is not served at %s yet", base)
	case status == http.StatusUnauthorized:
		out.printf("probe: POST %s/v1/messages answered 401 — the key is not a virtual key for this gateway (the probe sends Authorization: Bearer itself, so your variable choice is not in play); check it with `kelvran keys list`\n", sanitizeCell(base))
		verdict = runtimeErr("the key was rejected by %s", base)
	case status >= 300 && status < 400:
		out.printf("probe: POST %s/v1/messages answered %d — a redirect, not followed (the probe never carries the key to a second host); point --url at the gateway itself, not at a proxy or portal in front of it\n", sanitizeCell(base), status)
		verdict = runtimeErr("%s redirected the probe (%d)", base, status)
	case status == http.StatusOK || (status == http.StatusBadRequest && isModelNotFound(resp)):
		why := ""
		if status == http.StatusBadRequest {
			why = " and rejected the probe's unknown model, as expected"
		}
		out.printf("probe: POST %s/v1/messages answered %d — URL and credential are good (the gateway authenticated the request%s)\n", sanitizeCell(base), status, why)
	default:
		out.printf("probe: POST %s/v1/messages answered %d%s\n", sanitizeCell(base), status, sanitizeCell(bodySummary(status, resp, secrets)))
		verdict = runtimeErr("unexpected answer %d from %s", status, base)
	}
	out.printf("settings: the probe did not exercise any settings file. In Claude Code, run /status and look for the \"Auth token or API key\" line naming %s; if %s is also set, Claude Code may prefer it — enable or clear it under /config → \"Use custom API key\".\n", envAnthropicBearer, envAnthropicAPIVar)
	for _, c := range scanClaudeSettings(env, "") {
		out.println("settings: " + sanitizeCell(c))
	}
	if err := firstErr(out); err != nil {
		return err
	}
	return verdict
}

// isModelNotFound recognises the gateway's own envelope for a model no
// deployment serves (`error.code` = model_not_found, docs/reference/error-codes.md);
// the gateway authenticates before it routes, so a 400 of this shape proves
// the credential. Any other 400 is reported as unexpected. Anthropic's own
// error envelope has no `code` member: the /v1/messages ingress (RFC-1, plan
// item 11) must keep Kelvran's `code` on its 400s for this branch to fire —
// recorded as a cross-reference in that RFC.
func isModelNotFound(body []byte) bool {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &env) == nil && env.Error.Code == "model_not_found"
}

// dataPlanePost is one bearer POST against the data plane: status and up to
// 1 MiB of body, redirects not followed (a redirect would carry the key).
func dataPlanePost(ctx context.Context, base, path string, body []byte, bearer string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := (&http.Client{Timeout: adminRequestTimeout, CheckRedirect: noRedirects, Transport: adminTransport}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, b, nil
}

// settingsFile is one Claude Code settings file connect may read or write.
type settingsFile struct {
	path     string
	writable bool // user settings.json or the project settings.local.json; never a project's settings.json
}

// claudeSettingsFiles lists the files decision 8 names: the user file, the
// project-local file, and the project's shared file (read-only, committed).
func claudeSettingsFiles(env IO) (user, projectLocal, projectShared settingsFile) {
	home := env.Getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	cwd, _ := os.Getwd()
	return settingsFile{filepath.Join(home, ".claude", "settings.json"), true},
		settingsFile{filepath.Join(cwd, ".claude", "settings.local.json"), true},
		settingsFile{filepath.Join(cwd, ".claude", "settings.json"), false}
}

// readSettings loads a settings file as a JSON object (an absent file is an
// empty object); anything else is an error naming the path only.
func readSettings(path string) (map[string]any, bool, error) {
	if err := mustBeRegularFile(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, false, nil
		}
		return nil, false, err
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: the operator's own Claude Code settings file
	if err != nil {
		return nil, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // an integer beyond float64's 2^53 survives the round trip as written
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, true, fmt.Errorf("%s is not a JSON object", path)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, true, fmt.Errorf("%s has content after its JSON object", path)
	}
	return m, true, nil
}

// credentialSources reports the conflicting credential sources a settings
// object carries: env.ANTHROPIC_API_KEY and a top-level apiKeyHelper (the
// bearer in Authorization wins over x-api-key, so leaving either beside a
// wrong ANTHROPIC_AUTH_TOKEN yields a 401 the probe cannot explain). Values
// are never returned.
func credentialSources(m map[string]any) []string {
	var found []string
	if envm, ok := m["env"].(map[string]any); ok {
		if _, has := envm[envAnthropicAPIVar]; has {
			found = append(found, "env."+envAnthropicAPIVar)
		}
	}
	if _, has := m[settingsHelperField]; has {
		found = append(found, settingsHelperField)
	}
	return found
}

// scanClaudeSettings reads, read-only, every settings file except skip and
// reports the credential sources each sets, as "also sets" lines; values are
// never read out.
func scanClaudeSettings(env IO, skip string) []string {
	var notes []string
	user, local, shared := claudeSettingsFiles(env)
	for _, f := range []settingsFile{user, local, shared} {
		if f.path == skip {
			continue
		}
		m, exists, err := readSettings(f.path)
		if err != nil || !exists {
			continue
		}
		for _, k := range credentialSources(m) {
			notes = append(notes, f.path+" also sets "+k+" (Claude Code sends it in x-api-key; Authorization wins, so set exactly one credential variable)")
		}
	}
	return notes
}

// writeClaudeSettings is --write: the target for --scope (a symbolic link
// refused unless --force, which writes through), the conflict rule, the git
// rules (project scope: decision 8's check-ignore; both scopes: the file at
// its resolved location must not be tracked), an idempotent merge that owns
// the env keys and preserves every other key, written 0o600 by temp file and
// rename. Nothing is reported until the write has happened.
func writeClaudeSettings(env IO, o connectOptions, base, secret string) error {
	user, local, _ := claudeSettingsFiles(env)
	target := user
	if o.scope == "project" {
		target = local
	}
	writePath, notes, err := resolveWriteTarget(target.path, o.force)
	if err != nil {
		return err
	}
	m, _, err := readSettings(writePath)
	if err != nil {
		return runtimeErr("%v", err)
	}
	if conflicts := credentialSources(m); len(conflicts) > 0 {
		if !o.replaceCredential {
			return runtimeErr("%s sets %s, which Claude Code would also send (in x-api-key; Authorization wins, so a wrong ANTHROPIC_AUTH_TOKEN is a 401 with no fallback) and which would yield a 401 the probe cannot explain; pass --replace-credential to delete it in the same write", writePath, strings.Join(conflicts, " and "))
		}
		if envm, ok := m["env"].(map[string]any); ok {
			delete(envm, envAnthropicAPIVar)
		}
		delete(m, settingsHelperField)
	}
	envm, ok := m["env"].(map[string]any)
	if m["env"] != nil && !ok {
		return runtimeErr("%s has an env entry that is not an object", writePath)
	}
	if envm == nil {
		envm = map[string]any{}
	}
	replaced := ""
	if old, _ := envm[envAnthropicBaseURL].(string); old != "" && old != base {
		replaced = describeBaseURL(old)
	}
	for k, v := range claudeEnvBlock(base, secret, o.discovery) {
		envm[k] = v
	}
	m["env"] = envm
	if o.scope == "project" {
		n, err := projectWriteAllowed(filepath.Dir(filepath.Dir(target.path)), o.force)
		if err != nil {
			return err
		}
		notes = appendNote(notes, n)
	}
	if o.scope == "user" || writePath != target.path {
		n, err := trackedWriteGuard(writePath, o.force)
		if err != nil {
			return err
		}
		notes = appendNote(notes, n)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return runtimeErr("encoding %s: %v", writePath, err)
	}
	if err := writeSecretFile(writePath, append(data, '\n')); err != nil {
		return runtimeErr("%v", err)
	}
	out := &printer{w: env.Stdout}
	pr := &printer{w: env.Stderr}
	out.printf("Wrote %s (mode 0600): env.%s set; every other key preserved.\n", sanitizeCell(writePath), strings.Join(claudeEnvNames(o.discovery), ", env."))
	if replaced != "" {
		pr.printf("kelvran connect claude: replaced %s %s → %s in %s\n", envAnthropicBaseURL, replaced, sanitizeCell(base), sanitizeCell(writePath))
	}
	for _, n := range notes {
		pr.println("kelvran connect claude: " + sanitizeCell(n))
	}
	for _, c := range scanClaudeSettings(env, target.path) {
		pr.println("kelvran connect claude: warning: " + sanitizeCell(c))
	}
	out.println("This gateway does not serve POST /v1/messages yet (plan item 11); `kelvran connect claude --check` reports the live state.")
	return firstErr(out, pr)
}

func appendNote(notes []string, n string) []string {
	if n == "" {
		return notes
	}
	return append(notes, n)
}

// describeBaseURL renders the ANTHROPIC_BASE_URL a write replaced: scheme://host
// at most, and "a different value" when the old value does not parse or
// carries userinfo, a query or a fragment — a secret pasted into it never
// reaches stderr.
func describeBaseURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "a different value"
	}
	return sanitizeCell(u.Scheme + "://" + u.Host)
}

// resolveWriteTarget applies the symbolic-link rule to the settings file: a
// link is refused unless --force, which writes through to its target so the
// link survives (and trackedWriteGuard then asks git at that location — a
// dotfiles repository behind the link is the reason to refuse). Anything
// else that exists and is not a regular file is refused.
func resolveWriteTarget(path string, force bool) (writePath string, notes []string, err error) {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return path, nil, nil
	case err != nil:
		return "", nil, runtimeErr("%v", err)
	case fi.Mode()&os.ModeSymlink != 0:
		real, rerr := filepath.EvalSymlinks(path)
		if rerr != nil {
			return "", nil, runtimeErr("%s is a symbolic link whose target cannot be resolved; replace it with a regular file", path)
		}
		if !force {
			return "", nil, runtimeErr("%s is a symbolic link to %s; connect writes a regular file, and a dotfiles repository behind the link would receive the key — pass --force to write through to the link's target", path, real)
		}
		return real, []string{path + " is a symbolic link; written through to " + real + " because of --force"}, nil
	case !fi.Mode().IsRegular():
		return "", nil, runtimeErr("%s is not a regular file", path)
	}
	return path, nil, nil
}

// writeSecretFile creates or replaces path with mode 0600 — the one file the
// CLI writes that holds a secret — by a temp file in the same directory and a
// rename; the directory is created 0700 when absent.
func writeSecretFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating a temp file beside %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	fail := func(step string, e error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("%s %s: %w", step, path, e)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail("writing", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("syncing", err)
	}
	if err := tmp.Close(); err != nil {
		return fail("closing", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fail("setting the mode of", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}
