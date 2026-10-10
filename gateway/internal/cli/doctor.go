package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// finding is one row of `kelvran doctor`'s report. Severity is "error"
// (the gateway would refuse to start, or the operator's premise is wrong),
// "warning" (it starts, but something will fail or is unsafe) or "info".
// The JSON keys of finding and doctorReport are public surface
// (docs/reference/kelvran-cli.md, docs/VERSIONING.md): fields may be added,
// never renamed or removed.
type finding struct {
	Severity string `json:"severity"`
	Check    string `json:"check"`
	Detail   string `json:"detail"`
}

var severityRank = map[string]int{"error": 0, "warning": 1, "info": 2}

type doctorOptions struct {
	config         string
	envFiles       repeatFlag
	strictEnv      bool
	url            string
	adminURL       string
	adminTokenFile string
	allowInsecure  bool
	jsonOut        bool
}

const doctorUsage = `usage: kelvran doctor [--config config.yaml] [--env-file PATH]... [--strict-env]
                      [--url URL] [--admin-url URL] [--admin-token-file PATH]
                      [--allow-insecure-http] [--json]

Loads and validates the config the way kelvran-gateway -validate does, then reports what -validate
cannot see: credential variables that are unset in the environment the gateway will run in (pass
--env-file for a systemd EnvironmentFile= or Docker env file; the packaged /etc/kelvran-gateway/env
is read automatically), unreadable credential files, the packaged layout's permissions, unpriced
models, the telemetry exporter, admin persistence, and, with --url or --admin-url, a running
gateway. One row per finding (severity, check, detail); exit 1 when any row is an error.
`

// Doctor runs `kelvran doctor` and returns the process exit code: 0 with no
// error-level finding, 1 with one (or when the config does not load), 2 on
// a usage error.
func Doctor(args []string, env IO) int {
	var o doctorOptions
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	// flag's own messages echo argv (`flag provided but not defined: -%s`), so
	// they go through the same escaper as every other line.
	fs.SetOutput(escapingWriter{env.Stderr})
	fs.Usage = func() { _, _ = fmt.Fprint(env.Stderr, doctorUsage) }
	fs.StringVar(&o.config, "config", defaultOut, "config file to examine")
	fs.Var(&o.envFiles, "env-file", "systemd EnvironmentFile= / Docker env file holding the gateway's variables (repeatable; values are never printed)")
	fs.BoolVar(&o.strictEnv, "strict-env", false, "treat this shell as the gateway's whole environment: every variable the config names that is unset here is an error")
	fs.StringVar(&o.url, "url", "", "data-plane base URL to probe (GET /readyz; GET /v1/models without a bearer must be 401)")
	fs.StringVar(&o.adminURL, "admin-url", "", "admin base URL to probe with the resolved admin token (GET /admin/config, GET /admin/deployments)")
	fs.StringVar(&o.adminTokenFile, "admin-token-file", "", "file holding the admin token (else KELVRAN_ADMIN_TOKEN_FILE, the config's admin.token_env variable, KELVRAN_ADMIN_TOKEN)")
	fs.BoolVar(&o.allowInsecure, "allow-insecure-http", false, "send the admin token to a non-loopback http:// --admin-url")
	fs.BoolVar(&o.jsonOut, "json", false, "print the findings as one JSON document")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(env.Stderr, "kelvran doctor: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	report, err := runDoctor(o, env)
	if err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			_, _ = fmt.Fprintln(env.Stderr, "kelvran doctor:", sanitizeCell(ee.msg))
			if ee.code == 2 {
				fs.Usage()
			}
			return ee.code
		}
		_, _ = fmt.Fprintln(env.Stderr, "kelvran doctor:", sanitizeCell(err.Error()))
		return 1
	}
	if err := report.print(env.Stdout, o.jsonOut); err != nil {
		_, _ = fmt.Fprintln(env.Stderr, "kelvran doctor:", sanitizeCell(err.Error()))
		return 1
	}
	if report.count("error") > 0 {
		return 1
	}
	return 0
}

// doctorReport collects findings in arrival order and prints them sorted.
// Findings is never nil, so --json emits [] for a clean config.
type doctorReport struct {
	Config   string    `json:"config"`
	Findings []finding `json:"findings"`
	Errors   int       `json:"errors"`
	Warnings int       `json:"warnings"`
	Info     int       `json:"info"`
}

