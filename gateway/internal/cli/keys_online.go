package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adminapi"
)

// persistenceWarning is printed after an online create/rotate/delete when the
// served config has neither admin.persist_path nor admin.redis_addr (RFC-3
// decision 3): the mutation lives in memory only.
const persistenceWarning = "warning: this gateway has no admin.persist_path or admin.redis_addr — this change is lost at the next restart (docs/reference/admin-api.md)"

func runKeysOnline(o keysOptions, spec keySpec, t *keysTarget, env IO) error {
	ctx := context.Background()
	switch o.verb {
	case "list":
		return listKeysOnline(ctx, o, t, env)
	case "create":
		return createKeyOnline(ctx, o, spec, t, env)
	case "rotate":
		return rotateKeyOnline(ctx, o, spec, t, env)
	default:
		return deleteKeyOnline(ctx, o, t, env)
	}
}

// adminErr maps a client error: a transport failure is decision 10's
// "unreachable" line (a single-user config has no admin listener), a non-2xx
// answer is reported as the client already summarised it.
func adminErr(t *keysTarget, err error) error {
	var httpErr *adminHTTPError
	if errors.As(err, &httpErr) {
		return runtimeErr("%v", err)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// A transport error can quote what the far end sent, bearer included
		// (a malformed status line echoing the request): redact it like a body.
		return runtimeErr("admin API unreachable at %s; if this is a single-user config it has no admin listener (%s)", t.base, redactSecrets(urlErr.Err.Error(), t.client.secrets))
	}
	// A body-read error (a chunked trailer that is the bearer with no colon)
	// quotes the server's bytes too.
	return runtimeErr("%s", redactSecrets(err.Error(), t.client.secrets))
}

