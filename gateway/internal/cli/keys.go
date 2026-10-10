package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adminapi"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

const keysUsage = `usage: kelvran keys create <name> [--budget USD] [--reset daily|weekly|monthly|none] [--warn FRACTION]
                           [--models a,b] [--expires 30d|720h|RFC3339] [--billing-subject ID] [--replace] [--force] [--json]
       kelvran keys list   [--spend] [--json]
       kelvran keys rotate <name> [--grace 10m] [--expires 30d|720h|RFC3339] [--json]
       kelvran keys delete <name> [--json]
  every verb: [--config PATH] [--admin-url URL] [--admin-token-file PATH] [--allow-insecure-http]

Online whenever an admin token resolves (--admin-token-file, KELVRAN_ADMIN_TOKEN_FILE, the variable the
config's admin.token_env names, KELVRAN_ADMIN_TOKEN): the verbs call the admin API at --admin-url
(KELVRAN_ADMIN_URL, default http://127.0.0.1:8081). Offline, with --config and no token: they rewrite the
virtual_keys block of the file byte-preservingly, never a persisted store; the change applies at the next
gateway start. The secret is printed once; only its SHA-256 hash is sent or written.
`

// Reset-interval names (RFC-3 decision 3): rolling windows, not calendar ones.
const (
	resetDaily   = 86400
	resetWeekly  = 604800
	resetMonthly = 2592000
)

type keysOptions struct {
	verb, name     string
	config         string
	adminURL       string
	adminTokenFile string
	allowInsecure  bool
	jsonOut        bool
	// create
	budget, reset, warn, models, expires, billingSubject string
	replace, force                                       bool
	// list
	spend bool
	// rotate
	grace string
}

// keySpec is a create or rotate request after flag validation: the wire body
// and the file lines both derive from it, so the two modes cannot drift.
type keySpec struct {
	name         string
	budget       decimal.Decimal // zero = unlimited
	budgetText   string          // as typed, for the file
	resetSeconds int
	warn         float64
	warnText     string
	models       []string
	billing      string
	expiresAt    time.Time // zero = never
	graceSeconds int       // rotate only
}

func (s keySpec) expiresText() string {
	if s.expiresAt.IsZero() {
		return ""
	}
	return s.expiresAt.UTC().Format(time.RFC3339)
}