func (r *doctorReport) add(severity, check, detail string) {
	r.Findings = append(r.Findings, finding{Severity: severity, Check: check, Detail: detail})
}

func (r *doctorReport) count(severity string) int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == severity {
			n++
		}
	}
	return n
}

func (r *doctorReport) print(w io.Writer, asJSON bool) error {
	sort.SliceStable(r.Findings, func(i, j int) bool {
		if severityRank[r.Findings[i].Severity] != severityRank[r.Findings[j].Severity] {
			return severityRank[r.Findings[i].Severity] < severityRank[r.Findings[j].Severity]
		}
		return r.Findings[i].Check < r.Findings[j].Check
	})
	r.Errors, r.Warnings, r.Info = r.count("error"), r.count("warning"), r.count("info")
	if asJSON {
		enc := json.NewEncoder(w)
		return enc.Encode(r)
	}
	pr := &printer{w: w}
	if len(r.Findings) == 0 {
		pr.printf("kelvran doctor: %s: no findings\n", sanitizeCell(r.Config))
		return firstErr(pr)
	}
	checkWidth := len("check")
	for _, f := range r.Findings {
		if n := len(sanitizeCell(f.Check)); n > checkWidth {
			checkWidth = n
		}
	}
	pr.printf("%-8s  %-*s  %s\n", "severity", checkWidth, "check", "detail")
	for _, f := range r.Findings {
		pr.printf("%-8s  %-*s  %s\n", f.Severity, checkWidth, sanitizeCell(f.Check), sanitizeCell(f.Detail))
	}
	pr.printf("\nkelvran doctor: %s: %d error(s), %d warning(s), %d info\n", sanitizeCell(r.Config), r.Errors, r.Warnings, r.Info)
	return firstErr(pr)
}

