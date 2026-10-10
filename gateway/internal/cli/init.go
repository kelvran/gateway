// Package cli holds the logic behind the kelvran binary (gateway/cmd/kelvran):
// a user's first five minutes — init, doctor, keys, connect, status, spend —
// per docs/rfcs/2026-10-09-gateway-kelvran-cli-and-single-user-mode.md. It
// depends on the config loader, identity.HashSecret, adapter.ProviderNames,
// the admin wire types and the exporter-name vocabulary, and on nothing else
// in the gateway (gateway/.go-arch-lint.yml's cli entry), so the binary
// carries no OpenTelemetry and never links the data plane.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/shopspring/decimal"
)

// IO is the process environment Init reads and writes through, so tests
// can drive it without a subprocess: Rand is crypto/rand.Reader in the
// binary and a deterministic reader in tests.
type IO struct {
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	Rand   io.Reader
}

// exitError carries the process exit code a failure maps to: 2 for a
// usage error (flags, missing required flags), 1 for everything else.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func usageErr(format string, a ...any) error {
	return &exitError{code: 2, msg: fmt.Sprintf(format, a...)}
}
func runtimeErr(format string, a ...any) error {
	return &exitError{code: 1, msg: fmt.Sprintf(format, a...)}
}

// repeatFlag collects a repeatable --flag value=... option.
type repeatFlag []string

func (r *repeatFlag) String() string     { return strings.Join(*r, ",") }
func (r *repeatFlag) Set(v string) error { *r = append(*r, v); return nil }

const (
	defaultListenAddr = "127.0.0.1:8080"
	defaultOut        = "config.yaml"
	defaultKeyName    = "default"
	adminTokenEnvName = "KELVRAN_ADMIN_TOKEN" //nolint:gosec // G101: the NAME of the variable admin.token_env points at, never a value
	// oauthTokenPrefix marks a Claude subscription OAuth token offered as an
	// API key (RFC-3 decision 9, gate G33): warned about in Stage 1, never
	// echoed.
	oauthTokenPrefix = "sk-ant-oat" //nolint:gosec // G101: a token-format PREFIX compared with HasPrefix, never a credential
	packagedStateDB  = "/var/lib/kelvran-gateway/identity.db"
	persistFileName  = "kelvran-identity.db"
)

// packagedConfigDir is where the deb/rpm/apk keep the config; a variable so
// tests can point the packaged-layout rules at a temp root.
var packagedConfigDir = "/etc/kelvran-gateway/"

type initOptions struct {
	singleUser  bool
	out         string
	provider    string
	baseURL     string
	region      string
	listen      string
	models      string
	upstream    repeatFlag
	prices      repeatFlag
	budget      string
	persistPath string
	noPersist   bool
	dryRun      bool
	force       bool
}

// deployment is one emitted deployments.<name> block.
type deployment struct {
	Name, Model, Provider, Upstream, BaseURL string
	APIKeyEnv                                string // non-bedrock
	Region                                   string // bedrock
	SessionTokenEnv                          bool   // bedrock: emit session_token_env when the variable is set
	Insecure                                 bool   // allow_insecure_http
}

// priceRow is one emitted price_table.<model> block.
type priceRow struct {
	Model, Prompt, Completion, CacheRead, CacheWrite string
}

// initPlan is everything the renderer needs, resolved and validated.
type initPlan struct {
	opts          initOptions
	providers     []string
	deployments   []deployment
	prices        []priceRow
	secret        string
	keyHash       string
	adminToken    string
	persistPath   string
	outAbs        string
	anthropicBase string
	openaiBase    string
	warnings      []string
}

const initUsage = `usage: kelvran init [--single-user] [--out config.yaml] [--provider auto|anthropic|openai|gemini|bedrock|openaicompat]
                    [--base-url URL] [--region REGION] [--listen 127.0.0.1:8080] [--models a,b]
                    [--upstream-model CANONICAL=BEDROCK_ID]... [--budget USD] [--price MODEL=PROMPT,COMPLETION]...
                    [--persist-path ABS] [--no-persist] [--dry-run] [--force]

Writes a minimal, priced config.yaml for the providers whose credential variables are set
(or the one named by --provider), generates the first virtual key and prints the client
exports once. The file holds no secret. --dry-run prints the YAML to stdout instead.
`