// Keys runs `kelvran keys <verb>` and returns the process exit code: 0 on
// success, 1 on a refusal or a failed request, 2 on a usage error.
func Keys(args []string, env IO) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(env.Stderr, keysUsage)
		return 2
	}
	o := keysOptions{verb: args[0]}
	switch o.verb {
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(env.Stderr, keysUsage)
		return 0
	case "create", "list", "rotate", "delete":
	default:
		_, _ = fmt.Fprintf(env.Stderr, "kelvran keys: unknown verb %q\n", sanitizeCell(o.verb))
		_, _ = fmt.Fprint(env.Stderr, keysUsage)
		return 2
	}
	fs := flag.NewFlagSet("keys "+o.verb, flag.ContinueOnError)
	fs.SetOutput(escapingWriter{env.Stderr})
	fs.Usage = func() { _, _ = fmt.Fprint(env.Stderr, keysUsage) }
	fs.StringVar(&o.config, "config", "", "config file: names the admin.token_env variable online, is rewritten offline")
	fs.StringVar(&o.adminURL, "admin-url", "", "admin base URL (else KELVRAN_ADMIN_URL, else http://127.0.0.1:8081)")
	fs.StringVar(&o.adminTokenFile, "admin-token-file", "", "file holding the admin token (else KELVRAN_ADMIN_TOKEN_FILE, the config's admin.token_env variable, KELVRAN_ADMIN_TOKEN)")
	fs.BoolVar(&o.allowInsecure, "allow-insecure-http", false, "send the admin token to a non-loopback http:// admin URL")
	fs.BoolVar(&o.jsonOut, "json", false, "print one JSON document instead of text")
	switch o.verb {
	case "create":
		fs.StringVar(&o.budget, "budget", "", "budget_usd (omitted = unlimited)")
		fs.StringVar(&o.reset, "reset", "", "budget window: daily|weekly|monthly|none (rolling, not calendar)")
		fs.StringVar(&o.warn, "warn", "", "budget_warn_percent as a fraction in (0, 1], e.g. 0.8")
		fs.StringVar(&o.models, "models", "", "allowed_models, comma-separated (omitted = every configured model)")
		fs.StringVar(&o.expires, "expires", "", "expires_at: 30d, 720h or an RFC 3339 instant, in the future")
		fs.StringVar(&o.billingSubject, "billing-subject", "", "billing_subject_id (metadata)")
		fs.BoolVar(&o.replace, "replace", false, "overwrite an existing key of this name (a full replace)")
		fs.BoolVar(&o.force, "force", false, "with --replace: proceed although fields the replace drops are set")
	case "list":
		fs.BoolVar(&o.spend, "spend", false, "add spent_usd and percent_used (GET /admin/virtual_keys?include=spend)")
	case "rotate":
		fs.StringVar(&o.grace, "grace", "", "how long the previous secret keeps working, in whole seconds (online only)")
		fs.StringVar(&o.expires, "expires", "", "new expires_at: 30d, 720h or an RFC 3339 instant")
	}
	// The name may come before or after the flags: parse, take the first
	// positional, parse the remainder.
	rest := args[1:]
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		if o.name != "" || o.verb == "list" {
			_, _ = fmt.Fprintf(env.Stderr, "kelvran keys %s: unexpected argument %q\n", o.verb, fs.Arg(0))
			fs.Usage()
			return 2
		}
		o.name, rest = fs.Arg(0), fs.Args()[1:]
	}
	if o.verb != "list" && o.name == "" {
		_, _ = fmt.Fprintf(env.Stderr, "kelvran keys %s: a key name is required\n", o.verb)
		fs.Usage()
		return 2
	}
	if err := runKeys(o, env); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			_, _ = fmt.Fprintln(env.Stderr, "kelvran keys:", sanitizeCell(ee.msg))
			if ee.code == 2 {
				fs.Usage()
			}
			return ee.code
		}
		_, _ = fmt.Fprintln(env.Stderr, "kelvran keys:", sanitizeCell(err.Error()))
		return 1
	}
	return 0
}

func runKeys(o keysOptions, env IO) error {
	spec, err := buildKeySpec(o, time.Now())
	if err != nil {
		return err
	}
	t, err := resolveKeysTarget(o, env)
	if err != nil {
		return err
	}
	if t.online {
		return runKeysOnline(o, spec, t, env)
	}
	return runKeysOffline(o, spec, t, env)
}

// buildKeySpec validates the create/rotate flags into the one struct both
// modes consume (decision 3's flag table).
func buildKeySpec(o keysOptions, now time.Time) (keySpec, error) {
	s := keySpec{name: o.name}
	if o.name != "" {
		if len(o.name) > 256 {
			return s, usageErr("the key name exceeds 256 bytes")
		}
		if strings.TrimSpace(o.name) != o.name || o.name == "" {
			return s, usageErr("the key name must not be empty or carry surrounding whitespace")
		}
	}
	var err error
	if o.budget != "" {
		if s.budget, err = decimal.NewFromString(o.budget); err != nil {
			return s, usageErr("--budget %q is not a decimal amount of USD", o.budget)
		}
		if s.budget.IsNegative() {
			return s, usageErr("--budget must not be negative (omit it for an unlimited key)")
		}
		if s.budget.IsPositive() {
			s.budgetText = o.budget
		}
	}
	switch o.reset {
	case "", "none":
	case "daily":
		s.resetSeconds = resetDaily
	case "weekly":
		s.resetSeconds = resetWeekly
	case "monthly":
		s.resetSeconds = resetMonthly
	default:
		return s, usageErr("--reset %q is not one of daily, weekly, monthly, none", o.reset)
	}
	if o.warn != "" {
		if s.warn, err = strconv.ParseFloat(o.warn, 64); err != nil || math.IsNaN(s.warn) || s.warn <= 0 || s.warn > 1 {
			// The server accepts [0, 100], so 80 instead of 0.8 would yield a threshold that never fires.
			return s, usageErr("--warn %q must be a fraction of the budget in (0, 1], such as 0.8", o.warn)
		}
		s.warnText = o.warn
	}
	if s.models, err = parseKeyModels(o.models); err != nil {
		return s, err
	}
	if strings.ContainsAny(o.billingSubject, "\"#\n\r") {
		return s, usageErr("--billing-subject must not contain a quote, a #, or a line break")
	}
	s.billing = o.billingSubject
	if s.expiresAt, err = parseExpires(o.expires, now); err != nil {
		return s, err
	}
	if s.graceSeconds, err = parseGrace(o.grace); err != nil {
		return s, err
	}
	return s, nil
}

