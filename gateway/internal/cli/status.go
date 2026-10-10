package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/kelvran/gateway/gateway/internal/adminapi"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

const statusUsage = `usage: kelvran status [--url http://127.0.0.1:8080] [--admin-url URL] [--admin-token-file PATH] [--config PATH]
                      [--allow-insecure-http] [--json]

Shows what the gateway serves: /readyz (with --url), listen_addr, the deployment table, the key count and
the admin store. Live from the admin API whenever an admin token resolves (--admin-token-file,
KELVRAN_ADMIN_TOKEN_FILE, the variable the config's admin.token_env names, KELVRAN_ADMIN_TOKEN); otherwise
from the config file, labelled as such, with the static columns only. Exit 1 when a probed data plane is
not ready or unreachable.
`

type statusOptions struct {
	url, config, adminURL, adminTokenFile string
	allowInsecure, jsonOut                bool
}

// statusDoc is `kelvran status --json`: one composite document, public
// surface (docs/reference/kelvran-cli.md) pinned by testdata/status_*.golden.json.
// Fields may be added, never renamed or removed.
type statusDoc struct {
	Mode        string             `json:"mode"`   // online | offline
	Source      string             `json:"source"` // the admin base URL, or the config path
	Readyz      *readyzResult      `json:"readyz,omitempty"`
	ListenAddr  string             `json:"listen_addr"`
	Deployments []statusDeployment `json:"deployments"`
	VirtualKeys statusKeys         `json:"virtual_keys"`
	AdminStore  string             `json:"admin_store"`
	Notes       []string           `json:"notes,omitempty"`
}

// readyzResult is one GET /readyz: the status, the parsed ready flag and
// the models the body reports without a healthy deployment.
type readyzResult struct {
	URL        string   `json:"url"`
	Status     int      `json:"status,omitempty"`
	Ready      bool     `json:"ready"`
	ModelsDown []string `json:"models_without_healthy_deployment,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// statusDeployment is one deployment row: the static columns from the config
// (both modes) and the live ones only online.
type statusDeployment struct {
	Name                 string `json:"name"`
	Model                string `json:"model"`
	UpstreamModel        string `json:"upstream_model"`
	Provider             string `json:"provider"`
	Kind                 string `json:"kind"`
	Healthy              *bool  `json:"healthy,omitempty"`
	Weight               *int   `json:"weight,omitempty"`
	LatencyFactorPercent *int   `json:"latency_factor_percent,omitempty"`
	Sticky               *bool  `json:"sticky,omitempty"`
}

// statusKeys: the keys the config declares, and online the keys the gateway
// serves right now (admin-API changes included).
type statusKeys struct {
	Configured int  `json:"configured"`
	Live       *int `json:"live,omitempty"`
}

// Status runs `kelvran status` and returns the exit code: 0, 1 when a probed
// data plane is not ready or a request failed, 2 on a usage error.
func Status(args []string, env IO) int {
	var o statusOptions
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(escapingWriter{env.Stderr})
	fs.Usage = func() { _, _ = fmt.Fprint(env.Stderr, statusUsage) }
	fs.StringVar(&o.url, "url", "", "data-plane base URL to probe (GET /readyz)")
	fs.StringVar(&o.config, "config", "", "config file: read offline, and names the admin.token_env variable online")
	fs.StringVar(&o.adminURL, "admin-url", "", "admin base URL (else KELVRAN_ADMIN_URL, else http://127.0.0.1:8081)")
	fs.StringVar(&o.adminTokenFile, "admin-token-file", "", "file holding the admin token (else KELVRAN_ADMIN_TOKEN_FILE, the config's admin.token_env variable, KELVRAN_ADMIN_TOKEN)")
	fs.BoolVar(&o.allowInsecure, "allow-insecure-http", false, "send the admin token to a non-loopback http:// admin URL")
	fs.BoolVar(&o.jsonOut, "json", false, "print one JSON document instead of text")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(env.Stderr, "kelvran status: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	return exitWith("kelvran status", runStatus(o, env), env, fs.Usage)
}

// exitWith prints a verb's failure and maps it to the exit code: 2 (usage,
// with the usage text) or 1; every message passes through the escaper.
func exitWith(prefix string, err error, env IO, usage func()) int {
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		_, _ = fmt.Fprintln(env.Stderr, prefix+":", sanitizeCell(ee.msg))
		if ee.code == 2 {
			usage()
		}
		return ee.code
	}
	_, _ = fmt.Fprintln(env.Stderr, prefix+":", sanitizeCell(err.Error()))
	return 1
}

func runStatus(o statusOptions, env IO) error {
	t, err := resolveKeysTarget(keysOptions{tool: "status", config: o.config, adminURL: o.adminURL, adminTokenFile: o.adminTokenFile, allowInsecure: o.allowInsecure}, env)
	if err != nil {
		return err
	}
	ctx := context.Background()
	var doc statusDoc
	if o.url != "" {
		if err := guardHTTPURL("--url", o.url); err != nil {
			return runtimeErr("%v", err)
		}
		doc.Readyz = probeReadyz(ctx, strings.TrimRight(o.url, "/"))
	}
	if t.online {
		err = fillStatusOnline(ctx, t, &doc)
	} else {
		fillStatusOffline(t, &doc)
	}
	if err != nil {
		return err
	}
	if err := printStatus(env, o, doc); err != nil {
		return err
	}
	if doc.Readyz != nil && !doc.Readyz.Ready {
		return runtimeErr("the data plane at %s is not ready", o.url)
	}
	return nil
}

// probeReadyz is one bearer-less GET /readyz: 200 means ready, 503 not
// ready; the body names the models without a healthy deployment.
func probeReadyz(ctx context.Context, base string) *readyzResult {
	r := &readyzResult{URL: base + "/readyz"}
	status, body, err := dataPlaneGet(ctx, base, "/readyz")
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Status = status
	var parsed struct {
		Ready  bool            `json:"ready"`
		Models map[string]bool `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		// A proxy page or a stranger answering 200 is not the gateway: fail
		// closed rather than read it as ready.
		r.Error = "the body is not the gateway's /readyz JSON"
		return r
	}
	r.Ready = status == http.StatusOK && parsed.Ready
	for m, ok := range parsed.Models {
		if !ok {
			r.ModelsDown = append(r.ModelsDown, m)
		}
	}
	sort.Strings(r.ModelsDown)
	return r
}