// Init runs `kelvran init` and returns the process exit code: 0 on success,
// 2 on a usage error (usage printed), 1 on any other failure (`kelvran init:
// <reason>` on stderr).
func Init(args []string, env IO) int {
	opts, code, done := parseInitFlags(args, env.Stderr)
	if done {
		return code
	}
	plan, err := resolveInit(opts, env)
	if err == nil {
		err = emitInit(plan, env)
	}
	if err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			_, _ = fmt.Fprintln(env.Stderr, "kelvran init:", ee.msg)
			if ee.code == 2 {
				_, _ = fmt.Fprint(env.Stderr, initUsage)
			}
			return ee.code
		}
		_, _ = fmt.Fprintln(env.Stderr, "kelvran init:", err)
		return 1
	}
	return 0
}

func parseInitFlags(args []string, stderr io.Writer) (initOptions, int, bool) {
	var o initOptions
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, initUsage) }
	fs.BoolVar(&o.singleUser, "single-user", false, "loopback listen, one unlimited key, no admin section")
	fs.StringVar(&o.out, "out", defaultOut, "config file to write")
	fs.StringVar(&o.provider, "provider", "auto", "auto|anthropic|openai|gemini|bedrock|openaicompat")
	fs.StringVar(&o.baseURL, "base-url", "", "openaicompat only: the server's OpenAI-compatible base URL")
	fs.StringVar(&o.region, "region", "", "bedrock only: AWS region (default AWS_REGION or AWS_DEFAULT_REGION)")
	fs.StringVar(&o.listen, "listen", defaultListenAddr, "listen_addr to write")
	fs.StringVar(&o.models, "models", "", "comma-separated canonical model ids (default: one per provider)")
	fs.Var(&o.upstream, "upstream-model", "bedrock only: CANONICAL=BEDROCK_ID (repeatable)")
	fs.StringVar(&o.budget, "budget", "", "budget_usd for the generated key (default: unlimited)")
	fs.Var(&o.prices, "price", "MODEL=PROMPT_PER_TOKEN,COMPLETION_PER_TOKEN in USD (repeatable)")
	fs.StringVar(&o.persistPath, "persist-path", "", "team mode: absolute admin.persist_path (default: beside the config)")
	fs.BoolVar(&o.noPersist, "no-persist", false, "team mode: omit admin.persist_path")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print the YAML to stdout, write nothing")
	fs.BoolVar(&o.force, "force", false, "overwrite an existing --out")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return o, 0, true
		}
		return o, 2, true
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "kelvran init: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return o, 2, true
	}
	return o, 0, false
}