// parseKeyModels splits --models a,b into a sorted, deduplicated allow-list.
func parseKeyModels(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	for _, m := range strings.Split(s, ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			return nil, usageErr("--models has an empty entry")
		}
		if slices.Contains(out, m) {
			return nil, usageErr("--models lists %q twice", m)
		}
		out = append(out, m)
	}
	slices.Sort(out)
	return out, nil
}

// parseExpires turns --expires into an absolute UTC instant: an RFC 3339
// literal, <N>d, or a Go duration such as 720h, each required to be in the
// future (RFC-3 decision 4) — the only guard the offline path has, since Load
// accepts a past value. Malformed is a usage error; not-in-the-future exits 1.
func parseExpires(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	var t time.Time
	switch {
	case func() bool { p, err := time.Parse(time.RFC3339, s); t = p; return err == nil }():
	case strings.HasSuffix(s, "d"):
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n <= 0 {
			return time.Time{}, usageErr("--expires %q is not RFC 3339, <N>d or a Go duration such as 720h", s)
		}
		t = now.Add(time.Duration(n) * 24 * time.Hour)
	default:
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return time.Time{}, usageErr("--expires %q is not RFC 3339, <N>d or a Go duration such as 720h", s)
		}
		t = now.Add(d)
	}
	t = t.UTC().Truncate(time.Second)
	if !t.After(now) {
		return time.Time{}, runtimeErr("--expires %s is not in the future", t.Format(time.RFC3339))
	}
	return t, nil
}

// parseGrace is --grace in whole seconds (grace_period_seconds); "" = 0 = the
// previous secret stops working immediately.
func parseGrace(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, usageErr("--grace %q is not a non-negative Go duration such as 10m", s)
	}
	if d%time.Second != 0 {
		return 0, usageErr("--grace %q must be whole seconds (grace_period_seconds)", s)
	}
	return int(d / time.Second), nil
}

// keysTarget is where a verb acts: the admin API (online) or the file.
type keysTarget struct {
	cfgPath, cfgAbs string
	cfg             *controlplane.Config
	loadErr         error
	online          bool
	client          *adminClient
	base            string
}