// redactSecrets replaces every token variant in s.
func redactSecrets(s string, secrets []string) string {
	for _, v := range secrets {
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}

func listKeysOnline(ctx context.Context, o keysOptions, t *keysTarget, env IO) error {
	path := "/admin/virtual_keys"
	if o.spend {
		path += "?include=spend"
	}
	var entries []adminapi.VirtualKeyListEntry
	if _, err := t.client.getJSON(ctx, path, &entries); err != nil {
		return adminErr(t, err)
	}
	return printKeyList(o, entries, env, "")
}

// printKeyList renders the table (or --json) both modes share; with --spend
// a row whose spend could not be read shows n/a, and the footer plus a
// non-zero exit keep the table from passing as an all-zero fleet (decision 5).
func printKeyList(o keysOptions, entries []adminapi.VirtualKeyListEntry, env IO, note string) error {
	if o.jsonOut {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if entries == nil {
			entries = []adminapi.VirtualKeyListEntry{}
		}
		if err := enc.Encode(entries); err != nil {
			return runtimeErr("writing output: %v", err)
		}
	} else {
		out := &printer{w: env.Stdout}
		if note != "" {
			out.println(note)
		}
		tbl := keysTable{header: listHeader(o.spend)}
		for _, e := range entries {
			tbl.rows = append(tbl.rows, listRow(e, o.spend))
		}
		tbl.print(out)
		if err := firstErr(out); err != nil {
			return err
		}
	}
	unavailable := 0
	for _, e := range entries {
		if o.spend && e.SpendUnavailable {
			unavailable++
		}
	}
	if unavailable > 0 {
		return runtimeErr("spend unavailable for %d key(s): budget backend error", unavailable)
	}
	return nil
}

func createKeyOnline(ctx context.Context, o keysOptions, spec keySpec, t *keysTarget, env IO) error {
	// The upsert answers 204 for create and replace alike, so an existing
	// name is detected first (decision 3) and the full-replace drops are
	// named (decision 4).
	var entries []adminapi.VirtualKeyListEntry
	if _, err := t.client.getJSON(ctx, "/admin/virtual_keys", &entries); err != nil {
		return adminErr(t, err)
	}
	for _, e := range entries {
		if e.ID != spec.name {
			continue
		}
		if !o.replace {
			return runtimeErr("virtual key %q already exists; pass --replace to overwrite it (a full replace: every field not on this command line is cleared)", spec.name)
		}
		var dropped []string
		if e.PreviousKeyHashExpiresAt != "" {
			dropped = append(dropped, "previous_key_hash_expires_at (the in-flight rotation grace ends at once)")
		}
		if e.MaxConcurrentRequests != 0 {
			dropped = append(dropped, "max_concurrent_requests (not settable through the admin API)")
		}
		if e.BillingSubjectID != "" && spec.billing == "" {
			dropped = append(dropped, "billing_subject_id (pass --billing-subject to keep one)")
		}
		if len(dropped) > 0 && !o.force {
			return runtimeErr("--replace would drop %s on %q; pass --force to proceed", strings.Join(dropped, ", "), spec.name)
		}
	}
	secret, hash, err := newSecret(env)
	if err != nil {
		return err
	}
	body := adminapi.VirtualKeyRequest{
		KeyHash:                    hash,
		BudgetUSD:                  spec.budget,
		BudgetResetIntervalSeconds: spec.resetSeconds,
		BudgetWarnPercent:          spec.warn,
		AllowedModels:              spec.models,
		ExpiresAt:                  spec.expiresText(),
		BillingSubjectID:           spec.billing,
	}
	path := "/admin/virtual_keys/" + url.PathEscape(spec.name)
	if _, err := t.client.doJSON(ctx, http.MethodPost, path, body, nil); err != nil {
		return adminErr(t, err)
	}
	listen := probePersistence(ctx, t, env)
	doc := issued{Verb: "create", Mode: "online", Name: spec.name, Key: secret, KeyHash: hash, ExpiresAt: spec.expiresText()}
	return printIssued(env, o, fmt.Sprintf("Created virtual key %q (online: POST %s).", spec.name, path), doc, listen)
}

func rotateKeyOnline(ctx context.Context, o keysOptions, spec keySpec, t *keysTarget, env IO) error {
	secret, hash, err := newSecret(env)
	if err != nil {
		return err
	}
	body := adminapi.RotateVirtualKeyRequest{NewKeyHash: hash, GracePeriodSeconds: spec.graceSeconds, ExpiresAt: spec.expiresText()}
	path := "/admin/virtual_keys/" + url.PathEscape(spec.name) + "/rotate"
	if _, err := t.client.doJSON(ctx, http.MethodPost, path, body, nil); err != nil {
		var httpErr *adminHTTPError
		switch {
		case errors.As(err, &httpErr) && httpErr.status == http.StatusNotFound:
			return runtimeErr("virtual key %q not found", spec.name)
		case errors.As(err, &httpErr) && httpErr.status == http.StatusConflict:
			return runtimeErr("%v — re-run with --expires to give the rotated key a new expiry", err)
		}
		return adminErr(t, err)
	}
	listen := probePersistence(ctx, t, env)
	grace := "the previous secret stops working immediately"
	if spec.graceSeconds > 0 {
		grace = "the previous secret keeps working for " + (time.Duration(spec.graceSeconds) * time.Second).String()
	}
	g := spec.graceSeconds
	doc := issued{Verb: "rotate", Mode: "online", Name: spec.name, Key: secret, KeyHash: hash, ExpiresAt: spec.expiresText(), GracePeriodSeconds: &g}
	return printIssued(env, o, fmt.Sprintf("Rotated virtual key %q (online: POST %s); %s.", spec.name, path, grace), doc, listen)
}

func deleteKeyOnline(ctx context.Context, o keysOptions, t *keysTarget, env IO) error {
	path := "/admin/virtual_keys/" + url.PathEscape(o.name)
	if _, err := t.client.doJSON(ctx, http.MethodDelete, path, nil, nil); err != nil {
		var httpErr *adminHTTPError
		switch {
		case errors.As(err, &httpErr) && httpErr.status == http.StatusNotFound:
			return runtimeErr("virtual key %q not found", o.name)
		case errors.As(err, &httpErr) && httpErr.status == http.StatusConflict:
			return runtimeErr("the gateway refuses to delete its last virtual key (%v)", err)
		}
		return adminErr(t, err)
	}
	probePersistence(ctx, t, env)
	if o.jsonOut {
		return jsonOut(env, deleted{Verb: "delete", Mode: "online", Name: o.name})
	}
	out := &printer{w: env.Stdout}
	out.printf("Deleted virtual key %q (online: DELETE %s).\n", o.name, path)
	return firstErr(out)
}

// probePersistence does decision 3's one GET /admin/config after a mutation:
// the not-persisted warning when the served config has no store, and the
// listen_addr for the export block. A failure (a 401 for an Operator token on
// rotate, or anything else) never fails the mutation: no warning, no URLs.
func probePersistence(ctx context.Context, t *keysTarget, env IO) (listenAddr string) {
	var served struct {
		ListenAddr string
		Admin      struct{ PersistPath, RedisAddr string }
	}
	if _, err := t.client.getJSON(ctx, "/admin/config", &served); err != nil {
		return ""
	}
	if served.Admin.PersistPath == "" && served.Admin.RedisAddr == "" {
		_, _ = fmt.Fprintln(env.Stderr, persistenceWarning)
	}
	return served.ListenAddr
}