// resolveInit turns options and the environment into a plan, or an
// exitError naming what is missing.
func resolveInit(o initOptions, env IO) (*initPlan, error) {
	p := &initPlan{opts: o}
	providers, err := selectProviders(o, env.Getenv)
	if err != nil {
		return nil, err
	}
	p.providers = providers
	if err := checkProviderScopedFlags(o, providers); err != nil {
		return nil, err
	}
	upstream, err := parsePairs(o.upstream, "--upstream-model", "CANONICAL=BEDROCK_ID")
	if err != nil {
		return nil, err
	}
	prices, err := parsePrices(o.prices)
	if err != nil {
		return nil, err
	}
	if o.budget != "" {
		d, err := decimal.NewFromString(o.budget)
		if err != nil || !d.IsPositive() {
			return nil, usageErr("--budget %q must be a positive decimal number of USD", o.budget)
		}
		p.opts.budget = d.String()
	}
	if _, _, err := ClientURLs(o.listen); err != nil {
		return nil, usageErr("--listen: %v", err)
	}
	p.anthropicBase, p.openaiBase, _ = ClientURLs(o.listen)
	host, _, _ := net.SplitHostPort(o.listen)
	if !isValidListenHost(host) {
		// The host is pasted into shell lines unquoted; a hostname or an IP
		// literal cannot carry shell syntax.
		return nil, usageErr("--listen host %q must be an IP address or a hostname", host)
	}
	if strings.ContainsAny(o.out, "'\n\r") {
		return nil, usageErr("--out must not contain a quote or a line break (it is pasted into shell lines)")
	}
	if !isLoopbackHost(host) {
		p.warnings = append(p.warnings, fmt.Sprintf("listen_addr %q is not loopback-only; every host that can reach it can present a virtual key", o.listen))
	}

	seenModel := map[string]bool{}
	for _, provider := range providers {
		deps, err := deploymentsFor(provider, o, env.Getenv, upstream)
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			p.deployments = append(p.deployments, d)
			if seenModel[d.Model] {
				continue
			}
			seenModel[d.Model] = true
			row, err := priceFor(provider, d.Model, prices)
			if err != nil {
				return nil, err
			}
			p.prices = append(p.prices, row)
		}
	}
	sort.Slice(p.prices, func(i, j int) bool { return p.prices[i].Model < p.prices[j].Model })

	if v := env.Getenv("ANTHROPIC_API_KEY"); strings.HasPrefix(v, oauthTokenPrefix) {
		// Gate G33, Stage 1: warn, never echo the value.
		p.warnings = append(p.warnings, "ANTHROPIC_API_KEY holds a value starting with "+oauthTokenPrefix+": that is a Claude subscription OAuth token, not an API key. Routing it through a gateway is own-token, single-user use, outside this gateway's scope today; use an API key from the Anthropic Console as the upstream credential")
	}

	p.secret, err = randomHex(env.Rand)
	if err != nil {
		return nil, err
	}
	p.keyHash = hashSecret(p.secret)
	if !o.singleUser {
		p.adminToken, err = randomHex(env.Rand)
		if err != nil {
			return nil, err
		}
	}
	p.outAbs, err = filepath.Abs(o.out)
	if err != nil {
		return nil, runtimeErr("resolving --out %q: %v", o.out, err)
	}
	// Resolve the directory's symlinks (filepath.Abs keeps a symlinked cwd)
	// so the /etc/kelvran-gateway/ persist-path rule sees the real location;
	// the file itself may not exist yet, so only the directory is resolved.
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p.outAbs)); err == nil {
		p.outAbs = filepath.Join(dir, filepath.Base(p.outAbs))
	}
	if !o.singleUser {
		p.persistPath, err = resolvePersistPath(o, p.outAbs)
		if err != nil {
			return nil, err
		}
		if p.persistPath == "" {
			p.warnings = append(p.warnings, "admin.persist_path omitted (--no-persist): keys created, rotated or deleted through the admin API will not survive a restart")
		}
	}
	return p, nil
}

// hostnameRe is an RFC 1123 hostname: labels of letters, digits and hyphens
// joined by dots. Together with net.ParseIP it is the whole set of listen
// hosts init accepts, so a printed URL can never carry shell syntax.
var hostnameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// awsRegionRe is the shape of an AWS region id (us-east-1, eu-west-2,
// us-gov-west-1, ap-southeast-2): the region is spliced into the Bedrock
// endpoint host, so anything else would redirect SigV4-signed requests.
var awsRegionRe = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)

func isValidListenHost(h string) bool {
	return h == "" || net.ParseIP(h) != nil || hostnameRe.MatchString(h)
}

// isLoopbackHost reports whether h binds only this host: a loopback IP or
// "localhost". An empty host (":8080") and the wildcards bind every
// interface, so they are not loopback.
func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// selectProviders honours --provider, or detects providers from the
// credential variables the generated config will name.
func selectProviders(o initOptions, getenv func(string) string) ([]string, error) {
	switch o.provider {
	case "auto":
	case "anthropic", "openai", "gemini", "bedrock", "openaicompat":
		return []string{o.provider}, nil
	default:
		return nil, usageErr("--provider %q is not one of auto, anthropic, openai, gemini, bedrock, openaicompat", o.provider)
	}
	var found []string
	if getenv("ANTHROPIC_API_KEY") != "" {
		found = append(found, "anthropic")
	}
	if getenv("OPENAI_API_KEY") != "" {
		found = append(found, "openai")
	}
	if getenv("GEMINI_API_KEY") != "" || getenv("GOOGLE_API_KEY") != "" {
		found = append(found, "gemini")
	}
	if getenv("AWS_ACCESS_KEY_ID") != "" && getenv("AWS_SECRET_ACCESS_KEY") != "" {
		found = append(found, "bedrock")
	}
	if len(found) == 0 {
		return nil, runtimeErr("no provider credential found in this environment. Looked for ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY (or GOOGLE_API_KEY), and AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY; export one, or pass --provider (openaicompat also needs --base-url, --models and --price)")
	}
	return found, nil
}