// resolveKeysTarget applies decision 3: online whenever a token resolves,
// offline with --config and no token, otherwise a usage error naming both.
func resolveKeysTarget(o keysOptions, env IO) (*keysTarget, error) {
	t := &keysTarget{cfgPath: o.config}
	tokenEnv := ""
	if o.config != "" {
		abs, err := filepath.Abs(o.config)
		if err != nil {
			return nil, runtimeErr("resolving --config %q: %v", o.config, err)
		}
		if r, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
			abs = filepath.Join(r, filepath.Base(abs))
		}
		t.cfgAbs = abs
		if cfg, err := controlplane.Load(abs); err != nil {
			t.loadErr = err
		} else {
			t.cfg, tokenEnv = cfg, cfg.Admin.TokenEnv
		}
	}
	token, _, err := resolveAdminToken(o.adminTokenFile, tokenEnv, env.Getenv)
	if err != nil {
		return nil, runtimeErr("%v", err)
	}
	if token != "" {
		base, err := resolveAdminURL(o.adminURL, o.allowInsecure, env.Getenv)
		if err != nil {
			return nil, runtimeErr("%v", err)
		}
		t.online, t.base = true, base
		t.client = newAdminClient(base, token, env.Stderr)
		if t.loadErr != nil {
			// The token decides the mode (decision 3), but a --config the user
			// passed and that does not load must not vanish silently.
			_, _ = fmt.Fprintf(env.Stderr, "kelvran keys: warning: --config %s does not load (%s); proceeding online with the resolved token\n", sanitizeCell(t.cfgAbs), sanitizeCell(redactConfigError(t.loadErr)))
		}
		return t, nil
	}
	if o.config == "" {
		return nil, usageErr("no admin token resolved (--admin-token-file, KELVRAN_ADMIN_TOKEN_FILE, the variable the config's admin.token_env names, KELVRAN_ADMIN_TOKEN) and no --config to edit offline; supply one of the two")
	}
	if t.loadErr != nil {
		return nil, runtimeErr("%s does not load: %s", t.cfgAbs, redactConfigError(t.loadErr))
	}
	return t, nil
}

// newSecret draws the key's secret and its hash; the secret is printed once
// by the caller and never sent or written.
func newSecret(env IO) (secret, hash string, err error) {
	secret, err = randomHex(env.Rand)
	if err != nil {
		return "", "", err
	}
	return secret, hashSecret(secret), nil
}

// issued is the --json document for create and rotate: the one place the
// secret appears in that mode.
type issued struct {
	Verb               string `json:"verb"`
	Mode               string `json:"mode"`
	Name               string `json:"name"`
	Config             string `json:"config,omitempty"`
	Key                string `json:"key"`
	KeyHash            string `json:"key_hash"`
	ExpiresAt          string `json:"expires_at,omitempty"`
	GracePeriodSeconds *int   `json:"grace_period_seconds,omitempty"`
}

// deleted is the --json document for delete.
type deleted struct {
	Verb   string `json:"verb"`
	Mode   string `json:"mode"`
	Name   string `json:"name"`
	Config string `json:"config,omitempty"`
}

// printIssued prints a created or rotated key: the headline, then the secret
// exactly once inside the client-shell export block init prints, with the
// base URLs when listen_addr is known.
func printIssued(env IO, o keysOptions, headline string, doc issued, listenAddr string) error {
	out := &printer{w: env.Stdout}
	if o.jsonOut {
		enc := json.NewEncoder(env.Stdout)
		return enc.Encode(doc)
	}
	out.println(headline)
	if doc.ExpiresAt != "" {
		out.printf("  expires_at: %s\n", doc.ExpiresAt)
	}
	out.println()
	out.println("  # In the CLIENT's shell (not the gateway's: exporting OPENAI_API_KEY there would replace the upstream credential an openai deployment reads):")
	out.printf("  export %s=%s\n", "KELVRAN_KEY", doc.Key)
	if anthropicBase, openaiBase, err := ClientURLs(listenAddr); listenAddr != "" && err == nil {
		out.printf("  export %s=%s\n", "ANTHROPIC_BASE_URL", sanitizeCell(anthropicBase))
		out.printf("  export %s=\"$%s\"\n", "ANTHROPIC_AUTH_TOKEN", "KELVRAN_KEY")
		out.printf("  export %s=%s\n", "OPENAI_BASE_URL", sanitizeCell(openaiBase))
		out.printf("  export %s=\"$%s\"\n", "OPENAI_API_KEY", "KELVRAN_KEY")
	} else {
		out.printf("  export %s=\"$%s\"   # with ANTHROPIC_BASE_URL=http://<listen_addr>\n", "ANTHROPIC_AUTH_TOKEN", "KELVRAN_KEY")
		out.printf("  export %s=\"$%s\"   # with OPENAI_BASE_URL=http://<listen_addr>/v1\n", "OPENAI_API_KEY", "KELVRAN_KEY")
	}
	out.println()
	out.println("  A pasted export persists in shell history; `read -rs KELVRAN_KEY` then `export KELVRAN_KEY` keeps it out.")
	return firstErr(out)
}