// fillStatusOnline reads GET /admin/config (PascalCase JSON of the served
// config; only listen_addr, the configured key count and the admin store are
// decoded — the body carries key hashes, which are never read), GET
// /admin/deployments (the live table) and GET /admin/virtual_keys (the live
// key count).
func fillStatusOnline(ctx context.Context, t *keysTarget, doc *statusDoc) error {
	var served struct {
		ListenAddr  string
		VirtualKeys []struct{ Name string }
		Admin       struct{ PersistPath, RedisAddr string }
	}
	if _, err := t.client.getJSON(ctx, "/admin/config", &served); err != nil {
		return adminErr(t, err)
	}
	var live []adminapi.DeploymentEntry
	if _, err := t.client.getJSON(ctx, "/admin/deployments", &live); err != nil {
		return adminErr(t, err)
	}
	var keys []adminapi.VirtualKeyListEntry
	if _, err := t.client.getJSON(ctx, "/admin/virtual_keys", &keys); err != nil {
		return adminErr(t, err)
	}
	doc.Mode, doc.Source, doc.ListenAddr = "online", t.base, served.ListenAddr
	doc.Deployments = make([]statusDeployment, 0, len(live))
	for _, d := range live {
		healthy, weight, latency, sticky := d.Healthy, d.Weight, d.LatencyFactorPercent, d.Sticky
		doc.Deployments = append(doc.Deployments, statusDeployment{Name: d.Name, Model: d.Model, UpstreamModel: d.UpstreamModel, Provider: d.Provider, Kind: kindOrChat(d.Kind), Healthy: &healthy, Weight: &weight, LatencyFactorPercent: &latency, Sticky: &sticky})
	}
	n := len(keys)
	doc.VirtualKeys = statusKeys{Configured: len(served.VirtualKeys), Live: &n}
	doc.AdminStore = adminStore(served.Admin.PersistPath, served.Admin.RedisAddr, true)
	return nil
}

// fillStatusOffline reads the file: the static columns only, labelled as the
// file's view, with decision 10's line on how to get the live one.
func fillStatusOffline(t *keysTarget, doc *statusDoc) {
	cfg := t.cfg
	doc.Mode, doc.Source, doc.ListenAddr = "offline", t.cfgAbs, cfg.ListenAddr
	doc.Deployments = make([]statusDeployment, 0, len(cfg.Deployments))
	for _, d := range cfg.Deployments {
		doc.Deployments = append(doc.Deployments, statusDeployment{Name: d.Name, Model: d.Model, UpstreamModel: d.UpstreamModel, Provider: d.Provider, Kind: kindOrChat(d.Kind)})
	}
	doc.VirtualKeys = statusKeys{Configured: len(cfg.VirtualKeys)}
	doc.AdminStore = adminStore(cfg.Admin.PersistPath, cfg.Admin.RedisAddr, cfg.Admin.TokenEnv != "")
	doc.Notes = []string{"from " + t.cfgAbs + "; a running gateway reflects this file only after restart", liveStateNote(cfg)}
}

func kindOrChat(kind string) string {
	if kind == "" {
		return "chat"
	}
	return kind
}

