package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/telemetry/exporterkind"
)

// hashSecret is identity.HashSecret: the key_hash the gateway will match.
func hashSecret(secret string) string { return identity.HashSecret(secret) }

// randomHex draws 32 bytes from r and returns them hex-encoded (64 chars),
// the shape `openssl rand -hex 32` produces.
func randomHex(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	var b [32]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", runtimeErr("generating a random secret: %v", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// renderConfig produces the config.yaml text for plan.
func renderConfig(p *initPlan) (string, error) {
	var y yamlBuilder
	y.comment(0, "Kelvran gateway configuration written by `kelvran init` on "+time.Now().UTC().Format("2006-01-02")+".")
	y.comment(0, "It holds no secret: every credential below is the NAME of an environment")
	y.comment(0, "variable the gateway reads at startup. Keys are annotated in")
	y.comment(0, "gateway/config.example.yaml and docs/reference/config.md.")
	y.str(0, "listen_addr", p.opts.listen)
	y.blank()
	y.comment(0, "The first virtual key. Clients send its SECRET as `Authorization: Bearer`;")
	y.comment(0, "only the SHA-256 digest is stored here, so the secret cannot be recovered")
	y.comment(0, "from this file. Add keys with `kelvran keys create` or by hand.")
	y.section(0, "virtual_keys")
	y.section(1, defaultKeyName)
	y.str(2, "key_hash", p.keyHash)
	if p.opts.budget != "" {
		y.raw(2, "budget_usd", p.opts.budget)
	}
	y.blank()
	y.comment(0, "One deployment per selected provider and model. `model` is the id clients")
	y.comment(0, "send; `upstream_model` is what the provider receives.")
	y.section(0, "deployments")
	for _, d := range p.deployments {
		y.section(1, d.Name)
		y.str(2, "model", d.Model)
		y.str(2, "provider", d.Provider)
		y.str(2, "upstream_model", d.Upstream)
		y.str(2, "base_url", d.BaseURL)
		if d.Insecure {
			y.raw(2, "allow_insecure_http", "true")
		}
		if d.Provider == "bedrock" {
			y.str(2, "access_key_id_env", "AWS_ACCESS_KEY_ID")
			y.str(2, "secret_access_key_env", "AWS_SECRET_ACCESS_KEY")
			if d.SessionTokenEnv {
				y.str(2, "session_token_env", "AWS_SESSION_TOKEN")
			}
			y.str(2, "region", d.Region)
		} else {
			y.str(2, "api_key_env", d.APIKeyEnv)
		}
	}
	y.blank()
	y.comment(0, "Spans and a 60 s metrics dump would otherwise share stdout with the JSON")
	y.comment(0, "logs; set exporter: \"otlp\" and otlp_endpoint: \"host:4318\" to ship them to")
	y.comment(0, "a collector.")
	y.section(0, "telemetry")
	y.str(1, "exporter", exporterkind.None)
	if !p.opts.singleUser {
		y.blank()
		y.comment(0, "Admin API on its own loopback listener (127.0.0.1:8081 by default): key")
		y.comment(0, "management, spend reads, deployment weights. The gateway refuses to start")
		y.comment(0, "unless the named variable holds the admin token.")
		y.section(0, "admin")
		y.str(1, "token_env", adminTokenEnvName)
		if p.persistPath != "" {
			y.comment(1, "Keys created through the admin API survive a restart only with this.")
			y.str(1, "persist_path", p.persistPath)
		}
	}
	y.blank()
	y.comment(0, "USD per token, as of "+priceTableAsOf+", from the providers' pricing pages (and the")
	y.comment(0, "repository's documented example for gpt-4o). Prices change: check them. A model")
	y.comment(0, "absent from this table costs $0 and never decrements a budget.")
	y.section(0, "price_table")
	for _, r := range p.prices {
		y.section(1, r.Model)
		y.raw(2, "prompt_per_token", r.Prompt)
		y.raw(2, "completion_per_token", r.Completion)
		if r.CacheRead != "" {
			y.raw(2, "cache_read_per_token", r.CacheRead)
		}
		if r.CacheWrite != "" {
			y.raw(2, "cache_creation_per_token", r.CacheWrite)
		}
	}
	return y.String()
}

// selfCheck parses the rendered YAML through the gateway's own loader and
// validator and asserts the parsed fields the renderer intended — the
// loader silently drops a non-mapping section, so "it loads" alone is not
// proof (RFC-3 verification (d)).
func selfCheck(yaml string, p *initPlan) error {
	// In memory, never through a temp file: the scratch image has no /tmp,
	// and the rendered document is all the loader needs.
	cfg, err := controlplane.Parse([]byte(yaml), "the generated config")
	if err != nil {
		return runtimeErr("the generated config does not load (a bug in kelvran init, not in your flags): %v", err)
	}
	if err := controlplane.Validate(cfg, adapter.ProviderNames()); err != nil {
		return runtimeErr("the generated config does not validate (a bug in kelvran init): %v", err)
	}
	var problems []string
	if cfg.Telemetry.Exporter != exporterkind.None {
		problems = append(problems, "telemetry.exporter")
	}
	if len(cfg.VirtualKeys) != 1 || cfg.VirtualKeys[0].KeyHash != p.keyHash {
		problems = append(problems, "virtual_keys")
	}
	if p.opts.singleUser && cfg.Admin.TokenEnv != "" {
		problems = append(problems, "admin (unexpected)")
	}
	if !p.opts.singleUser && (cfg.Admin.TokenEnv != adminTokenEnvName || cfg.Admin.PersistPath != p.persistPath) {
		problems = append(problems, "admin")
	}
	if len(cfg.Deployments) != len(p.deployments) {
		problems = append(problems, "deployments")
	}
	for _, d := range cfg.Deployments {
		if _, ok := cfg.PriceTable[d.Model]; !ok {
			problems = append(problems, "price_table."+d.Model)
		}
	}
	if len(problems) > 0 {
		return runtimeErr("the generated config loaded but not as intended (a bug in kelvran init): %v", problems)
	}
	return nil
}

// writeConfig writes yaml to path with mode 0644 (the packaged unit's
// DynamicUser must read it; the file holds no secret). A missing path is
// created with O_EXCL, so a file that appears between the check and the
// write is refused, never clobbered; an existing path is refused unless
// force, and then replaced atomically through a temp file and rename. A
// symlinked path (an /etc config pointing elsewhere) is written through to
// its target and the link is kept.
func writeConfig(path, yaml string, force bool) error {
	fi, err := os.Lstat(path)
	switch {
	case err == nil:
		target := path
		if fi.Mode()&os.ModeSymlink != 0 {
			if target, err = filepath.EvalSymlinks(path); err != nil {
				return runtimeErr("resolving the symlink %s: %v", path, err)
			}
		}
		if st, err := os.Stat(target); err != nil {
			return runtimeErr("checking %s: %v", path, err)
		} else if st.IsDir() {
			return runtimeErr("%s is a directory; --out must name a file", path)
		}
		if !force {
			return runtimeErr("%s already exists; pass --force to overwrite it (the existing file is unchanged)", path)
		}
		return replaceConfig(target, yaml)
	case errors.Is(err, os.ErrNotExist):
		return createConfig(path, yaml)
	default:
		return runtimeErr("checking %s: %v", path, err)
	}
}

// createConfig creates path exclusively (O_EXCL) and writes yaml; a failed
// write removes the partial file. The explicit Chmod makes the mode 0644
// regardless of the caller's umask.
func createConfig(path, yaml string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // G302/G304: the config holds no secret and the packaged unit's DynamicUser must read it; the path is the user's --out
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return runtimeErr("%s already exists; pass --force to overwrite it (the existing file is unchanged)", path)
		}
		return runtimeErr("creating %s: %v", path, err)
	}
	abandon := func(step string, err error) error {
		_ = f.Close()
		_ = os.Remove(path)
		return runtimeErr("%s %s: %v", step, path, err)
	}
	if _, err := f.WriteString(yaml); err != nil {
		return abandon("writing", err)
	}
	if err := f.Chmod(0o644); err != nil { //nolint:gosec // see above
		return abandon("setting the mode of", err)
	}
	if err := f.Sync(); err != nil {
		return abandon("syncing", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return runtimeErr("closing %s: %v", path, err)
	}
	return nil
}

