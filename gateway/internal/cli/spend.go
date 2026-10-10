package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/kelvran/gateway/gateway/internal/adminapi"
)

const spendUsage = `usage: kelvran spend [--by key] [--admin-url URL] [--admin-token-file PATH] [--config PATH]
                     [--allow-insecure-http] [--json]

Spend per virtual key from GET /admin/virtual_keys?include=spend: key | spent_usd | budget_usd |
percent_used | expires_at. A row whose spend could not be read shows n/a, a footer counts them and the
exit code is 1. Needs an admin token (--admin-token-file, KELVRAN_ADMIN_TOKEN_FILE, the variable the
config's admin.token_env names, KELVRAN_ADMIN_TOKEN): spend lives only in the gateway's budget store, so
there is no offline view. --by model|tool|session waits for the spend ledger (plan item 13d).
`

// spendLedgerNote is the refusal for the --by dimensions the ledger (RFC-2,
// docs/rfcs/2026-10-09-gateway-attribution-and-spend-ledger.md) will serve.
const spendLedgerNote = "needs the spend ledger (plan item 13d, RFC-2 docs/rfcs/2026-10-09-gateway-attribution-and-spend-ledger.md); only --by key is served today"

type spendOptions struct {
	by, config, adminURL, adminTokenFile string
	allowInsecure, jsonOut               bool
}

// Spend runs `kelvran spend --by key` and returns the exit code: 0, 1 when
// a request failed, no token is in reach, or a row's spend is unavailable,
// 2 on a usage error.
func Spend(args []string, env IO) int {
	var o spendOptions
	fs := flag.NewFlagSet("spend", flag.ContinueOnError)
	fs.SetOutput(escapingWriter{env.Stderr})
	fs.Usage = func() { _, _ = fmt.Fprint(env.Stderr, spendUsage) }
	fs.StringVar(&o.by, "by", "key", "dimension: key (model, tool and session wait for the spend ledger, plan item 13d)")
	fs.StringVar(&o.config, "config", "", "config file naming the admin.token_env variable")
	fs.StringVar(&o.adminURL, "admin-url", "", "admin base URL (else KELVRAN_ADMIN_URL, else http://127.0.0.1:8081)")
	fs.StringVar(&o.adminTokenFile, "admin-token-file", "", "file holding the admin token (else KELVRAN_ADMIN_TOKEN_FILE, the config's admin.token_env variable, KELVRAN_ADMIN_TOKEN)")
	fs.BoolVar(&o.allowInsecure, "allow-insecure-http", false, "send the admin token to a non-loopback http:// admin URL")
	fs.BoolVar(&o.jsonOut, "json", false, "print the served entries as one JSON document")
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
	case "model", "tool", "session":
		return usageErr("--by %s %s", o.by, spendLedgerNote)
	default:
		return usageErr("--by %q is not one of key, model, tool, session", o.by)
	}
	t, err := resolveKeysTarget(keysOptions{tool: "spend", config: o.config, adminURL: o.adminURL, adminTokenFile: o.adminTokenFile, allowInsecure: o.allowInsecure}, env)
	if err != nil {
		return err
	}
	if !t.online {
		// Spend lives in the budget store the admin API fronts; the file has none (decision 10 fails closed).
		return runtimeErr("%s", liveStateNote(t.cfg))
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

// spendRow renders decision 10's columns from the cell spellings `keys list`
// shares (budgetCell, expiresCell, spendCells).
func spendRow(e adminapi.VirtualKeyListEntry) []string {
	spent, pct := spendCells(e)
	return []string{e.ID, spent, budgetCell(e), pct, expiresCell(e)}
}