// checkProviderScopedFlags rejects flags that only mean something for a
// provider that was not selected, so nothing is silently ignored.
func checkProviderScopedFlags(o initOptions, providers []string) error {
	has := func(p string) bool {
		for _, q := range providers {
			if q == p {
				return true
			}
		}
		return false
	}
	if o.models != "" && len(providers) != 1 {
		return usageErr("--models applies to one provider; %d were detected (%s) — pass --provider", len(providers), strings.Join(providers, ", "))
	}
	if o.baseURL != "" && !has("openaicompat") {
		if has("bedrock") {
			return usageErr("--base-url is not accepted for bedrock: the Converse URL is derived from --region and the Bedrock model id")
		}
		return usageErr("--base-url is only used with --provider openaicompat")
	}
	if len(o.upstream) > 0 && !has("bedrock") {
		return usageErr("--upstream-model is only used with --provider bedrock")
	}
	if o.region != "" && !has("bedrock") {
		return usageErr("--region is only used with --provider bedrock")
	}
	if has("openaicompat") {
		if o.baseURL == "" || o.models == "" {
			return usageErr("--provider openaicompat needs --base-url and --models (it is a protocol, not a vendor: no default endpoint or model set exists)")
		}
	}
	if o.singleUser && (o.persistPath != "" || o.noPersist) {
		return usageErr("--persist-path and --no-persist configure the admin section, which --single-user omits")
	}
	return nil
}

func parsePairs(values []string, flagName, shape string) (map[string]string, error) {
	out := map[string]string{}
	for _, v := range values {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" || val == "" {
			return nil, usageErr("%s %q is not %s", flagName, v, shape)
		}
		out[k] = val
	}
	return out, nil
}

// parsePrices reads --price MODEL=PROMPT,COMPLETION into normalised
// decimal strings; zero is allowed (the documented way to opt into $0
// knowingly), negatives are not.
func parsePrices(values []string) (map[string]priceRow, error) {
	out := map[string]priceRow{}
	for _, v := range values {
		model, rest, ok := strings.Cut(v, "=")
		prompt, completion, ok2 := strings.Cut(rest, ",")
		if !ok || !ok2 || model == "" {
			return nil, usageErr("--price %q is not MODEL=PROMPT_PER_TOKEN,COMPLETION_PER_TOKEN", v)
		}
		pd, err1 := decimal.NewFromString(strings.TrimSpace(prompt))
		cd, err2 := decimal.NewFromString(strings.TrimSpace(completion))
		if err1 != nil || err2 != nil || pd.IsNegative() || cd.IsNegative() {
			return nil, usageErr("--price %q: both rates must be non-negative decimals in USD per token", v)
		}
		out[model] = priceRow{Model: model, Prompt: pd.String(), Completion: cd.String()}
	}
	return out, nil
}