// replaceConfig atomically replaces target (--force): a temp file in the
// target's directory, mode 0644, then rename.
func replaceConfig(target, yaml string) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".kelvran-init-*")
	if err != nil {
		return runtimeErr("creating a temporary file beside %s: %v", target, err)
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	if _, err := tmp.WriteString(yaml); err != nil {
		_ = tmp.Close()
		cleanup()
		return runtimeErr("writing %s: %v", target, err)
	}
	if err := tmp.Chmod(0o644); err != nil { //nolint:gosec // the config holds no secret and the packaged unit's DynamicUser must read it
		_ = tmp.Close()
		cleanup()
		return runtimeErr("setting the mode of %s: %v", target, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return runtimeErr("syncing %s: %v", target, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return runtimeErr("closing %s: %v", target, err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		cleanup()
		return runtimeErr("moving the new file into place at %s: %v", target, err)
	}
	return nil
}

// shellQuote single-quotes s for the printed shell lines. --out is refused
// when it contains a single quote, so no escaping is needed.
func shellQuote(s string) string { return "'" + s + "'" }

// emitInit renders, self-checks, writes (or prints under --dry-run) and
// prints the next steps. Under --dry-run the YAML is the only thing on
// stdout, so it can be piped straight into `kelvran-gateway -validate`.
func emitInit(p *initPlan, env IO) error {
	yaml, err := renderConfig(p)
	if err != nil {
		return runtimeErr("%v", err)
	}
	if err := selfCheck(yaml, p); err != nil {
		return err
	}
	pr := &printer{w: env.Stderr}
	for _, w := range p.warnings {
		pr.println("kelvran init: warning:", w)
	}
	if p.opts.dryRun {
		out := &printer{w: env.Stdout}
		out.print(yaml)
		pr.printf("kelvran init: --dry-run: nothing written; the YAML above is what --out %s would hold. The Next steps below (stderr) carry the generated secret.\n", shellQuote(p.opts.out))
		printNextSteps(pr, p)
		return firstErr(out, pr)
	}
	if err := writeConfig(p.outAbs, yaml, p.opts.force); err != nil {
		return err
	}
	out := &printer{w: env.Stdout}
	out.printf("Wrote %s (mode 0644; it holds no secret).\n", p.outAbs)
	printNextSteps(out, p)
	return firstErr(out, pr)
}

// printer records the first write error so a block of output stays linear;
// the caller checks it once at the end (errcheck would otherwise want every
// Fprintf checked).
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) print(s string) {
	if p.err == nil {
		_, p.err = io.WriteString(p.w, s)
	}
}

func (p *printer) printf(format string, a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, a...)
	}
}

