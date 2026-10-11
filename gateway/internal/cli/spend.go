package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adminapi"
)

const spendUsage = `usage: kelvran spend [--by key|model|tool|session] [--key NAME] [--since RFC3339] [--until RFC3339]
                     [--admin-url URL] [--admin-token-file PATH] [--config PATH] [--allow-insecure-http] [--json]

--by key (the default): spend per virtual key from GET /admin/virtual_keys?include=spend: key | spent_usd |
budget_usd | percent_used | expires_at. A row whose spend could not be read shows n/a, a footer counts them
and the exit code is 1.
--by model|tool|session: the spend ledger from GET /admin/spend, aggregated over a window (default the
last 24 h, clamped to the gateway's ledger.retention_hours; the served window is printed first): the
dimension | spend_usd | requests, with a key column when --key narrows to one key or --by is session. A
cost-viewer token reads only with --key. A session named other is what the ledger's caps folded.
Needs an admin token (--admin-token-file, KELVRAN_ADMIN_TOKEN_FILE, the variable the config's
admin.token_env names, KELVRAN_ADMIN_TOKEN): spend lives only behind the admin API, so there is no
offline view.
`

type spendOptions struct {
	by, key, since, until, config, adminURL, adminTokenFile string
	allowInsecure, jsonOut                                  bool
}