// adminStore names where admin-API key changes persist, or why they do not.
func adminStore(persistPath, redisAddr string, adminPresent bool) string {
	switch {
	case redisAddr != "":
		if strings.Contains(redisAddr, "://") {
			// A URL form can carry a password; a bare host:port cannot and
			// must stay legible (redactOneURL withholds anything schemeless).
			redisAddr = redactedURL(redisAddr)
		}
		return "redis_addr " + redisAddr
	case persistPath != "":
		return "persist_path " + persistPath
	case adminPresent:
		return "none (keys changed through the admin API are in-memory only)"
	}
	return "none (single-user: no admin section)"
}

// liveStateNote is decision 10's one line for the offline view: what the
// file cannot show and how to get it.
func liveStateNote(cfg *controlplane.Config) string {
	if cfg.Admin.TokenEnv == "" {
		return "live key/deployment state and spend need the admin API; this config has no `admin:` section (single-user mode) — re-run `init` without `--single-user`, or add a block-mapping `admin:` section with `token_env: " + adminTokenEnvName + "` (the admin listener defaults to `127.0.0.1:8081`, so it stays loopback-only)"
	}
	if !envNameRe.MatchString(cfg.Admin.TokenEnv) {
		// A pasted value where a variable name belongs (doctor reports it as an error): never echoed.
		return "live key/deployment state and spend need the admin API, but admin.token_env does not name an environment variable (value not shown); run `kelvran doctor`"
	}
	return "live key/deployment state and spend need the admin API: export " + cfg.Admin.TokenEnv + " (the variable admin.token_env names) in this shell, or pass --admin-token-file"
}

func printStatus(env IO, o statusOptions, doc statusDoc) error {
	if o.jsonOut {
		return jsonOut(env, doc)
	}
	out := &printer{w: env.Stdout}
	if doc.Mode == "online" {
		out.printf("kelvran status — online via %s\n", sanitizeCell(doc.Source))
	} else {
		out.printf("kelvran status — offline from %s; a running gateway reflects this file only after restart\n", sanitizeCell(doc.Source))
	}
	switch r := doc.Readyz; {
	case r == nil:
		out.println("readyz:        not probed (pass --url http://<listen_addr> to check the data plane)")
	case r.Error != "" && r.Status == 0:
		out.printf("readyz:        unreachable at %s (%s)\n", sanitizeCell(r.URL), sanitizeCell(r.Error))
	case r.Error != "":
		out.printf("readyz:        %d not ready (%s): %s\n", r.Status, sanitizeCell(r.URL), sanitizeCell(r.Error))
	case r.Ready:
		out.printf("readyz:        %d ready (%s)\n", r.Status, sanitizeCell(r.URL))
	default:
		down := ""
		if len(r.ModelsDown) > 0 {
			down = "; models without a healthy deployment: " + sanitizeCell(strings.Join(r.ModelsDown, ", "))
		}
		out.printf("readyz:        %d not ready (%s)%s\n", r.Status, sanitizeCell(r.URL), down)
	}
	out.printf("listen_addr:   %s\n", sanitizeCell(doc.ListenAddr))
	if doc.Mode == "online" {
		healthy := 0
		for _, d := range doc.Deployments {
			if d.Healthy != nil && *d.Healthy {
				healthy++
			}
		}
		out.printf("deployments:   %d configured, %d healthy\n", len(doc.Deployments), healthy)
		out.printf("virtual keys:  %d live, %d in the served config\n", *doc.VirtualKeys.Live, doc.VirtualKeys.Configured)
	} else {
		out.printf("deployments:   %d configured (static view)\n", len(doc.Deployments))
		out.printf("virtual keys:  %d in the file\n", doc.VirtualKeys.Configured)
	}
	out.printf("admin store:   %s\n", sanitizeCell(doc.AdminStore))
	for _, n := range doc.Notes {
		if strings.HasPrefix(n, "from ") {
			continue // the headline already says it; --json keeps it in notes
		}
		out.println(sanitizeCell(n))
	}
	out.println()
	tbl := keysTable{header: []string{"name", "model", "upstream_model", "provider", "kind"}}
	if doc.Mode == "online" {
		tbl.header = append(tbl.header, "healthy", "weight", "latency_factor_percent", "sticky")
	}
	for _, d := range doc.Deployments {
		row := []string{d.Name, d.Model, d.UpstreamModel, d.Provider, d.Kind}
		if doc.Mode == "online" {
			row = append(row, yesNo(d.Healthy), intOrDash(d.Weight), intOrDash(d.LatencyFactorPercent), yesNo(d.Sticky))
		}
		tbl.rows = append(tbl.rows, row)
	}
	tbl.print(out)
	return firstErr(out)
}

func yesNo(b *bool) string {
	switch {
	case b == nil:
		return "-"
	case *b:
		return "yes"
	}
	return "no"
}

func intOrDash(n *int) string {
	if n == nil {
		return "-"
	}
	return strconv.Itoa(*n)
}