// keysTable renders the list columns decision 3 fixes, with sanitised cells.
type keysTable struct {
	header []string
	rows   [][]string
}

func (t keysTable) print(out *printer) {
	widths := make([]int, len(t.header))
	for i, h := range t.header {
		widths[i] = len(h)
	}
	for _, r := range t.rows {
		for i, c := range r {
			if n := len(sanitizeCell(c)); n > widths[i] {
				widths[i] = n
			}
		}
	}
	line := func(cells []string) {
		parts := make([]string, len(cells))
		for i, c := range cells {
			parts[i] = fmt.Sprintf("%-*s", widths[i], sanitizeCell(c))
		}
		out.println(strings.TrimRight(strings.Join(parts, "  "), " "))
	}
	line(t.header)
	for _, r := range t.rows {
		line(r)
	}
}

// listRow renders one admin list entry (the offline list is converted into
// the same type so both modes share one renderer).
func listRow(e adminapi.VirtualKeyListEntry, withSpend bool) []string {
	budget := "unlimited"
	if d, err := decimal.NewFromString(e.BudgetUSD); err == nil && d.IsPositive() {
		budget = e.BudgetUSD
	}
	warn := "-"
	if e.BudgetWarnPercent > 0 {
		warn = strconv.FormatFloat(e.BudgetWarnPercent, 'f', -1, 64)
	}
	models := "all"
	if len(e.AllowedModels) > 0 {
		models = strings.Join(e.AllowedModels, ",")
	}
	expires := "never"
	if e.ExpiresAt != "" {
		expires = e.ExpiresAt
	}
	row := []string{e.ID, budget, resetName(e.BudgetResetIntervalSeconds), warn, models, expires}
	if withSpend {
		spent, pct := "n/a", "n/a"
		if !e.SpendUnavailable {
			spent = e.SpentUSD
			if spent == "" {
				spent = "0"
			}
			pct = "-"
			if e.PercentUsed != nil {
				pct = strconv.FormatFloat(*e.PercentUsed*100, 'f', 1, 64) + "%"
			}
		}
		row = append(row, spent, pct)
	}
	return row
}

func listHeader(withSpend bool) []string {
	h := []string{"key", "budget_usd", "reset", "warn", "models", "expires_at"}
	if withSpend {
		h = append(h, "spent_usd", "percent_used")
	}
	return h
}

func resetName(secs int) string {
	switch secs {
	case 0:
		return "never"
	case resetDaily:
		return "daily"
	case resetWeekly:
		return "weekly"
	case resetMonthly:
		return "monthly"
	}
	return strconv.Itoa(secs) + "s"
}

// fileKeyEntry converts a loaded config key into the admin list shape so the
// offline list shares the online renderer and --json document.
func fileKeyEntry(vk controlplane.VirtualKeyConfig) adminapi.VirtualKeyListEntry {
	e := adminapi.VirtualKeyListEntry{
		ID:                         vk.Name,
		BudgetUSD:                  vk.BudgetUSD.String(),
		BudgetResetIntervalSeconds: vk.BudgetResetIntervalSeconds,
		BudgetWarnPercent:          vk.BudgetWarnPercent,
		AllowedModels:              vk.AllowedModels,
		AllowedRegions:             vk.AllowedRegions,
		AllowedSourceCIDRs:         vk.AllowedSourceCIDRs,
		CacheScopeToEndUser:        vk.CacheScopeToEndUser,
		AttributionIDsDisabled:     vk.AttributionIDsDisabled,
		RateLimitBurst:             vk.RateLimitBurst,
		RateLimitRefill:            vk.RateLimitRefill,
		BillingSubjectID:           vk.BillingSubjectID,
		MaxConcurrentRequests:      vk.MaxConcurrentRequests,
	}
	if !vk.ExpiresAt.IsZero() {
		e.ExpiresAt = vk.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return e
}
