package cli

import (
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adminapi"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// Offline keys (RFC-3 decision 3): the virtual_keys.<name> block of the file
// is rewritten as a text operation that preserves every other byte, and the
// result is accepted only when the loader reads it back as the original plus
// exactly the intended change.

func runKeysOffline(o keysOptions, spec keySpec, t *keysTarget, env IO) error {
	cfg := t.cfg
	if o.verb == "list" {
		if o.spend {
			return runtimeErr("spend lives only in the gateway's budget store, which needs the admin API; this config has no admin token in reach — export %s (or pass --admin-token-file) for a running gateway with an admin: section", adminTokenEnvName)
		}
		entries := make([]adminapi.VirtualKeyListEntry, 0, len(cfg.VirtualKeys))
		for _, vk := range cfg.VirtualKeys {
			entries = append(entries, fileKeyEntry(vk))
		}
		return printKeyList(o, entries, env, fmt.Sprintf("from %s; a running gateway reflects this file only after restart", t.cfgAbs))
	}
	idx := slices.IndexFunc(cfg.VirtualKeys, func(vk controlplane.VirtualKeyConfig) bool { return vk.Name == spec.name })
	data, err := os.ReadFile(t.cfgAbs) //nolint:gosec // G304: the operator's own --config
	if err != nil {
		return runtimeErr("reading %s: %v", t.cfgAbs, err)
	}
	doc, err := parseYAMLDoc(data)
	if err != nil {
		return runtimeErr("%s: %v", t.cfgAbs, err)
	}
	expected := slices.Clone(cfg.VirtualKeys)
	var doc2 *yamlDoc
	var headline string
	var out issued
	var extraCaveats []string
	switch o.verb {
	case "create":
		if idx >= 0 && !o.replace {
			return runtimeErr("virtual key %q already exists in %s; pass --replace to rewrite it (offline --replace = delete + append: every field not on this command line is dropped)", spec.name, t.cfgAbs)
		}
		for _, m := range spec.models {
			if err := checkYAMLKey(m); err != nil {
				return usageErr("--models entry %q cannot be a key in the config parser's subset; create this key online", m)
			}
		}
		secret, hash, err := newSecret(env)
		if err != nil {
			return err
		}
		if idx >= 0 {
			if doc, err = doc.deleteEntry(spec.name); err != nil {
				return runtimeErr("%s: %v", t.cfgAbs, err)
			}
			expected = slices.Delete(expected, idx, idx+1)
		}
		if doc2, err = doc.insertEntry(spec, hash); err != nil {
			return err
		}
		intended := controlplane.VirtualKeyConfig{Name: spec.name, KeyHash: hash, BudgetUSD: spec.budget, BudgetResetIntervalSeconds: spec.resetSeconds, BudgetWarnPercent: spec.warn, AllowedModels: spec.models, BillingSubjectID: spec.billing, ExpiresAt: spec.expiresAt}
		expected = append(expected, intended)
		headline = fmt.Sprintf("Created virtual key %q in %s (offline; takes effect on the next gateway start).", spec.name, t.cfgAbs)
		out = issued{Verb: "create", Mode: "offline", Name: spec.name, Config: t.cfgAbs, Key: secret, KeyHash: hash, ExpiresAt: spec.expiresText()}
	case "rotate":
		if o.grace != "" {
			return usageErr("--grace has no offline meaning: config.yaml holds a single key_hash (no previous_key_hash), so an offline rotation is a hard cut at the next gateway start; rotate online for a grace period")
		}
		if idx < 0 {
			return runtimeErr("virtual key %q not found in %s", spec.name, t.cfgAbs)
		}
		current := cfg.VirtualKeys[idx]
		if !current.ExpiresAt.IsZero() && !current.ExpiresAt.After(time.Now()) && spec.expiresAt.IsZero() {
			return runtimeErr("virtual key %q expired at %s; pass --expires to rotate it, or the fresh secret would still be rejected with key_expired", spec.name, current.ExpiresAt.UTC().Format(time.RFC3339))
		}
		secret, hash, err := newSecret(env)
		if err != nil {
			return err
		}
		if doc2, err = doc.rotateEntry(spec.name, hash, spec.expiresText()); err != nil {
			return runtimeErr("%s: %v", t.cfgAbs, err)
		}
		expected[idx].KeyHash = hash
		if !spec.expiresAt.IsZero() {
			expected[idx].ExpiresAt = spec.expiresAt
		}
		extraCaveats = append(extraCaveats, "offline rotate is a hard cut: the previous secret stops working at the gateway start that picks up this file (config.yaml holds one key_hash; a grace period needs the admin API)")
		headline = fmt.Sprintf("Rotated virtual key %q in %s (offline; a hard cut at the next gateway start).", spec.name, t.cfgAbs)
		g := 0
		out = issued{Verb: "rotate", Mode: "offline", Name: spec.name, Config: t.cfgAbs, Key: secret, KeyHash: hash, ExpiresAt: spec.expiresText(), GracePeriodSeconds: &g}
	default: // delete
		if idx < 0 {
			return runtimeErr("virtual key %q not found in %s", spec.name, t.cfgAbs)
		}
		if len(cfg.VirtualKeys) == 1 {
			return runtimeErr("%q is the only virtual key in %s; the gateway refuses to start without one — create another first", spec.name, t.cfgAbs)
		}
		if doc2, err = doc.deleteEntry(spec.name); err != nil {
			return runtimeErr("%s: %v", t.cfgAbs, err)
		}
		expected = slices.Delete(expected, idx, idx+1)
		headline = fmt.Sprintf("Deleted virtual key %q from %s (offline; takes effect on the next gateway start).", spec.name, t.cfgAbs)
	}
	candidate := doc2.render()
	if err := checkRewrite(cfg, candidate, t.cfgAbs, expected); err != nil {
		return err
	}
	mode, target, err := writeInPlace(t.cfgAbs, candidate)
	if err != nil {
		return runtimeErr("%v", err)
	}
	pr := &printer{w: env.Stderr}
	offlineCaveats(pr, cfg, target, spec.name, extraCaveats)
	if strings.HasPrefix(target, packagedConfigDir) && mode&0o004 == 0 {
		pr.printf("kelvran keys: warning: %s has mode %04o: unreadable by the unit's DynamicUser; ExecStartPre -validate will fail (keep it 0644 — it holds hashes and variable names, never a secret)\n", target, mode)
	}
	if err := firstErr(pr); err != nil {
		return err
	}
	if o.verb == "delete" {
		if o.jsonOut {
			return jsonOut(env, deleted{Verb: "delete", Mode: "offline", Name: spec.name, Config: t.cfgAbs})
		}
		o2 := &printer{w: env.Stdout}
		o2.println(headline)
		return firstErr(o2)
	}
	return printIssued(env, o, headline, out, cfg.ListenAddr)
}

// checkRewrite is the semantic safety check: the candidate must load, every
// Config field but VirtualKeys must be deep-equal to the original's, and
// VirtualKeys must be the original's plus exactly the intended delta. This
// fails closed on every parser quirk — a name Load accepts but re-spells
// included — without enumerating the grammar.
func checkRewrite(orig *controlplane.Config, candidate []byte, source string, expected []controlplane.VirtualKeyConfig) error {
	cand, err := controlplane.Parse(candidate, source)
	if err != nil {
		return runtimeErr("the rewritten file would not load (%s); nothing was written", redactConfigError(err))
	}
	a, b := *orig, *cand
	a.VirtualKeys, b.VirtualKeys = nil, nil
	if !reflect.DeepEqual(a, b) {
		return runtimeErr("the rewrite would change something outside virtual_keys; nothing was written")
	}
	slices.SortFunc(expected, func(x, y controlplane.VirtualKeyConfig) int { return strings.Compare(x.Name, y.Name) })
	if !virtualKeysEqual(expected, cand.VirtualKeys) {
		return runtimeErr("the rewritten virtual_keys differ from the intended change (the name or a value may be one the config parser re-spells); nothing was written — use the online path")
	}
	return nil
}

func virtualKeysEqual(a, b []controlplane.VirtualKeyConfig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Name != y.Name || x.KeyHash != y.KeyHash || !x.BudgetUSD.Equal(y.BudgetUSD) ||
			x.BudgetResetIntervalSeconds != y.BudgetResetIntervalSeconds || x.BudgetWarnPercent != y.BudgetWarnPercent ||
			!slices.Equal(x.AllowedModels, y.AllowedModels) || !slices.Equal(x.AllowedRegions, y.AllowedRegions) ||
			!slices.Equal(x.AllowedSourceCIDRs, y.AllowedSourceCIDRs) || x.RateLimitBurst != y.RateLimitBurst ||
			x.RateLimitRefill != y.RateLimitRefill || x.TPMCapacity != y.TPMCapacity || x.TPMRefillPerSecond != y.TPMRefillPerSecond ||
			!maps.Equal(x.PerModelRateLimits, y.PerModelRateLimits) || x.MaxConcurrentRequests != y.MaxConcurrentRequests ||
			x.BillingSubjectID != y.BillingSubjectID || x.CacheScopeToEndUser != y.CacheScopeToEndUser ||
			x.AttributionIDsDisabled != y.AttributionIDsDisabled || !x.ExpiresAt.Equal(y.ExpiresAt) {
			return false
		}
	}
	return true
}

// offlineCaveats prints decision 3's restart notes on stderr: always the
// next-start note; the store-overrides note when a persisted store is
// configured; the in-memory-only note when an admin: section has no store.
func offlineCaveats(pr *printer, cfg *controlplane.Config, target, name string, extra []string) {
	pr.printf("kelvran keys: %s rewritten; the change takes effect on the next gateway start\n", target)
	switch {
	case cfg.Admin.RedisAddr != "" || cfg.Admin.PersistPath != "":
		var store []string
		if cfg.Admin.RedisAddr != "" {
			store = append(store, "admin.redis_addr "+cfg.Admin.RedisAddr)
		}
		if cfg.Admin.PersistPath != "" {
			store = append(store, "admin.persist_path "+cfg.Admin.PersistPath)
		}
		pr.printf("kelvran keys: if %q was ever created, rotated or deleted through the admin API, the persisted store (%s) overrides this file for it on restart\n", name, strings.Join(store, " / "))
	case cfg.Admin.TokenEnv != "":
		pr.println("kelvran keys: this gateway has no admin.persist_path/redis_addr; keys changed through the admin API are in-memory only and this file wins on restart")
	}
	for _, e := range extra {
		pr.println("kelvran keys:", e)
	}
}