// sanitizeCell escapes control characters (and the Unicode line and
// paragraph separators) as \u00XX in every line doctor prints — table cells,
// the `kelvran doctor:` error lines (which echo --config and --env-file
// paths) and the admin client's stderr host note — so a deployment name,
// a path or a host cannot steer the terminal. url.Parse already rejects C0
// control bytes in a URL; this covers the separators it admits and the
// paths that never pass through it. The JSON encoder escapes the same
// runes itself.
func sanitizeCell(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			_, _ = fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// escapingWriter passes every write through sanitizeCell; it carries the
// flag package's own error output, which echoes argv.
type escapingWriter struct{ w io.Writer }

func (e escapingWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(e.w, sanitizeCell(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// runDoctor loads the config and runs every check, returning the report; a
// usage-shaped problem (a bad URL, an unreadable --env-file) is an error
// rather than a finding because nothing meaningful was examined yet.
func runDoctor(o doctorOptions, env IO) (*doctorReport, error) {
	cfgAbs, err := filepath.Abs(o.config)
	if err != nil {
		return nil, runtimeErr("resolving --config %q: %v", o.config, err)
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(cfgAbs)); err == nil {
		cfgAbs = filepath.Join(dir, filepath.Base(cfgAbs))
	}
	if isEnvFilePath(cfgAbs, o.envFiles) {
		// The loader's parse errors quote the offending line; an env file's
		// lines ARE the secrets, so refuse before Load can echo one.
		return nil, usageErr("--config %s is an environment file, not a config", cfgAbs)
	}
	report := &doctorReport{Config: cfgAbs, Findings: []finding{}}
	cfg, err := controlplane.Load(cfgAbs)
	if err != nil {
		report.add("error", "config.load", redactConfigError(err)+" — the gateway would refuse to start")
		return report, nil
	}
	if err := controlplane.Validate(cfg, adapter.ProviderNames()); err != nil {
		report.add("error", "config.validate", redactConfigError(err)+" — kelvran-gateway -validate fails the same way")
	}
	packaged := strings.HasPrefix(cfgAbs, packagedConfigDir)

	src, err := buildEnvSource(o, packaged, env.Getenv, report)
	if err != nil {
		return nil, err
	}
	checkEnvVariables(cfg, src, o.strictEnv, report)
	checkOAuthPrefix(cfg, src, report)
	checkFiles(cfg, cfgAbs, packaged, report)
	checkPricesAndTelemetry(cfg, report)
	checkAdminAndListen(cfg, report)
	checkPropagationKey(cfg, report)
	if o.url != "" {
		probeDataPlane(context.Background(), o.url, report)
	}
	if o.adminURL != "" {
		if err := probeAdmin(context.Background(), o, cfg, src, env, report); err != nil {
			return nil, err
		}
	}
	return report, nil
}

// quotedGot matches the mini-parser's `got "<whole line>"` echo
// (gateway/internal/gateway/controlplane/config.go, parseYAMLMini).
var quotedGot = regexp.MustCompile(`got "(?:[^"\\]|\\.)*"`)

// goQuoted matches a Go %q-quoted string in an error message.
var goQuoted = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// bareURL matches an unquoted URL-shaped token in an error message.
var bareURL = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)

// redactConfigError keeps a load or validation error's position and kind but
// withholds what could be a secret: the mini-parser's quoted line content (a
// config line that fails to parse may be a pasted secret) and the password of
// any URL the message echoes — Load prints base_url verbatim when it is not
// https. Doctor promises never to print a value. (`-validate` keeps both
// echoes; changing it is outside RFC-3.)
func redactConfigError(err error) string {
	return redactURLs(quotedGot.ReplaceAllString(err.Error(), "got <line content withheld>"))
}

// redactURLs rewrites every URL in s, quoted or bare, through redactOneURL,
// and withholds every other quoted string that is a fragment of a URL that
// carries something sensitive (userinfo, a query, or one that does not
// parse): a wrapped url.Error echoes the offending host or port on its own
// (`invalid port ":pw%40host:9" after host`), a quoted value with no "://"
// that still carries the password. A name that is a substring of such a URL
// is over-redacted rather than risk the reverse — a deployment named after
// its own password must not be printed either — while a plain
// `http://host/v1` withholds nothing, so `deployment "v1"` stays legible.
func redactURLs(s string) string {
	var sensitive []string
	for _, m := range goQuoted.FindAllString(s, -1) {
		if v, err := strconv.Unquote(m); err == nil && strings.Contains(v, "://") && redactOneURL(v) != v {
			sensitive = append(sensitive, v)
		}
	}
	for _, v := range bareURL.FindAllString(s, -1) {
		if redactOneURL(v) != v {
			sensitive = append(sensitive, v)
		}
	}
	if len(sensitive) == 0 && !strings.Contains(s, "://") {
		return s
	}
	s = goQuoted.ReplaceAllStringFunc(s, func(m string) string {
		v, err := strconv.Unquote(m)
		switch {
		case err != nil:
			return `"<quoted value withheld>"`
		case strings.Contains(v, "://"):
			return strconv.Quote(redactOneURL(v))
		}
		for _, u := range sensitive {
			if v != "" && strings.Contains(u, v) {
				return `"<URL fragment withheld>"`
			}
		}
		return m
	})
	return bareURL.ReplaceAllStringFunc(s, redactOneURL)
}

// redactOneURL renders a URL for output: the password replaced
// (url.URL.Redacted), the query and fragment withheld (a Gemini-style
// `?key=` would otherwise print), and a URL that does not parse withheld
// whole rather than guessed at — `user:pw%40host` fails to parse without
// a literal "@" and the password would otherwise pass through verbatim.
func redactOneURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// Not parseable as an absolute URL; anything parses as a bare path,
		// so a scheme and a host are required before a value is echoed.
		return "<URL withheld>"
	}
	suffix := ""
	if u.RawQuery != "" || u.ForceQuery {
		suffix = "?<query withheld>"
		u.RawQuery, u.ForceQuery = "", false
	}
	u.Fragment, u.RawFragment = "", ""
	return u.Redacted() + suffix
}

// isEnvFilePath reports whether the config path is the packaged env file or
// one of the --env-file paths, compared after resolving symlinks on both
// sides — the config file's own symlink included, since runDoctor resolves
// only its directory (the file may not exist yet) and a config that is a
// symlink to the env file must still be caught.
func isEnvFilePath(cfgAbs string, envFiles []string) bool {
	resolve := func(p string) string {
		abs, err := filepath.Abs(p)
		if err != nil {
			return p
		}
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			return r
		}
		return abs
	}
	cfg := resolve(cfgAbs)
	if resolve(packagedEnvFile()) == cfg {
		return true
	}
	for _, p := range envFiles {
		if resolve(p) == cfg {
			return true
		}
	}
	return false
}