func splitModels(csv string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, m := range strings.Split(csv, ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if seen[m] {
			return nil, usageErr("--models lists %q twice", m)
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, usageErr("--models is empty")
	}
	return out, nil
}

// deploymentsFor builds provider's deployment blocks: one per model.
func deploymentsFor(provider string, o initOptions, getenv func(string) string, upstream map[string]string) ([]deployment, error) {
	var modelIDs []string
	if o.models != "" {
		var err error
		if modelIDs, err = splitModels(o.models); err != nil {
			return nil, err
		}
	} else {
		modelIDs = []string{defaultModel[provider]}
	}
	var out []deployment
	nameOf := map[string]string{}
	for _, model := range modelIDs {
		d := deployment{Name: deploymentName(provider, model), Model: model, Provider: provider, Upstream: model}
		if other, dup := nameOf[d.Name]; dup {
			return nil, usageErr("--models %q and %q would both become the deployment key %q (characters outside [A-Za-z0-9._-] are replaced by -); rename one", other, model, d.Name)
		}
		nameOf[d.Name] = model
		switch provider {
		case "anthropic":
			d.BaseURL = "https://api.anthropic.com/v1/messages"
			d.APIKeyEnv = "ANTHROPIC_API_KEY"
		case "openai":
			d.BaseURL = "https://api.openai.com/v1/chat/completions"
			d.APIKeyEnv = "OPENAI_API_KEY"
		case "gemini":
			d.BaseURL = "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent"
			d.APIKeyEnv = "GEMINI_API_KEY"
			if getenv("GEMINI_API_KEY") == "" && getenv("GOOGLE_API_KEY") != "" {
				d.APIKeyEnv = "GOOGLE_API_KEY"
			}
		case "bedrock":
			region := o.region
			if region == "" {
				region = getenv("AWS_REGION")
			}
			if region == "" {
				region = getenv("AWS_DEFAULT_REGION")
			}
			if region == "" {
				return nil, runtimeErr("bedrock needs a region: pass --region or set AWS_REGION (init refuses to guess one)")
			}
			if !awsRegionRe.MatchString(region) {
				if o.region != "" {
					return nil, usageErr("--region %q is not an AWS region id (expected a shape like us-east-1)", region)
				}
				// The environment value is not echoed.
				return nil, runtimeErr("the AWS_REGION/AWS_DEFAULT_REGION value is not an AWS region id (expected a shape like us-east-1); pass --region")
			}
			id, err := bedrockIDFor(model, upstream)
			if err != nil {
				return nil, runtimeErr("%v", err)
			}
			d.Upstream = id
			d.Region = region
			d.BaseURL = "https://bedrock-runtime." + region + ".amazonaws.com/model/" + id + "/converse"
			d.SessionTokenEnv = getenv("AWS_SESSION_TOKEN") != ""
		case "openaicompat":
			// The URL is never echoed: it may carry a password a user pasted by
			// mistake, which is exactly what the next check refuses.
			u, err := url.Parse(o.baseURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, usageErr("--base-url must be an http(s) URL with a host")
			}
			if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return nil, usageErr("--base-url must not carry credentials, a query or a fragment: the deployment reads OPENAI_API_KEY and posts to the path as given")
			}
			if !strings.HasSuffix(u.Path, "/chat/completions") {
				u.Path = strings.TrimRight(u.Path, "/") + "/chat/completions"
			}
			d.BaseURL = u.String()
			d.APIKeyEnv = "OPENAI_API_KEY"
			d.Insecure = u.Scheme == "http"
		}
		out = append(out, d)
	}
	return out, nil
}

// priceFor resolves the price_table row for model under provider: --price
// first, then the embedded table (openaicompat never uses the table — it
// prices a vendor's hosted endpoint, not an arbitrary compatible server).
func priceFor(provider, model string, overrides map[string]priceRow) (priceRow, error) {
	if row, ok := overrides[model]; ok {
		return row, nil
	}
	if provider == "openaicompat" {
		return priceRow{}, runtimeErr("--provider openaicompat prices nothing by default: pass --price %s=<prompt_per_token>,<completion_per_token> (use --price %s=0,0 to opt into $0 knowingly)", model, model)
	}
	e, ok := lookupModel(model)
	if !ok || e.Vendor != providerVendor[provider] {
		return priceRow{}, runtimeErr("%q is not in the embedded price table for %s (as of %s it prices %s); pass --price %s=<prompt_per_token>,<completion_per_token> — an unpriced model never decrements a budget",
			model, provider, priceTableAsOf, strings.Join(embeddedModelsFor(provider), ", "), model)
	}
	return priceRow{Model: model, Prompt: e.PromptUSD, Completion: e.CompletionUSD, CacheRead: e.CacheReadUSD, CacheWrite: e.CacheWriteUSD}, nil
}

// resolvePersistPath picks admin.persist_path for team mode: --persist-path
// (absolute), or none under --no-persist, or a default beside the config —
// /var/lib/kelvran-gateway/identity.db when the config lives in the
// package's /etc/kelvran-gateway/ (the unit's StateDirectory).
func resolvePersistPath(o initOptions, outAbs string) (string, error) {
	if o.noPersist {
		return "", nil
	}
	if o.persistPath != "" {
		if !filepath.IsAbs(o.persistPath) {
			return "", usageErr("--persist-path must be absolute (bbolt resolves it against the gateway's working directory, which the systemd unit does not set)")
		}
		return o.persistPath, nil
	}
	if strings.HasPrefix(outAbs, packagedConfigDir) {
		return packagedStateDB, nil
	}
	return filepath.Join(filepath.Dir(outAbs), persistFileName), nil
}