// Spend runs `kelvran spend` and returns the exit code: 0, 1 when a request
// failed, no token is in reach, or a row's spend is unavailable, 2 on a
// usage error.
func Spend(args []string, env IO) int {
	var o spendOptions
	fs := flag.NewFlagSet("spend", flag.ContinueOnError)
	fs.SetOutput(escapingWriter{env.Stderr})
	fs.Usage = func() { _, _ = fmt.Fprint(env.Stderr, spendUsage) }
	fs.StringVar(&o.by, "by", "key", "dimension: key (the budget view), or model, tool or session (the spend ledger)")
	fs.StringVar(&o.key, "key", "", "with --by model|tool|session: one virtual key (what a cost-viewer token needs)")
	fs.StringVar(&o.since, "since", "", "with --by model|tool|session: the window's start, RFC 3339 (default until minus 24 h)")
	fs.StringVar(&o.until, "until", "", "with --by model|tool|session: the window's end, exclusive, RFC 3339 (default now)")
	fs.StringVar(&o.config, "config", "", "config file naming the admin.token_env variable")
	fs.StringVar(&o.adminURL, "admin-url", "", "admin base URL (else KELVRAN_ADMIN_URL, else http://127.0.0.1:8081)")
	fs.StringVar(&o.adminTokenFile, "admin-token-file", "", "file holding the admin token (else KELVRAN_ADMIN_TOKEN_FILE, the config's admin.token_env variable, KELVRAN_ADMIN_TOKEN)")
	fs.BoolVar(&o.allowInsecure, "allow-insecure-http", false, "send the admin token to a non-loopback http:// admin URL")
	fs.BoolVar(&o.jsonOut, "json", false, "print the served document as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(env.Stderr, "kelvran spend: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	return exitWith("kelvran spend", runSpend(o, env), env, fs.Usage)
}

func runSpend(o spendOptions, env IO) error {
	switch o.by {
	case "key":
		if o.key != "" || o.since != "" || o.until != "" {
			return usageErr("--key, --since and --until apply to --by model|tool|session; --by key lists every key's budget view")
		}
	case "model", "tool", "session":
		for _, b := range []struct{ flag, value string }{{"since", o.since}, {"until", o.until}} {
			if b.value == "" {
				continue
			}
			if _, err := time.Parse(time.RFC3339, b.value); err != nil {
				return usageErr("--%s %q is not an RFC 3339 timestamp (like 2026-10-11T00:00:00Z)", b.flag, b.value)
			}
		}
	default:
		return usageErr("--by %q is not one of key, model, tool, session", o.by)
	}
	t, err := resolveKeysTarget(keysOptions{tool: "spend", config: o.config, adminURL: o.adminURL, adminTokenFile: o.adminTokenFile, allowInsecure: o.allowInsecure}, env)
	if err != nil {
		return err
	}
	if !t.online {
		// Spend lives behind the admin API -- the budget store and the spend
		// ledger; the file has neither (decision 10 fails closed).
		return runtimeErr("%s", liveStateNote(t.cfg))
	}
	if o.by != "key" {
		return runSpendLedger(o, t, env)
	}
	var entries []adminapi.VirtualKeyListEntry
	if _, err := t.client.getJSON(context.Background(), "/admin/virtual_keys?include=spend", &entries); err != nil {
		return adminErr(t, err)
	}
	if o.jsonOut {
		if entries == nil {
			entries = []adminapi.VirtualKeyListEntry{}
		}
		if err := jsonOut(env, entries); err != nil {
			return err
		}
	} else {
		out := &printer{w: env.Stdout}
		tbl := keysTable{header: []string{"key", "spent_usd", "budget_usd", "percent_used", "expires_at"}}
		for _, e := range entries {
			tbl.rows = append(tbl.rows, spendRow(e))
		}
		tbl.print(out)
		if err := firstErr(out); err != nil {
			return err
		}
	}
	unavailable := 0
	for _, e := range entries {
		if e.SpendUnavailable {
			unavailable++
		}
	}
	if unavailable > 0 {
		return runtimeErr("spend unavailable for %d key(s): budget backend error", unavailable)
	}
	return nil
}

// runSpendLedger serves --by model|tool|session from GET /admin/spend (item
// 13d): the dimension, the optional key and window pass through as query
// parameters; the served window (after the gateway's retention clamp) is
// printed before the table so a short answer is never mistaken for a short
// history. A key column appears when the rows carry one: --key named a key,
// or --by session (the ledger keys sessions per virtual key).
func runSpendLedger(o spendOptions, t *keysTarget, env IO) error {
	q := url.Values{"group_by": {o.by}}
	for name, value := range map[string]string{"key": o.key, "since": o.since, "until": o.until} {
		if value != "" {
			q.Set(name, value)
		}
	}
	var resp adminapi.SpendResponse
	if _, err := t.client.getJSON(context.Background(), "/admin/spend?"+q.Encode(), &resp); err != nil {
		// The client never prints a 401 body (it could echo the token), so the
		// one documented 401 of this route is spelled out here: a cost-viewer
		// token reads the ledger only with --key.
		var httpErr *adminHTTPError
		if errors.As(err, &httpErr) && httpErr.status == http.StatusUnauthorized && o.key == "" {
			return runtimeErr("%v (a cost-viewer token reads --by %s only with --key NAME; admin and viewer tokens read without it)", err, o.by)
		}
		return adminErr(t, err)
	}
	if resp.Rows == nil {
		resp.Rows = []adminapi.SpendRow{}
	}
	if o.jsonOut {
		return jsonOut(env, resp)
	}
	out := &printer{w: env.Stdout}
	out.printf("window %s .. %s\n", resp.Since.UTC().Format(time.RFC3339), resp.Until.UTC().Format(time.RFC3339))
	withKey := o.key != "" || o.by == "session"
	header := []string{o.by, "spend_usd", "requests"}
	if withKey {
		header = append([]string{"key"}, header...)
	}
	tbl := keysTable{header: header}
	for _, r := range resp.Rows {
		dimension := map[string]string{"model": r.Model, "tool": r.Tool, "session": r.Session}[o.by]
		row := []string{dimension, r.SpendUSD, fmt.Sprint(r.Requests)}
		if withKey {
			row = append([]string{r.Key}, row...)
		}
		tbl.rows = append(tbl.rows, row)
	}
	tbl.print(out)
	return firstErr(out)
}

// spendRow renders decision 10's columns from the cell spellings `keys list`
// shares (budgetCell, expiresCell, spendCells).
func spendRow(e adminapi.VirtualKeyListEntry) []string {
	spent, pct := spendCells(e)
	return []string{e.ID, spent, budgetCell(e), pct, expiresCell(e)}
}