func (p *printer) println(a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintln(p.w, a...)
	}
}

func firstErr(ps ...*printer) error {
	for _, p := range ps {
		if p.err != nil {
			return runtimeErr("writing output: %v", p.err)
		}
	}
	return nil
}

// printNextSteps prints the one place each secret appears: a shell
// assignment to KELVRAN_KEY (and KELVRAN_ADMIN_TOKEN in team mode); every
// other line references the variable, never the literal.
func printNextSteps(w *printer, p *initPlan) {
	w.println()
	w.println("Next steps")
	w.printf("  kelvran-gateway -config %s -validate\n", shellQuote(p.opts.out))
	if !p.opts.singleUser {
		w.printf("  export %s=%s   # the gateway's shell: admin.token_env names it; the gateway refuses to start without it\n", adminTokenEnvName, p.adminToken)
	}
	w.printf("  kelvran-gateway -config %s\n", shellQuote(p.opts.out))
	w.println()
	w.println("  # In the CLIENT's shell (not the gateway's: exporting OPENAI_API_KEY there would replace the upstream credential an openai deployment reads):")
	w.printf("  export %s=%s\n", "KELVRAN_KEY", p.secret)
	w.printf("  export %s=%s\n", "ANTHROPIC_BASE_URL", p.anthropicBase)
	w.printf("  export %s=\"$%s\"   # Claude Code; its native Anthropic Messages mode posts to /v1/messages, served since gateway/v0.19.0\n", "ANTHROPIC_AUTH_TOKEN", "KELVRAN_KEY")
	w.printf("  export %s=%s\n", "OPENAI_BASE_URL", p.openaiBase)
	w.printf("  export %s=\"$%s\"   # OpenAI SDKs and tools; POST /v1/responses (the Responses API) is not served, use Chat Completions\n", "OPENAI_API_KEY", "KELVRAN_KEY")
	w.println()
	w.println("  A pasted export persists in shell history; `read -rs KELVRAN_KEY` then `export KELVRAN_KEY` keeps it out.")
	if p.opts.singleUser {
		w.println("  --single-user wrote no admin section: `kelvran spend` and the live columns of `kelvran status` need one (run init without --single-user).")
	}
	if len(p.deployments) == 1 {
		w.printf("  One model is configured (%s); rerun with --models a,b to serve more.\n", p.deployments[0].Model)
	}
}
